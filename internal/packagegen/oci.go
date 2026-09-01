package packagegen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

// containerUser is the UID:GID the image runs as, and the owner of the cache
// directory that seeds writable cache volumes.
const containerUser = 65532

// buildOCILayer packs the binary and the seeded cache directory into one
// gzipped layer. The cache directory is world-writable on purpose: cache
// volumes are initialized from it, and documented run commands may remap the
// container user to match a 0600 token file on the host.
func buildOCILayer(binary []byte) (compressed bytes.Buffer, diffID string, err error) {
	var layer bytes.Buffer
	writer := tar.NewWriter(&layer)
	entries := []struct {
		header *tar.Header
		body   []byte
	}{
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "gogs-mcp",
				Mode:     0o755,
				Size:     int64(len(binary)),
				Format:   tar.FormatPAX,
			},
			body: binary,
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "cache/",
				Mode:     0o777,
				Uid:      containerUser,
				Gid:      containerUser,
				ModTime:  time.Unix(0, 0),
				Format:   tar.FormatPAX,
			},
		},
	}
	for _, entry := range entries {
		if err := writer.WriteHeader(entry.header); err != nil {
			return compressed, "", errors.Wrap(err, "write the image layer entry")
		}
		if entry.body != nil {
			if _, err := writer.Write(entry.body); err != nil {
				return compressed, "", errors.Wrap(err, "write the image layer body")
			}
		}
	}
	if err := writer.Close(); err != nil {
		return compressed, "", errors.Wrap(err, "close the image layer")
	}
	layerDigest := sha256.Sum256(layer.Bytes())
	diffID = "sha256:" + hex.EncodeToString(layerDigest[:])

	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(layer.Bytes()); err != nil {
		return compressed, "", errors.Wrap(err, "compress the image layer")
	}
	if err := gzipWriter.Close(); err != nil {
		return compressed, "", errors.Wrap(err, "close the image layer compression")
	}
	return compressed, diffID, nil
}

// blobDescriptor describes one blob of the OCI image layout.
type blobDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

// writeOCIArchive assembles the OCI image layout tar: a single scratch layer
// holding the binary and the cache directory, the image configuration that
// runs as a non-root user, and the manifest and index that tie them together.
func writeOCIArchive(outputPath, reference, version, commit, created string, binary []byte) error {
	layer, diffID, err := buildOCILayer(binary)
	if err != nil {
		return err
	}
	layerDigest := sha256.Sum256(layer.Bytes())
	layerDescriptor := blobDescriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    "sha256:" + hex.EncodeToString(layerDigest[:]),
		Size:      layer.Len(),
	}

	config, err := json.Marshal(map[string]any{
		"created":      created,
		"architecture": "amd64",
		"os":           "linux",
		"config": map[string]any{
			"User":       fmt.Sprintf("%d:%d", containerUser, containerUser),
			"Env":        []string{"GOGS_MCP_CACHE_DIR=/cache"},
			"Entrypoint": []string{"/gogs-mcp"},
			"Cmd":        []string{"serve"},
			"WorkingDir": "/",
			"Volumes":    map[string]any{"/cache": map[string]any{}},
		},
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{diffID},
		},
		"history": []map[string]any{{
			"created":    created,
			"created_by": "gogs-mcp mkpackage",
		}},
	})
	if err != nil {
		return errors.Wrap(err, "encode the image configuration")
	}
	configDigest := sha256.Sum256(config)
	configDescriptor := blobDescriptor{
		MediaType: "application/vnd.oci.image.config.v1+json",
		Digest:    "sha256:" + hex.EncodeToString(configDigest[:]),
		Size:      len(config),
	}

	annotations := map[string]string{
		"org.opencontainers.image.title":    "gogs-mcp",
		"org.opencontainers.image.version":  version,
		"org.opencontainers.image.revision": commit,
		"org.opencontainers.image.created":  created,
	}
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        configDescriptor,
		"layers":        []blobDescriptor{layerDescriptor},
		"annotations":   annotations,
	})
	if err != nil {
		return errors.Wrap(err, "encode the image manifest")
	}
	manifestDigest := sha256.Sum256(manifest)

	indexAnnotations := map[string]string{
		"org.opencontainers.image.ref.name": reference,
	}
	for key, value := range annotations {
		indexAnnotations[key] = value
	}
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{
			"mediaType":   "application/vnd.oci.image.manifest.v1+json",
			"digest":      "sha256:" + hex.EncodeToString(manifestDigest[:]),
			"size":        len(manifest),
			"annotations": indexAnnotations,
		}},
	})
	if err != nil {
		return errors.Wrap(err, "encode the image index")
	}

	blobs := map[string][]byte{
		configDescriptor.Digest:                           config,
		layerDescriptor.Digest:                            layer.Bytes(),
		"sha256:" + hex.EncodeToString(manifestDigest[:]): manifest,
	}
	var archive bytes.Buffer
	archiveWriter := tar.NewWriter(&archive)
	if err := writeBlob(archiveWriter, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`+"\n")); err != nil {
		return err
	}
	if err := writeBlob(archiveWriter, "index.json", index); err != nil {
		return err
	}
	for _, digest := range []string{configDescriptor.Digest, layerDescriptor.Digest, "sha256:" + hex.EncodeToString(manifestDigest[:])} {
		if err := writeBlob(archiveWriter, filepath.Join("blobs", "sha256", strings.TrimPrefix(digest, "sha256:")), blobs[digest]); err != nil {
			return err
		}
	}
	if err := archiveWriter.Close(); err != nil {
		return errors.Wrap(err, "close the image archive")
	}
	return os.WriteFile(outputPath, archive.Bytes(), 0o644)
}

// writeBlob appends one regular file entry with the given contents to the
// layout archive.
func writeBlob(writer *tar.Writer, name string, contents []byte) error {
	if err := writer.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(contents)),
		Format:   tar.FormatPAX,
	}); err != nil {
		return errors.Wrapf(err, "write %s to the image archive", name)
	}
	if _, err := writer.Write(contents); err != nil {
		return errors.Wrapf(err, "write the contents of %s", name)
	}
	return nil
}

// ImageArchive describes the OCI image added to an offline bundle.
type ImageArchive struct {
	Reference string
	Path      string
	SHA256    string
}

// BuildImageArchive writes the container image of the bundle next to the
// binary it packages and returns its identity for the metadata documents.
func BuildImageArchive(packageDir, version, commit, created string) (ImageArchive, error) {
	binary, err := os.ReadFile(filepath.Join(packageDir, "bin", "gogs-mcp"))
	if err != nil {
		return ImageArchive{}, errors.Wrap(err, "read the packaged binary")
	}
	imagesDir := filepath.Join(packageDir, "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		return ImageArchive{}, errors.Wrap(err, "create the images directory")
	}
	path := filepath.Join(imagesDir, fmt.Sprintf("gogs-mcp-%s-linux-amd64-oci.tar", version))
	reference := "gogs-mcp:" + version
	if err := writeOCIArchive(path, reference, version, commit, created, binary); err != nil {
		return ImageArchive{}, err
	}
	digest, err := FileSHA256(path)
	if err != nil {
		return ImageArchive{}, err
	}
	return ImageArchive{Reference: reference, Path: path, SHA256: digest}, nil
}

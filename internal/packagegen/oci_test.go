package packagegen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readLayoutArchive unpacks an OCI layout tar into a map of file contents.
func readLayoutArchive(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(archive))
	files := map[string][]byte{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		require.NoError(t, err)
		contents, err := io.ReadAll(reader)
		require.NoError(t, err)
		files[header.Name] = contents
	}
}

func TestBuildImageArchive(t *testing.T) {
	packageDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(packageDir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(packageDir, "bin", "gogs-mcp"), []byte("binary"), 0o755))

	archive, err := BuildImageArchive(packageDir, "1.2.3", "abc1234", "2026-09-01T00:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, "gogs-mcp:1.2.3", archive.Reference)
	assert.FileExists(t, archive.Path)
	assert.Equal(t, filepath.Join(packageDir, "images", "gogs-mcp-1.2.3-linux-amd64-oci.tar"), archive.Path)
	expectedDigest, err := FileSHA256(archive.Path)
	require.NoError(t, err)
	assert.Equal(t, expectedDigest, archive.SHA256)
}

func TestBuildImageArchiveIsDeterministic(t *testing.T) {
	packageDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(packageDir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(packageDir, "bin", "gogs-mcp"), []byte("binary"), 0o755))

	first, err := BuildImageArchive(packageDir, "1.2.3", "abc1234", "2026-09-01T00:00:00Z")
	require.NoError(t, err)
	firstBytes, err := os.ReadFile(first.Path)
	require.NoError(t, err)

	second, err := BuildImageArchive(packageDir, "1.2.3", "abc1234", "2026-09-01T00:00:00Z")
	require.NoError(t, err)
	secondBytes, err := os.ReadFile(second.Path)
	require.NoError(t, err)

	assert.Equal(t, firstBytes, secondBytes, "the same inputs must produce a byte-identical archive")
}

func TestImageArchiveLayout(t *testing.T) {
	packageDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(packageDir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(packageDir, "bin", "gogs-mcp"), []byte("binary"), 0o755))

	archive, err := BuildImageArchive(packageDir, "1.2.3", "abc1234", "2026-09-01T00:00:00Z")
	require.NoError(t, err)
	archiveBytes, err := os.ReadFile(archive.Path)
	require.NoError(t, err)
	files := readLayoutArchive(t, archiveBytes)

	assert.JSONEq(t, `{"imageLayoutVersion":"1.0.0"}`, string(files["oci-layout"]))
	assert.NotEmpty(t, files["index.json"])

	var index struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	require.NoError(t, json.Unmarshal(files["index.json"], &index))
	require.Len(t, index.Manifests, 1)
	assert.Equal(t, "gogs-mcp:1.2.3", index.Manifests[0].Annotations["org.opencontainers.image.ref.name"])
	assert.Equal(t, "abc1234", index.Manifests[0].Annotations["org.opencontainers.image.revision"])

	manifestPath := "blobs/" + strings.ReplaceAll(index.Manifests[0].Digest, ":", "/")
	manifestBytes, ok := files[manifestPath]
	require.True(t, ok, "the manifest blob must exist in the layout")

	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	require.Len(t, manifest.Layers, 1)

	configPath := "blobs/" + strings.ReplaceAll(manifest.Config.Digest, ":", "/")
	var config struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Config       struct {
			User       string   `json:"User"`
			Entrypoint []string `json:"Entrypoint"`
		} `json:"config"`
		Rootfs struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	require.NoError(t, json.Unmarshal(files[configPath], &config))
	assert.Equal(t, "amd64", config.Architecture)
	assert.Equal(t, "linux", config.OS)
	assert.Equal(t, "65532:65532", config.Config.User)
	assert.Equal(t, []string{"/gogs-mcp"}, config.Config.Entrypoint)

	// The single layer must unpack to exactly the binary and the cache
	// directory, and its uncompressed digest must match the config diff ID.
	layerPath := "blobs/" + strings.ReplaceAll(manifest.Layers[0].Digest, ":", "/")
	layerBytes, ok := files[layerPath]
	require.True(t, ok, "the layer blob must exist in the layout")
	decompressed, err := gzip.NewReader(bytes.NewReader(layerBytes))
	require.NoError(t, err)
	uncompressed, err := io.ReadAll(decompressed)
	require.NoError(t, err)
	diffDigest := sha256.Sum256(uncompressed)
	assert.Equal(t, "sha256:"+hex.EncodeToString(diffDigest[:]), config.Rootfs.DiffIDs[0])

	layerReader := tar.NewReader(bytes.NewReader(uncompressed))
	var entries []string
	for {
		header, err := layerReader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		entries = append(entries, header.Name)
	}
	assert.Equal(t, []string{"gogs-mcp", "cache/"}, entries)
}

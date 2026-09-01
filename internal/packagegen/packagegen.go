// Package packagegen assembles the metadata artifacts of the offline binary
// bundle: a CycloneDX SBOM, the third-party license text, the source
// identity record, and the SHA-256 manifest of the whole package.
package packagegen

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
)

const manifestName = "MANIFEST.sha256"

// BinaryInfo describes the target of a built Linux binary.
type BinaryInfo struct {
	Arch   string
	Static bool
}

// InspectBinary reads the ELF header of a Linux binary and reports its
// machine architecture and whether it is statically linked, so packaging can
// refuse to ship a binary built for the wrong target.
func InspectBinary(path string) (BinaryInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return BinaryInfo{}, errors.Wrap(err, "open binary")
	}
	defer func() { _ = file.Close() }()

	header := make([]byte, 64)
	if _, err := io.ReadFull(file, header); err != nil {
		return BinaryInfo{}, errors.Wrap(err, "read the ELF header")
	}
	if string(header[0:4]) != "\x7fELF" {
		return BinaryInfo{}, errors.New("the binary is not an ELF file")
	}
	if header[4] != 2 || header[5] != 1 {
		return BinaryInfo{}, errors.New("the binary is not a 64-bit little-endian ELF file")
	}
	// ELF64 header layout: e_machine sits at offset 18, after e_type.
	machine := binary.LittleEndian.Uint16(header[18:20])
	if machine != 62 {
		return BinaryInfo{}, errors.Newf("the binary targets machine %d instead of amd64", machine)
	}

	info := BinaryInfo{Arch: "amd64", Static: true}
	programHeaderOffset := binary.LittleEndian.Uint64(header[32:40])
	entrySize := binary.LittleEndian.Uint16(header[54:56])
	entryCount := binary.LittleEndian.Uint16(header[56:58])
	// A malformed entry size below the 4-byte type field would otherwise make
	// the fixed-size read below panic instead of reporting the bad header.
	if entrySize < 4 {
		return BinaryInfo{}, errors.New("the ELF program header entry size is too small")
	}
	typeField := make([]byte, 4)
	for index := uint16(0); index < entryCount; index++ {
		if _, err := file.ReadAt(typeField, int64(programHeaderOffset)+int64(index)*int64(entrySize)); err != nil {
			return BinaryInfo{}, errors.Wrap(err, "read the ELF program headers")
		}
		// Program header entry: type, flags, offset, vaddr, paddr, filesz.
		// Only the type field decides whether an interpreter is requested.
		if binary.LittleEndian.Uint32(typeField) == 3 { // PT_INTERP
			info.Static = false
		}
	}
	return info, nil
}

// Module is one third-party Go module recorded in the vendor manifest.
type Module struct {
	Path    string
	Version string
}

// ParseModulesTxt extracts the vendored modules from a vendor/modules.txt
// file. Module lines start with "# ", so the module list covers exactly what
// the vendored source tree contains.
func ParseModulesTxt(content string) []Module {
	var modules []Module
	for _, line := range strings.Split(content, "\n") {
		path, found := strings.CutPrefix(line, "# ")
		if !found {
			continue
		}
		module, version, _ := strings.Cut(path, " ")
		if module == "" || version == "" {
			continue
		}
		modules = append(modules, Module{Path: module, Version: version})
	}
	return modules
}

// licenseFileNames are the file names recognized as licenses inside a
// vendored module, checked in this order.
var licenseFileNames = []string{
	"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENSE-MIT", "MIT-LICENSE",
	"LICENCE", "LICENCE.md", "LICENCE.txt",
	"COPYING", "COPYING.md", "COPYING.txt",
	"NOTICE", "NOTICE.md", "NOTICE.txt",
}

// CollectLicenses returns the license files of every vendored module. A
// module without a recognized license file is reported with an empty file
// list so the license document can state the gap honestly.
func CollectLicenses(vendorDir string, modules []Module) ([]ModuleLicense, error) {
	collected := make([]ModuleLicense, 0, len(modules))
	for _, module := range modules {
		moduleDir := filepath.Join(vendorDir, filepath.FromSlash(module.Path))
		var files []string
		for _, name := range licenseFileNames {
			candidate := filepath.Join(moduleDir, name)
			info, err := os.Stat(candidate)
			if err != nil || info.IsDir() {
				continue
			}
			files = append(files, name)
		}
		collected = append(collected, ModuleLicense{Module: module, Files: files})
	}
	return collected, nil
}

// ModuleLicense pairs a vendored module with its license file names relative
// to the module directory.
type ModuleLicense struct {
	Module Module
	Files  []string
}

// BuildLicensesText renders the third-party license document: one section per
// vendored module with the full text of every recognized license file.
func BuildLicensesText(vendorDir string, modules []Module) ([]byte, error) {
	licenses, err := CollectLicenses(vendorDir, modules)
	if err != nil {
		return nil, err
	}
	var document strings.Builder
	document.WriteString("Third-party licenses of gogs-mcp\n")
	document.WriteString("=================================\n\n")
	document.WriteString("gogs-mcp vendors every Go dependency it ships. The sections below\n")
	document.WriteString("contain the license text of each vendored module.\n\n")
	for _, license := range licenses {
		fmt.Fprintf(&document, "=== %s %s ===\n", license.Module.Path, license.Module.Version)
		if len(license.Files) == 0 {
			document.WriteString("No license file was found in the vendored copy.\n\n")
			continue
		}
		for _, name := range license.Files {
			contents, err := os.ReadFile(filepath.Join(vendorDir, filepath.FromSlash(license.Module.Path), name))
			if err != nil {
				return nil, errors.Wrapf(err, "read the license file of %s", license.Module.Path)
			}
			fmt.Fprintf(&document, "--- %s ---\n", name)
			document.Write(contents)
			document.WriteString("\n")
		}
		document.WriteString("\n")
	}
	return []byte(document.String()), nil
}

// BuildSBOM renders a minimal but valid CycloneDX 1.5 document listing the
// product, its container image, and every vendored module.
func BuildSBOM(product, version, commit, imageReference string, modules []Module) ([]byte, error) {
	serial, err := randomUUID()
	if err != nil {
		return nil, err
	}
	components := []map[string]any{
		{
			"type":    "application",
			"bom-ref": "pkg:golang/" + product + "@" + version,
			"name":    product,
			"version": version,
			"purl":    "pkg:golang/" + product + "@" + version,
		},
		{
			"type":    "container",
			"bom-ref": "pkg:oci/" + imageReference,
			"name":    imageReference,
			"version": version,
			"purl":    "pkg:oci/" + imageReference,
		},
	}
	for _, module := range modules {
		components = append(components, map[string]any{
			"type":    "library",
			"bom-ref": "pkg:golang/" + module.Path + "@" + module.Version,
			"name":    module.Path,
			"version": module.Version,
			"purl":    "pkg:golang/" + module.Path + "@" + module.Version,
		})
	}
	document := map[string]any{
		"bomFormat":    "CycloneDX",
		"specVersion":  "1.5",
		"serialNumber": "urn:uuid:" + serial,
		"version":      1,
		"metadata": map[string]any{
			"component": map[string]any{"name": product, "version": version, "commit": commit},
		},
		"components": components,
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, errors.Wrap(err, "encode the SBOM")
	}
	return append(encoded, '\n'), nil
}

func randomUUID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.Wrap(err, "generate the SBOM serial number")
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// SourceMetadata records the identity of everything inside one offline
// package so verification can tie the binary, the source archive, and the
// build metadata together.
type SourceMetadata struct {
	Product   string         `json:"product"`
	Version   string         `json:"version"`
	Commit    string         `json:"commit"`
	BuildTime string         `json:"build_time"`
	GoVersion string         `json:"go_version"`
	Target    Target         `json:"target"`
	GogsAPI   string         `json:"gogs_api_target"`
	Binary    Artifact       `json:"binary"`
	Source    Artifact       `json:"source_archive"`
	Image     ContainerImage `json:"container_image"`
	Static    bool           `json:"statically_linked"`
}

// ContainerImage records the reference of the bundled OCI image and the
// archive that carries it.
type ContainerImage struct {
	Reference string   `json:"reference"`
	Archive   Artifact `json:"archive"`
}

// Target is the operating system and architecture the binary was built for.
type Target struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// Artifact is one package file together with its SHA-256 digest.
type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// BuildSourceMetadata renders the SOURCE-METADATA.json document.
func BuildSourceMetadata(metadata SourceMetadata) ([]byte, error) {
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, errors.Wrap(err, "encode the source metadata")
	}
	return append(encoded, '\n'), nil
}

// WriteManifest walks root, hashes every regular file except the manifest
// itself, and writes MANIFEST.sha256 in the format sha256sum verifies. The
// lines follow the lexical walk order, so the manifest of the same tree is
// byte-for-byte stable.
func WriteManifest(root string) error {
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return errors.Wrap(err, "resolve the package file path")
		}
		if relative == manifestName {
			return nil
		}
		digest, err := FileSHA256(path)
		if err != nil {
			return err
		}
		lines = append(lines, digest+"  "+filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "walk the package tree")
	}
	return os.WriteFile(filepath.Join(root, manifestName), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// VerifyManifest checks every entry of root/MANIFEST.sha256 against the tree
// and reports a mismatch by name.
func VerifyManifest(root string) error {
	contents, err := os.ReadFile(filepath.Join(root, manifestName))
	if err != nil {
		return errors.Wrap(err, "read the package manifest")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		digest, path, found := strings.Cut(line, "  ")
		if !found || digest == "" || path == "" {
			return errors.Newf("malformed manifest line %q", line)
		}
		actual, err := FileSHA256(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return errors.Wrapf(err, "the packaged file %s is missing or unreadable", path)
		}
		if actual != digest {
			return errors.Newf("the packaged file %s does not match its SHA-256 digest", path)
		}
	}
	return nil
}

func FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", errors.Wrap(err, "hash the file")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

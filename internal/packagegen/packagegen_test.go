package packagegen

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectBinaryRejectsNonELF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gogs-mcp")
	script := strings.Repeat("#!/bin/sh\necho not a binary\n", 6)
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))

	_, err := InspectBinary(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an ELF file")
}

func TestInspectBinaryRejectsTruncatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gogs-mcp")
	require.NoError(t, os.WriteFile(path, make([]byte, 20), 0o755))

	_, err := InspectBinary(path)
	require.Error(t, err)
}

func TestInspectBinaryClassifiesStaticELF(t *testing.T) {
	tests := map[string]struct {
		machine     uint16
		interpreter bool
		wantErr     string
	}{
		"amd64 static":  {machine: 62},
		"amd64 dynamic": {machine: 62, interpreter: true},
		"wrong machine": {machine: 183, wantErr: "instead of amd64"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gogs-mcp")
			require.NoError(t, os.WriteFile(path, buildELF(t, 2, test.machine, test.interpreter), 0o755))

			info, err := InspectBinary(path)
			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "amd64", info.Arch)
			assert.Equal(t, !test.interpreter, info.Static)
		})
	}
}

func TestInspectBinaryAcceptsStaticPIELayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gogs-mcp")
	require.NoError(t, os.WriteFile(path, buildELF(t, 3, 62, false), 0o755))

	info, err := InspectBinary(path)
	require.NoError(t, err)
	assert.Equal(t, "amd64", info.Arch)
	assert.True(t, info.Static)
}

func TestInspectBinaryRejectsTinyProgramHeaderEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gogs-mcp")
	header := make([]byte, 64)
	copy(header, "\x7fELF")
	header[4] = 2 // 64-bit
	header[5] = 1 // little-endian
	binary.LittleEndian.PutUint16(header[18:20], 62)
	binary.LittleEndian.PutUint64(header[32:40], 64)
	binary.LittleEndian.PutUint16(header[54:56], 2) // smaller than the type field
	binary.LittleEndian.PutUint16(header[56:58], 1)
	require.NoError(t, os.WriteFile(path, append(header, make([]byte, 56)...), 0o755))

	_, err := InspectBinary(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entry size is too small")
}

func TestParseModulesTxt(t *testing.T) {
	modules := ParseModulesTxt(strings.Join([]string{
		"# github.com/cockroachdb/errors v1.12.0",
		"## explicit; go 1.20",
		"github.com/cockroachdb/errors",
		"",
		"# github.com/stretchr/testify v1.11.1",
		"## explicit; go 1.17",
		"github.com/stretchr/testify/require",
		"github.com/stretchr/testify/assert",
	}, "\n"))

	require.Len(t, modules, 2)
	assert.Equal(t, Module{Path: "github.com/cockroachdb/errors", Version: "v1.12.0"}, modules[0])
	assert.Equal(t, Module{Path: "github.com/stretchr/testify", Version: "v1.11.1"}, modules[1])
}

func TestBuildLicensesText(t *testing.T) {
	vendor := t.TempDir()
	writeVendored(t, vendor, "github.com/a/module", map[string]string{
		"LICENSE": "Copyright (c) Module A. MIT license text.",
	})
	writeVendored(t, vendor, "github.com/b/module", nil)

	document, err := BuildLicensesText(vendor, []Module{
		{Path: "github.com/a/module", Version: "v1.0.0"},
		{Path: "github.com/b/module", Version: "v2.0.0"},
	})
	require.NoError(t, err)

	text := string(document)
	assert.Contains(t, text, "=== github.com/a/module v1.0.0 ===")
	assert.Contains(t, text, "Copyright (c) Module A. MIT license text.")
	assert.Contains(t, text, "=== github.com/b/module v2.0.0 ===")
	assert.Contains(t, text, "No license file was found in the vendored copy.")
}

func TestBuildSBOMListsProductAndModules(t *testing.T) {
	document, err := BuildSBOM("gogs-mcp", "1b11910", "1b119106e2", []Module{
		{Path: "github.com/cockroachdb/errors", Version: "v1.12.0"},
	})
	require.NoError(t, err)

	var parsed struct {
		BOMFormat    string `json:"bomFormat"`
		SpecVersion  string `json:"specVersion"`
		SerialNumber string `json:"serialNumber"`
		Components   []struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Version string `json:"version"`
			PURL    string `json:"purl"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(document, &parsed))
	assert.Equal(t, "CycloneDX", parsed.BOMFormat)
	assert.Equal(t, "1.5", parsed.SpecVersion)
	assert.True(t, strings.HasPrefix(parsed.SerialNumber, "urn:uuid:"))
	require.Len(t, parsed.Components, 2)
	assert.Equal(t, "application", parsed.Components[0].Type)
	assert.Equal(t, "gogs-mcp", parsed.Components[0].Name)
	assert.Equal(t, "pkg:golang/github.com/cockroachdb/errors@v1.12.0", parsed.Components[1].PURL)
}

func TestManifestRoundTrip(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin", "gogs-mcp"), []byte("binary"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "SBOM.cdx.json"), []byte("{}"), 0o644))

	require.NoError(t, WriteManifest(root))
	require.NoError(t, VerifyManifest(root))

	manifest, err := os.ReadFile(filepath.Join(root, "MANIFEST.sha256"))
	require.NoError(t, err)
	text := string(manifest)
	assert.Contains(t, text, "bin/gogs-mcp")
	assert.Contains(t, text, "SBOM.cdx.json")
	assert.NotContains(t, text, "MANIFEST.sha256")

	require.NoError(t, os.WriteFile(filepath.Join(root, "bin", "gogs-mcp"), []byte("tampered"), 0o755))
	err = VerifyManifest(root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match its SHA-256 digest")
}

// buildELF assembles a minimal 64-bit little-endian ELF header with an
// optional PT_INTERP program header entry.
func buildELF(t *testing.T, elfType, machine uint16, interpreter bool) []byte {
	t.Helper()
	header := make([]byte, 64)
	copy(header, "\x7fELF")
	header[4] = 2 // 64-bit
	header[5] = 1 // little-endian
	binary.LittleEndian.PutUint16(header[16:18], elfType)
	binary.LittleEndian.PutUint16(header[18:20], machine)
	const entrySize = 56
	entryCount := uint16(0)
	programHeaders := []byte(nil)
	if interpreter {
		entryCount = 1
		programHeaders = make([]byte, entrySize)
		binary.LittleEndian.PutUint32(programHeaders[0:4], 3) // PT_INTERP
	}
	offset := uint64(64)
	binary.LittleEndian.PutUint64(header[32:40], offset)
	binary.LittleEndian.PutUint16(header[54:56], entrySize)
	binary.LittleEndian.PutUint16(header[56:58], entryCount)
	return append(header, programHeaders...)
}

func writeVendored(t *testing.T, vendor, module string, files map[string]string) {
	t.Helper()
	directory := filepath.Join(vendor, filepath.FromSlash(module))
	require.NoError(t, os.MkdirAll(directory, 0o755))
	for name, contents := range files {
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o644))
	}
}

//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogs-mcp/internal/mcpserver"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	integrationOwnerSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	integrationTagSHA    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	integrationCommitSHA = "cccccccccccccccccccccccccccccccccccccccc"
)

type searchContractState struct {
	branchRequests  int
	tagRequests     int
	commitRequests  int
	repoRequests    int
	archiveRequests int
	archiveSHAs     []string
}

func TestSearchCodeUsesResolvedSHAAndImmutableArchive(t *testing.T) {
	const token = "integration-secret-token"
	state := new(searchContractState)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "token "+token, request.Header.Get("Authorization"))
		path := request.URL.Path
		switch {
		case path == "/api/v1/user":
			writeJSON(t, writer, `{"id":7,"login":"tester"}`)
		case path == "/api/v1/repos/owner/private-shared":
			state.repoRequests++
			writeJSON(t, writer, `{"id":1,"name":"private-shared","full_name":"owner/private-shared","default_branch":"trunk","permissions":{"pull":true,"push":false,"admin":false}}`)
		case path == "/api/v1/repos/owner/private-shared/branches/trunk":
			state.branchRequests++
			writeJSON(t, writer, `{"name":"trunk","commit":{"id":"`+integrationOwnerSHA+`"}}`)
		case path == "/api/v1/repos/owner/private-shared/branches/v1.0.0":
			state.branchRequests++
			http.Error(writer, "not found", http.StatusNotFound)
		case path == "/api/v1/repos/owner/private-shared/branches/bbbbbbb":
			state.branchRequests++
			http.Error(writer, "not found", http.StatusNotFound)
		case path == "/api/v1/repos/owner/private-shared/tags":
			state.tagRequests++
			writeJSON(t, writer, `[{"name":"v1.0.0","commit":{"id":"`+integrationTagSHA+`"}}]`)
		case strings.HasPrefix(path, "/api/v1/repos/owner/private-shared/commits/ccccccc"):
			state.commitRequests++
			writeJSON(t, writer, `{"sha":"`+integrationCommitSHA+`","commit":{"message":"Add fixtures"}}`)
		case strings.HasPrefix(path, "/api/v1/repos/owner/private-shared/commits/"):
			state.commitRequests++
			writeJSON(t, writer, `{"sha":"`+integrationTagSHA+`","commit":{"message":"Add fixtures"}}`)
		case strings.HasPrefix(path, "/api/v1/repos/owner/private-shared/branches/ccccccc"):
			state.branchRequests++
			http.Error(writer, "not found", http.StatusNotFound)
		case strings.HasPrefix(path, "/api/v1/repos/owner/private-shared/archive/"):
			state.archiveRequests++
			state.archiveSHAs = append(state.archiveSHAs, strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/repos/owner/private-shared/archive/"), ".tar.gz"))
			archive := integrationArchive(t)
			writer.Header().Set("Content-Type", "application/x-gzip")
			_, err := writer.Write(archive)
			assert.NoError(t, err)
		default:
			t.Errorf("Unexpected Gogs endpoint: %s", request.URL.String())
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	session := connectWithSnapshotCache(t, server.URL+"/api/v1/", token, t.TempDir())

	// An empty ref resolves through the repository default branch.
	empty := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "private-shared", "query": "integration-needle",
	})
	require.Nil(t, empty.Error)
	assertSearchHit(t, empty, integrationOwnerSHA, false)

	// A tag resolves through the tag list when no branch matches.
	tag := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "private-shared", "query": "integration-needle", "ref": "v1.0.0",
	})
	require.Nil(t, tag.Error)
	assertSearchHit(t, tag, integrationTagSHA, false)

	// A short SHA resolves through the tag list; the snapshot is keyed by the
	// full commit SHA, so it is shared with the tag ref.
	short := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "private-shared", "query": "integration-needle", "ref": "bbbbbbb",
	})
	require.Nil(t, short.Error)
	assertSearchHit(t, short, integrationTagSHA, true)

	// An unmatched short SHA falls through to the commit endpoint.
	commit := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "private-shared", "query": "integration-needle", "ref": "ccccccc",
	})
	require.Nil(t, commit.Error)
	assertSearchHit(t, commit, integrationCommitSHA, false)

	// Repeating the empty-ref search hits the published snapshot.
	repeat := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "private-shared", "query": "integration-needle",
	})
	require.Nil(t, repeat.Error)
	assertSearchHit(t, repeat, integrationOwnerSHA, true)

	assert.Equal(t, []string{integrationOwnerSHA, integrationTagSHA, integrationCommitSHA}, state.archiveSHAs)
	assert.Equal(t, 3, state.archiveRequests)
}

func TestSearchCodeMapsUnresolvableRef(t *testing.T) {
	const token = "integration-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		switch {
		case path == "/api/v1/user":
			writeJSON(t, writer, `{"id":7,"login":"tester"}`)
		case path == "/api/v1/repos/owner/private-shared":
			writeJSON(t, writer, `{"id":1,"name":"private-shared","full_name":"owner/private-shared","default_branch":"trunk"}`)
		case strings.HasPrefix(path, "/api/v1/repos/owner/private-shared/branches/"):
			http.Error(writer, "not found", http.StatusNotFound)
		case path == "/api/v1/repos/owner/private-shared/tags":
			writeJSON(t, writer, `[]`)
		default:
			t.Errorf("Unexpected Gogs endpoint: %s", request.URL.String())
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	session := connectWithSnapshotCache(t, server.URL+"/api/v1/", token, t.TempDir())

	response := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "private-shared", "query": "needle", "ref": "does-not-exist",
	})
	require.NotNil(t, response.Error)
	assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", response.Error.Code)
}

func TestSearchCodeSnapshotLayoutIsPrivate(t *testing.T) {
	const token = "integration-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		switch {
		case path == "/api/v1/user":
			writeJSON(t, writer, `{"id":7,"login":"tester"}`)
		case path == "/api/v1/repos/owner/private-shared":
			writeJSON(t, writer, `{"id":1,"name":"private-shared","full_name":"owner/private-shared","default_branch":"trunk"}`)
		case path == "/api/v1/repos/owner/private-shared/branches/trunk":
			writeJSON(t, writer, `{"name":"trunk","commit":{"id":"`+integrationOwnerSHA+`"}}`)
		case strings.HasPrefix(path, "/api/v1/repos/owner/private-shared/archive/"):
			archive := integrationArchive(t)
			_, err := writer.Write(archive)
			assert.NoError(t, err)
		default:
			t.Errorf("Unexpected Gogs endpoint: %s", request.URL.String())
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	// Nest the cache below the temporary root so permission assertions are not
	// affected by the temporary directory's own mode.
	cacheDir := filepath.Join(t.TempDir(), "cache")
	session := connectWithSnapshotCache(t, server.URL+"/api/v1/", token, cacheDir)

	response := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "private-shared", "query": "integration-needle",
	})
	require.Nil(t, response.Error)

	// No credential material may be stored under the cache root, and every
	// directory must be private to the current user.
	require.NoError(t, filepath.WalkDir(cacheDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		require.NoError(t, err)
		assert.LessOrEqual(t, int(info.Mode().Perm()), 0o700, path)
		assert.NotContains(t, path, token)
		return nil
	}))
}

func assertSearchHit(t *testing.T, response mcpserver.ToolResponse[mcpserver.SearchPage], expectedSHA string, expectedCacheHit bool) {
	t.Helper()
	require.NotNil(t, response.Data)
	assert.Equal(t, expectedSHA, response.Data.CommitSHA)
	assert.Equal(t, expectedCacheHit, response.Meta.CacheHit)
	require.Len(t, response.Data.Matches, 1)
	match := response.Data.Matches[0]
	// The archive wrapper directory must not leak into reported paths, and the
	// symlink entry must not have been materialized as a second hit.
	assert.Equal(t, "src/version.txt", match.Path)
	assert.Equal(t, 1, match.Line)
	assert.Equal(t, 1, match.Column)
}

// integrationArchive builds a Gogs-style tar.gz with the wrapper directory and
// a symlink entry, mirroring git archive --prefix output.
func integrationArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(writer)

	content := "integration-needle\n第二行\n"
	require.NoError(t, archive.WriteHeader(&tar.Header{
		Name: "private-shared/", Typeflag: tar.TypeDir,
	}))
	require.NoError(t, archive.WriteHeader(&tar.Header{
		Name: "private-shared/src/", Typeflag: tar.TypeDir,
	}))
	require.NoError(t, archive.WriteHeader(&tar.Header{
		Name: "private-shared/src/version.txt", Typeflag: tar.TypeReg, Size: int64(len(content)),
	}))
	_, err := archive.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, archive.WriteHeader(&tar.Header{
		Name: "private-shared/src/link.txt", Typeflag: tar.TypeSymlink, Linkname: "version.txt",
	}))

	require.NoError(t, archive.Close())
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func callSearchCode(t *testing.T, session *mcp.ClientSession, arguments map[string]any) mcpserver.ToolResponse[mcpserver.SearchPage] {
	t.Helper()
	result := callTool(t, session, "search_code", arguments)
	var response mcpserver.ToolResponse[mcpserver.SearchPage]
	decode(t, result.StructuredContent, &response)
	return response
}

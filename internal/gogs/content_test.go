package gogs

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const contentTestSHA = "0123456789abcdef0123456789abcdef01234567"

func contentFixture(t *testing.T, serveContent http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "token secret-token", r.Header.Get("Authorization"))
		switch {
		case r.URL.Path == "/api/v1/repos/owner/project":
			writeResponse(t, w, `{"default_branch":"main"}`)
		case strings.Contains(r.URL.Path, "/branches/"):
			if strings.HasSuffix(r.URL.Path, "/main") || strings.HasSuffix(r.URL.Path, "/feature/name") {
				writeResponse(t, w, `{"commit":{"id":"`+contentTestSHA+`"}}`)
			} else {
				http.NotFound(w, r)
			}
		case strings.HasSuffix(r.URL.Path, "/tags"):
			writeResponse(t, w, `[{"name":"v1.0.0","commit":{"id":"`+contentTestSHA+`"}}]`)
		case strings.Contains(r.URL.Path, "/commits/"):
			writeResponse(t, w, `{"sha":"`+contentTestSHA+`"}`)
		case strings.HasSuffix(r.URL.Path, "/git/trees/"+contentTestSHA):
			writeResponse(t, w, `{"tree":[{"path":"src","type":"tree","sha":"subtree"}]}`)
		case strings.HasSuffix(r.URL.Path, "/git/trees/subtree"):
			writeResponse(t, w, `{"tree":[{"path":"version.txt","type":"blob","size":12,"sha":"blob"},{"path":"link","type":"blob","size":11,"sha":"link"},{"path":"module","type":"commit","sha":"module"},{"path":"nested","type":"tree","sha":"nested"},{"path":"large.bin","type":"blob","size":16777216,"sha":"large"}]}`)
		case strings.Contains(r.URL.Path, "/contents/"):
			assert.Equal(t, contentTestSHA, r.URL.Query().Get("ref"), "content must be pinned to the metadata commit")
			if serveContent != nil {
				serveContent(w, r)
				return
			}
			switch {
			case strings.HasSuffix(r.URL.Path, "/link"):
				writeResponse(t, w, `{"type":"symlink","target":"version.txt","size":11,"sha":"link"}`)
			case strings.HasSuffix(r.URL.Path, "/module"):
				writeResponse(t, w, `{"type":"submodule","submodule_git_url":"https://example.test/module.git","sha":"module"}`)
			case strings.HasSuffix(r.URL.Path, "/large.bin"):
				_, _ = fmt.Fprint(w, `{"type":"file","size":16777216,"content":"`+strings.Repeat("A", 10<<20)+`"}`)
			default:
				writeResponse(t, w, fmt.Sprintf(`{"type":"file","encoding":"base64","size":12,"path":"src/version.txt","sha":"blob","content":%q}`, base64.StdEncoding.EncodeToString([]byte("hello world\n"))))
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestListDirectoryUsesRepositoryDefaultBranch(t *testing.T) {
	server := contentFixture(t, nil)
	defer server.Close()
	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	entries, ref, err := client.ListDirectory(context.Background(), "owner", "project", "src", "")
	require.NoError(t, err)
	assert.Equal(t, "main", ref)
	require.Len(t, entries, 5)
	assert.Equal(t, "src/version.txt", entries[0].Path)
}
func TestGetFileSupportsBranchTagAndCommitRefs(t *testing.T) {
	server := contentFixture(t, nil)
	defer server.Close()
	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	for _, ref := range []string{"feature/name", "v1.0.0", contentTestSHA} {
		file, err := client.GetFile(context.Background(), "owner", "project", "src/version.txt", ref)
		require.NoError(t, err)
		assert.Equal(t, ref, file.Ref)
		assert.Equal(t, "hello world\n", string(file.Data))
	}
}
func TestListDirectoryMapsAllGogsEntryTypes(t *testing.T) {
	server := contentFixture(t, nil)
	defer server.Close()
	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	entries, _, err := client.ListDirectory(context.Background(), "owner", "project", "src", "main")
	require.NoError(t, err)
	require.Len(t, entries, 5)
	assert.Equal(t, []string{"file", "symlink", "submodule", "dir", "file"}, []string{entries[0].Type, entries[1].Type, entries[2].Type, entries[3].Type, entries[4].Type})
	assert.Equal(t, "version.txt", entries[1].Target)
	assert.Equal(t, "https://example.test/module.git", entries[2].SubmoduleURL)
}
func TestContentPathsAreRejectedBeforeHTTPRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); http.NotFound(w, r) }))
	defer server.Close()
	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	for _, path := range []string{"/absolute/path", "../escape", "src/../escape", "src\\..\\escape", "src/\x00/file", "src//file", "./src"} {
		_, _, err := client.ListDirectory(context.Background(), "owner", "project", path, "")
		require.Error(t, err)
		assert.Equal(t, CodeValidationFailed, AsError(err).Code)
		_, err = client.GetFile(context.Background(), "owner", "project", path, "")
		require.Error(t, err)
		assert.Equal(t, CodeValidationFailed, AsError(err).Code)
	}
	_, err := client.GetFile(context.Background(), "owner", "project", "", "")
	require.Error(t, err)
	assert.Zero(t, requests.Load())
}
func TestGetFileRejectsDirectoryResponse(t *testing.T) {
	server := contentFixture(t, nil)
	defer server.Close()
	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.GetFile(context.Background(), "owner", "project", "src", "main")
	require.Error(t, err)
	assert.Equal(t, CodeValidationFailed, AsError(err).Code)
}
func TestGetLargeFileReturnsMetadataWithoutDownloadingPayload(t *testing.T) {
	var written atomic.Int64
	server := contentFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"type":"file","size":16777216,"content":"`))
		w.(http.Flusher).Flush()
		for range 256 {
			n, err := w.Write([]byte(strings.Repeat("A", 64<<10)))
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	defer server.Close()
	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	file, err := client.GetFile(context.Background(), "owner", "project", "src/large.bin", "main")
	require.NoError(t, err)
	assert.Equal(t, int64(16<<20), file.Size)
	assert.Empty(t, file.Data)
	assert.Equal(t, "large", file.SHA)
	server.Close()
	assert.Less(t, written.Load(), int64(16<<20))
}

func TestDirectoryPageFetchesMetadataOnlyForSelectedEntries(t *testing.T) {
	var requests atomic.Int32
	server := contentFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.True(t, strings.HasSuffix(r.URL.Path, "/link"))
		writeResponse(t, w, `{"type":"symlink","target":"version.txt","size":11}`)
	})
	defer server.Close()
	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	entries, ref, total, err := client.ListDirectoryPage(context.Background(), "owner", "project", "src", "main", 2, 1)
	require.NoError(t, err)
	assert.Equal(t, "main", ref)
	assert.Equal(t, 5, total)
	require.Len(t, entries, 1)
	assert.Equal(t, "src/link", entries[0].Path)
	assert.Equal(t, "symlink", entries[0].Type)
	assert.Equal(t, int32(1), requests.Load())
	entries, _, total, err = client.ListDirectoryPage(context.Background(), "owner", "project", "src", "main", 999, 1)
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.Equal(t, 5, total)
	assert.Equal(t, int32(1), requests.Load())
}

package gogs

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListDirectoryUsesRepositoryDefaultBranch(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		switch request.URL.Path {
		case "/api/v1/repos/owner/project":
			assert.Empty(t, request.URL.RawQuery)
			writeResponse(t, writer, `{"name":"project","full_name":"owner/project","owner":{"login":"owner"},"default_branch":"main"}`)
		case "/api/v1/repos/owner/project/contents/src":
			assert.Equal(t, "main", request.URL.Query().Get("ref"))
			writeResponse(t, writer, `[{"type":"file","size":12,"name":"main.go","path":"src/main.go","sha":"blob-sha"}]`)
		default:
			t.Errorf("Unexpected request path: %s", request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	entries, ref, err := client.ListDirectory(context.Background(), "owner", "project", "src", "")
	require.NoError(t, err)
	assert.Equal(t, "main", ref)
	require.Len(t, entries, 1)
	assert.Equal(t, "src/main.go", entries[0].Path)
	assert.Equal(t, int32(2), requests.Load())
}

func TestGetFileSupportsBranchTagAndCommitRefs(t *testing.T) {
	refs := []string{"feature/name", "v1.0.0", "0123456789abcdef0123456789abcdef01234567"}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		index := int(requests.Add(1) - 1)
		if !assert.Less(t, index, len(refs)) {
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
			return
		}
		assert.Equal(t, "/api/v1/repos/owner/project/contents/src/version.txt", request.URL.Path)
		assert.Equal(t, refs[index], request.URL.Query().Get("ref"))
		content := base64.StdEncoding.EncodeToString([]byte(refs[index] + "\n"))
		writeResponse(t, writer, fmt.Sprintf(`{"type":"file","encoding":"base64","size":%d,"name":"version.txt","path":"src/version.txt","sha":"sha-%d","content":%q}`, len(refs[index])+1, index, content))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	for _, ref := range refs {
		file, err := client.GetFile(context.Background(), "owner", "project", "src/version.txt", ref)
		require.NoError(t, err)
		assert.Equal(t, ref, file.Ref)
		assert.Equal(t, ref+"\n", string(file.Data))
	}
	assert.Equal(t, int32(3), requests.Load())
}

func TestListDirectoryMapsAllGogsEntryTypes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeResponse(t, writer, `[
			{"type":"file","size":10,"name":"file.txt","path":"src/file.txt","sha":"file-sha"},
			{"type":"dir","size":0,"name":"nested","path":"src/nested","sha":"tree-sha"},
			{"type":"symlink","size":8,"name":"link","path":"src/link","sha":"link-sha","target":"file.txt"},
			{"type":"submodule","size":0,"name":"module","path":"src/module","sha":"module-sha","submodule_git_url":"https://example.test/module.git"}
		]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	entries, ref, err := client.ListDirectory(context.Background(), "owner", "project", "src", "main")
	require.NoError(t, err)
	assert.Equal(t, "main", ref)
	require.Len(t, entries, 4)
	assert.Equal(t, []string{"file", "dir", "symlink", "submodule"}, []string{entries[0].Type, entries[1].Type, entries[2].Type, entries[3].Type})
	assert.Equal(t, "file.txt", entries[2].Target)
	assert.Equal(t, "https://example.test/module.git", entries[3].SubmoduleURL)
}

func TestContentPathsAreRejectedBeforeHTTPRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)

	invalidPaths := []string{
		"/absolute/path",
		"../escape",
		"src/../escape",
		"src\\..\\escape",
		"src/\x00/file",
		"src//file",
		"./src",
	}
	for _, repositoryPath := range invalidPaths {
		t.Run(repositoryPath, func(t *testing.T) {
			_, _, err := client.ListDirectory(context.Background(), "owner", "project", repositoryPath, "")
			require.Error(t, err)
			assert.Equal(t, CodeValidationFailed, AsError(err).Code)
			_, err = client.GetFile(context.Background(), "owner", "project", repositoryPath, "")
			require.Error(t, err)
			assert.Equal(t, CodeValidationFailed, AsError(err).Code)
		})
	}
	_, err := client.GetFile(context.Background(), "owner", "project", "", "")
	require.Error(t, err)
	assert.Equal(t, CodeValidationFailed, AsError(err).Code)
	assert.Zero(t, requests.Load())
}

func TestGetFileRejectsDirectoryResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeResponse(t, writer, `[]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.GetFile(context.Background(), "owner", "project", "src", "main")
	require.Error(t, err)
	assert.Equal(t, CodeValidationFailed, AsError(err).Code)
}

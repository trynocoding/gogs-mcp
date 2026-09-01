//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gogs-mcp/internal/gogs"
	"gogs-mcp/internal/mcpserver"
	"gogs-mcp/internal/snapshot"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type contractState struct {
	listRequests    atomic.Int32
	detailRequests  atomic.Int32
	contentRequests atomic.Int32
}

func TestRepositoryToolsUseGogsV0142Contracts(t *testing.T) {
	const token = "integration-secret-token"
	state := new(contractState)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "token "+token, request.Header.Get("Authorization"))
		assert.Equal(t, "gogs-mcp/integration", request.Header.Get("User-Agent"))
		switch request.URL.Path {
		case "/api/v1/user/repos":
			state.listRequests.Add(1)
			assert.Empty(t, request.URL.RawQuery)
			writeJSON(t, writer, `[
				{"id":2,"name":"owned","full_name":"collaborator/owned","owner":{"login":"collaborator"},"description":"Owned repository","default_branch":"main","private":true,"clone_url":"http://gogs/collaborator/owned.git","html_url":"http://gogs/collaborator/owned","permissions":{"pull":true,"push":true,"admin":true}},
				{"id":1,"name":"private-shared","full_name":"owner/private-shared","owner":{"login":"owner"},"description":"Needle collaborator project","default_branch":"trunk","private":true,"clone_url":"http://gogs/owner/private-shared.git","html_url":"http://gogs/owner/private-shared","permissions":{"pull":true,"push":false,"admin":false}}
			]`)
		case "/api/v1/repos/owner/private-shared":
			state.detailRequests.Add(1)
			writeJSON(t, writer, `{"id":1,"name":"private-shared","full_name":"owner/private-shared","owner":{"login":"owner"},"description":"Needle collaborator project","default_branch":"trunk","private":true,"clone_url":"http://gogs/owner/private-shared.git","html_url":"http://gogs/owner/private-shared","permissions":{"pull":true,"push":false,"admin":false}}`)
		case "/api/v1/repos/owner/missing":
			state.detailRequests.Add(1)
			http.Error(writer, "not found", http.StatusNotFound)
		case "/api/v1/repos/owner/private-shared/contents/src":
			state.contentRequests.Add(1)
			assert.Equal(t, "trunk", request.URL.Query().Get("ref"))
			writeJSON(t, writer, `[
				{"type":"symlink","size":11,"name":"link","path":"src/link","sha":"link-sha","target":"version.txt"},
				{"type":"file","size":17,"name":"version.txt","path":"src/version.txt","sha":"version-sha"}
			]`)
		case "/api/v1/repos/owner/private-shared/contents/src/version.txt":
			state.contentRequests.Add(1)
			assert.Equal(t, "v1.0.0", request.URL.Query().Get("ref"))
			encoded := base64.StdEncoding.EncodeToString([]byte("first\n你好\nthird\n"))
			writeJSON(t, writer, fmt.Sprintf(`{"type":"file","encoding":"base64","size":19,"name":"version.txt","path":"src/version.txt","sha":"version-sha","content":%q}`, encoded))
		default:
			t.Errorf("Unexpected Gogs endpoint: %s", request.URL.String())
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	session := connect(t, server.URL+"/api/v1/", token)

	list := callPage(t, session, "list_repositories", map[string]any{})
	assert.Equal(t, []string{"collaborator/owned", "owner/private-shared"}, fullNames(list.Data.Repositories))
	assert.Equal(t, 1, list.Data.Page)
	assert.Equal(t, 30, list.Data.PerPage)

	search := callPage(t, session, "search_repositories", map[string]any{"query": "nEeDlE"})
	require.Len(t, search.Data.Repositories, 1)
	assert.Equal(t, "owner/private-shared", search.Data.Repositories[0].FullName)
	assert.True(t, search.Data.Repositories[0].Private)

	detailResult := callTool(t, session, "get_repository", map[string]any{"owner": "owner", "repo": "private-shared"})
	assert.False(t, detailResult.IsError)
	var detail mcpserver.ToolResponse[mcpserver.Repository]
	decode(t, detailResult.StructuredContent, &detail)
	require.NotNil(t, detail.Data)
	assert.Equal(t, "trunk", detail.Data.DefaultBranch)
	assert.True(t, detail.Data.Permissions.Pull)
	assert.False(t, detail.Data.Permissions.Push)
	assert.False(t, detail.Data.Permissions.Admin)

	missingResult := callTool(t, session, "get_repository", map[string]any{"owner": "owner", "repo": "missing"})
	assert.True(t, missingResult.IsError)
	var missing mcpserver.ToolResponse[mcpserver.Repository]
	decode(t, missingResult.StructuredContent, &missing)
	require.NotNil(t, missing.Error)
	assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", missing.Error.Code)
	assert.Equal(t, "The repository does not exist or the current user cannot access it.", missing.Error.Message)

	directoryResult := callTool(t, session, "list_directory", map[string]any{
		"owner": "owner", "repo": "private-shared", "path": "src",
	})
	assert.False(t, directoryResult.IsError)
	var directory mcpserver.ToolResponse[mcpserver.DirectoryPage]
	decode(t, directoryResult.StructuredContent, &directory)
	require.NotNil(t, directory.Data)
	assert.Equal(t, "trunk", directory.Data.Ref)
	require.Len(t, directory.Data.Entries, 2)
	assert.Equal(t, "src/link", directory.Data.Entries[0].Path)
	assert.Equal(t, "symlink", directory.Data.Entries[0].Type)

	fileResult := callTool(t, session, "get_file", map[string]any{
		"owner": "owner", "repo": "private-shared", "path": "src/version.txt", "ref": "v1.0.0", "start_line": 2, "line_count": 1,
	})
	assert.False(t, fileResult.IsError)
	var file mcpserver.ToolResponse[mcpserver.File]
	decode(t, fileResult.StructuredContent, &file)
	require.NotNil(t, file.Data)
	assert.Equal(t, "你好", file.Data.Content)
	assert.Equal(t, 2, file.Data.StartLine)
	assert.Equal(t, 2, file.Data.EndLine)
	assert.True(t, file.Meta.Truncated)
	require.NotNil(t, file.Meta.NextStartLine)
	assert.Equal(t, 3, *file.Meta.NextStartLine)

	requestsBeforeInvalidPath := state.detailRequests.Load() + state.contentRequests.Load()
	invalidPathResult := callTool(t, session, "get_file", map[string]any{
		"owner": "owner", "repo": "private-shared", "path": "../secret",
	})
	assert.True(t, invalidPathResult.IsError)
	assert.Equal(t, requestsBeforeInvalidPath, state.detailRequests.Load()+state.contentRequests.Load())

	assert.Equal(t, int32(2), state.listRequests.Load())
	assert.Equal(t, int32(3), state.detailRequests.Load())
	assert.Equal(t, int32(2), state.contentRequests.Load())
}

func connect(t *testing.T, apiRoot, token string) *mcp.ClientSession {
	t.Helper()
	return connectWithSnapshotCache(t, apiRoot, token, "")
}

// connectWithSnapshotCache wires an in-memory server against a real snapshot
// manager rooted at cacheDir, mirroring the production assembly.
func connectWithSnapshotCache(t *testing.T, apiRoot, token, cacheDir string) *mcp.ClientSession {
	t.Helper()
	parsed, err := url.Parse(apiRoot)
	require.NoError(t, err)
	client, err := gogs.NewClient(gogs.Options{
		APIRoot:   parsed,
		Token:     token,
		Timeout:   time.Second,
		UserAgent: "gogs-mcp/integration",
	})
	require.NoError(t, err)

	var snapshots *snapshot.Manager
	if cacheDir != "" {
		instance := *parsed
		instance.Path = strings.TrimSuffix(parsed.Path, "/api/v1/")
		snapshots, err = snapshot.NewManager(cacheDir, instance.String(), snapshot.DefaultLimits(), snapshot.DefaultEviction())
		require.NoError(t, err)
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := mcpserver.New(client, snapshots, slog.New(slog.NewTextHandler(io.Discard, nil)), mcpserver.DefaultSearchDefaults())
	serverSession, err := server.MCP().Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, serverSession.Close())
	})

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "test"}, nil)
	session, err := mcpClient.Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, session.Close())
	})
	return session
}

func callPage(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) mcpserver.ToolResponse[mcpserver.RepositoryPage] {
	t.Helper()
	result := callTool(t, session, name, arguments)
	require.False(t, result.IsError)
	var response mcpserver.ToolResponse[mcpserver.RepositoryPage]
	decode(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	require.NoError(t, err)
	return result
}

func decode(t *testing.T, source, destination any) {
	t.Helper()
	encoded, err := json.Marshal(source)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, destination))
}

func fullNames(repositories []mcpserver.Repository) []string {
	names := make([]string, len(repositories))
	for index, repository := range repositories {
		names[index] = repository.FullName
	}
	return names
}

func writeJSON(t *testing.T, writer http.ResponseWriter, body string) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	_, err := fmt.Fprint(writer, body)
	assert.NoError(t, err)
}

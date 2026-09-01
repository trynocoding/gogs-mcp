//go:build integration

package integration

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gogs-mcp/internal/mcpserver"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitToolsUseGogsV0142Contracts(t *testing.T) {
	const token = "integration-secret-token"
	var branchListRequests, branchDetailRequests, commitListRequests, commitDetailRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "token "+token, request.Header.Get("Authorization"))
		switch request.URL.Path {
		case "/api/v1/repos/owner/private-shared/branches":
			branchListRequests.Add(1)
			assert.Empty(t, request.URL.RawQuery)
			writeJSON(t, writer, `[
				{"name":"main","commit":{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","message":"Change main version","author":{"name":"Alice","email":"alice@example.test","date":"2026-09-01T00:00:00Z"}}},
				{"name":"feature/content","commit":{"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","message":"Change feature version","author":{"name":"Alice","email":"alice@example.test","date":"2026-08-31T00:00:00Z"}}}
			]`)
		case "/api/v1/repos/owner/private-shared/branches/feature/content":
			branchDetailRequests.Add(1)
			assert.Empty(t, request.URL.RawQuery)
			writeJSON(t, writer, `{"name":"feature/content","commit":{"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","message":"Change feature version","author":{"name":"Alice","email":"alice@example.test","date":"2026-08-31T00:00:00Z"}}}`)
		case "/api/v1/repos/owner/private-shared/branches/missing":
			branchDetailRequests.Add(1)
			http.Error(writer, "not found", http.StatusNotFound)
		case "/api/v1/repos/owner/private-missing/branches":
			branchListRequests.Add(1)
			http.Error(writer, "not found", http.StatusNotFound)
		case "/api/v1/repos/owner/private-shared/commits":
			commitListRequests.Add(1)
			assert.Equal(t, "pageSize=30", request.URL.RawQuery)
			writeJSON(t, writer, `[
				{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","html_url":"http://gogs/owner/private-shared/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","commit":{"message":"Change main version","author":{"name":"Alice","email":"alice@example.test","date":"2026-09-01T00:00:00Z"}},"parents":[]}
			]`)
		case "/api/v1/repos/owner/private-shared/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":
			commitDetailRequests.Add(1)
			assert.Empty(t, request.URL.RawQuery)
			writeJSON(t, writer, `{
				"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"html_url":"http://gogs/owner/private-shared/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"commit":{"message":"Change main version","author":{"name":"Alice","email":"alice@example.test","date":"2026-09-01T00:00:00Z"},"committer":{"name":"Alice","email":"alice@example.test","date":"2026-09-01T00:00:01Z"}},
				"parents":[{"sha":"cccccccccccccccccccccccccccccccccccccccc"}]
			}`)
		case "/api/v1/repos/owner/private-shared/commits/missing":
			commitDetailRequests.Add(1)
			http.Error(writer, "not found", http.StatusNotFound)
		case "/api/v1/repos/owner/private-missing/commits":
			commitListRequests.Add(1)
			http.Error(writer, "not found", http.StatusNotFound)
		default:
			t.Errorf("Unexpected Gogs endpoint: %s", request.URL.String())
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	session := connect(t, server.URL+"/api/v1/", token)

	branches := callBranchPage(t, session, map[string]any{"owner": "owner", "repo": "private-shared"})
	assert.Equal(t, []string{"feature/content", "main"}, branchNames(branches.Data.Branches))
	assert.Equal(t, 1, branches.Data.Page)
	assert.Equal(t, 30, branches.Data.PerPage)
	assert.Equal(t, 2, branches.Data.Total)

	branch := callBranch(t, session, map[string]any{"owner": "owner", "repo": "private-shared", "branch": "feature/content"})
	assert.Equal(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", branch.Data.HeadSHA)

	commits := callCommitPage(t, session, map[string]any{"owner": "owner", "repo": "private-shared"})
	assert.Equal(t, 30, commits.Data.Limit)
	require.Len(t, commits.Data.Commits, 1)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", commits.Data.Commits[0].SHA)
	assert.Equal(t, "Change main version", commits.Data.Commits[0].Message)
	assert.Equal(t, "Alice", commits.Data.Commits[0].AuthorName)

	commit := callCommit(t, session, map[string]any{"owner": "owner", "repo": "private-shared", "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", commit.Data.SHA)
	assert.Equal(t, "http://gogs/owner/private-shared/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", commit.Data.WebURL)
	assert.Equal(t, "2026-09-01T00:00:01Z", commit.Data.Committer.Date)
	assert.Equal(t, []string{"cccccccccccccccccccccccccccccccccccccccc"}, commit.Data.ParentSHAs)

	missingBranchList := callTool(t, session, "list_branches", map[string]any{"owner": "owner", "repo": "private-missing"})
	assert.True(t, missingBranchList.IsError)
	var missingBranchListResponse mcpserver.ToolResponse[mcpserver.BranchPage]
	decode(t, missingBranchList.StructuredContent, &missingBranchListResponse)
	require.NotNil(t, missingBranchListResponse.Error)
	assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", missingBranchListResponse.Error.Code)

	missingCommitList := callTool(t, session, "list_commits", map[string]any{"owner": "owner", "repo": "private-missing"})
	assert.True(t, missingCommitList.IsError)
	var missingCommitListResponse mcpserver.ToolResponse[mcpserver.CommitPage]
	decode(t, missingCommitList.StructuredContent, &missingCommitListResponse)
	require.NotNil(t, missingCommitListResponse.Error)
	assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", missingCommitListResponse.Error.Code)

	missingBranch := callTool(t, session, "get_branch", map[string]any{"owner": "owner", "repo": "private-shared", "branch": "missing"})
	assert.True(t, missingBranch.IsError)
	var missingBranchResponse mcpserver.ToolResponse[mcpserver.Branch]
	decode(t, missingBranch.StructuredContent, &missingBranchResponse)
	require.NotNil(t, missingBranchResponse.Error)
	assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", missingBranchResponse.Error.Code)

	missingCommit := callTool(t, session, "get_commit", map[string]any{"owner": "owner", "repo": "private-shared", "sha": "missing"})
	assert.True(t, missingCommit.IsError)
	var missingCommitResponse mcpserver.ToolResponse[mcpserver.Commit]
	decode(t, missingCommit.StructuredContent, &missingCommitResponse)
	require.NotNil(t, missingCommitResponse.Error)
	assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", missingCommitResponse.Error.Code)

	assert.Equal(t, int32(2), branchListRequests.Load())
	assert.Equal(t, int32(2), branchDetailRequests.Load())
	assert.Equal(t, int32(2), commitListRequests.Load())
	assert.Equal(t, int32(2), commitDetailRequests.Load())
}

func callBranchPage(t *testing.T, session *mcp.ClientSession, arguments map[string]any) mcpserver.ToolResponse[mcpserver.BranchPage] {
	t.Helper()
	result := callTool(t, session, "list_branches", arguments)
	require.False(t, result.IsError)
	var response mcpserver.ToolResponse[mcpserver.BranchPage]
	decode(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func callBranch(t *testing.T, session *mcp.ClientSession, arguments map[string]any) mcpserver.ToolResponse[mcpserver.Branch] {
	t.Helper()
	result := callTool(t, session, "get_branch", arguments)
	require.False(t, result.IsError)
	var response mcpserver.ToolResponse[mcpserver.Branch]
	decode(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func callCommitPage(t *testing.T, session *mcp.ClientSession, arguments map[string]any) mcpserver.ToolResponse[mcpserver.CommitPage] {
	t.Helper()
	result := callTool(t, session, "list_commits", arguments)
	require.False(t, result.IsError)
	var response mcpserver.ToolResponse[mcpserver.CommitPage]
	decode(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func callCommit(t *testing.T, session *mcp.ClientSession, arguments map[string]any) mcpserver.ToolResponse[mcpserver.Commit] {
	t.Helper()
	result := callTool(t, session, "get_commit", arguments)
	require.False(t, result.IsError)
	var response mcpserver.ToolResponse[mcpserver.Commit]
	decode(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func branchNames(branches []mcpserver.Branch) []string {
	names := make([]string, len(branches))
	for index, branch := range branches {
		names[index] = branch.Name
	}
	return names
}

package mcpserver

import (
	"context"
	"strings"
	"testing"

	"gogs-mcp/internal/gogs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListBranchesSortsAndPaginates(t *testing.T) {
	client := &fakeClient{branches: []gogs.Branch{
		{Name: "main", HeadSHA: "main-sha"},
		{Name: "feature/content", HeadSHA: "feature-sha"},
		{Name: "develop", HeadSHA: "develop-sha"},
	}}
	session := connectTestClient(t, client)

	first := callBranchPage(t, session, map[string]any{"owner": "owner", "repo": "project", "per_page": 2})
	assert.Equal(t, 1, first.Data.Page)
	assert.Equal(t, 2, first.Data.PerPage)
	assert.Equal(t, 3, first.Data.Total)
	assert.Equal(t, []string{"develop", "feature/content"}, branchNames(first.Data.Branches))
	require.NotNil(t, first.Meta.NextPage)
	assert.Equal(t, 2, *first.Meta.NextPage)

	second := callBranchPage(t, session, map[string]any{"owner": "owner", "repo": "project", "page": 2, "per_page": 2})
	assert.Equal(t, []string{"main"}, branchNames(second.Data.Branches))
	assert.Nil(t, second.Meta.NextPage)
}

func TestGetBranchPassesSlashSeparatedName(t *testing.T) {
	client := &fakeClient{branch: gogs.Branch{Name: "feature/content", HeadSHA: "feature-sha"}}
	session := connectTestClient(t, client)

	response := callBranch(t, session, map[string]any{"owner": "owner", "repo": "project", "branch": "feature/content"})

	assert.Equal(t, "feature/content", response.Data.Name)
	assert.Equal(t, "feature-sha", response.Data.HeadSHA)
	assert.Equal(t, "feature/content", client.branchName)
}

func TestListCommitsAppliesDefaultLimitAndShortensLongMessages(t *testing.T) {
	client := &fakeClient{commits: []gogs.Commit{
		{
			SHA:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Message: strings.Repeat("long ", 60),
			Author:  gogs.CommitPerson{Name: "Alice", Email: "alice@example.test", Date: "2026-09-01T00:00:00Z"},
		},
		{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Message: "Short subject"},
	}}
	session := connectTestClient(t, client)

	response := callCommitPage(t, session, map[string]any{"owner": "owner", "repo": "project"})

	assert.Equal(t, 30, client.commitLimit)
	assert.Equal(t, 30, response.Data.Limit)
	require.Len(t, response.Data.Commits, 2)
	assert.Len(t, []rune(response.Data.Commits[0].Message), maximumCommitSummaryRunes)
	assert.Equal(t, "Alice", response.Data.Commits[0].AuthorName)
	assert.Equal(t, "2026-09-01T00:00:00Z", response.Data.Commits[0].AuthorDate)
	assert.Equal(t, "Short subject", response.Data.Commits[1].Message)
	assert.True(t, response.Meta.Truncated)
	require.Len(t, response.Meta.Warnings, 1)
	assert.Contains(t, response.Meta.Warnings[0], "get_commit")
}

func TestListCommitsRejectsLimitAboveMaximum(t *testing.T) {
	client := &fakeClient{}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_commits",
		Arguments: map[string]any{
			"owner": "owner", "repo": "project", "limit": 101,
		},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Zero(t, client.commitCalls)
}

func TestListCommitsTruncationBoundary(t *testing.T) {
	testCases := []struct {
		name      string
		runes     int
		truncated bool
	}{
		{name: "exactly the maximum", runes: maximumCommitSummaryRunes, truncated: false},
		{name: "one over the maximum", runes: maximumCommitSummaryRunes + 1, truncated: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			client := &fakeClient{commits: []gogs.Commit{{
				SHA:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Message: strings.Repeat("x", testCase.runes),
			}}}
			session := connectTestClient(t, client)

			response := callCommitPage(t, session, map[string]any{"owner": "owner", "repo": "project"})

			require.Len(t, response.Data.Commits, 1)
			assert.Len(t, []rune(response.Data.Commits[0].Message), maximumCommitSummaryRunes)
			assert.Equal(t, testCase.truncated, response.Meta.Truncated)
			if testCase.truncated {
				require.Len(t, response.Meta.Warnings, 1)
			} else {
				assert.Empty(t, response.Meta.Warnings)
			}
		})
	}
}

func TestListBranchesPaginationBeyondLastPage(t *testing.T) {
	client := &fakeClient{branches: []gogs.Branch{
		{Name: "main", HeadSHA: "main-sha"},
		{Name: "develop", HeadSHA: "develop-sha"},
		{Name: "feature/content", HeadSHA: "feature-sha"},
	}}
	session := connectTestClient(t, client)

	response := callBranchPage(t, session, map[string]any{"owner": "owner", "repo": "project", "page": 3, "per_page": 2})

	// The branch list must stay a JSON array even when empty, not null.
	require.NotNil(t, response.Data.Branches)
	assert.Empty(t, response.Data.Branches)
	assert.Equal(t, 3, response.Data.Page)
	assert.Equal(t, 2, response.Data.PerPage)
	assert.Equal(t, 3, response.Data.Total)
	assert.Nil(t, response.Meta.NextPage)
}

func TestGitToolSchemasOmitRef(t *testing.T) {
	session := connectTestClient(t, &fakeClient{})
	list, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	for _, name := range []string{"list_branches", "get_branch", "list_commits", "get_commit"} {
		t.Run(name, func(t *testing.T) {
			tool := findTool(t, list.Tools, name)
			require.NotNil(t, tool.InputSchema)
			properties, ok := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
			require.True(t, ok)
			assert.NotContains(t, properties, "ref")
		})
	}

	// list_commits is fixed to the default branch, so it takes no page number either.
	tool := findTool(t, list.Tools, "list_commits")
	properties := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
	assert.NotContains(t, properties, "page")
}

func TestGetCommitMapsAuthorCommitterAndParents(t *testing.T) {
	client := &fakeClient{commit: gogs.Commit{
		SHA:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Message: "Change main version",
		WebURL:  "http://gogs.test/owner/project/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Author:  gogs.CommitPerson{Name: "Gogs MCP E2E", Email: "gogs-mcp@example.test", Date: "2026-09-01T00:00:00Z"},
		Committer: gogs.CommitPerson{
			Name:  "Gogs MCP E2E",
			Email: "gogs-mcp@example.test",
			Date:  "2026-09-01T00:00:01Z",
		},
		ParentSHAs: []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}}
	session := connectTestClient(t, client)

	response := callCommit(t, session, map[string]any{
		"owner": "owner", "repo": "project", "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})

	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", response.Data.SHA)
	assert.Equal(t, "Change main version", response.Data.Message)
	assert.Equal(t, "http://gogs.test/owner/project/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", response.Data.WebURL)
	assert.Equal(t, "Gogs MCP E2E", response.Data.Author.Name)
	assert.Equal(t, "gogs-mcp@example.test", response.Data.Author.Email)
	assert.Equal(t, "2026-09-01T00:00:01Z", response.Data.Committer.Date)
	assert.Equal(t, []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, response.Data.ParentSHAs)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", client.commitSHA)
}

func TestGitInputIsRejectedBeforeCallingClient(t *testing.T) {
	testCases := []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{
			name:      "empty branch",
			tool:      "get_branch",
			arguments: map[string]any{"owner": "owner", "repo": "project", "branch": ""},
		},
		{
			name:      "backslash branch",
			tool:      "get_branch",
			arguments: map[string]any{"owner": "owner", "repo": "project", "branch": `feature\content`},
		},
		{
			name:      "parent branch segment",
			tool:      "get_branch",
			arguments: map[string]any{"owner": "owner", "repo": "project", "branch": "feature/../escape"},
		},
		{
			name:      "empty sha",
			tool:      "get_commit",
			arguments: map[string]any{"owner": "owner", "repo": "project", "sha": ""},
		},
		{
			name:      "dot sha",
			tool:      "get_commit",
			arguments: map[string]any{"owner": "owner", "repo": "project", "sha": "."},
		},
		{
			name:      "dot dot sha",
			tool:      "get_commit",
			arguments: map[string]any{"owner": "owner", "repo": "project", "sha": ".."},
		},
		{
			name:      "backslash sha",
			tool:      "get_commit",
			arguments: map[string]any{"owner": "owner", "repo": "project", "sha": `main\feature`},
		},
		{
			name:      "path escape sha",
			tool:      "get_commit",
			arguments: map[string]any{"owner": "owner", "repo": "project", "sha": "../escape"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			client := &fakeClient{}
			session := connectTestClient(t, client)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      testCase.tool,
				Arguments: testCase.arguments,
			})
			require.NoError(t, err)
			assert.True(t, result.IsError)
			assert.Zero(t, client.branchCalls)
			assert.Zero(t, client.commitCalls)
		})
	}
}

func callBranchPage(t *testing.T, session *mcp.ClientSession, arguments map[string]any) ToolResponse[BranchPage] {
	t.Helper()
	return decodeGitResponse[BranchPage](t, session, "list_branches", arguments)
}

func callBranch(t *testing.T, session *mcp.ClientSession, arguments map[string]any) ToolResponse[Branch] {
	t.Helper()
	return decodeGitResponse[Branch](t, session, "get_branch", arguments)
}

func callCommitPage(t *testing.T, session *mcp.ClientSession, arguments map[string]any) ToolResponse[CommitPage] {
	t.Helper()
	return decodeGitResponse[CommitPage](t, session, "list_commits", arguments)
}

func callCommit(t *testing.T, session *mcp.ClientSession, arguments map[string]any) ToolResponse[Commit] {
	t.Helper()
	return decodeGitResponse[Commit](t, session, "get_commit", arguments)
}

func decodeGitResponse[T any](t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) ToolResponse[T] {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: arguments,
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	var response ToolResponse[T]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func branchNames(branches []Branch) []string {
	names := make([]string, len(branches))
	for index, branch := range branches {
		names[index] = branch.Name
	}
	return names
}

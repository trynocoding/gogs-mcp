package mcpserver

import (
	"context"
	"testing"

	"gogs-mcp/internal/gogs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPullToolsAdvertiseReadOnlyAndScope(t *testing.T) {
	session := connectTestClient(t, &fakeClient{})

	list, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	for _, name := range []string{"list_pull_requests", "get_pull_request", "get_pull_request_diff"} {
		tool := findTool(t, list.Tools, name)
		require.NotNil(t, tool.Annotations)
		assert.True(t, tool.Annotations.ReadOnlyHint, name)
		assert.True(t, tool.Annotations.IdempotentHint, name)
		require.NotNil(t, tool.Annotations.DestructiveHint)
		assert.False(t, *tool.Annotations.DestructiveHint, name)
		assert.Contains(t, tool.Description, "no pull request API", name)
		assert.Contains(t, tool.Description, "refs/pull/{number}/head", name)
	}
}

func TestListPullRequestsDefaultsToOpenAndMapsSummaries(t *testing.T) {
	client := &fakeClient{
		pullSummaries: []gogs.PullRequestSummary{
			{
				Number:      1,
				Title:       "Add multiply operation",
				State:       "open",
				User:        gogs.User{Username: "reader", FullName: "Reader Example"},
				NumComments: 2,
				CreatedAt:   "2026-09-02T11:12:49+08:00",
				UpdatedAt:   "2026-09-02T11:30:00+08:00",
				HeadSHA:     "987c169bfc6cd12cb23030b26281c5ad0d15fb1b",
			},
		},
		pullTotal: 4,
	}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_pull_requests",
		Arguments: map[string]any{"owner": "owner", "repo": "calculator"},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, "open", client.listPullsState, "the state must default to open")
	assert.Equal(t, 30, client.listPullsLimit)

	var output ToolResponse[PullRequestPage]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Data)
	require.Len(t, output.Data.PullRequests, 1)
	assert.Equal(t, int64(1), output.Data.PullRequests[0].Number)
	assert.Equal(t, "987c169bfc6cd12cb23030b26281c5ad0d15fb1b", output.Data.PullRequests[0].HeadSHA)
	assert.Equal(t, "reader", output.Data.PullRequests[0].User.Username)
	assert.Equal(t, "open", output.Data.State)
	assert.Equal(t, 30, output.Data.Limit)
	assert.Equal(t, 4, output.Data.Total)
}

func TestListPullRequestsRejectsUnknownState(t *testing.T) {
	session := connectTestClient(t, &fakeClient{})

	// The input schema enum rejects unknown states before the client runs.
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_pull_requests",
		Arguments: map[string]any{"owner": "owner", "repo": "calculator", "state": "merged"},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Nil(t, result.StructuredContent)
}

func TestGetPullRequestMapsIssueFields(t *testing.T) {
	client := &fakeClient{
		pull: gogs.PullRequest{
			Number:         5,
			Title:          "Add multiply operation",
			Body:           "Adds a mul operation to the CLI.",
			State:          "open",
			User:           gogs.User{Username: "reader"},
			Labels:         []gogs.IssueLabel{{Name: "needs-review", Color: "#00ff00"}},
			NumComments:    3,
			WebURL:         "http://127.0.0.1:3000/owner/calculator/issues/5",
			HeadSHA:        "abc123",
			BaseRef:        "main",
			BaseRefAssumed: true,
		},
	}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_pull_request",
		Arguments: map[string]any{"owner": "owner", "repo": "calculator", "number": 5},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, int64(5), client.getPullNumber)

	var output ToolResponse[PullRequest]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Data)
	assert.Equal(t, "Add multiply operation", output.Data.Title)
	assert.Equal(t, "needs-review", output.Data.Labels[0].Name)
	assert.Equal(t, "abc123", output.Data.HeadSHA)
	assert.Equal(t, "main", output.Data.BaseRef)
	assert.True(t, output.Data.BaseRefAssumed)
}

func TestGetPullRequestDiffWarnsAboutAssumedBaseAndTruncation(t *testing.T) {
	client := &fakeClient{
		pullDiff: gogs.PullRequestDiff{
			Number:         1,
			BaseRef:        "main",
			BaseRefAssumed: true,
			MergeBase:      "8bb8836",
			Diff:           "diff --git a/main.go b/main.go\n...",
			Files:          []gogs.DiffFileStat{{Path: "main.go", Status: "modified", Additions: 1, Deletions: 1}},
			Commits:        []gogs.PullCommit{{SHA: "987c169", Message: "Add multiply operation"}},
			Truncated:      true,
		},
	}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_pull_request_diff",
		Arguments: map[string]any{"owner": "owner", "repo": "calculator", "number": 1},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, int64(1), client.pullDiffNumber)
	assert.Empty(t, client.pullDiffBaseRef, "the base ref must be resolved server-side")
	assert.Equal(t, 256*1024, client.pullDiffMaxBytes, "the byte limit must default to the shared constant")

	var output ToolResponse[PullRequestDiffPage]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Data)
	assert.Equal(t, "main", output.Data.BaseRef)
	assert.Equal(t, "8bb8836", output.Data.MergeBase)
	require.Len(t, output.Data.Files, 1)
	assert.Equal(t, "main.go", output.Data.Files[0].Path)
	require.Len(t, output.Data.Commits, 1)
	assert.True(t, output.Meta.Truncated)
	require.Len(t, output.Meta.Warnings, 2)
	assert.Contains(t, output.Meta.Warnings[0], "cut off at the byte limit")
	assert.Contains(t, output.Meta.Warnings[1], "does not expose the pull request target branch")
}

func TestGetPullRequestDiffPassesExplicitBaseRef(t *testing.T) {
	client := &fakeClient{
		pullDiff: gogs.PullRequestDiff{
			Number:  1,
			BaseRef: "release-1",
		},
	}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_pull_request_diff",
		Arguments: map[string]any{
			"owner": "owner", "repo": "calculator", "number": 1, "base_ref": "release-1", "max_bytes": 4096,
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, "release-1", client.pullDiffBaseRef)
	assert.Equal(t, 4096, client.pullDiffMaxBytes)

	var output ToolResponse[PullRequestDiffPage]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Data)
	assert.False(t, output.Data.BaseRefAssumed)
	assert.Empty(t, output.Meta.Warnings)
}

func TestPullToolErrorsSurfaceAsToolErrors(t *testing.T) {
	session := connectTestClient(t, &fakeClient{
		listPullsErr: &gogs.Error{Code: gogs.CodeAuthenticationFailed, Message: "Git authentication failed."},
		getPullErr:   &gogs.Error{Code: gogs.CodeInvalidArgument, Message: "Issue #3 has no pull request head ref and is not a pull request."},
	})

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_pull_requests",
		Arguments: map[string]any{"owner": "owner", "repo": "calculator"},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	var listOutput ToolResponse[PullRequestPage]
	decodeStructuredContent(t, result.StructuredContent, &listOutput)
	require.NotNil(t, listOutput.Error)
	assert.Equal(t, "AUTHENTICATION_FAILED", listOutput.Error.Code)

	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_pull_request",
		Arguments: map[string]any{"owner": "owner", "repo": "calculator", "number": 3},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	var getOutput ToolResponse[PullRequest]
	decodeStructuredContent(t, result.StructuredContent, &getOutput)
	require.NotNil(t, getOutput.Error)
	assert.Equal(t, "INVALID_ARGUMENT", getOutput.Error.Code)
}

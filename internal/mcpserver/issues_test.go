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

func TestIssueToolsAdvertiseReadOnlyAndJiraScope(t *testing.T) {
	session := connectTestClient(t, &fakeClient{})

	list, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	for _, name := range []string{"list_issues", "get_issue", "list_issue_comments"} {
		tool := findTool(t, list.Tools, name)
		require.NotNil(t, tool.Annotations)
		assert.True(t, tool.Annotations.ReadOnlyHint, name)
		assert.True(t, tool.Annotations.IdempotentHint, name)
		require.NotNil(t, tool.Annotations.DestructiveHint)
		assert.False(t, *tool.Annotations.DestructiveHint, name)
		assert.Contains(t, tool.Description, "Jira remains the requirements system of record", name)
		assert.Contains(t, tool.Description, "does not sync with Jira", name)
	}
}

func TestListIssuesReturnsSummariesAndNextPage(t *testing.T) {
	client := &fakeClient{
		issueSummaries: []gogs.IssueSummary{
			{
				Number:      7,
				Title:       "Fix the parser",
				State:       "open",
				User:        gogs.User{Username: "alice", FullName: "Alice Example"},
				Labels:      []gogs.IssueLabel{{Name: "bug", Color: "#ff0000"}},
				NumComments: 2,
				CreatedAt:   "2026-01-02T15:04:05Z",
				UpdatedAt:   "2026-01-03T10:00:00Z",
			},
		},
		issueNextPage: 2,
	}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_issues",
		Arguments: map[string]any{"owner": "alice", "repo": "project", "state": "open", "page": 1},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var output ToolResponse[IssuePage]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Data)
	assert.Equal(t, "open", output.Data.State)
	assert.Equal(t, 1, output.Data.Page)
	require.Len(t, output.Data.Issues, 1)
	assert.Equal(t, int64(7), output.Data.Issues[0].Number)
	assert.Equal(t, "Fix the parser", output.Data.Issues[0].Title)
	assert.Equal(t, "alice", output.Data.Issues[0].User.Username)
	assert.Equal(t, 2, output.Data.Issues[0].NumComments)
	require.NotNil(t, output.Meta.NextPage)
	assert.Equal(t, 2, *output.Meta.NextPage)
	assert.Equal(t, 1, client.listIssuesCalls)
	assert.Equal(t, "open", client.listIssuesState)
	assert.Equal(t, 1, client.listIssuesPage)
}

func TestListIssuesDefaultsToOpenStateAndFirstPage(t *testing.T) {
	client := &fakeClient{}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_issues",
		Arguments: map[string]any{"owner": "alice", "repo": "project"},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var output ToolResponse[IssuePage]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Data)
	assert.Equal(t, "open", output.Data.State)
	assert.Equal(t, defaultPage, output.Data.Page)
	assert.Nil(t, output.Meta.NextPage)
	assert.Equal(t, 1, client.listIssuesCalls)
	assert.Equal(t, "open", client.listIssuesState)
	assert.Equal(t, defaultPage, client.listIssuesPage)
}

func TestListIssuesRejectsInvalidInputBeforeCallingGogs(t *testing.T) {
	testCases := map[string]map[string]any{
		"unknown state":     {"state": "all"},
		"empty state":       {"state": ""},
		"zero page":         {"page": 0},
		"negative page":     {"page": -3},
		"unknown parameter": {"per_page": 10},
	}
	for name, arguments := range testCases {
		t.Run(name, func(t *testing.T) {
			client := &fakeClient{}
			session := connectTestClient(t, client)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "list_issues",
				Arguments: arguments,
			})
			require.NoError(t, err)
			assert.True(t, result.IsError)
			assert.Zero(t, client.listIssuesCalls)
		})
	}
}

func TestGetIssueReturnsFullRecord(t *testing.T) {
	assignee := gogs.User{ID: 4, Username: "bob", FullName: "Bob Example"}
	client := &fakeClient{issue: gogs.Issue{
		Number:      7,
		Title:       "Fix the parser",
		Body:        "The parser fails on empty input.",
		State:       "open",
		User:        gogs.User{ID: 3, Username: "alice", FullName: "Alice Example"},
		Assignee:    &assignee,
		Labels:      []gogs.IssueLabel{{Name: "bug", Color: "#ff0000"}},
		Milestone:   &gogs.IssueMilestone{Title: "v1.0", State: "open"},
		NumComments: 2,
		CreatedAt:   "2026-01-02T15:04:05Z",
		UpdatedAt:   "2026-01-03T10:00:00Z",
	}}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_issue",
		Arguments: map[string]any{"owner": "alice", "repo": "project", "number": 7},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var output ToolResponse[Issue]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Data)
	assert.Equal(t, int64(7), output.Data.Number)
	assert.Equal(t, "The parser fails on empty input.", output.Data.Body)
	assert.Equal(t, "open", output.Data.State)
	assert.Equal(t, "alice", output.Data.User.Username)
	require.NotNil(t, output.Data.Assignee)
	assert.Equal(t, "bob", output.Data.Assignee.Username)
	require.NotNil(t, output.Data.Milestone)
	assert.Equal(t, "v1.0", output.Data.Milestone.Title)
	assert.Equal(t, 2, output.Data.NumComments)
	assert.Equal(t, int64(7), client.getIssueNumber)
}

func TestGetIssueReturnsNotfoundWithoutLeakingExistence(t *testing.T) {
	client := &fakeClient{getIssueErr: &gogs.Error{
		Code:       gogs.CodeResourceNotFoundOrForbidden,
		Message:    "The issue does not exist or the current user cannot access it.",
		HTTPStatus: 404,
	}}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_issue",
		Arguments: map[string]any{"owner": "alice", "repo": "private", "number": 99},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)

	var output ToolResponse[Issue]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Error)
	assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", output.Error.Code)
	assert.Equal(t, "The issue does not exist or the current user cannot access it.", output.Error.Message)
}

func TestListIssueCommentsReturnsCommentsAndEchoesSince(t *testing.T) {
	client := &fakeClient{issueComments: []gogs.IssueComment{
		{ID: 900, User: gogs.User{Username: "bob"}, Body: "Reproduced on Linux.", CreatedAt: "2026-01-03T09:00:00Z", UpdatedAt: "2026-01-03T09:00:00Z"},
		{ID: 901, User: gogs.User{Username: "alice"}, Body: "Thanks for the report.", CreatedAt: "2026-01-04T09:00:00Z", UpdatedAt: "2026-01-04T09:00:00Z"},
	}}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_issue_comments",
		Arguments: map[string]any{"owner": "alice", "repo": "project", "number": 7, "since": "2026-01-03T00:00:00Z"},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var output ToolResponse[IssueCommentPage]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Data)
	assert.Len(t, output.Data.Comments, 2)
	assert.Equal(t, int64(900), output.Data.Comments[0].ID)
	assert.Equal(t, "bob", output.Data.Comments[0].User.Username)
	assert.Equal(t, "Reproduced on Linux.", output.Data.Comments[0].Body)
	assert.Equal(t, "2026-01-03T09:00:00Z", output.Data.Comments[0].CreatedAt)
	assert.Equal(t, "2026-01-03T00:00:00Z", output.Data.Since)
	assert.Equal(t, defaultMaxComments, output.Data.Max)
	assert.False(t, output.Meta.Truncated)
	assert.Empty(t, output.Meta.Warnings)
	assert.Equal(t, int64(7), client.commentsNumber)
	assert.Equal(t, "2026-01-03T00:00:00Z", client.commentsSince)
}

func TestListIssueCommentsRejectsInvalidSinceBeforeCallingGogs(t *testing.T) {
	for name, since := range map[string]string{
		"not a timestamp": "yesterday",
		"date only":       "2026-01-02",
		"unix seconds":    "1767225600",
	} {
		t.Run(name, func(t *testing.T) {
			client := &fakeClient{}
			session := connectTestClient(t, client)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "list_issue_comments",
				Arguments: map[string]any{"owner": "alice", "repo": "project", "number": 7, "since": since},
			})
			require.NoError(t, err)
			assert.True(t, result.IsError)

			var output ToolResponse[IssueCommentPage]
			decodeStructuredContent(t, result.StructuredContent, &output)
			require.NotNil(t, output.Error)
			assert.Equal(t, "INVALID_ARGUMENT", output.Error.Code)
			assert.Contains(t, output.Error.Message, "since must be an RFC3339 timestamp")
			assert.Zero(t, client.commentsCalls)
		})
	}
}

func TestListIssueCommentsRejectsOutOfRangeMaxCommentsBeforeCallingGogs(t *testing.T) {
	for name, maxComments := range map[string]any{"negative": -1, "oversized": 501} {
		t.Run(name, func(t *testing.T) {
			client := &fakeClient{}
			session := connectTestClient(t, client)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "list_issue_comments",
				Arguments: map[string]any{"owner": "alice", "repo": "project", "number": 7, "max_comments": maxComments},
			})
			require.NoError(t, err)
			assert.True(t, result.IsError)
			assert.Zero(t, client.commentsCalls)
		})
	}
}

func TestListIssueCommentsAcceptsMaximumMaxComments(t *testing.T) {
	client := &fakeClient{}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_issue_comments",
		Arguments: map[string]any{"owner": "alice", "repo": "project", "number": 7, "max_comments": maximumMaxComments},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, 1, client.commentsCalls)
}

func TestListIssueCommentsTruncatesAtMaxComments(t *testing.T) {
	client := &fakeClient{issueComments: []gogs.IssueComment{
		{ID: 1, User: gogs.User{Username: "alice"}, Body: "first", CreatedAt: "2026-01-02T09:00:00Z", UpdatedAt: "2026-01-02T09:00:00Z"},
		{ID: 2, User: gogs.User{Username: "bob"}, Body: "second", CreatedAt: "2026-01-03T09:00:00Z", UpdatedAt: "2026-01-03T09:00:00Z"},
		{ID: 3, User: gogs.User{Username: "carol"}, Body: "third", CreatedAt: "2026-01-04T09:00:00Z", UpdatedAt: "2026-01-04T09:00:00Z"},
	}}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_issue_comments",
		Arguments: map[string]any{"owner": "alice", "repo": "project", "number": 7, "max_comments": 2},
	})
	require.NoError(t, err)

	var output ToolResponse[IssueCommentPage]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Data)
	require.Len(t, output.Data.Comments, 2)
	assert.Equal(t, int64(2), output.Data.Comments[1].ID)
	assert.Equal(t, 2, output.Data.Max)
	assert.True(t, output.Meta.Truncated)
	require.Len(t, output.Meta.Warnings, 1)
	assert.Contains(t, output.Meta.Warnings[0], "max_comments limit of 2")
}

func TestListIssueCommentsTruncatesAtByteBudget(t *testing.T) {
	client := &fakeClient{issueComments: []gogs.IssueComment{
		{ID: 1, User: gogs.User{Username: "alice"}, Body: strings.Repeat("a", 40<<10), CreatedAt: "2026-01-02T09:00:00Z", UpdatedAt: "2026-01-02T09:00:00Z"},
		{ID: 2, User: gogs.User{Username: "bob"}, Body: strings.Repeat("b", 40<<10), CreatedAt: "2026-01-03T09:00:00Z", UpdatedAt: "2026-01-03T09:00:00Z"},
	}}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_issue_comments",
		Arguments: map[string]any{"owner": "alice", "repo": "project", "number": 7},
	})
	require.NoError(t, err)

	var output ToolResponse[IssueCommentPage]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Data)
	require.Len(t, output.Data.Comments, 1)
	assert.Equal(t, int64(1), output.Data.Comments[0].ID)
	assert.True(t, output.Meta.Truncated)
	require.NotEmpty(t, output.Meta.Warnings)
	assert.Contains(t, output.Meta.Warnings[0], "64 KiB structured output limit")
}

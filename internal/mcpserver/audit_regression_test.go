package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"gogs-mcp/internal/gogs"
	"gogs-mcp/internal/snapshot"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIssueBodyCanBeReassembledAcrossChunks(t *testing.T) {
	body := strings.Repeat("中文<&>\\\"\n", 7000)
	client := &fakeClient{issue: gogs.Issue{Number: 7, Body: body}}
	session := connectTestClient(t, client)
	var complete strings.Builder
	offset := 0
	for {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_issue", Arguments: map[string]any{"owner": "owner", "repo": "repo", "number": 7, "body_offset": offset}})
		require.NoError(t, err)
		require.False(t, result.IsError)
		encoded, err := json.Marshal(result.StructuredContent)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(encoded), 64<<10)
		var response ToolResponse[Issue]
		decodeStructuredContent(t, result.StructuredContent, &response)
		require.NotNil(t, response.Data)
		assert.True(t, utf8.ValidString(response.Data.Body))
		complete.WriteString(response.Data.Body)
		if response.Meta.NextBodyOffset == nil {
			break
		}
		require.Greater(t, *response.Meta.NextBodyOffset, offset)
		offset = *response.Meta.NextBodyOffset
	}
	assert.Equal(t, body, complete.String())
}

func TestOversizedFirstCommentHasPreviewAndReadableRemainder(t *testing.T) {
	body := strings.Repeat("中文<&>\n", 15000)
	client := &fakeClient{issueComments: []gogs.IssueComment{{ID: 10, Body: body}, {ID: 11, Body: "next"}}}
	session := connectTestClient(t, client)
	call := func(arguments map[string]any) ToolResponse[IssueCommentPage] {
		arguments["owner"] = "owner"
		arguments["repo"] = "repo"
		arguments["number"] = 7
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_issue_comments", Arguments: arguments})
		require.NoError(t, err)
		require.False(t, result.IsError)
		payload, err := json.Marshal(result.StructuredContent)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(payload), 64<<10)
		var response ToolResponse[IssueCommentPage]
		decodeStructuredContent(t, result.StructuredContent, &response)
		return response
	}
	first := call(map[string]any{"max_comments": 1})
	require.Len(t, first.Data.Comments, 1)
	require.NotNil(t, first.Data.Comments[0].NextBodyOffset)
	assert.Equal(t, int64(10), first.Meta.NextAfterID)
	next := call(map[string]any{"after_id": first.Meta.NextAfterID})
	require.Len(t, next.Data.Comments, 1)
	assert.Equal(t, int64(11), next.Data.Comments[0].ID)
	var complete strings.Builder
	offset := 0
	for {
		page := call(map[string]any{"comment_id": 10, "body_offset": offset})
		require.Len(t, page.Data.Comments, 1)
		comment := page.Data.Comments[0]
		complete.WriteString(comment.Body)
		if comment.NextBodyOffset == nil {
			break
		}
		require.Greater(t, *comment.NextBodyOffset, offset)
		offset = *comment.NextBodyOffset
	}
	assert.Equal(t, body, complete.String())
}

func TestWriteResponseKeepsIdentityWhenBodyIsTruncated(t *testing.T) {
	body := strings.Repeat("x", 100000)
	client := &fakeClient{createdComment: gogs.IssueComment{ID: 123, Body: body}}
	session := connectWritingTestClient(t, client)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_issue_comment", Arguments: map[string]any{"owner": "owner", "repo": "repo", "number": 7, "body": body}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	var response ToolResponse[IssueComment]
	decodeStructuredContent(t, result.StructuredContent, &response)
	assert.Equal(t, int64(123), response.Data.ID)
	require.NotNil(t, response.Data.NextBodyOffset)
	assert.Equal(t, 1, client.createCommentCalls)
	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), 64<<10)
}

type pausingMatcher struct {
	started chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (m *pausingMatcher) FindAllIndex([]byte) [][]int {
	m.once.Do(func() { close(m.started); <-m.resume })
	return nil
}
func TestSearchStopsAfterCallerCancellation(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("two"), 0600))
	matcher := &pausingMatcher{started: make(chan struct{}), resume: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan snapshot.Stats, 1)
	go func() {
		_, stats, _ := searchSnapshot(ctx, dir, snapshot.Options{Matcher: matcher, MaxResults: 50, MaxFileBytes: 1024}, time.Minute)
		done <- stats
	}()
	<-matcher.started
	cancel()
	close(matcher.resume)
	select {
	case stats := <-done:
		assert.True(t, stats.TimedOut)
	case <-time.After(time.Second):
		t.Fatal("search ignored caller cancellation")
	}
}

func TestOutputTrimmingPreservesResourceIdentifiers(t *testing.T) {
	path := strings.Repeat("nested/", 100) + "file.txt"
	data := map[string]any{"entries": []any{map[string]any{"name": "file.txt", "path": path, "sha": strings.Repeat("a", 40), "url": "https://gogs.test/" + path}}, "description": strings.Repeat("x", 200), "diff": strings.Repeat("+x\n", 100)}
	trimOutput(data, 8)
	entry := data["entries"].([]any)[0].(map[string]any)
	assert.Equal(t, path, entry["path"])
	assert.Equal(t, "file.txt", entry["name"])
	assert.Equal(t, "https://gogs.test/"+path, entry["url"])
	assert.Equal(t, true, data["truncated"])
}

func TestOversizedBranchPageFailsWithoutSkippingEntries(t *testing.T) {
	branches := make([]gogs.Branch, 100)
	for i := range branches {
		branches[i] = gogs.Branch{Name: strings.Repeat("long-name-", 80) + strings.Repeat("x", i), HeadSHA: strings.Repeat("a", 40)}
	}
	client := &fakeClient{branches: branches}
	session := connectTestClient(t, client)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_branches", Arguments: map[string]any{"owner": "owner", "repo": "repo", "per_page": 100}})
	require.NoError(t, err, "size errors must remain tool results, not JSON-RPC errors")
	require.True(t, result.IsError)
	var failure ToolResponse[BranchPage]
	decodeStructuredContent(t, result.StructuredContent, &failure)
	require.NotNil(t, failure.Error)
	assert.Equal(t, "RESPONSE_TOO_LARGE", failure.Error.Code)
	assert.NotEmpty(t, failure.Meta.RequestID)
	// Reducing page size preserves exact identifiers and every page member.
	page := callBranchPage(t, session, map[string]any{"owner": "owner", "repo": "repo", "per_page": 50})
	require.Len(t, page.Data.Branches, 50)
	require.NotNil(t, page.Meta.NextPage)
	assert.Equal(t, 2, *page.Meta.NextPage)
	assert.Equal(t, branches[0].Name, page.Data.Branches[0].Name)
}

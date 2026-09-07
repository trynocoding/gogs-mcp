package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"gogs-mcp/internal/gogs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListDirectoryAppliesDefaultsSortsAndPaginates(t *testing.T) {
	client := &fakeClient{
		directoryRef: "main",
		directoryEntries: []gogs.ContentEntry{
			{Name: "z.go", Path: "src/z.go", Type: "file", Size: 30, SHA: "z-sha"},
			{Name: "a", Path: "src/a", Type: "dir", Size: 0, SHA: "a-sha"},
			{Name: "link", Path: "src/link", Type: "symlink", Size: 4, SHA: "link-sha", Target: "z.go"},
		},
	}
	session := connectTestClient(t, client)

	first := callDirectoryPage(t, session, map[string]any{"owner": "owner", "repo": "project", "path": "src", "per_page": 2})
	assert.Equal(t, "src", first.Data.Path)
	assert.Equal(t, "main", first.Data.Ref)
	assert.Equal(t, 1, first.Data.Page)
	assert.Equal(t, 2, first.Data.PerPage)
	assert.Equal(t, 3, first.Data.Total)
	assert.Equal(t, []string{"src/a", "src/link"}, directoryPaths(first.Data.Entries))
	require.NotNil(t, first.Meta.NextPage)
	assert.Equal(t, 2, *first.Meta.NextPage)
	assert.Equal(t, "z.go", first.Data.Entries[1].Target)

	second := callDirectoryPage(t, session, map[string]any{"owner": "owner", "repo": "project", "path": "src", "page": 2, "per_page": 2})
	assert.Equal(t, []string{"src/z.go"}, directoryPaths(second.Data.Entries))
	assert.Nil(t, second.Meta.NextPage)
	assert.Equal(t, "src", client.directoryPath)
}

func TestGetFileDefaultsToTwoHundredLinesAndReturnsContinuation(t *testing.T) {
	lines := make([]string, 250)
	for index := range lines {
		lines[index] = fmt.Sprintf("line-%03d", index+1)
	}
	content := strings.Join(lines, "\n") + "\n"
	client := &fakeClient{file: gogs.FileContent{
		Path: "src/lines.txt", Type: "file", Size: int64(len(content)), SHA: "file-sha", Ref: "main", Data: []byte(content),
	}}
	session := connectTestClient(t, client)

	response := callFile(t, session, map[string]any{"owner": "owner", "repo": "project", "path": "src/lines.txt"})

	assert.Equal(t, "text", response.Data.Type)
	assert.True(t, response.Data.TrailingNewline)
	assert.Equal(t, 1, response.Data.StartLine)
	assert.Equal(t, 200, response.Data.EndLine)
	assert.Equal(t, 250, response.Data.TotalLines)
	assert.Contains(t, response.Data.Content, "line-001")
	assert.Contains(t, response.Data.Content, "line-200")
	assert.NotContains(t, response.Data.Content, "line-201")
	assert.True(t, response.Meta.Truncated)
	require.NotNil(t, response.Meta.NextStartLine)
	assert.Equal(t, 201, *response.Meta.NextStartLine)
	assert.NotEmpty(t, response.Meta.Warnings)
	assert.Equal(t, "src/lines.txt", client.filePath)
	assert.Empty(t, client.contentRef)
}

func TestGetFileReturnsRequestedUnicodeLineRange(t *testing.T) {
	content := "first\n你好，世界\nemoji 😀\nlast\n"
	client := &fakeClient{file: gogs.FileContent{
		Path: "unicode.txt", Type: "file", Size: int64(len(content)), SHA: "unicode-sha", Ref: "v1.0.0", Data: []byte(content),
	}}
	session := connectTestClient(t, client)

	response := callFile(t, session, map[string]any{
		"owner": "owner", "repo": "project", "path": "unicode.txt", "ref": "v1.0.0", "start_line": 2, "line_count": 2,
	})

	assert.Equal(t, "你好，世界\nemoji 😀", response.Data.Content)
	// The flag describes the file, not the returned range.
	assert.True(t, response.Data.TrailingNewline)
	assert.Equal(t, 2, response.Data.StartLine)
	assert.Equal(t, 3, response.Data.EndLine)
	assert.Equal(t, 4, response.Data.TotalLines)
	assert.True(t, response.Meta.Truncated)
	require.NotNil(t, response.Meta.NextStartLine)
	assert.Equal(t, 4, *response.Meta.NextStartLine)
	assert.Equal(t, "v1.0.0", client.contentRef)
}

func TestGetFileDistinguishesFilesByTrailingNewline(t *testing.T) {
	testCases := []struct {
		name       string
		data       string
		content    string
		totalLines int
		present    bool
	}{
		{name: "with trailing newline", data: "a\nb\n", content: "a\nb", totalLines: 2, present: true},
		{name: "without trailing newline", data: "a\nb", content: "a\nb", totalLines: 2, present: false},
		{name: "empty file", data: "", content: "", totalLines: 0, present: false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			client := &fakeClient{file: gogs.FileContent{
				Path: "lines.txt", Type: "file", Size: int64(len(testCase.data)), SHA: "sha", Ref: "main", Data: []byte(testCase.data),
			}}
			session := connectTestClient(t, client)

			response := callFile(t, session, map[string]any{"owner": "owner", "repo": "project", "path": "lines.txt"})

			assert.Equal(t, testCase.content, response.Data.Content)
			assert.Equal(t, testCase.totalLines, response.Data.TotalLines)
			assert.Equal(t, testCase.present, response.Data.TrailingNewline)
			encoded, err := json.Marshal(response.Data)
			require.NoError(t, err)
			assert.Equal(t, testCase.present, strings.Contains(string(encoded), `"trailing_newline":true`))
		})
	}
}

func TestGetFileReturnsBinaryMetadataWithoutPayload(t *testing.T) {
	client := &fakeClient{file: gogs.FileContent{
		Path: "image.bin", Type: "file", Size: 5, SHA: "binary-sha", Ref: "main", Data: []byte{'a', 0, 'b', 0xff, 'c'},
	}}
	session := connectTestClient(t, client)

	response := callFile(t, session, map[string]any{"owner": "owner", "repo": "project", "path": "image.bin"})

	assert.Equal(t, "binary", response.Data.Type)
	assert.False(t, response.Data.TrailingNewline)
	assert.Equal(t, int64(5), response.Data.Size)
	assert.Equal(t, "binary-sha", response.Data.SHA)
	assert.Empty(t, response.Data.Content)
	assert.Zero(t, response.Data.StartLine)
	assert.False(t, response.Meta.Truncated)
}

func TestGetFileReturnsTooLargeMetadataAndExplicitWarning(t *testing.T) {
	client := &fakeClient{file: gogs.FileContent{
		Path: "large.txt", Type: "file", Size: maximumReadableBytes + 1, SHA: "large-sha", Ref: "main", Data: []byte("must not be returned"),
	}}
	session := connectTestClient(t, client)

	response := callFile(t, session, map[string]any{"owner": "owner", "repo": "project", "path": "large.txt"})

	assert.Equal(t, "too_large", response.Data.Type)
	assert.Empty(t, response.Data.Content)
	assert.True(t, response.Meta.Truncated)
	assert.Contains(t, strings.Join(response.Meta.Warnings, " "), "1 MiB")
}

func TestGetFileEnforcesStructuredOutputLimitWithoutBreakingUTF8(t *testing.T) {
	line := strings.Repeat(`"\<&界`, 30)
	lines := make([]string, 1000)
	for index := range lines {
		lines[index] = line
	}
	content := strings.Join(lines, "\n")
	client := &fakeClient{file: gogs.FileContent{
		Path: "wide.txt", Type: "file", Size: int64(len(content)), SHA: "wide-sha", Ref: "main", Data: []byte(content),
	}}
	session := connectTestClient(t, client)

	response := callFile(t, session, map[string]any{
		"owner": "owner", "repo": "project", "path": "wide.txt", "line_count": 1000,
	})

	assert.LessOrEqual(t, len(response.Data.Content), maximumFileTextBytes)
	assert.True(t, utf8.ValidString(response.Data.Content))
	assert.True(t, response.Meta.Truncated)
	require.NotNil(t, response.Meta.NextStartLine)
	assert.Greater(t, *response.Meta.NextStartLine, 1)
	assert.Contains(t, strings.Join(response.Meta.Warnings, " "), "64 KiB")
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), 64<<10)
}

func TestGetFileReturnsSymlinkAndSubmoduleMetadata(t *testing.T) {
	testCases := map[string]gogs.FileContent{
		"symlink": {
			Path: "src/link", Type: "symlink", Size: 8, SHA: "link-sha", Ref: "main", Target: "file.txt",
		},
		"submodule": {
			Path: "vendor/module", Type: "submodule", SHA: "module-sha", Ref: "main", SubmoduleURL: "https://example.test/module.git",
		},
	}
	for name, content := range testCases {
		t.Run(name, func(t *testing.T) {
			session := connectTestClient(t, &fakeClient{file: content})
			response := callFile(t, session, map[string]any{"owner": "owner", "repo": "project", "path": content.Path})
			assert.Equal(t, content.Type, response.Data.Type)
			assert.Equal(t, content.Target, response.Data.Target)
			assert.Equal(t, content.SubmoduleURL, response.Data.SubmoduleURL)
			assert.Empty(t, response.Data.Content)
		})
	}
}

func TestContentInputIsRejectedBeforeCallingClient(t *testing.T) {
	testCases := []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{name: "absolute directory", tool: "list_directory", arguments: map[string]any{"owner": "owner", "repo": "project", "path": "/etc"}},
		{name: "parent directory", tool: "list_directory", arguments: map[string]any{"owner": "owner", "repo": "project", "path": "src/../etc"}},
		{name: "backslash escape", tool: "get_file", arguments: map[string]any{"owner": "owner", "repo": "project", "path": "src\\..\\etc"}},
		{name: "nul", tool: "get_file", arguments: map[string]any{"owner": "owner", "repo": "project", "path": "src/\x00file"}},
		{name: "line count", tool: "get_file", arguments: map[string]any{"owner": "owner", "repo": "project", "path": "file", "line_count": 1001}},
		{name: "page size", tool: "list_directory", arguments: map[string]any{"owner": "owner", "repo": "project", "per_page": 101}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			client := &fakeClient{}
			session := connectTestClient(t, client)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: testCase.tool, Arguments: testCase.arguments})
			require.NoError(t, err)
			assert.True(t, result.IsError)
			assert.Zero(t, client.directoryCalls)
			assert.Zero(t, client.fileCalls)
		})
	}
}

func callDirectoryPage(t *testing.T, session *mcp.ClientSession, arguments map[string]any) ToolResponse[DirectoryPage] {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_directory", Arguments: arguments})
	require.NoError(t, err)
	require.False(t, result.IsError)
	var response ToolResponse[DirectoryPage]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func callFile(t *testing.T, session *mcp.ClientSession, arguments map[string]any) ToolResponse[File] {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_file", Arguments: arguments})
	require.NoError(t, err)
	require.False(t, result.IsError)
	var response ToolResponse[File]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func directoryPaths(entries []DirectoryEntry) []string {
	paths := make([]string, len(entries))
	for index, entry := range entries {
		paths[index] = entry.Path
	}
	return paths
}

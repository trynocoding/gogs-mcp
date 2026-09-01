package mcpserver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"gogs-mcp/internal/gogs"
	"gogs-mcp/internal/snapshot"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fullTestSHA = "0123456789abcdef0123456789abcdef01234567"

// singleFileArchive returns a download callback serving a Gogs-style archive
// whose entries are wrapped in one top-level directory.
func singleFileArchive(t *testing.T, name, body string) func() (io.ReadCloser, error) {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(writer)
	header := &tar.Header{Name: "repo-0123456/" + name, Typeflag: tar.TypeReg, Size: int64(len(body))}
	require.NoError(t, archive.WriteHeader(header))
	_, err := archive.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, writer.Close())

	encoded := buffer.Bytes()
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(encoded)), nil
	}
}

func searchTestManager(t *testing.T, limits snapshot.Limits) *snapshot.Manager {
	t.Helper()
	manager, err := snapshot.NewManager(t.TempDir(), "https://gogs.example.test/", limits)
	require.NoError(t, err)
	return manager
}

type searchResponse struct {
	Data struct {
		Query     string        `json:"query"`
		Ref       string        `json:"ref"`
		CommitSHA string        `json:"commit_sha"`
		Mode      string        `json:"mode"`
		Matches   []searchMatch `json:"matches"`
	} `json:"data"`
	Error *toolError       `json:"error"`
	Meta  responseTestMeta `json:"meta"`
}

type searchMatch struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	LineText string `json:"line_text"`
	Context  []struct {
		Line int    `json:"line"`
		Text string `json:"text"`
	} `json:"context"`
}

type toolError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type responseTestMeta struct {
	RequestID string   `json:"request_id"`
	Truncated bool     `json:"truncated"`
	CacheHit  bool     `json:"cache_hit"`
	Warnings  []string `json:"warnings"`
}

func callSearchCode(t *testing.T, session *mcp.ClientSession, arguments map[string]any) searchResponse {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "search_code",
		Arguments: arguments,
	})
	require.NoError(t, err)
	var response searchResponse
	decodeStructuredContent(t, result.StructuredContent, &response)
	return response
}

func TestSearchCodeFindsLiteralMatches(t *testing.T) {
	fake := &fakeClient{
		user:        gogs.User{ID: 42, Username: "tester"},
		resolvedSHA: fullTestSHA,
		archiveBody: singleFileArchive(t, "src/main.go", "package main\n\nvar needle = true\n"),
	}
	manager := searchTestManager(t, snapshot.DefaultLimits())
	session := connectTestClientWithSnapshots(t, fake, manager)

	first := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "repo", "query": "needle", "ref": "feature",
	})
	require.Nil(t, first.Error)
	assert.Equal(t, fullTestSHA, first.Data.CommitSHA)
	assert.Equal(t, "feature", first.Data.Ref)
	assert.Equal(t, "literal", first.Data.Mode)
	require.Len(t, first.Data.Matches, 1)
	match := first.Data.Matches[0]
	assert.Equal(t, "src/main.go", match.Path)
	assert.Equal(t, 3, match.Line)
	assert.Equal(t, 5, match.Column)
	assert.Equal(t, "var needle = true", match.LineText)
	require.Len(t, match.Context, 2)
	assert.Equal(t, 1, match.Context[0].Line)
	assert.Equal(t, "package main", match.Context[0].Text)
	assert.False(t, first.Meta.CacheHit)
	assert.Equal(t, fullTestSHA, fake.archiveSHA, "the archive must be requested with the resolved SHA")

	second := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "repo", "query": "needle", "ref": "feature",
	})
	require.Nil(t, second.Error)
	assert.True(t, second.Meta.CacheHit)
	assert.Equal(t, 1, fake.archiveCalls, "the archive must be downloaded only once")
	assert.Equal(t, 2, fake.resolveCalls, "each search resolves its ref; only the archive is cached")
}

func TestSearchCodeMemoizesUserIDPerProcess(t *testing.T) {
	fake := &fakeClient{
		user:        gogs.User{ID: 9, Username: "tester"},
		resolvedSHA: fullTestSHA,
		archiveBody: singleFileArchive(t, "file.txt", "needle\n"),
	}
	manager := searchTestManager(t, snapshot.DefaultLimits())
	session := connectTestClientWithSnapshots(t, fake, manager)

	for index := 0; index < 3; index++ {
		response := callSearchCode(t, session, map[string]any{
			"owner": "owner", "repo": "repo", "query": "needle",
		})
		require.Nil(t, response.Error)
	}
	assert.Equal(t, 1, fake.userCalls, "GetAuthenticatedUser must be called once per process")
}

func TestSearchCodeWithoutManagerReportsUnavailable(t *testing.T) {
	session := connectTestClient(t, &fakeClient{})
	response := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "repo", "query": "needle",
	})
	require.NotNil(t, response.Error)
	assert.Equal(t, "GOGS_ERROR", response.Error.Code)
}

func TestSearchCodeMapsResolveErrors(t *testing.T) {
	fake := &fakeClient{
		resolveErr: &gogs.Error{Code: gogs.CodeResourceNotFoundOrForbidden, Message: "missing"},
	}
	manager := searchTestManager(t, snapshot.DefaultLimits())
	session := connectTestClientWithSnapshots(t, fake, manager)

	response := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "repo", "query": "needle",
	})
	require.NotNil(t, response.Error)
	assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", response.Error.Code)
	assert.Equal(t, 0, fake.archiveCalls)
}

func TestSearchCodeMapsSnapshotLimits(t *testing.T) {
	fake := &fakeClient{
		user:        gogs.User{ID: 1, Username: "tester"},
		resolvedSHA: fullTestSHA,
		archiveBody: singleFileArchive(t, "big.txt", strings.Repeat("x", 8192)),
	}
	manager := searchTestManager(t, snapshot.Limits{MaxCompressedBytes: 32, MaxDecompressedBytes: 1 << 20, MaxEntries: 100})
	session := connectTestClientWithSnapshots(t, fake, manager)

	response := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "repo", "query": "needle",
	})
	require.NotNil(t, response.Error)
	assert.Equal(t, "RESPONSE_TOO_LARGE", response.Error.Code)
}

func TestSearchCodeMapsUnsafeArchive(t *testing.T) {
	fake := &fakeClient{
		user:        gogs.User{ID: 1, Username: "tester"},
		resolvedSHA: fullTestSHA,
		archiveBody: func() (io.ReadCloser, error) {
			return nil, snapshot.ErrUnsafeArchiveEntry
		},
	}
	manager := searchTestManager(t, snapshot.DefaultLimits())
	session := connectTestClientWithSnapshots(t, fake, manager)

	response := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "repo", "query": "needle",
	})
	require.NotNil(t, response.Error)
	assert.Equal(t, "GOGS_ERROR", response.Error.Code)
}

func TestSearchCodeBoundsEncodedOutput(t *testing.T) {
	// Long lines make each encoded match large enough that the 64 KiB output
	// budget is exhausted before the match-count cap.
	var lines []string
	for index := 0; index < 100; index++ {
		lines = append(lines, "needle "+strings.Repeat("内容", 200)+"\n")
	}
	fake := &fakeClient{
		user:        gogs.User{ID: 1, Username: "tester"},
		resolvedSHA: fullTestSHA,
		archiveBody: singleFileArchive(t, "many.txt", strings.Join(lines, "")),
	}
	manager := searchTestManager(t, snapshot.DefaultLimits())
	session := connectTestClientWithSnapshots(t, fake, manager)

	response := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "repo", "query": "needle",
	})
	require.Nil(t, response.Error)
	assert.True(t, response.Meta.Truncated)
	assert.NotEmpty(t, response.Meta.Warnings)
	require.NotEmpty(t, response.Data.Matches)
	assert.Less(t, len(response.Data.Matches), len(lines))

	encoded, err := json.Marshal(response.Data.Matches)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), 56<<10)
}

func TestSearchCodeCacheIsIsolatedPerUserAndRef(t *testing.T) {
	fake := &fakeClient{
		user:        gogs.User{ID: 5, Username: "tester"},
		resolvedSHA: fullTestSHA,
		archiveBody: singleFileArchive(t, "file.txt", "needle\n"),
	}
	root := t.TempDir()
	managerA, err := snapshot.NewManager(root, "https://a.example.test/", snapshot.DefaultLimits())
	require.NoError(t, err)
	managerB, err := snapshot.NewManager(root, "https://b.example.test/", snapshot.DefaultLimits())
	require.NoError(t, err)
	sessionA := connectTestClientWithSnapshots(t, fake, managerA)
	sessionB := connectTestClientWithSnapshots(t, fake, managerB)

	first := callSearchCode(t, sessionA, map[string]any{
		"owner": "owner", "repo": "repo", "query": "needle",
	})
	require.Nil(t, first.Error)
	assert.False(t, first.Meta.CacheHit)

	second := callSearchCode(t, sessionB, map[string]any{
		"owner": "owner", "repo": "repo", "query": "needle",
	})
	require.Nil(t, second.Error)
	assert.False(t, second.Meta.CacheHit, "a different instance must not share snapshots")
}

func TestSearchCodeSchema(t *testing.T) {
	session := connectTestClient(t, &fakeClient{})
	list, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	tool := findTool(t, list.Tools, "search_code")
	require.NotNil(t, tool.InputSchema)

	schema, ok := tool.InputSchema.(map[string]any)
	require.True(t, ok)
	required, ok := schema["required"].([]any)
	require.True(t, ok)
	names := make([]string, 0, len(required))
	for _, value := range required {
		names = append(names, value.(string))
	}
	assert.ElementsMatch(t, []string{"owner", "repo", "query"}, names)

	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, properties, "ref")
	query, ok := properties["query"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 1000, query["maxLength"])
}

func TestSearchCodeRejectsOversizedQuery(t *testing.T) {
	fake := &fakeClient{resolvedSHA: fullTestSHA}
	manager := searchTestManager(t, snapshot.DefaultLimits())
	session := connectTestClientWithSnapshots(t, fake, manager)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_code",
		Arguments: map[string]any{
			"owner": "owner", "repo": "repo", "query": strings.Repeat("a", 1001),
		},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Equal(t, 0, fake.resolveCalls)
}

func TestSearchCodeNoSnapshotFilesEscapeCacheRoot(t *testing.T) {
	fake := &fakeClient{
		user:        gogs.User{ID: 3, Username: "tester"},
		resolvedSHA: fullTestSHA,
		archiveBody: singleFileArchive(t, "sub/file.txt", "needle\n"),
	}
	root := t.TempDir()
	manager, err := snapshot.NewManager(root, "https://gogs.example.test/", snapshot.DefaultLimits())
	require.NoError(t, err)
	session := connectTestClientWithSnapshots(t, fake, manager)

	response := callSearchCode(t, session, map[string]any{
		"owner": "owner", "repo": "repo", "query": "needle",
	})
	require.Nil(t, response.Error)
	require.Len(t, response.Data.Matches, 1)
	assert.Equal(t, "sub/file.txt", response.Data.Matches[0].Path)

	// The cache root contains only the instance hash and temporary directory.
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Len(t, entries, 2)
}

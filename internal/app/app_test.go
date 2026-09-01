package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogs-mcp/internal/config"
	"gogs-mcp/internal/snapshot"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionJSON(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := Run(context.Background(), []string{"version", "--json"}, strings.NewReader(""), &stdout, &stderr, emptyEnvironment)
	assert.Equal(t, exitOK, exitCode)
	assert.Empty(t, stderr.String())

	var result map[string]string
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	assert.NotEmpty(t, result["version"])
	assert.NotEmpty(t, result["commit"])
	assert.NotEmpty(t, result["build_time"])
	assert.Contains(t, result["go_version"], "go")
}

func TestVerifyReturnsSafeDiagnostics(t *testing.T) {
	const token = "diagnostic-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "token "+token, request.Header.Get("Authorization"))
		_, err := fmt.Fprint(writer, `{"id":21,"username":"verify-user","full_name":"Verify User","email":"verify@example.test"}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := Run(context.Background(), []string{"verify"}, strings.NewReader(""), &stdout, &stderr, mapEnvironment(map[string]string{
		"GOGS_BASE_URL":            server.URL,
		"GOGS_TOKEN":               token,
		"GOGS_ALLOW_INSECURE_HTTP": "true",
	}))
	assert.Equal(t, exitOK, exitCode)
	assert.Empty(t, stderr.String())
	assert.NotContains(t, stdout.String(), token)
	assert.NotContains(t, stdout.String(), "Authorization")

	var result diagnostic
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	assert.True(t, result.OK)
	require.NotNil(t, result.User)
	assert.Equal(t, "verify-user", result.User.Username)
}

func TestVerifyAuthenticationFailureIsSafe(t *testing.T) {
	const token = "invalid-diagnostic-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	var stdout bytes.Buffer
	exitCode := Run(context.Background(), []string{"verify"}, strings.NewReader(""), &stdout, &bytes.Buffer{}, mapEnvironment(map[string]string{
		"GOGS_BASE_URL":            server.URL,
		"GOGS_TOKEN":               token,
		"GOGS_ALLOW_INSECURE_HTTP": "true",
	}))
	assert.Equal(t, exitConnection, exitCode)
	assert.NotContains(t, stdout.String(), token)
	require.Contains(t, stdout.String(), "AUTHENTICATION_FAILED")
}

func TestServeOverStdioWithOfficialClient(t *testing.T) {
	const token = "stdio-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "token "+token, request.Header.Get("Authorization"))
		_, err := fmt.Fprint(writer, `{"id":88,"username":"stdio-user","full_name":"Stdio User","email":"stdio@example.test"}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	command := exec.Command(os.Args[0], "-test.run=^TestServeHelperProcess$")
	command.Env = append(filteredEnvironment(os.Environ()),
		"GOGS_MCP_HELPER_PROCESS=1",
		"GOGS_BASE_URL="+server.URL,
		"GOGS_TOKEN="+token,
		"GOGS_ALLOW_INSECURE_HTTP=true",
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "gogs-mcp-stdio-test",
		Version: "test",
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.CommandTransport{
		Command:           command,
		TerminateDuration: 5 * time.Second,
	}, nil)
	require.NoError(t, err)

	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	names := make([]string, len(tools.Tools))
	for index, tool := range tools.Tools {
		names[index] = tool.Name
	}
	assert.ElementsMatch(t, []string{
		"get_authenticated_user",
		"list_repositories",
		"search_repositories",
		"get_repository",
		"list_directory",
		"get_file",
		"list_branches",
		"get_branch",
		"list_commits",
		"get_commit",
		"search_code",
		"list_issues",
		"get_issue",
		"list_issue_comments",
	}, names)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_authenticated_user",
		Arguments: map[string]any{},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"username":"stdio-user"`)

	require.NoError(t, session.Close())
	assert.NotContains(t, stderr.String(), token)
	assert.NotContains(t, stderr.String(), "token "+token)
}

func TestServeOverStdioReturnsStructuredAuthenticationFailure(t *testing.T) {
	const token = "stdio-invalid-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	command := exec.Command(os.Args[0], "-test.run=^TestServeHelperProcess$")
	command.Env = append(filteredEnvironment(os.Environ()),
		"GOGS_MCP_HELPER_PROCESS=1",
		"GOGS_BASE_URL="+server.URL,
		"GOGS_TOKEN="+token,
		"GOGS_ALLOW_INSECURE_HTTP=true",
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "gogs-mcp-stdio-test",
		Version: "test",
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.CommandTransport{
		Command:           command,
		TerminateDuration: 5 * time.Second,
	}, nil)
	require.NoError(t, err)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_authenticated_user",
		Arguments: map[string]any{},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"code":"AUTHENTICATION_FAILED"`)
	assert.NotContains(t, string(encoded), token)

	require.NoError(t, session.Close())
	assert.NotContains(t, stderr.String(), token)
	assert.NotContains(t, stderr.String(), "token "+token)
}

func TestServeHelperProcess(t *testing.T) {
	if os.Getenv("GOGS_MCP_HELPER_PROCESS") != "1" {
		return
	}
	exitCode := Run(context.Background(), []string{"serve"}, os.Stdin, os.Stdout, os.Stderr, os.LookupEnv)
	os.Exit(exitCode)
}

func filteredEnvironment(source []string) []string {
	filtered := make([]string, 0, len(source))
	for _, value := range source {
		if strings.HasPrefix(value, "GOGS_") {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func mapEnvironment(values map[string]string) config.LookupEnv {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func emptyEnvironment(string) (string, bool) {
	return "", false
}

// minimalArchive is a tiny Gogs-style archive for cache maintenance tests.
func minimalArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(writer)
	header := &tar.Header{Name: "repo-aaa/file.txt", Typeflag: tar.TypeReg, Size: int64(len("content\n"))}
	require.NoError(t, archive.WriteHeader(header))
	_, err := archive.Write([]byte("content\n"))
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func seedSnapshotCache(t *testing.T, cacheRoot, baseURL string, userIDs ...int64) {
	t.Helper()
	manager, err := snapshot.NewManager(cacheRoot, baseURL, snapshot.DefaultLimits(), snapshot.DefaultEviction())
	require.NoError(t, err)
	for _, userID := range userIDs {
		result, err := manager.Ensure(context.Background(), snapshot.Key{
			UserID:    userID,
			Owner:     "owner",
			Repo:      "repo",
			CommitSHA: strings.Repeat("a", 40),
		}, func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(minimalArchive(t))), nil
		})
		require.NoError(t, err)
		result.Release()
	}
}

func TestCacheCleanRemovesCurrentUserCache(t *testing.T) {
	const token = "clean-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "token "+token, request.Header.Get("Authorization"))
		_, err := fmt.Fprint(writer, `{"id":21,"username":"clean-user","full_name":"Clean User","email":"clean@example.test"}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	cacheRoot := t.TempDir()
	seedSnapshotCache(t, cacheRoot, server.URL, 21, 22)

	var stdout bytes.Buffer
	exitCode := Run(context.Background(), []string{"cache", "clean"}, strings.NewReader(""), &stdout, &bytes.Buffer{}, mapEnvironment(map[string]string{
		"GOGS_BASE_URL":            server.URL,
		"GOGS_TOKEN":               token,
		"GOGS_ALLOW_INSECURE_HTTP": "true",
		"GOGS_MCP_CACHE_DIR":       cacheRoot,
	}))
	assert.Equal(t, exitOK, exitCode)
	assert.Contains(t, stdout.String(), "clean-user")
	assert.NotContains(t, stdout.String(), token)

	instance := instanceRoot(t, cacheRoot)
	remaining, err := os.ReadDir(instance)
	require.NoError(t, err)
	userDirs := make([]string, 0, len(remaining))
	for _, entry := range remaining {
		userDirs = append(userDirs, entry.Name())
	}
	assert.Equal(t, []string{"22"}, userDirs, "only the authenticated user's cache is removed")
}

func TestCacheCleanAllRemovesVerifiedRootsOnly(t *testing.T) {
	cacheRoot := t.TempDir()
	seedSnapshotCache(t, cacheRoot, "https://gogs.example.test/", 21)
	require.NoError(t, os.WriteFile(filepath.Join(cacheRoot, "keep.txt"), []byte("keep"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(cacheRoot, "tmp"), 0o700))
	instance := instanceRoot(t, cacheRoot)

	var stdout bytes.Buffer
	exitCode := Run(context.Background(), []string{"cache", "clean", "--all"}, strings.NewReader(""), &stdout, &bytes.Buffer{}, mapEnvironment(map[string]string{
		"GOGS_BASE_URL":            "https://gogs.example.test/",
		"GOGS_TOKEN":               "clean-secret-token",
		"GOGS_MCP_CACHE_DIR":       cacheRoot,
		"GOGS_ALLOW_INSECURE_HTTP": "true",
	}))
	assert.Equal(t, exitOK, exitCode)

	_, err := os.Stat(instance)
	assert.True(t, os.IsNotExist(err), "the verified instance root must be removed")
	_, err = os.Stat(filepath.Join(cacheRoot, "keep.txt"))
	require.NoError(t, err, "unrelated files must survive")
	_, err = os.Stat(filepath.Join(cacheRoot, "tmp"))
	assert.NoError(t, err, "the temporary area is not an instance root")
}

func TestCacheCleanRejectsBadInvocation(t *testing.T) {
	environment := mapEnvironment(map[string]string{
		"GOGS_BASE_URL":            "https://gogs.example.test/",
		"GOGS_TOKEN":               "clean-secret-token",
		"GOGS_ALLOW_INSECURE_HTTP": "true",
	})
	for _, args := range [][]string{
		{"cache"},
		{"cache", "scrub"},
		{"cache", "clean", "extra"},
	} {
		exitCode := Run(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, environment)
		assert.Equal(t, exitConfig, exitCode, "%v", args)
	}
}

func TestCacheCleanConnectionFailureExitsWithConnectionCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	var stderr bytes.Buffer
	exitCode := Run(context.Background(), []string{"cache", "clean"}, strings.NewReader(""), &bytes.Buffer{}, &stderr, mapEnvironment(map[string]string{
		"GOGS_BASE_URL":            server.URL,
		"GOGS_TOKEN":               "clean-secret-token",
		"GOGS_ALLOW_INSECURE_HTTP": "true",
		"GOGS_MCP_CACHE_DIR":       t.TempDir(),
	}))
	assert.Equal(t, exitConnection, exitCode)
	assert.NotContains(t, stderr.String(), "clean-secret-token")
}

// instanceRoot returns the single per-instance directory inside a seeded
// cache root.
func instanceRoot(t *testing.T, cacheRoot string) string {
	t.Helper()
	entries, err := os.ReadDir(cacheRoot)
	require.NoError(t, err)
	for _, entry := range entries {
		if len(entry.Name()) == 64 && entry.IsDir() {
			return filepath.Join(cacheRoot, entry.Name())
		}
	}
	t.Fatal("no instance root found in the cache")
	return ""
}

func TestServeOverStdioRegistersWriteToolsWhenEnabled(t *testing.T) {
	const token = "stdio-write-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "token "+token, request.Header.Get("Authorization"))
		_, err := fmt.Fprint(writer, `{"id":88,"username":"stdio-user","full_name":"Stdio User","email":"stdio@example.test"}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	command := exec.Command(os.Args[0], "-test.run=^TestServeHelperProcess$")
	command.Env = append(filteredEnvironment(os.Environ()),
		"GOGS_MCP_HELPER_PROCESS=1",
		"GOGS_BASE_URL="+server.URL,
		"GOGS_TOKEN="+token,
		"GOGS_ALLOW_INSECURE_HTTP=true",
		"GOGS_MCP_WRITE_ENABLED=true",
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "gogs-mcp-stdio-test",
		Version: "test",
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.CommandTransport{
		Command:           command,
		TerminateDuration: 5 * time.Second,
	}, nil)
	require.NoError(t, err)

	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	names := make([]string, len(tools.Tools))
	for index, tool := range tools.Tools {
		names[index] = tool.Name
	}
	assert.ElementsMatch(t, []string{
		"get_authenticated_user",
		"list_repositories",
		"search_repositories",
		"get_repository",
		"list_directory",
		"get_file",
		"list_branches",
		"get_branch",
		"list_commits",
		"get_commit",
		"search_code",
		"list_issues",
		"get_issue",
		"list_issue_comments",
		"create_issue",
	}, names)
	create := make([]*mcp.Tool, 0, 1)
	for _, tool := range tools.Tools {
		if tool.Name == "create_issue" {
			create = append(create, tool)
		}
	}
	require.Len(t, create, 1)
	require.NotNil(t, create[0].Annotations)
	assert.False(t, create[0].Annotations.ReadOnlyHint)

	require.NoError(t, session.Close())
	assert.NotContains(t, stderr.String(), token)
}

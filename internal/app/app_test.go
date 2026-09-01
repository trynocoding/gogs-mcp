package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"gogs-mcp/internal/config"

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

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// e2eHTTPTransport adds one Gogs credential header to every request, the way
// MCP clients present their own personal access token to a central
// deployment.
type e2eHTTPTransport struct {
	token string
}

func (t *e2eHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header.Set("Authorization", "token "+t.token)
	return http.DefaultTransport.RoundTrip(cloned)
}

func TestHTTPTransport(t *testing.T) {
	environment, projectRoot, identifier := prepareSmokeEnvironment(t)
	commandContext, cancelCommands := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelCommands()

	_, err := runCommand(commandContext, "create the HTTP transport E2E network", "docker", "network", "create", environment.network)
	require.NoError(t, err)
	dataDir := filepath.Join(environment.tempDir, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "log"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "app.ini"), []byte(gogsConfig(identifier)), 0o600))

	t.Log("Creating two Gogs accounts with personal access tokens.")
	bootstrapOutput, err := runSensitiveCommand(
		commandContext,
		"bootstrap the Gogs HTTP transport E2E database",
		"run",
		"--rm",
		"--name", environment.bootstrapContainer,
		"--network", environment.network,
		"--volume", dataDir+":/data:Z",
		"--entrypoint", "/app/gogs-e2e-repositories",
		environment.image,
		"--config", "/data/app.ini",
	)
	require.NoError(t, err)
	bootstrap := parseRepositoryBootstrap(t, bootstrapOutput)
	secrets := []string{bootstrap.Owner.Token, bootstrap.Outsider.Token}
	for _, secret := range secrets {
		require.NotEmpty(t, secret)
	}

	t.Log("Starting Gogs for the HTTP transport scenario.")
	_, err = runCommand(
		commandContext,
		"start the Gogs HTTP transport E2E container",
		"docker",
		"run",
		"--detach",
		"--name", environment.container,
		"--network", environment.network,
		"--publish", "127.0.0.1::3000",
		"--volume", dataDir+":/data:Z",
		environment.image,
	)
	require.NoError(t, err)
	baseURL := waitForGogs(t, environment.container)

	t.Log("Starting the gogs-mcp HTTP transport.")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	var stderr bytes.Buffer
	serverCommand := exec.Command(filepath.Join(projectRoot, ".bin", "gogs-mcp"), "serve")
	serverCommand.Stderr = &stderr
	serverCommand.Env = append(filteredEnvironment(os.Environ()),
		"GOGS_BASE_URL="+baseURL,
		"GOGS_ALLOW_INSECURE_HTTP=true",
		"GOGS_MCP_TRANSPORT=http",
		"GOGS_MCP_HTTP_ADDR="+addr,
		"GOGS_MCP_CACHE_DIR="+filepath.Join(environment.tempDir, "cache"),
		"NO_PROXY=127.0.0.1,localhost",
	)
	require.NoError(t, serverCommand.Start())
	t.Cleanup(func() {
		_ = serverCommand.Process.Kill()
		_ = serverCommand.Wait()
	})

	require.Eventually(t, func() bool {
		response, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 10*time.Second, 100*time.Millisecond, "the HTTP transport must serve the health probe")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "gogs-mcp-e2e-http", Version: "test"}, nil)
	connect := func(token string) *mcp.ClientSession {
		t.Helper()
		transport := &mcp.StreamableClientTransport{
			Endpoint:             "http://" + addr + "/mcp",
			HTTPClient:           &http.Client{Transport: &e2eHTTPTransport{token: token}},
			DisableStandaloneSSE: true,
		}
		session, err := client.Connect(ctx, transport, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = session.Close() })
		return session
	}

	t.Log("Verifying that every token sees only its own identity.")
	owner := connect(bootstrap.Owner.Token)
	tools, err := owner.ListTools(ctx, nil)
	require.NoError(t, err)
	names := make([]string, len(tools.Tools))
	for index, tool := range tools.Tools {
		names[index] = tool.Name
	}
	assert.ElementsMatch(t, e2eToolNames, names)

	for _, testCase := range []struct {
		session  *mcp.ClientSession
		username string
	}{
		{owner, bootstrap.Owner.Username},
		{connect(bootstrap.Outsider.Token), bootstrap.Outsider.Username},
	} {
		result, err := testCase.session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "get_authenticated_user",
			Arguments: map[string]any{},
		})
		require.NoError(t, err)
		require.False(t, result.IsError)
		var response toolResponse
		decodeStructuredContent(t, result.StructuredContent, &response)
		require.NotNil(t, response.Data)
		assert.Equal(t, testCase.username, response.Data.Username)
	}

	t.Log("Rejecting a request without a credential.")
	response, err := http.Post("http://"+addr+"/mcp", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)

	_ = serverCommand.Process.Kill()
	_ = serverCommand.Wait()
	assertNoSecrets(t, stderr.Bytes(), secrets...)

	t.Log("Removing all HTTP transport E2E resources.")
	environment.cleanup(t)
	assertResourcesRemoved(t, environment)
}

//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLegacyInitializeProtocol speaks raw JSON-RPC over stdio with the first
// MCP protocol version instead of the official client. Every other E2E
// scenario already covers the modern negotiation path through the SDK, so
// this test proves that an older client can complete the handshake and see
// the same tool list.
func TestLegacyInitializeProtocol(t *testing.T) {
	projectRoot := projectRoot(t)
	const legacyToken = "legacy-protocol-token"

	command := exec.Command(filepath.Join(projectRoot, ".bin", "gogs-mcp"), "serve")
	command.Env = append(filteredEnvironment(os.Environ()),
		"GOGS_BASE_URL=https://gogs.legacy.example/",
		"GOGS_TOKEN="+legacyToken,
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdin, err := command.StdinPipe()
	require.NoError(t, err)
	stdout, err := command.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	readResponse := func() map[string]any {
		t.Helper()
		line := make(chan string, 1)
		go func() {
			if scanner.Scan() {
				line <- scanner.Text()
				return
			}
			line <- ""
		}()
		select {
		case text := <-line:
			require.NotEmpty(t, text, "the server closed the protocol stream")
			var response map[string]any
			require.NoError(t, json.Unmarshal([]byte(text), &response), "the server sent malformed JSON: %s", text)
			return response
		case <-time.After(30 * time.Second):
			require.FailNow(t, "the server did not answer within 30 seconds")
			return nil
		}
	}
	writeJSON := func(value any) {
		t.Helper()
		require.NoError(t, json.NewEncoder(stdin).Encode(value))
	}

	t.Log("Negotiating the session with the 2024-11-05 protocol version.")
	writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "gogs-mcp-legacy-e2e", "version": "test"},
		},
	})
	response := readResponse()
	require.Nil(t, response["error"], "the initialize request failed: %v", response["error"])
	result, ok := response["result"].(map[string]any)
	require.True(t, ok, "the initialize response carries no result: %v", response)
	assert.NotEmpty(t, result["protocolVersion"], "the response must name the negotiated protocol version")
	serverInfo, ok := result["serverInfo"].(map[string]any)
	require.True(t, ok, "the response carries no server info: %v", result)
	assert.NotEmpty(t, serverInfo["name"])

	t.Log("Listing tools through the legacy session.")
	writeJSON(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	writeJSON(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	response = readResponse()
	require.Nil(t, response["error"], "the tools/list request failed: %v", response["error"])
	result, ok = response["result"].(map[string]any)
	require.True(t, ok, "the tools/list response carries no result: %v", response)
	tools, ok := result["tools"].([]any)
	require.True(t, ok, "the tools/list response carries no tools: %v", result)
	var names []string
	for _, tool := range tools {
		entry, ok := tool.(map[string]any)
		require.True(t, ok, "a tool entry is not an object: %v", tool)
		name, ok := entry["name"].(string)
		require.True(t, ok, "a tool entry carries no name: %v", tool)
		names = append(names, name)
	}
	assert.ElementsMatch(t, e2eToolNames, names,
		"a legacy client must see exactly the default read-only tool list")

	assertNoSecrets(t, stderr.Bytes(), legacyToken)
}

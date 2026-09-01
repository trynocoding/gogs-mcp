//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type searchMatch struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	LineText string `json:"line_text"`
}

type searchData struct {
	Query     string        `json:"query"`
	Ref       string        `json:"ref"`
	CommitSHA string        `json:"commit_sha"`
	Mode      string        `json:"mode"`
	Matches   []searchMatch `json:"matches"`
}

type searchResponse struct {
	Data  *searchData `json:"data,omitempty"`
	Error *toolError  `json:"error,omitempty"`
	Meta  struct {
		RequestID string   `json:"request_id"`
		Truncated bool     `json:"truncated"`
		CacheHit  bool     `json:"cache_hit"`
		Warnings  []string `json:"warnings"`
	} `json:"meta"`
}

func TestSearch(t *testing.T) {
	environment, projectRoot, identifier := prepareSmokeEnvironment(t)
	commandContext, cancelCommands := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelCommands()

	_, err := runCommand(commandContext, "create the search E2E network", "docker", "network", "create", environment.network)
	require.NoError(t, err)
	dataDir := filepath.Join(environment.tempDir, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "log"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "app.ini"), []byte(gogsConfig(identifier)), 0o600))

	t.Log("Seeding a repository with branches, tags, and a multi-commit history.")
	bootstrapOutput, err := runSensitiveCommand(
		commandContext,
		"bootstrap the Gogs search E2E database",
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
	secrets := []string{bootstrap.Owner.Token, bootstrap.Collaborator.Token, bootstrap.Outsider.Token}

	_, err = runCommand(
		commandContext,
		"start the Gogs search E2E container",
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

	// A shared cache directory lets two consecutive server processes verify
	// that snapshots persist across restarts.
	cacheDir := filepath.Join(environment.tempDir, "cache")

	t.Log("Searching literal code through a real stdio MCP process.")
	mcpLogs := useMCPClientWithCache(t, projectRoot, baseURL, bootstrap.Collaborator.Token, cacheDir, func(session *mcp.ClientSession) {
		feature := callSearch(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "query": "feature-version", "ref": bootstrap.Refs.FeatureBranch,
		})
		assert.Equal(t, bootstrap.Refs.FeatureSHA, feature.Data.CommitSHA)
		assert.Equal(t, bootstrap.Refs.FeatureBranch, feature.Data.Ref)
		assert.Equal(t, "literal", feature.Data.Mode)
		require.Len(t, feature.Data.Matches, 1)
		match := feature.Data.Matches[0]
		assert.Equal(t, "src/version.txt", match.Path)
		assert.Equal(t, 1, match.Line)
		assert.Equal(t, 1, match.Column)
		assert.Equal(t, "feature-version", match.LineText)
		assert.False(t, feature.Meta.CacheHit)
		assert.NotEmpty(t, feature.Meta.RequestID)

		// Matching is case-sensitive and reports zero matches rather than an error.
		caseSensitive := callSearch(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "query": "Feature-Version", "ref": bootstrap.Refs.FeatureBranch,
		})
		assert.Empty(t, caseSensitive.Data.Matches)
		assert.False(t, caseSensitive.Meta.Truncated)

		// An empty ref resolves through the repository default branch.
		main := callSearch(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "query": "main-version",
		})
		assert.Equal(t, bootstrap.Refs.MainCommitSHA, main.Data.CommitSHA)
		require.Len(t, main.Data.Matches, 1)
		assert.Equal(t, "main-version", main.Data.Matches[0].LineText)
		assert.False(t, main.Meta.CacheHit)

		// An unknown ref is reported as not found without downloading anything.
		unknown := callE2ETool(t, session, "search_code", map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "query": "feature-version", "ref": "does-not-exist",
		})
		assert.True(t, unknown.IsError)
		var unknownResponse searchResponse
		decodeStructuredContent(t, unknown.StructuredContent, &unknownResponse)
		require.NotNil(t, unknownResponse.Error)
		assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", unknownResponse.Error.Code)

		// A repeat search within the same process hits the published snapshot.
		repeat := callSearch(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "query": "feature-version", "ref": bootstrap.Refs.FeatureBranch,
		})
		assert.True(t, repeat.Meta.CacheHit)
		require.Len(t, repeat.Data.Matches, 1)
	})

	assertNoSecrets(t, mcpLogs, secrets...)

	t.Log("Verifying the snapshot cache survives a server restart.")
	restartLogs := useMCPClientWithCache(t, projectRoot, baseURL, bootstrap.Collaborator.Token, cacheDir, func(session *mcp.ClientSession) {
		cached := callSearch(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "query": "feature-version", "ref": bootstrap.Refs.FeatureBranch,
		})
		assert.True(t, cached.Meta.CacheHit)
		require.Len(t, cached.Data.Matches, 1)
		assert.Equal(t, bootstrap.Refs.FeatureSHA, cached.Data.CommitSHA)
	})
	assertNoSecrets(t, restartLogs, secrets...)

	gogsLogs, err := runCommand(commandContext, "read Gogs search E2E logs", "docker", "logs", environment.container)
	require.NoError(t, err)
	assertNoSecrets(t, gogsLogs, secrets...)
	// The archive endpoint must be requested with the resolved full commit SHA.
	assert.Contains(t, string(gogsLogs), "/api/v1/repos/owner/private-shared/archive/"+bootstrap.Refs.FeatureSHA+".tar.gz")
	assert.Contains(t, string(gogsLogs), "/api/v1/repos/owner/private-shared/archive/"+bootstrap.Refs.MainCommitSHA+".tar.gz")
	assert.NotContains(t, strings.ToLower(string(gogsLogs)), "escape")

	t.Log("Removing all search E2E resources.")
	environment.cleanup(t)
	assertResourcesRemoved(t, environment)
}

func callSearch(t *testing.T, session *mcp.ClientSession, arguments map[string]any) searchResponse {
	t.Helper()
	result := callE2ETool(t, session, "search_code", arguments)
	require.False(t, result.IsError)
	var response searchResponse
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

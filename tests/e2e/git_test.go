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

type branch struct {
	Name    string `json:"name"`
	HeadSHA string `json:"head_sha"`
}

type branchPage struct {
	Branches []branch `json:"branches"`
	Page     int      `json:"page"`
	PerPage  int      `json:"per_page"`
	Total    int      `json:"total"`
}

type commitSummary struct {
	SHA        string `json:"sha"`
	Message    string `json:"message"`
	AuthorName string `json:"author_name"`
	AuthorDate string `json:"author_date"`
}

type commitPage struct {
	Commits []commitSummary `json:"commits"`
	Limit   int             `json:"limit"`
}

type commitPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type commitDetail struct {
	SHA        string       `json:"sha"`
	Message    string       `json:"message"`
	WebURL     string       `json:"web_url"`
	Author     commitPerson `json:"author"`
	Committer  commitPerson `json:"committer"`
	ParentSHAs []string     `json:"parent_shas,omitempty"`
}

type gitResponse[T any] struct {
	Data  *T         `json:"data,omitempty"`
	Error *toolError `json:"error,omitempty"`
	Meta  struct {
		RequestID string   `json:"request_id"`
		Truncated bool     `json:"truncated"`
		NextPage  *int     `json:"next_page,omitempty"`
		Warnings  []string `json:"warnings,omitempty"`
	} `json:"meta"`
}

func TestGit(t *testing.T) {
	environment, projectRoot, identifier := prepareSmokeEnvironment(t)
	commandContext, cancelCommands := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelCommands()

	_, err := runCommand(commandContext, "create the git E2E network", "docker", "network", "create", environment.network)
	require.NoError(t, err)
	dataDir := filepath.Join(environment.tempDir, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "log"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "app.ini"), []byte(gogsConfig(identifier)), 0o600))

	t.Log("Seeding a repository with branches, tags, and a multi-commit history.")
	bootstrapOutput, err := runSensitiveCommand(
		commandContext,
		"bootstrap the Gogs git E2E database",
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
	assertRepositoryRefs(t, bootstrap.Refs)
	secrets := []string{bootstrap.Owner.Token, bootstrap.Collaborator.Token, bootstrap.Outsider.Token}

	_, err = runCommand(
		commandContext,
		"start the Gogs git E2E container",
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

	t.Log("Inspecting branches and commits through a real stdio MCP process.")
	mcpLogs := useMCPClient(t, projectRoot, baseURL, bootstrap.Collaborator.Token, func(session *mcp.ClientSession) {
		branches := callBranchPage(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository,
		})
		require.Len(t, branches.Data.Branches, 2)
		assert.Equal(t, "feature/content", branches.Data.Branches[0].Name)
		assert.Equal(t, bootstrap.Refs.FeatureSHA, branches.Data.Branches[0].HeadSHA)
		assert.Equal(t, "main", branches.Data.Branches[1].Name)
		assert.Equal(t, bootstrap.Refs.MainCommitSHA, branches.Data.Branches[1].HeadSHA)
		assert.Equal(t, 1, branches.Data.Page)
		assert.Equal(t, 2, branches.Data.Total)
		assert.NotEmpty(t, branches.Meta.RequestID)

		featureBranch := callBranch(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "branch": bootstrap.Refs.FeatureBranch,
		})
		assert.Equal(t, "feature/content", featureBranch.Data.Name)
		assert.Equal(t, bootstrap.Refs.FeatureSHA, featureBranch.Data.HeadSHA)

		mainBranch := callBranch(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "branch": bootstrap.Refs.DefaultBranch,
		})
		assert.Equal(t, bootstrap.Refs.MainCommitSHA, mainBranch.Data.HeadSHA)

		commits := callCommitPage(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository,
		})
		assert.Equal(t, 30, commits.Data.Limit)
		require.Len(t, commits.Data.Commits, 3)
		latest := commits.Data.Commits[0]
		assert.Equal(t, bootstrap.Refs.MainCommitSHA, latest.SHA)
		assert.Equal(t, "Change main version", latest.Message)
		// Gogs v0.14.2 only exposes the first line of a commit message; the
		// seeded message carries a body that must never appear in any response.
		assert.NotContains(t, latest.Message, "not exposed")
		assert.Equal(t, "Gogs MCP E2E", latest.AuthorName)
		assert.NotEmpty(t, latest.AuthorDate)
		assert.False(t, commits.Meta.Truncated)

		limited := callCommitPage(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "limit": 2,
		})
		assert.Equal(t, 2, limited.Data.Limit)
		require.Len(t, limited.Data.Commits, 2)
		assert.Equal(t, bootstrap.Refs.MainCommitSHA, limited.Data.Commits[0].SHA)
		assert.Equal(t, bootstrap.Refs.TagSHA, limited.Data.Commits[1].SHA)

		commit := callCommit(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "sha": bootstrap.Refs.MainCommitSHA,
		})
		assert.Equal(t, bootstrap.Refs.MainCommitSHA, commit.Data.SHA)
		assert.Equal(t, "Change main version", commit.Data.Message)
		assert.NotContains(t, commit.Data.Message, "not exposed")
		assert.Equal(t, "Gogs MCP E2E", commit.Data.Author.Name)
		assert.Equal(t, "gogs-mcp@example.test", commit.Data.Author.Email)
		assert.NotEmpty(t, commit.Data.Author.Date)
		assert.Contains(t, commit.Data.WebURL, "/commits/"+bootstrap.Refs.MainCommitSHA)
		require.Len(t, commit.Data.ParentSHAs, 1)
		assert.Equal(t, bootstrap.Refs.TagSHA, commit.Data.ParentSHAs[0])

		shortSHA := bootstrap.Refs.TagSHA[:10]
		shortCommit := callCommit(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "sha": shortSHA,
		})
		assert.Equal(t, bootstrap.Refs.TagSHA, shortCommit.Data.SHA)
		assert.Equal(t, "Add submodule fixture", shortCommit.Data.Message)

		// Empty names are rejected by the input schema before any handler runs,
		// so only names that reach the server are asserted here.
		for _, invalidBranch := range []string{"does-not-exist", "../escape", `feature\content`, "feature//content"} {
			result := callE2ETool(t, session, "get_branch", map[string]any{
				"owner": "owner", "repo": bootstrap.SharedRepository, "branch": invalidBranch,
			})
			assert.True(t, result.IsError, "branch %q should fail", invalidBranch)
			var invalid gitResponse[branch]
			decodeStructuredContent(t, result.StructuredContent, &invalid)
			if invalidBranch == "does-not-exist" {
				require.NotNil(t, invalid.Error)
				assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", invalid.Error.Code)
			} else {
				require.NotNil(t, invalid.Error)
				assert.Equal(t, "VALIDATION_FAILED", invalid.Error.Code)
			}
		}

		// Gogs resolves unknown revisions through `git rev-parse`, so a revision
		// that rev-parse rejects returns 404. A well-formed full SHA is echoed by
		// rev-parse and then rejected by `git cat-file`, which Gogs reports as a
		// server error rather than a 404; both behaviors are asserted as they are.
		unknownCommit := callE2ETool(t, session, "get_commit", map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "sha": "does-not-exist",
		})
		assert.True(t, unknownCommit.IsError)
		var unknown gitResponse[commitDetail]
		decodeStructuredContent(t, unknownCommit.StructuredContent, &unknown)
		require.NotNil(t, unknown.Error)
		assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", unknown.Error.Code)

		wellFormedCommit := callE2ETool(t, session, "get_commit", map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "sha": "ffffffffffffffffffffffffffffffffffffffff",
		})
		assert.True(t, wellFormedCommit.IsError)
		var wellFormed gitResponse[commitDetail]
		decodeStructuredContent(t, wellFormedCommit.StructuredContent, &wellFormed)
		require.NotNil(t, wellFormed.Error)
		assert.Equal(t, "GOGS_ERROR", wellFormed.Error.Code)

		for _, invalidSHA := range []string{".", "..", "../escape", `main\feature`, "main/\x00escape"} {
			result := callE2ETool(t, session, "get_commit", map[string]any{
				"owner": "owner", "repo": bootstrap.SharedRepository, "sha": invalidSHA,
			})
			assert.True(t, result.IsError, "sha %q should fail", invalidSHA)
			var invalid gitResponse[commitDetail]
			decodeStructuredContent(t, result.StructuredContent, &invalid)
			require.NotNil(t, invalid.Error)
			assert.Equal(t, "VALIDATION_FAILED", invalid.Error.Code)
		}
	})

	assertNoSecrets(t, mcpLogs, secrets...)
	gogsLogs, err := runCommand(commandContext, "read Gogs git E2E logs", "docker", "logs", environment.container)
	require.NoError(t, err)
	assertNoSecrets(t, gogsLogs, secrets...)
	assert.Contains(t, string(gogsLogs), "/api/v1/repos/owner/private-shared/branches/feature/content")
	assert.Contains(t, string(gogsLogs), "/api/v1/repos/owner/private-shared/commits/"+bootstrap.Refs.MainCommitSHA)
	assert.NotContains(t, strings.ToLower(string(gogsLogs)), "escape")

	t.Log("Removing all git E2E resources.")
	environment.cleanup(t)
	assertResourcesRemoved(t, environment)
}

func callBranchPage(t *testing.T, session *mcp.ClientSession, arguments map[string]any) gitResponse[branchPage] {
	t.Helper()
	result := callE2ETool(t, session, "list_branches", arguments)
	require.False(t, result.IsError)
	var response gitResponse[branchPage]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func callBranch(t *testing.T, session *mcp.ClientSession, arguments map[string]any) gitResponse[branch] {
	t.Helper()
	result := callE2ETool(t, session, "get_branch", arguments)
	require.False(t, result.IsError)
	var response gitResponse[branch]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func callCommitPage(t *testing.T, session *mcp.ClientSession, arguments map[string]any) gitResponse[commitPage] {
	t.Helper()
	result := callE2ETool(t, session, "list_commits", arguments)
	require.False(t, result.IsError)
	var response gitResponse[commitPage]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func callCommit(t *testing.T, session *mcp.ClientSession, arguments map[string]any) gitResponse[commitDetail] {
	t.Helper()
	result := callE2ETool(t, session, "get_commit", arguments)
	require.False(t, result.IsError)
	var response gitResponse[commitDetail]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

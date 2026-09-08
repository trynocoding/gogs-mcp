//go:build e2e

package e2e

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pullRequestSummary struct {
	Number      int64     `json:"number"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	User        issueUser `json:"user"`
	NumComments int       `json:"num_comments"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
	HeadSHA     string    `json:"head_sha"`
}

type pullRequestPage struct {
	PullRequests []pullRequestSummary `json:"pull_requests"`
	State        string               `json:"state"`
	Limit        int                  `json:"limit"`
	Total        int                  `json:"total"`
}

type pullRequest struct {
	Number         int64        `json:"number"`
	Title          string       `json:"title"`
	Body           string       `json:"body"`
	State          string       `json:"state"`
	User           issueUser    `json:"user"`
	Labels         []issueLabel `json:"labels"`
	NumComments    int          `json:"num_comments"`
	CreatedAt      string       `json:"created_at"`
	UpdatedAt      string       `json:"updated_at"`
	WebURL         string       `json:"web_url"`
	HeadSHA        string       `json:"head_sha"`
	BaseRef        string       `json:"base_ref"`
	BaseRefAssumed bool         `json:"base_ref_assumed"`
}

type pullDiffFile struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	IsBinary  bool   `json:"is_binary"`
}

type pullCommit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

type pullRequestDiff struct {
	Number             int64          `json:"number"`
	BaseRef            string         `json:"base_ref"`
	BaseRefAssumed     bool           `json:"base_ref_assumed"`
	MergeBase          string         `json:"merge_base"`
	Diff               string         `json:"diff"`
	Files              []pullDiffFile `json:"files"`
	Commits            []pullCommit   `json:"commits"`
	Truncated          bool           `json:"truncated"`
	MergeState         string         `json:"merge_state"`
	MergeConflictPaths []string       `json:"merge_conflict_paths"`
	BaseCommits        int            `json:"base_commits"`
}

type pullResponse[T any] struct {
	Data  *T           `json:"data,omitempty"`
	Error *toolError   `json:"error,omitempty"`
	Meta  responseMeta `json:"meta"`
}

func TestPulls(t *testing.T) {
	environment, projectRoot, identifier := prepareSmokeEnvironment(t)
	commandContext, cancelCommands := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelCommands()

	_, err := runCommand(commandContext, "create the pull E2E network", "docker", "network", "create", environment.network)
	require.NoError(t, err)
	dataDir := filepath.Join(environment.tempDir, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "log"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "app.ini"), []byte(gogsConfig(identifier)), 0o600))

	t.Log("Seeding a repository with a refs/pull/1/head ref and its underlying issue.")
	bootstrapOutput, err := runSensitiveCommand(
		commandContext,
		"bootstrap the Gogs pull E2E database",
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
		"start the Gogs pull E2E container",
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

	t.Log("Listing and reading the seeded pull request through a real stdio MCP process.")
	collaboratorLogs := useMCPClient(t, projectRoot, baseURL, bootstrap.Collaborator.Token, func(session *mcp.ClientSession) {
		listing := callPullPage(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository,
		})
		assert.Equal(t, "open", listing.Data.State)
		assert.Equal(t, 30, listing.Data.Limit)
		assert.Equal(t, 1, listing.Data.Total)
		require.Len(t, listing.Data.PullRequests, 1)
		summary := listing.Data.PullRequests[0]
		assert.Equal(t, int64(1), summary.Number)
		assert.Equal(t, "Merge feature/content into main", summary.Title)
		assert.Equal(t, "open", summary.State)
		assert.Equal(t, "owner", summary.User.Username)
		assert.Equal(t, bootstrap.Refs.PullSHA, summary.HeadSHA)
		assert.Equal(t, 0, summary.NumComments)
		assert.NotEmpty(t, summary.CreatedAt)
		assert.NotEmpty(t, summary.UpdatedAt)
		assert.NotEmpty(t, listing.Meta.RequestID)

		closed := callPullPage(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "state": "closed",
		})
		assert.Empty(t, closed.Data.PullRequests)

		all := callPullPage(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "state": "all",
		})
		require.Len(t, all.Data.PullRequests, 1)
		assert.Equal(t, "Merge feature/content into main", all.Data.PullRequests[0].Title)

		detail := callPull(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "number": 1,
		})
		assert.Equal(t, int64(1), detail.Data.Number)
		assert.Equal(t, "Merge feature/content into main", detail.Data.Title)
		assert.Equal(t, "The pull request fixture of the E2E suite.", detail.Data.Body)
		assert.Equal(t, "open", detail.Data.State)
		assert.Equal(t, "owner", detail.Data.User.Username)
		assert.Empty(t, detail.Data.Labels)
		assert.Equal(t, bootstrap.Refs.PullSHA, detail.Data.HeadSHA)
		assert.Equal(t, "main", detail.Data.BaseRef)
		assert.True(t, detail.Data.BaseRefAssumed)
		assert.Contains(t, detail.Data.WebURL, "owner/private-shared")
		assert.NotEmpty(t, detail.Meta.RequestID)

		missing := callE2ETool(t, session, "get_pull_request", map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "number": 999,
		})
		assert.True(t, missing.IsError)
		var missingPull pullResponse[pullRequest]
		decodeStructuredContent(t, missing.StructuredContent, &missingPull)
		require.NotNil(t, missingPull.Error)
		// A number without a refs/pull/{n}/head entry is reported as an
		// argument error, whether or not an issue with that number exists.
		assert.Equal(t, "INVALID_ARGUMENT", missingPull.Error.Code)

		t.Log("Diffing the pull request against the assumed default branch.")
		assumed := callPullDiff(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "number": 1,
		})
		assert.Equal(t, int64(1), assumed.Data.Number)
		assert.Equal(t, "main", assumed.Data.BaseRef)
		assert.True(t, assumed.Data.BaseRefAssumed)
		assert.Equal(t, bootstrap.Refs.TagSHA, assumed.Data.MergeBase)
		assert.Equal(t, "conflicting", assumed.Data.MergeState)
		assert.Equal(t, []string{"src/version.txt"}, assumed.Data.MergeConflictPaths)
		assert.Equal(t, 1, assumed.Data.BaseCommits)
		assert.False(t, assumed.Data.Truncated)
		assert.Contains(t, assumed.Data.Diff, "diff --git a/src/version.txt b/src/version.txt")
		assert.Contains(t, assumed.Data.Diff, "-tag-version")
		assert.Contains(t, assumed.Data.Diff, "+feature-version")
		assert.Contains(t, assumed.Data.Diff, "第二行")
		require.Len(t, assumed.Data.Files, 1)
		assert.Equal(t, "src/version.txt", assumed.Data.Files[0].Path)
		assert.Equal(t, "modified", assumed.Data.Files[0].Status)
		assert.Equal(t, 1, assumed.Data.Files[0].Additions)
		assert.Equal(t, 1, assumed.Data.Files[0].Deletions)
		assert.False(t, assumed.Data.Files[0].IsBinary)
		require.Len(t, assumed.Data.Commits, 1)
		assert.Equal(t, bootstrap.Refs.PullSHA, assumed.Data.Commits[0].SHA)
		assert.Equal(t, "Change feature version", assumed.Data.Commits[0].Message)
		assert.Equal(t, "Gogs MCP E2E", assumed.Data.Commits[0].Author)
		assert.NotEmpty(t, assumed.Data.Commits[0].Date)
		warnings := strings.Join(assumed.Meta.Warnings, " ")
		assert.Contains(t, warnings, "carries 1 commits since the merge base")
		assert.Contains(t, warnings, "src/version.txt")
		assert.NotEmpty(t, assumed.Meta.RequestID)

		t.Log("Diffing the pull request against an explicit branch at the merge base.")
		base := callPullDiff(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "number": 1, "base_ref": bootstrap.Refs.ReleaseBranch,
		})
		assert.Equal(t, bootstrap.Refs.ReleaseBranch, base.Data.BaseRef)
		assert.False(t, base.Data.BaseRefAssumed)
		assert.Equal(t, "fast_forward", base.Data.MergeState)
		assert.Empty(t, base.Data.MergeConflictPaths)
		assert.Equal(t, 0, base.Data.BaseCommits)
		require.Len(t, base.Data.Files, 1)
		assert.Equal(t, "src/version.txt", base.Data.Files[0].Path)
		assert.NotContains(t, strings.Join(base.Meta.Warnings, " "), "default branch")

		t.Log("Rejecting a base ref that names no branch.")
		missingBranch := callE2ETool(t, session, "get_pull_request_diff", map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "number": 1, "base_ref": "v1.0.0",
		})
		assert.True(t, missingBranch.IsError)
		var missingBranchDiff pullResponse[pullRequestDiff]
		decodeStructuredContent(t, missingBranch.StructuredContent, &missingBranchDiff)
		require.NotNil(t, missingBranchDiff.Error)
		// The base ref is resolved from refs/heads only, so a tag name is
		// reported as a missing branch rather than being silently accepted.
		assert.Equal(t, "INVALID_ARGUMENT", missingBranchDiff.Error.Code)

		t.Log("Restricting the diff with an empty path filter.")
		filtered := callPullDiff(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "number": 1, "paths": []string{"vendor/"},
		})
		assert.Empty(t, filtered.Data.Files)
		assert.Equal(t, "conflicting", filtered.Data.MergeState)
		assert.Contains(t, strings.Join(filtered.Meta.Warnings, " "), "No changes matched")

		invalidRef := callE2ETool(t, session, "get_pull_request_diff", map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "number": 1, "base_ref": "../escape",
		})
		assert.True(t, invalidRef.IsError)
		var invalidDiff pullResponse[pullRequestDiff]
		decodeStructuredContent(t, invalidRef.StructuredContent, &invalidDiff)
		require.NotNil(t, invalidDiff.Error)
		// The base ref only enters a git refspec, never a URL path, so a
		// traversal-shaped name fails ref resolution like any other missing
		// branch instead of hitting the path validation code.
		assert.Equal(t, "INVALID_ARGUMENT", invalidDiff.Error.Code)
	})

	t.Log("Diffing through a server with an explicit cache directory.")
	cacheDir := t.TempDir()
	cacheLogs := useMCPClientWithCache(t, projectRoot, baseURL, bootstrap.Collaborator.Token, cacheDir, func(session *mcp.ClientSession) {
		callPullDiff(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "number": 1,
		})
	})
	roots, err := filepath.Glob(filepath.Join(cacheDir, "*", "*", ".pull"))
	require.NoError(t, err)
	require.Len(t, roots, 1)
	pullRoot := roots[0]
	entries, err := os.ReadDir(pullRoot)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	info, err := entries[0].Info()
	require.NoError(t, err)
	require.True(t, info.IsDir())
	assert.Equal(t, fs.FileMode(0o700), info.Mode().Perm())

	t.Log("Verifying that the outsider cannot see the private pull request.")
	outsiderLogs := useMCPClient(t, projectRoot, baseURL, bootstrap.Outsider.Token, func(session *mcp.ClientSession) {
		for _, call := range []struct {
			tool      string
			arguments map[string]any
		}{
			{"list_pull_requests", map[string]any{"owner": "owner", "repo": bootstrap.SharedRepository}},
			{"get_pull_request", map[string]any{"owner": "owner", "repo": bootstrap.SharedRepository, "number": 1}},
			{"get_pull_request_diff", map[string]any{"owner": "owner", "repo": bootstrap.SharedRepository, "number": 1}},
		} {
			result := callE2ETool(t, session, call.tool, call.arguments)
			assert.True(t, result.IsError, "%s should fail for the outsider", call.tool)
			var response pullResponse[pullRequest]
			decodeStructuredContent(t, result.StructuredContent, &response)
			require.NotNil(t, response.Error)
			// The pull request tools resolve refs over the git protocol, and
			// Gogs answers an authenticated but unauthorized git request with
			// 403, which the shared taxonomy reports as an authentication
			// failure rather than the API not-found code.
			assert.Equal(t, "AUTHENTICATION_FAILED", response.Error.Code)
		}

		own := callPullPage(t, session, map[string]any{
			"owner": "outsider", "repo": "outsider-private",
		})
		assert.Empty(t, own.Data.PullRequests)
		assert.Equal(t, 0, own.Data.Total)
	})

	allLogs := collaboratorLogs
	allLogs = append(allLogs, cacheLogs...)
	allLogs = append(allLogs, outsiderLogs...)
	// The pull tools return private source and issue bodies verbatim, so the
	// leak scan must include them alongside the tokens.
	leakNeedles := append(slices.Clone(secrets), "feature-version")
	assertNoSecrets(t, allLogs, leakNeedles...)
	gogsLogs, err := runCommand(commandContext, "read Gogs pull E2E logs", "docker", "logs", environment.container)
	require.NoError(t, err)
	assertNoSecrets(t, gogsLogs, leakNeedles...)
	assert.Contains(t, string(gogsLogs), "/api/v1/repos/owner/private-shared/issues/1")
	assert.NotContains(t, strings.ToLower(string(gogsLogs)), "escape")

	t.Log("Removing all pull E2E resources.")
	environment.cleanup(t)
	assertResourcesRemoved(t, environment)
}

func callPullPage(t *testing.T, session *mcp.ClientSession, arguments map[string]any) pullResponse[pullRequestPage] {
	t.Helper()
	result := callE2ETool(t, session, "list_pull_requests", arguments)
	require.False(t, result.IsError)
	var response pullResponse[pullRequestPage]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func callPull(t *testing.T, session *mcp.ClientSession, arguments map[string]any) pullResponse[pullRequest] {
	t.Helper()
	result := callE2ETool(t, session, "get_pull_request", arguments)
	require.False(t, result.IsError)
	var response pullResponse[pullRequest]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func callPullDiff(t *testing.T, session *mcp.ClientSession, arguments map[string]any) pullResponse[pullRequestDiff] {
	t.Helper()
	result := callE2ETool(t, session, "get_pull_request_diff", arguments)
	require.False(t, result.IsError)
	var response pullResponse[pullRequestDiff]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

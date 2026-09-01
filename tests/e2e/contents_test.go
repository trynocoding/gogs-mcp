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

type contentMeta struct {
	Truncated     bool     `json:"truncated"`
	NextPage      *int     `json:"next_page,omitempty"`
	NextStartLine *int     `json:"next_start_line,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

type directoryEntry struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	SHA          string `json:"sha"`
	Target       string `json:"target,omitempty"`
	SubmoduleURL string `json:"submodule_url,omitempty"`
}

type directoryPage struct {
	Entries []directoryEntry `json:"entries"`
	Path    string           `json:"path"`
	Ref     string           `json:"ref"`
	Page    int              `json:"page"`
	PerPage int              `json:"per_page"`
	Total   int              `json:"total"`
}

type fileContent struct {
	Path         string `json:"path"`
	Ref          string `json:"ref"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	SHA          string `json:"sha"`
	Content      string `json:"content,omitempty"`
	StartLine    int    `json:"start_line,omitempty"`
	EndLine      int    `json:"end_line,omitempty"`
	TotalLines   int    `json:"total_lines,omitempty"`
	Target       string `json:"target,omitempty"`
	SubmoduleURL string `json:"submodule_url,omitempty"`
}

type contentResponse[T any] struct {
	Data  *T          `json:"data,omitempty"`
	Error *toolError  `json:"error,omitempty"`
	Meta  contentMeta `json:"meta"`
}

func TestContents(t *testing.T) {
	environment, projectRoot, identifier := prepareSmokeEnvironment(t)
	commandContext, cancelCommands := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelCommands()

	_, err := runCommand(commandContext, "create the content E2E network", "docker", "network", "create", environment.network)
	require.NoError(t, err)
	dataDir := filepath.Join(environment.tempDir, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "log"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "app.ini"), []byte(gogsConfig(identifier)), 0o600))

	t.Log("Creating versioned text, Unicode, binary, oversized, symlink, and submodule fixtures.")
	bootstrapOutput, err := runSensitiveCommand(
		commandContext,
		"bootstrap the Gogs content E2E database",
		"docker",
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
		"start the Gogs content E2E container",
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

	t.Log("Browsing and reading each supported ref through a real stdio MCP process.")
	mcpLogs := useMCPClient(t, projectRoot, baseURL, bootstrap.Collaborator.Token, func(session *mcp.ClientSession) {
		mainDirectory := callDirectory(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "path": "src",
		})
		assert.Equal(t, bootstrap.Refs.DefaultBranch, mainDirectory.Data.Ref)
		assertDirectoryFixtures(t, mainDirectory.Data.Entries)

		refExpectations := []struct {
			ref     string
			content string
		}{
			{ref: bootstrap.Refs.DefaultBranch, content: "main-version\n第二行\nthird"},
			{ref: bootstrap.Refs.FeatureBranch, content: "feature-version\n第二行\nthird"},
			{ref: bootstrap.Refs.Tag, content: "tag-version\n第二行\nthird"},
			{ref: bootstrap.Refs.FeatureSHA, content: "feature-version\n第二行\nthird"},
		}
		for _, expectation := range refExpectations {
			arguments := map[string]any{
				"owner": "owner", "repo": bootstrap.SharedRepository, "path": "src/version.txt",
			}
			if expectation.ref != bootstrap.Refs.DefaultBranch {
				arguments["ref"] = expectation.ref
			}
			file := callContentFile(t, session, arguments)
			assert.Equal(t, expectation.ref, file.Data.Ref)
			assert.Equal(t, expectation.content, file.Data.Content)
			assert.Equal(t, "text", file.Data.Type)
			assert.Equal(t, "src/version.txt", file.Data.Path)
			assert.Positive(t, file.Data.Size)
			assert.NotEmpty(t, file.Data.SHA)
			assert.Equal(t, 1, file.Data.StartLine)
			assert.Equal(t, 3, file.Data.EndLine)

			directory := callDirectory(t, session, map[string]any{
				"owner": "owner", "repo": bootstrap.SharedRepository, "path": "src", "ref": expectation.ref,
			})
			assert.Equal(t, expectation.ref, directory.Data.Ref)
			assert.NotEmpty(t, findDirectoryEntry(t, directory.Data.Entries, "src/version.txt").SHA)
		}

		unicode := callContentFile(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "path": "src/unicode.txt",
		})
		assert.Equal(t, "你好，Gogs\nemoji 😀", unicode.Data.Content)
		assert.Equal(t, 2, unicode.Data.TotalLines)

		firstLines := callContentFile(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "path": "src/lines.txt",
		})
		assert.Equal(t, 1, firstLines.Data.StartLine)
		assert.Equal(t, 200, firstLines.Data.EndLine)
		assert.True(t, firstLines.Meta.Truncated)
		require.NotNil(t, firstLines.Meta.NextStartLine)
		assert.Equal(t, 201, *firstLines.Meta.NextStartLine)
		remainingLines := callContentFile(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "path": "src/lines.txt", "start_line": 201,
		})
		assert.Equal(t, 201, remainingLines.Data.StartLine)
		assert.Equal(t, 250, remainingLines.Data.EndLine)
		assert.False(t, remainingLines.Meta.Truncated)

		binary := callContentFile(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "path": "src/binary.dat",
		})
		assert.Equal(t, "binary", binary.Data.Type)
		assert.Equal(t, int64(6), binary.Data.Size)
		assert.Empty(t, binary.Data.Content)

		large := callContentFile(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "path": "src/large.txt",
		})
		assert.Equal(t, "too_large", large.Data.Type)
		assert.Greater(t, large.Data.Size, int64(1<<20))
		assert.Empty(t, large.Data.Content)
		assert.True(t, large.Meta.Truncated)
		assert.Contains(t, strings.Join(large.Meta.Warnings, " "), "1 MiB")

		symlink := callContentFile(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "path": "src/version-link",
		})
		assert.Equal(t, "symlink", symlink.Data.Type)
		assert.Equal(t, "version.txt", symlink.Data.Target)

		vendor := callDirectory(t, session, map[string]any{
			"owner": "owner", "repo": bootstrap.SharedRepository, "path": "vendor",
		})
		module := findDirectoryEntry(t, vendor.Data.Entries, "vendor/module")
		assert.Equal(t, "submodule", module.Type)
		assert.Equal(t, "https://example.test/module.git", module.SubmoduleURL)

		for _, invalidPath := range []string{"/absolute", "../escape", "src/../escape", "src\\..\\escape", "src/\x00escape"} {
			result := callE2ETool(t, session, "get_file", map[string]any{
				"owner": "owner", "repo": bootstrap.SharedRepository, "path": invalidPath,
			})
			assert.True(t, result.IsError)
			var invalid contentResponse[fileContent]
			decodeStructuredContent(t, result.StructuredContent, &invalid)
			require.NotNil(t, invalid.Error)
			assert.Equal(t, "VALIDATION_FAILED", invalid.Error.Code)
		}
	})

	assertNoSecrets(t, mcpLogs, secrets...)
	gogsLogs, err := runCommand(commandContext, "read Gogs content E2E logs", "docker", "logs", environment.container)
	require.NoError(t, err)
	assertNoSecrets(t, gogsLogs, secrets...)
	assert.Contains(t, string(gogsLogs), "/api/v1/repos/owner/private-shared/contents/src")
	assert.NotContains(t, string(gogsLogs), "escape")

	t.Log("Removing all content E2E resources.")
	environment.cleanup(t)
	assertResourcesRemoved(t, environment)
}

func callDirectory(t *testing.T, session *mcp.ClientSession, arguments map[string]any) contentResponse[directoryPage] {
	t.Helper()
	result := callE2ETool(t, session, "list_directory", arguments)
	require.False(t, result.IsError)
	var response contentResponse[directoryPage]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func callContentFile(t *testing.T, session *mcp.ClientSession, arguments map[string]any) contentResponse[fileContent] {
	t.Helper()
	result := callE2ETool(t, session, "get_file", arguments)
	require.False(t, result.IsError)
	var response contentResponse[fileContent]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func findDirectoryEntry(t *testing.T, entries []directoryEntry, repositoryPath string) directoryEntry {
	t.Helper()
	for _, entry := range entries {
		if entry.Path == repositoryPath {
			return entry
		}
	}
	require.FailNow(t, "Directory entry was not returned.", repositoryPath)
	return directoryEntry{}
}

func assertDirectoryFixtures(t *testing.T, entries []directoryEntry) {
	t.Helper()
	expectedTypes := map[string]string{
		"src/binary.dat":   "file",
		"src/large.txt":    "file",
		"src/lines.txt":    "file",
		"src/unicode.txt":  "file",
		"src/version-link": "symlink",
		"src/version.txt":  "file",
	}
	for repositoryPath, expectedType := range expectedTypes {
		entry := findDirectoryEntry(t, entries, repositoryPath)
		assert.Equal(t, expectedType, entry.Type)
		assert.NotEmpty(t, entry.Name)
		assert.NotEmpty(t, entry.SHA)
		assert.Positive(t, entry.Size)
	}
}

func assertRepositoryRefs(t *testing.T, refs repositoryRefs) {
	t.Helper()
	assert.Equal(t, "main", refs.DefaultBranch)
	assert.Equal(t, "feature/content", refs.FeatureBranch)
	assert.Equal(t, "v1.0.0", refs.Tag)
	assert.Len(t, refs.MainCommitSHA, 40)
	assert.Len(t, refs.FeatureSHA, 40)
	assert.Len(t, refs.TagSHA, 40)
}

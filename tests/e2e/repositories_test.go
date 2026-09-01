//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type repositoryIdentity struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

type repositoryBootstrapResult struct {
	Owner            repositoryIdentity `json:"owner"`
	Collaborator     repositoryIdentity `json:"collaborator"`
	Outsider         repositoryIdentity `json:"outsider"`
	SharedRepository string             `json:"shared_repository"`
	Refs             repositoryRefs     `json:"refs"`
}

type repositoryRefs struct {
	DefaultBranch string `json:"default_branch"`
	FeatureBranch string `json:"feature_branch"`
	Tag           string `json:"tag"`
	MainCommitSHA string `json:"main_commit_sha"`
	FeatureSHA    string `json:"feature_sha"`
	TagSHA        string `json:"tag_sha"`
}

type repositoryPermissions struct {
	Pull  bool `json:"pull"`
	Push  bool `json:"push"`
	Admin bool `json:"admin"`
}

type repository struct {
	ID            int64                 `json:"id"`
	Name          string                `json:"name"`
	FullName      string                `json:"full_name"`
	Owner         string                `json:"owner"`
	Description   string                `json:"description"`
	DefaultBranch string                `json:"default_branch"`
	Private       bool                  `json:"private"`
	CloneURL      string                `json:"clone_url"`
	WebURL        string                `json:"web_url"`
	Permissions   repositoryPermissions `json:"permissions"`
}

type repositoryPage struct {
	Repositories []repository `json:"repositories"`
	Page         int          `json:"page"`
	PerPage      int          `json:"per_page"`
	Total        int          `json:"total"`
}

type repositoryPageResponse struct {
	Data  *repositoryPage `json:"data,omitempty"`
	Error *toolError      `json:"error,omitempty"`
}

type repositoryResponse struct {
	Data  *repository `json:"data,omitempty"`
	Error *toolError  `json:"error,omitempty"`
}

func TestRepositories(t *testing.T) {
	environment, projectRoot, identifier := prepareSmokeEnvironment(t)
	commandContext, cancelCommands := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelCommands()

	_, err := runCommand(commandContext, "create the repository E2E network", "docker", "network", "create", environment.network)
	require.NoError(t, err)
	dataDir := filepath.Join(environment.tempDir, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "log"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "app.ini"), []byte(gogsConfig(identifier)), 0o600))

	t.Log("Creating owner, collaborator, outsider, private repositories, and personal access tokens.")
	bootstrapOutput, err := runSensitiveCommand(
		commandContext,
		"bootstrap the Gogs repository E2E database",
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
	secrets := []string{bootstrap.Owner.Token, bootstrap.Collaborator.Token, bootstrap.Outsider.Token}
	for _, secret := range secrets {
		require.NotEmpty(t, secret)
	}

	t.Log("Starting Gogs for the repository visibility scenario.")
	_, err = runCommand(
		commandContext,
		"start the Gogs repository E2E container",
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

	t.Log("Verifying the owner repository view.")
	ownerLogs := useMCPClient(t, projectRoot, baseURL, bootstrap.Owner.Token, func(session *mcp.ClientSession) {
		page := callRepositoryPage(t, session, "list_repositories", map[string]any{})
		assert.Equal(t, []string{"owner/private-shared"}, repositoryNames(page.Repositories))
		detail := callRepository(t, session, "owner", bootstrap.SharedRepository)
		require.NotNil(t, detail.Data)
		assertRepositoryContract(t, *detail.Data)
		assert.True(t, detail.Data.Permissions.Pull)
		assert.True(t, detail.Data.Permissions.Push)
		assert.True(t, detail.Data.Permissions.Admin)
	})

	t.Log("Verifying owned and private collaborator repository discovery.")
	collaboratorLogs := useMCPClient(t, projectRoot, baseURL, bootstrap.Collaborator.Token, func(session *mcp.ClientSession) {
		first := callRepositoryPage(t, session, "list_repositories", map[string]any{})
		second := callRepositoryPage(t, session, "list_repositories", map[string]any{})
		expected := []string{"collaborator/collaborator-owned", "owner/private-shared"}
		assert.Equal(t, expected, repositoryNames(first.Repositories))
		assert.Equal(t, expected, repositoryNames(second.Repositories))

		search := callRepositoryPage(t, session, "search_repositories", map[string]any{"query": "NEEDLE"})
		require.Len(t, search.Repositories, 1)
		assert.Equal(t, "owner/private-shared", search.Repositories[0].FullName)
		assert.True(t, search.Repositories[0].Private)

		detail := callRepository(t, session, "owner", bootstrap.SharedRepository)
		require.NotNil(t, detail.Data)
		assertRepositoryContract(t, *detail.Data)
		assert.True(t, detail.Data.Permissions.Pull)
		assert.False(t, detail.Data.Permissions.Push)
		assert.False(t, detail.Data.Permissions.Admin)
	})

	t.Log("Verifying that the outsider cannot enumerate or inspect the private shared repository.")
	outsiderLogs := useMCPClient(t, projectRoot, baseURL, bootstrap.Outsider.Token, func(session *mcp.ClientSession) {
		page := callRepositoryPage(t, session, "list_repositories", map[string]any{})
		assert.Equal(t, []string{"outsider/outsider-private"}, repositoryNames(page.Repositories))

		responses := make([]repositoryResponse, 0, 2)
		for _, name := range []string{bootstrap.SharedRepository, "does-not-exist"} {
			result := callE2ETool(t, session, "get_repository", map[string]any{
				"owner": "owner",
				"repo":  name,
			})
			assert.True(t, result.IsError)
			var response repositoryResponse
			decodeStructuredContent(t, result.StructuredContent, &response)
			require.NotNil(t, response.Error)
			assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", response.Error.Code)
			responses = append(responses, response)
		}
		assert.Equal(t, responses[0].Error.Code, responses[1].Error.Code)
	})

	allMCPLogs := bytes.Join([][]byte{ownerLogs, collaboratorLogs, outsiderLogs}, nil)
	assertNoSecrets(t, allMCPLogs, secrets...)
	gogsLogs, err := runCommand(commandContext, "read Gogs repository E2E logs", "docker", "logs", environment.container)
	require.NoError(t, err)
	assertNoSecrets(t, gogsLogs, secrets...)
	assert.Contains(t, string(gogsLogs), "/api/v1/user/repos")
	assert.NotContains(t, string(gogsLogs), "/api/v1/repos/search")

	t.Log("Removing all repository E2E resources.")
	environment.cleanup(t)
	assertResourcesRemoved(t, environment)
}

func parseRepositoryBootstrap(t *testing.T, output []byte) repositoryBootstrapResult {
	t.Helper()
	lines := bytes.Split(output, []byte("\n"))
	for index := len(lines) - 1; index >= 0; index-- {
		line := bytes.TrimSpace(lines[index])
		if len(line) == 0 {
			continue
		}
		var result repositoryBootstrapResult
		if err := json.Unmarshal(line, &result); err == nil && result.Owner.Token != "" {
			return result
		}
	}
	require.FailNow(t, "The repository bootstrap command did not return all identities.")
	return repositoryBootstrapResult{}
}

func callRepositoryPage(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) repositoryPage {
	t.Helper()
	result := callE2ETool(t, session, name, arguments)
	require.False(t, result.IsError)
	var response repositoryPageResponse
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return *response.Data
}

func callRepository(t *testing.T, session *mcp.ClientSession, owner, name string) repositoryResponse {
	t.Helper()
	result := callE2ETool(t, session, "get_repository", map[string]any{"owner": owner, "repo": name})
	require.False(t, result.IsError)
	var response repositoryResponse
	decodeStructuredContent(t, result.StructuredContent, &response)
	return response
}

func callE2ETool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	require.NoError(t, err)
	return result
}

func assertRepositoryContract(t *testing.T, actual repository) {
	t.Helper()
	assert.Positive(t, actual.ID)
	assert.Equal(t, "private-shared", actual.Name)
	assert.Equal(t, "owner/private-shared", actual.FullName)
	assert.Equal(t, "owner", actual.Owner)
	assert.Equal(t, "Needle private collaborator repository", actual.Description)
	assert.Equal(t, "main", actual.DefaultBranch)
	assert.True(t, actual.Private)
	assert.True(t, strings.HasSuffix(actual.CloneURL, "/owner/private-shared.git"))
	assert.True(t, strings.HasSuffix(actual.WebURL, "/owner/private-shared"))
}

func repositoryNames(repositories []repository) []string {
	names := make([]string, len(repositories))
	for index, repository := range repositories {
		names[index] = repository.FullName
	}
	return names
}

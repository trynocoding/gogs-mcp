package mcpserver

import (
	"context"
	"testing"

	"gogs-mcp/internal/gogs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListRepositoriesAppliesDefaultsAndStableFullNameOrder(t *testing.T) {
	client := &fakeClient{repositories: []gogs.Repository{
		{ID: 3, Name: "third", FullName: "zoe/third"},
		{ID: 1, Name: "first", FullName: "alice/first"},
		{ID: 2, Name: "second", FullName: "bob/second"},
	}}
	session := connectTestClient(t, client)

	first := callRepositoryPage(t, session, "list_repositories", map[string]any{})
	second := callRepositoryPage(t, session, "list_repositories", map[string]any{})

	assert.Equal(t, defaultPage, first.Data.Page)
	assert.Equal(t, defaultPerPage, first.Data.PerPage)
	assert.Equal(t, 3, first.Data.Total)
	assert.Equal(t, []string{"alice/first", "bob/second", "zoe/third"}, repositoryFullNames(first.Data.Repositories))
	assert.Equal(t, repositoryFullNames(first.Data.Repositories), repositoryFullNames(second.Data.Repositories))
	assert.Nil(t, first.Meta.NextPage)
	assert.Equal(t, 2, client.listCalls)
}

func TestListRepositoriesPaginatesAfterSorting(t *testing.T) {
	client := &fakeClient{repositories: []gogs.Repository{
		{ID: 3, FullName: "c/repo"},
		{ID: 1, FullName: "a/repo"},
		{ID: 2, FullName: "b/repo"},
	}}
	session := connectTestClient(t, client)

	first := callRepositoryPage(t, session, "list_repositories", map[string]any{"page": 1, "per_page": 2})
	require.NotNil(t, first.Meta.NextPage)
	assert.Equal(t, 2, *first.Meta.NextPage)
	assert.Equal(t, []string{"a/repo", "b/repo"}, repositoryFullNames(first.Data.Repositories))

	second := callRepositoryPage(t, session, "list_repositories", map[string]any{"page": 2, "per_page": 2})
	assert.Nil(t, second.Meta.NextPage)
	assert.Equal(t, []string{"c/repo"}, repositoryFullNames(second.Data.Repositories))

	beyond := callRepositoryPage(t, session, "list_repositories", map[string]any{"page": 1000000000, "per_page": 2})
	assert.Empty(t, beyond.Data.Repositories)
}

func TestRepositoryPaginationIsRejectedBeforeCallingGogs(t *testing.T) {
	testCases := map[string]map[string]any{
		"zero page":         {"page": 0},
		"negative page":     {"page": -1},
		"zero page size":    {"per_page": 0},
		"oversized page":    {"per_page": 101},
		"unknown parameter": {"unexpected": true},
	}
	for name, arguments := range testCases {
		t.Run(name, func(t *testing.T) {
			client := &fakeClient{}
			session := connectTestClient(t, client)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "list_repositories",
				Arguments: arguments,
			})
			require.NoError(t, err)
			assert.True(t, result.IsError)
			assert.Zero(t, client.listCalls)
		})
	}
}

func TestSearchRepositoriesFiltersAllAccessibleRepositoriesLocally(t *testing.T) {
	client := &fakeClient{repositories: []gogs.Repository{
		{Name: "NeedleName", FullName: "owner/NeedleName", Description: "first"},
		{Name: "project", FullName: "NeedleOwner/project", Description: "second"},
		{Name: "private-shared", FullName: "owner/private-shared", Description: "Contains Needle Description", Private: true},
		{Name: "unmatched", FullName: "owner/unmatched", Description: "other"},
	}}
	session := connectTestClient(t, client)

	response := callRepositoryPage(t, session, "search_repositories", map[string]any{"query": "nEeDlE"})

	require.NotNil(t, response.Data)
	assert.Equal(t, 3, response.Data.Total)
	assert.Equal(t, []string{"NeedleOwner/project", "owner/NeedleName", "owner/private-shared"}, repositoryFullNames(response.Data.Repositories))
	assert.True(t, response.Data.Repositories[2].Private)
	assert.Equal(t, 1, client.listCalls)
}

func TestSearchRepositoriesRequiresNonemptyQueryBeforeCallingGogs(t *testing.T) {
	for name, arguments := range map[string]map[string]any{
		"missing": {},
		"empty":   {"query": ""},
	} {
		t.Run(name, func(t *testing.T) {
			client := &fakeClient{}
			session := connectTestClient(t, client)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "search_repositories",
				Arguments: arguments,
			})
			require.NoError(t, err)
			assert.True(t, result.IsError)
			assert.Zero(t, client.listCalls)
		})
	}
}

func TestGetRepositoryReturnsAllContractFields(t *testing.T) {
	client := &fakeClient{repository: gogs.Repository{
		ID:            91,
		Name:          "private-repo",
		FullName:      "owner/private-repo",
		Owner:         "owner",
		Description:   "Private repository",
		DefaultBranch: "main",
		Private:       true,
		CloneURL:      "https://gogs.example/owner/private-repo.git",
		WebURL:        "https://gogs.example/owner/private-repo",
		Permissions: gogs.RepositoryPermissions{
			Pull: true,
			Push: true,
		},
	}}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_repository",
		Arguments: map[string]any{"owner": "owner", "repo": "private-repo"},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	var response ToolResponse[Repository]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	assert.Equal(t, int64(91), response.Data.ID)
	assert.Equal(t, "private-repo", response.Data.Name)
	assert.Equal(t, "owner/private-repo", response.Data.FullName)
	assert.Equal(t, "owner", response.Data.Owner)
	assert.Equal(t, "Private repository", response.Data.Description)
	assert.Equal(t, "main", response.Data.DefaultBranch)
	assert.True(t, response.Data.Private)
	assert.Equal(t, "https://gogs.example/owner/private-repo.git", response.Data.CloneURL)
	assert.Equal(t, "https://gogs.example/owner/private-repo", response.Data.WebURL)
	assert.True(t, response.Data.Permissions.Pull)
	assert.True(t, response.Data.Permissions.Push)
	assert.False(t, response.Data.Permissions.Admin)
	assert.Equal(t, "owner", client.requestedOwner)
	assert.Equal(t, "private-repo", client.requestedRepo)
}

func TestGetRepositoryReturnsHiddenNotFoundError(t *testing.T) {
	client := &fakeClient{getErr: &gogs.Error{
		Code:    gogs.CodeResourceNotFoundOrForbidden,
		Message: "The repository does not exist or the current user cannot access it.",
	}}
	session := connectTestClient(t, client)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_repository",
		Arguments: map[string]any{"owner": "owner", "repo": "missing"},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	var response ToolResponse[Repository]
	decodeStructuredContent(t, result.StructuredContent, &response)
	assert.Nil(t, response.Data)
	require.NotNil(t, response.Error)
	assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", response.Error.Code)
	assert.Equal(t, "The repository does not exist or the current user cannot access it.", response.Error.Message)
}

func callRepositoryPage(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) ToolResponse[RepositoryPage] {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: arguments,
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	var response ToolResponse[RepositoryPage]
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return response
}

func repositoryFullNames(repositories []Repository) []string {
	fullNames := make([]string, len(repositories))
	for index, repository := range repositories {
		fullNames[index] = repository.FullName
	}
	return fullNames
}

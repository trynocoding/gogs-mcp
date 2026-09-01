package gogs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListRepositoriesUsesCompleteAuthenticatedRepositoryEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodGet, request.Method)
		assert.Equal(t, "/api/v1/user/repos", request.URL.Path)
		assert.Empty(t, request.URL.RawQuery)
		writeResponse(t, writer, `[{"id":9,"name":"private-repo","full_name":"owner/private-repo","owner":{"login":"owner"},"description":"Private collaborator repository","default_branch":"main","private":true,"clone_url":"https://gogs.example/owner/private-repo.git","html_url":"https://gogs.example/owner/private-repo","permissions":{"pull":true,"push":false,"admin":false}}]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	repositories, err := client.ListRepositories(context.Background())
	require.NoError(t, err)
	require.Len(t, repositories, 1)
	assert.Equal(t, Repository{
		ID:            9,
		Name:          "private-repo",
		FullName:      "owner/private-repo",
		Owner:         "owner",
		Description:   "Private collaborator repository",
		DefaultBranch: "main",
		Private:       true,
		CloneURL:      "https://gogs.example/owner/private-repo.git",
		WebURL:        "https://gogs.example/owner/private-repo",
		Permissions: RepositoryPermissions{
			Pull: true,
		},
	}, repositories[0])
}

func TestGetRepositoryReturnsMetadataAndPermissions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/api/v1/repos/alice/project", request.URL.Path)
		writeResponse(t, writer, `{"id":12,"name":"project","full_name":"alice/project","owner":{"username":"alice"},"description":"Example","default_branch":"trunk","private":false,"clone_url":"https://gogs.example/alice/project.git","html_url":"https://gogs.example/alice/project","permissions":{"pull":true,"push":true,"admin":true}}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	repository, err := client.GetRepository(context.Background(), "alice", "project")
	require.NoError(t, err)
	assert.Equal(t, "alice/project", repository.FullName)
	assert.Equal(t, "alice", repository.Owner)
	assert.Equal(t, "trunk", repository.DefaultBranch)
	assert.True(t, repository.Permissions.Pull)
	assert.True(t, repository.Permissions.Push)
	assert.True(t, repository.Permissions.Admin)
}

func TestGetRepositoryEscapesPathSegments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/api/v1/repos/space%20owner/repo%2Fname", request.URL.EscapedPath())
		writeResponse(t, writer, `{}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.GetRepository(context.Background(), "space owner", "repo/name")
	require.NoError(t, err)
}

func TestGetRepositoryHidesMissingAndForbiddenDifference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.GetRepository(context.Background(), "owner", "private")
	require.Error(t, err)
	classified := AsError(err)
	assert.Equal(t, CodeResourceNotFoundOrForbidden, classified.Code)
	assert.Equal(t, "The repository does not exist or the current user cannot access it.", classified.Message)
	assert.Equal(t, http.StatusNotFound, classified.HTTPStatus)
	assert.NotContains(t, fmt.Sprintf("%+v", err), "secret-token")
}

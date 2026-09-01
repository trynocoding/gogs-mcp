package gogs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListBranchesReturnsHeadSHAsWithoutQuery(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		assert.Equal(t, "/api/v1/repos/owner/project/branches", request.URL.Path)
		assert.Empty(t, request.URL.RawQuery)
		writeResponse(t, writer, `[
			{"name":"main","commit":{"id":"main-sha"}},
			{"name":"feature/content","commit":{"id":"feature-sha"}}
		]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	branches, err := client.ListBranches(context.Background(), "owner", "project")
	require.NoError(t, err)
	require.Len(t, branches, 2)
	assert.Equal(t, "main", branches[0].Name)
	assert.Equal(t, "main-sha", branches[0].HeadSHA)
	assert.Equal(t, "feature/content", branches[1].Name)
	assert.Equal(t, "feature-sha", branches[1].HeadSHA)
}

func TestGetBranchBuildsSegmentedURLForSlashNames(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		assert.Equal(t, "/api/v1/repos/owner/project/branches/feature/content", request.URL.Path)
		assert.Empty(t, request.URL.RawQuery)
		writeResponse(t, writer, `{"name":"feature/content","commit":{"id":"feature-sha"}}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	branch, err := client.GetBranch(context.Background(), "owner", "project", "feature/content")
	require.NoError(t, err)
	assert.Equal(t, "feature/content", branch.Name)
	assert.Equal(t, "feature-sha", branch.HeadSHA)
	assert.Equal(t, int32(1), requests.Load())
}

func TestListCommitsSendsOnlyPageSize(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		assert.Equal(t, "/api/v1/repos/owner/project/commits", request.URL.Path)
		assert.Equal(t, "pageSize=30", request.URL.RawQuery)
		writeResponse(t, writer, `[
			{
				"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"url":"http://gogs.test/api/v1/repos/owner/project/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"html_url":"http://gogs.test/owner/project/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"commit":{
					"message":"Change main version",
					"author":{"name":"Alice","email":"alice@example.test","date":"2026-09-01T00:00:00Z"},
					"committer":{"name":"Bob","email":"bob@example.test","date":"2026-09-01T00:00:01Z"}
				},
				"parents":[{"url":"http://gogs.test/api/v1/x","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]
			},
			{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","commit":{"message":"Add fixtures","author":{"name":"Alice","email":"alice@example.test","date":"2026-08-31T00:00:00Z"}},"parents":[]}
		]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	commits, err := client.ListCommits(context.Background(), "owner", "project", 30)
	require.NoError(t, err)
	require.Len(t, commits, 2)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", commits[0].SHA)
	assert.Equal(t, "Change main version", commits[0].Message)
	assert.Equal(t, "http://gogs.test/owner/project/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", commits[0].WebURL)
	assert.Equal(t, "Alice", commits[0].Author.Name)
	assert.Equal(t, "alice@example.test", commits[0].Author.Email)
	assert.Equal(t, "2026-09-01T00:00:00Z", commits[0].Author.Date)
	assert.Equal(t, "Bob", commits[0].Committer.Name)
	assert.Equal(t, []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, commits[0].ParentSHAs)
	assert.Empty(t, commits[1].ParentSHAs)
}

func TestGetCommitFetchesSHASegment(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		assert.Equal(t, "/api/v1/repos/owner/project/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", request.URL.Path)
		assert.Empty(t, request.URL.RawQuery)
		writeResponse(t, writer, `{
			"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"html_url":"http://gogs.test/owner/project/commits/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"commit":{"message":"Initial commit","author":{"name":"Alice","email":"alice@example.test","date":"2026-08-30T00:00:00Z"}},
			"parents":[]
		}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	commit, err := client.GetCommit(context.Background(), "owner", "project", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	assert.Equal(t, "Initial commit", commit.Message)
	assert.Equal(t, "Alice", commit.Author.Name)
	assert.Empty(t, commit.ParentSHAs)
}

func TestGitReferencesAreRejectedBeforeHTTPRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)

	invalidBranches := []string{
		"",
		"/absolute",
		"feature/../escape",
		"feature//content",
		"feature/",
		`feature\content`,
		"feature/\x00name",
		".",
		"..",
	}
	for _, branch := range invalidBranches {
		t.Run("branch "+branch, func(t *testing.T) {
			_, err := client.GetBranch(context.Background(), "owner", "project", branch)
			require.Error(t, err)
			assert.Equal(t, CodeValidationFailed, AsError(err).Code)
		})
	}

	invalidSHAs := []string{
		"",
		".",
		"..",
		"../escape",
		`feature\content`,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00",
	}
	for _, sha := range invalidSHAs {
		t.Run("sha "+sha, func(t *testing.T) {
			_, err := client.GetCommit(context.Background(), "owner", "project", sha)
			require.Error(t, err)
			assert.Equal(t, CodeValidationFailed, AsError(err).Code)
		})
	}
	assert.Zero(t, requests.Load())
}

func TestGetCommitDefersUnknownRevisionsToGogs(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		assert.Equal(t, "/api/v1/repos/owner/project/commits/not-a-sha", request.URL.Path)
		http.Error(writer, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.GetCommit(context.Background(), "owner", "project", "not-a-sha")
	require.Error(t, err)
	assert.Equal(t, CodeResourceNotFoundOrForbidden, AsError(err).Code)
	assert.Equal(t, int32(1), requests.Load())
}

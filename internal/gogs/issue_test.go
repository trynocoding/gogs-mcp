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

func TestListIssuesSendsStateAndPageAndFollowsNextLink(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/api/v1/repos/alice/project/issues", request.URL.Path)
		assert.Equal(t, "closed", request.URL.Query().Get("state"))
		assert.Equal(t, "2", request.URL.Query().Get("page"))
		assert.Empty(t, request.URL.Query().Get("limit"), "Gogs v0.14.2 fixes the page size server-side")
		base := "http://" + request.Host
		writer.Header().Set("Link", fmt.Sprintf(`<%s/api/v1/repos/alice/project/issues?page=3>; rel="next", <%s/api/v1/repos/alice/project/issues?page=5>; rel="last"`, base, base))
		writeResponse(t, writer, `[{"number":7,"title":"Fix the parser","state":"closed","user":{"id":3,"username":"alice","full_name":"Alice Example"},"labels":[{"name":"bug","color":"#ff0000"}],"comments":2,"created_at":"2026-01-02T15:04:05Z","updated_at":"2026-01-03T10:00:00Z"}]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	issues, nextPage, err := client.ListIssues(context.Background(), "alice", "project", IssueStateClosed, 2)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, 3, nextPage)
	assert.Equal(t, IssueSummary{
		Number:      7,
		Title:       "Fix the parser",
		State:       "closed",
		User:        User{ID: 3, Username: "alice", FullName: "Alice Example"},
		Labels:      []IssueLabel{{Name: "bug", Color: "#ff0000"}},
		NumComments: 2,
		CreatedAt:   "2026-01-02T15:04:05Z",
		UpdatedAt:   "2026-01-03T10:00:00Z",
	}, issues[0])
}

func TestListIssuesWithoutNextLinkReportsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "open", request.URL.Query().Get("state"))
		assert.Equal(t, "1", request.URL.Query().Get("page"))
		writeResponse(t, writer, `[]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	issues, nextPage, err := client.ListIssues(context.Background(), "alice", "project", IssueStateOpen, 1)
	require.NoError(t, err)
	require.Empty(t, issues)
	assert.Equal(t, 0, nextPage)
}

func TestListIssuesIgnoresLastPageLinkWithoutNext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		base := "http://" + request.Host
		writer.Header().Set("Link", fmt.Sprintf(`<%s/api/v1/repos/alice/project/issues?page=1>; rel="first", <%s/api/v1/repos/alice/project/issues?page=3>; rel="last"`, base, base))
		writeResponse(t, writer, `[]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, nextPage, err := client.ListIssues(context.Background(), "alice", "project", IssueStateOpen, 3)
	require.NoError(t, err)
	assert.Equal(t, 0, nextPage)
}

func TestGetIssueMapsAllFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/api/v1/repos/alice/project/issues/7", request.URL.Path)
		writeResponse(t, writer, `{
			"id": 41,
			"number": 7,
			"user": {"id": 3, "username": "alice", "full_name": "Alice Example"},
			"title": "Fix the parser",
			"body": "The parser fails on empty input.",
			"labels": [{"name": "bug", "color": "#ff0000"}, {"name": "urgent", "color": "#00ff00"}],
			"milestone": {"title": "v1.0", "state": "open"},
			"assignee": {"id": 4, "username": "bob", "full_name": "Bob Example"},
			"state": "open",
			"comments": 2,
			"created_at": "2026-01-02T15:04:05Z",
			"updated_at": "2026-01-03T10:00:00Z"
		}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	issue, err := client.GetIssue(context.Background(), "alice", "project", 7)
	require.NoError(t, err)
	assignee := User{ID: 4, Username: "bob", FullName: "Bob Example"}
	assert.Equal(t, Issue{
		Number: 7,
		Title:  "Fix the parser",
		Body:   "The parser fails on empty input.",
		State:  "open",
		User:   User{ID: 3, Username: "alice", FullName: "Alice Example"},
		Labels: []IssueLabel{
			{Name: "bug", Color: "#ff0000"},
			{Name: "urgent", Color: "#00ff00"},
		},
		Milestone:   &IssueMilestone{Title: "v1.0", State: "open"},
		Assignee:    &assignee,
		NumComments: 2,
		CreatedAt:   "2026-01-02T15:04:05Z",
		UpdatedAt:   "2026-01-03T10:00:00Z",
	}, issue)
}

func TestGetIssueOmitsAbsentOptionalFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeResponse(t, writer, `{"number": 8, "title": "Empty", "state": "open", "user": {"login": "carol"}}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	issue, err := client.GetIssue(context.Background(), "alice", "project", 8)
	require.NoError(t, err)
	assert.Equal(t, "carol", issue.User.Username, "the login field fills in a missing username")
	assert.Nil(t, issue.Assignee)
	assert.Nil(t, issue.Milestone)
	assert.Empty(t, issue.Labels)
}

func TestGetIssueHidesMissingAndForbiddenDifference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.GetIssue(context.Background(), "alice", "private", 99)
	require.Error(t, err)
	classified := AsError(err)
	assert.Equal(t, CodeResourceNotFoundOrForbidden, classified.Code)
	assert.Equal(t, "The issue does not exist or the current user cannot access it.", classified.Message)
	assert.Equal(t, http.StatusNotFound, classified.HTTPStatus)
	assert.NotContains(t, fmt.Sprintf("%+v", err), "secret-token")
}

func TestListIssueCommentsSendsSinceOnlyWhenSet(t *testing.T) {
	var requestedSince []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/api/v1/repos/alice/project/issues/7/comments", request.URL.Path)
		requestedSince = append(requestedSince, request.URL.Query().Get("since"))
		switch len(requestedSince) {
		case 1:
			writeResponse(t, writer, `[{"id": 900, "user": {"username": "bob"}, "body": "Reproduced on Linux.", "created_at": "2026-01-03T09:00:00Z", "updated_at": "2026-01-03T09:00:00Z"}]`)
		case 2:
			writeResponse(t, writer, `[]`)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	comments, err := client.ListIssueComments(context.Background(), "alice", "project", 7, "2026-01-03T00:00:00Z")
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, IssueComment{
		ID:        900,
		User:      User{Username: "bob"},
		Body:      "Reproduced on Linux.",
		CreatedAt: "2026-01-03T09:00:00Z",
		UpdatedAt: "2026-01-03T09:00:00Z",
	}, comments[0])

	_, err = client.ListIssueComments(context.Background(), "alice", "project", 7, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"2026-01-03T00:00:00Z", ""}, requestedSince)
}

func TestValidateIssueSince(t *testing.T) {
	assert.NoError(t, ValidateIssueSince(""))
	assert.NoError(t, ValidateIssueSince("2026-01-02T15:04:05Z"))
	assert.NoError(t, ValidateIssueSince("2026-01-02T15:04:05+08:00"))

	err := ValidateIssueSince("01/02/2026")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "since must be an RFC3339 timestamp")

	err = ValidateIssueSince("2026-01-02")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "since must be an RFC3339 timestamp")
}

package gogs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
		WebURL: server.URL + "/alice/project/issues/7",
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

func TestCreateIssueSendsPayloadOnceAndMapsResponse(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/api/v1/repos/alice/project/issues", request.URL.Path)
		assert.Equal(t, "application/json", request.Header.Get("Content-Type"))
		assert.Equal(t, "token secret-token", request.Header.Get("Authorization"))
		var payload map[string]any
		assert.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
		assert.Equal(t, "Fix the parser", payload["title"])
		assert.Equal(t, "The parser fails on empty input.", payload["body"])
		assert.Equal(t, "bob", payload["assignee"])
		assert.EqualValues(t, 3, payload["milestone"])
		assert.Equal(t, []any{float64(1), float64(2)}, payload["labels"])
		writeResponse(t, writer, `{
			"number": 7,
			"title": "Fix the parser",
			"body": "The parser fails on empty input.",
			"state": "open",
			"user": {"id": 3, "username": "alice"},
			"assignee": {"id": 4, "username": "bob"},
			"labels": [{"name": "bug", "color": "#ff0000"}],
			"milestone": {"id": 3, "title": "v1.0", "state": "open"},
			"comments": 0,
			"created_at": "2026-01-02T15:04:05Z",
			"updated_at": "2026-01-02T15:04:05Z"
		}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	issue, err := client.CreateIssue(context.Background(), "alice", "project", CreateIssueOptions{
		Title:       "Fix the parser",
		Body:        "The parser fails on empty input.",
		Assignee:    "bob",
		LabelIDs:    []int64{1, 2},
		MilestoneID: 3,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
	assert.Equal(t, Issue{
		Number:    7,
		Title:     "Fix the parser",
		Body:      "The parser fails on empty input.",
		State:     "open",
		User:      User{ID: 3, Username: "alice"},
		Assignee:  &User{ID: 4, Username: "bob"},
		Labels:    []IssueLabel{{Name: "bug", Color: "#ff0000"}},
		Milestone: &IssueMilestone{Title: "v1.0", State: "open"},
		CreatedAt: "2026-01-02T15:04:05Z",
		UpdatedAt: "2026-01-02T15:04:05Z",
		WebURL:    server.URL + "/alice/project/issues/7",
	}, issue)
}

func TestCreateIssueOmitsEmptyAdministrativeFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, err := io.ReadAll(request.Body)
		assert.NoError(t, err)
		var payload map[string]any
		assert.NoError(t, json.Unmarshal(raw, &payload))
		assert.Equal(t, map[string]any{"title": "Report a typo", "body": ""}, payload)
		writeResponse(t, writer, `{"number": 1, "title": "Report a typo", "state": "open"}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.CreateIssue(context.Background(), "alice", "project", CreateIssueOptions{Title: "Report a typo"})
	require.NoError(t, err)
}

func TestCreateIssueDoesNotRetryWhenOutcomeIsUnknown(t *testing.T) {
	testCases := map[string]struct {
		handle func(writer http.ResponseWriter, request *http.Request)
	}{
		"server error": {
			handle: func(writer http.ResponseWriter, _ *http.Request) {
				http.Error(writer, "boom", http.StatusInternalServerError)
			},
		},
		"connection reset": {
			handle: func(writer http.ResponseWriter, request *http.Request) {
				hijacker, ok := writer.(http.Hijacker)
				if !assert.True(t, ok) {
					return
				}
				connection, _, err := hijacker.Hijack()
				if !assert.NoError(t, err) {
					return
				}
				_ = request.Body.Close()
				_ = connection.Close()
			},
		},
		"invalid response JSON": {
			handle: func(writer http.ResponseWriter, _ *http.Request) {
				writeResponse(t, writer, `not-json`)
			},
		},
	}
	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				testCase.handle(writer, request)
			}))
			defer server.Close()

			client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
			_, err := client.CreateIssue(context.Background(), "alice", "project", CreateIssueOptions{Title: "Fix the parser"})
			require.Error(t, err)
			assert.Equal(t, int32(1), requests.Load(), "a write must never be retried")
			assert.Equal(t, CodeWriteOutcomeUnknown, AsError(err).Code)
		})
	}
}

func TestCreateIssueDoesNotRetryTLSHandshakeFailure(t *testing.T) {
	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writeResponse(t, writer, `{"number": 1}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.CreateIssue(context.Background(), "alice", "project", CreateIssueOptions{Title: "Fix the parser"})
	require.Error(t, err)
	assert.Equal(t, 0, requests, "the request never reached the server")
	assert.Equal(t, CodeTLSError, AsError(err).Code)
}

func TestCreateIssueClassifiesDefinitiveRejectionsWithoutRetrying(t *testing.T) {
	testCases := map[string]struct {
		status int
		code   ErrorCode
	}{
		"validation failure": {status: http.StatusUnprocessableEntity, code: CodeValidationFailed},
		"forbidden":          {status: http.StatusForbidden, code: CodePermissionDenied},
	}
	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			var requests int
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				requests++
				http.Error(writer, "rejected", testCase.status)
			}))
			defer server.Close()

			client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
			_, err := client.CreateIssue(context.Background(), "alice", "project", CreateIssueOptions{Title: "Fix the parser"})
			require.Error(t, err)
			assert.Equal(t, 1, requests)
			classified := AsError(err)
			assert.Equal(t, testCase.code, classified.Code)
			assert.Equal(t, testCase.status, classified.HTTPStatus)
		})
	}
}

func TestListRepositoryLabelsAndMilestonesMapIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/repos/alice/project/labels":
			writeResponse(t, writer, `[
				{"id": 1, "name": "bug", "color": "#ff0000"},
				{"id": 2, "name": "enhancement", "color": "#00ff00"}
			]`)
		case "/api/v1/repos/alice/project/milestones":
			writeResponse(t, writer, `[
				{"id": 3, "title": "v1.0", "state": "open"},
				{"id": 4, "title": "v0.9", "state": "closed"}
			]`)
		default:
			http.Error(writer, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	labels, err := client.ListRepositoryLabels(context.Background(), "alice", "project")
	require.NoError(t, err)
	assert.Equal(t, []RepositoryLabel{
		{ID: 1, Name: "bug", Color: "#ff0000"},
		{ID: 2, Name: "enhancement", Color: "#00ff00"},
	}, labels)

	milestones, err := client.ListRepositoryMilestones(context.Background(), "alice", "project")
	require.NoError(t, err)
	assert.Equal(t, []RepositoryMilestone{
		{ID: 3, Title: "v1.0", State: "open"},
		{ID: 4, Title: "v0.9", State: "closed"},
	}, milestones)
}

func TestUserExistsUsesRepositoryListing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/users/bob/repos" {
			writeResponse(t, writer, `[]`)
			return
		}
		http.Error(writer, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	exists, err := client.UserExists(context.Background(), "bob")
	require.NoError(t, err)
	assert.True(t, exists)

	exists, err = client.UserExists(context.Background(), "ghost")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestUpdateIssueSendsExplicitFieldsAndMapsResponse(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		assert.Equal(t, http.MethodPatch, request.Method)
		assert.Equal(t, "/api/v1/repos/alice/project/issues/7", request.URL.Path)
		var payload map[string]any
		assert.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
		assert.Equal(t, "Renamed issue", payload["title"])
		assert.Equal(t, "Rewritten body.", payload["body"])
		assert.Equal(t, "closed", payload["state"])
		assert.NotContains(t, payload, "assignee")
		assert.NotContains(t, payload, "milestone")
		writeResponse(t, writer, `{
			"number": 7,
			"title": "Renamed issue",
			"body": "Rewritten body.",
			"state": "closed",
			"user": {"id": 3, "username": "alice"},
			"comments": 0,
			"created_at": "2026-01-02T15:04:05Z",
			"updated_at": "2026-01-03T10:00:00Z"
		}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	body := "Rewritten body."
	state := "closed"
	issue, err := client.UpdateIssue(context.Background(), "alice", "project", 7, UpdateIssueOptions{
		Title: "Renamed issue",
		Body:  &body,
		State: &state,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
	assert.Equal(t, int64(7), issue.Number)
	assert.Equal(t, "Renamed issue", issue.Title)
	assert.Equal(t, "Rewritten body.", issue.Body)
	assert.Equal(t, "closed", issue.State)
	assert.Equal(t, "alice", issue.User.Username)
	assert.Nil(t, issue.Assignee)
	assert.Nil(t, issue.Milestone)
	assert.Empty(t, issue.Labels)
	assert.Equal(t, "2026-01-02T15:04:05Z", issue.CreatedAt)
	assert.Equal(t, "2026-01-03T10:00:00Z", issue.UpdatedAt)
	assert.Equal(t, server.URL+"/alice/project/issues/7", issue.WebURL)
}

func TestUpdateIssueSendsClearValuesForPointedFields(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		var payload map[string]any
		assert.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
		assert.Empty(t, payload["body"])
		assert.Empty(t, payload["assignee"])
		assert.EqualValues(t, 0, payload["milestone"])
		assert.NotContains(t, payload, "title")
		assert.NotContains(t, payload, "state")
		writeResponse(t, writer, `{"number": 7, "state": "open"}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	cleared := ""
	milestone := int64(0)
	_, err := client.UpdateIssue(context.Background(), "alice", "project", 7, UpdateIssueOptions{
		Body:      &cleared,
		Assignee:  &cleared,
		Milestone: &milestone,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
}

func TestUpdateIssueDoesNotRetryWhenOutcomeIsUnknown(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeResponse(t, writer, `not-json`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.UpdateIssue(context.Background(), "alice", "project", 7, UpdateIssueOptions{Title: "Renamed issue"})
	require.Error(t, err)
	assert.Equal(t, int32(1), requests.Load(), "a write must never be retried")
	assert.Equal(t, CodeWriteOutcomeUnknown, AsError(err).Code)
}

func TestUpdateIssueClassifiesDefinitiveRejectionWithoutRetrying(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(writer, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.UpdateIssue(context.Background(), "alice", "project", 7, UpdateIssueOptions{Title: "Renamed issue"})
	require.Error(t, err)
	assert.Equal(t, 1, requests)
	classified := AsError(err)
	assert.Equal(t, CodePermissionDenied, classified.Code)
	assert.Equal(t, http.StatusForbidden, classified.HTTPStatus)
}

func TestCreateIssueCommentSendsBodyAndMapsResponse(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/api/v1/repos/alice/project/issues/7/comments", request.URL.Path)
		var payload map[string]any
		assert.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
		assert.Equal(t, "Confirmed on my machine.", payload["body"])
		writeResponse(t, writer, `{
			"id": 11,
			"user": {"id": 4, "username": "reader"},
			"body": "Confirmed on my machine.",
			"created_at": "2026-01-04T09:00:00Z",
			"updated_at": "2026-01-04T09:00:00Z"
		}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	comment, err := client.CreateIssueComment(context.Background(), "alice", "project", 7, "Confirmed on my machine.")
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
	assert.Equal(t, IssueComment{
		ID:        11,
		User:      User{ID: 4, Username: "reader"},
		Body:      "Confirmed on my machine.",
		CreatedAt: "2026-01-04T09:00:00Z",
		UpdatedAt: "2026-01-04T09:00:00Z",
	}, comment)
}

func TestCreateIssueCommentDoesNotRetryWhenOutcomeIsUnknown(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.CreateIssueComment(context.Background(), "alice", "project", 7, "Confirmed on my machine.")
	require.Error(t, err)
	assert.Equal(t, int32(1), requests.Load(), "a write must never be retried")
	assert.Equal(t, CodeWriteOutcomeUnknown, AsError(err).Code)
}

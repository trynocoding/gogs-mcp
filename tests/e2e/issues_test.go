//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type issueIdentity struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

type issuesBootstrapResult struct {
	User               issueIdentity `json:"user"`
	Reader             issueIdentity `json:"reader"`
	Repository         string        `json:"repository"`
	OpenIssueNumber    int64         `json:"open_issue_number"`
	CommentIssueNumber int64         `json:"comment_issue_number"`
	ClosedIssueNumber  int64         `json:"closed_issue_number"`
	FirstCommentAt     string        `json:"first_comment_at"`
	SecondCommentAt    string        `json:"second_comment_at"`
	LabelName          string        `json:"label_name"`
	MilestoneTitle     string        `json:"milestone_title"`
}

type issueUser struct {
	Username string `json:"username"`
	FullName string `json:"full_name"`
}

type issueLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type issueMilestone struct {
	Title string `json:"title"`
	State string `json:"state"`
}

type issueSummary struct {
	Number      int64        `json:"number"`
	Title       string       `json:"title"`
	State       string       `json:"state"`
	User        issueUser    `json:"user"`
	Labels      []issueLabel `json:"labels"`
	NumComments int          `json:"num_comments"`
	CreatedAt   string       `json:"created_at"`
	UpdatedAt   string       `json:"updated_at"`
	Body        string       `json:"body"`
}

type issuePage struct {
	Issues []issueSummary `json:"issues"`
	State  string         `json:"state"`
	Page   int            `json:"page"`
}

type issue struct {
	Number      int64           `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	User        issueUser       `json:"user"`
	Assignee    *issueUser      `json:"assignee"`
	Labels      []issueLabel    `json:"labels"`
	Milestone   *issueMilestone `json:"milestone"`
	NumComments int             `json:"num_comments"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	WebURL      string          `json:"web_url"`
}

type issueComment struct {
	ID        int64     `json:"id"`
	User      issueUser `json:"user"`
	Body      string    `json:"body"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
}

type issueCommentPage struct {
	Comments []issueComment `json:"comments"`
	Since    string         `json:"since"`
	Max      int            `json:"max_comments"`
}

type responseMeta struct {
	RequestID string   `json:"request_id"`
	Truncated bool     `json:"truncated"`
	Warnings  []string `json:"warnings"`
}

type issuePageResponse struct {
	Data  *issuePage `json:"data,omitempty"`
	Error *toolError `json:"error,omitempty"`
}

type issueResponse struct {
	Data  *issue     `json:"data,omitempty"`
	Error *toolError `json:"error,omitempty"`
}

type issueCommentPageResponse struct {
	Data  *issueCommentPage `json:"data,omitempty"`
	Error *toolError        `json:"error,omitempty"`
	Meta  *responseMeta     `json:"meta,omitempty"`
}

func TestIssues(t *testing.T) {
	environment, projectRoot, identifier := prepareSmokeEnvironment(t)
	commandContext, cancelCommands := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelCommands()

	_, err := runCommand(commandContext, "create the issues E2E network", "docker", "network", "create", environment.network)
	require.NoError(t, err)
	dataDir := filepath.Join(environment.tempDir, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "log"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "app.ini"), []byte(gogsConfig(identifier)), 0o600))

	t.Log("Creating the issues E2E database with open, closed, empty, and commented issues.")
	bootstrapOutput, err := runSensitiveCommand(
		commandContext,
		"bootstrap the Gogs issues E2E database",
		"run",
		"--rm",
		"--name", environment.bootstrapContainer,
		"--network", environment.network,
		"--volume", dataDir+":/data:Z",
		"--entrypoint", "/app/gogs-e2e-issues",
		environment.image,
		"--config", "/data/app.ini",
	)
	require.NoError(t, err)
	bootstrap := parseIssuesBootstrap(t, bootstrapOutput)
	require.NotEmpty(t, bootstrap.User.Token)

	t.Log("Starting Gogs for the issues scenario.")
	_, err = runCommand(
		commandContext,
		"start the Gogs issues E2E container",
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

	t.Log("Verifying issue listing, retrieval, and comments.")
	mcpLogs := useMCPClient(t, projectRoot, baseURL, bootstrap.User.Token, func(session *mcp.ClientSession) {
		openPage := callIssuePage(t, session, "list_issues", map[string]any{
			"owner": bootstrap.User.Username,
			"repo":  bootstrap.Repository,
		})
		assert.Equal(t, "open", openPage.State)
		assert.Equal(t, 1, openPage.Page)
		assertIssueNumbers(t, openPage.Issues, []int64{bootstrap.OpenIssueNumber, bootstrap.CommentIssueNumber})
		for _, summary := range openPage.Issues {
			assert.Empty(t, summary.Body, "list output must not include issue bodies")
		}
		commented := findIssueSummary(t, openPage.Issues, bootstrap.CommentIssueNumber)
		assert.Equal(t, bootstrap.LabelName, commented.Labels[0].Name)
		assert.Equal(t, 2, commented.NumComments)
		assert.Equal(t, bootstrap.User.Username, commented.User.Username)

		closedPage := callIssuePage(t, session, "list_issues", map[string]any{
			"owner": bootstrap.User.Username,
			"repo":  bootstrap.Repository,
			"state": "closed",
		})
		assertIssueNumbers(t, closedPage.Issues, []int64{bootstrap.ClosedIssueNumber})
		assert.Equal(t, "closed", closedPage.Issues[0].State)

		detail := callIssue(t, session, map[string]any{
			"owner":  bootstrap.User.Username,
			"repo":   bootstrap.Repository,
			"number": bootstrap.CommentIssueNumber,
		})
		require.NotNil(t, detail.Data)
		assert.Equal(t, bootstrap.CommentIssueNumber, detail.Data.Number)
		assert.Equal(t, "The parser fails on empty input.", detail.Data.Body)
		assert.Equal(t, "open", detail.Data.State)
		assert.Equal(t, bootstrap.User.Username, detail.Data.User.Username)
		require.NotNil(t, detail.Data.Assignee)
		assert.Equal(t, bootstrap.User.Username, detail.Data.Assignee.Username)
		require.Len(t, detail.Data.Labels, 1)
		assert.Equal(t, bootstrap.LabelName, detail.Data.Labels[0].Name)
		require.NotNil(t, detail.Data.Milestone)
		assert.Equal(t, bootstrap.MilestoneTitle, detail.Data.Milestone.Title)
		assert.Equal(t, "open", detail.Data.Milestone.State)
		assert.Equal(t, 2, detail.Data.NumComments)
		assert.NotEmpty(t, detail.Data.CreatedAt)
		assert.NotEmpty(t, detail.Data.UpdatedAt)

		missing := callIssue(t, session, map[string]any{
			"owner":  bootstrap.User.Username,
			"repo":   bootstrap.Repository,
			"number": 99999,
		})
		require.NotNil(t, missing.Error)
		assert.Equal(t, "RESOURCE_NOT_FOUND_OR_FORBIDDEN", missing.Error.Code)

		emptyComments := callIssueCommentPage(t, session, map[string]any{
			"owner":  bootstrap.User.Username,
			"repo":   bootstrap.Repository,
			"number": bootstrap.OpenIssueNumber,
		})
		require.NotNil(t, emptyComments.Data)
		assert.Empty(t, emptyComments.Data.Comments)

		allComments := callIssueCommentPage(t, session, map[string]any{
			"owner":  bootstrap.User.Username,
			"repo":   bootstrap.Repository,
			"number": bootstrap.CommentIssueNumber,
		})
		require.NotNil(t, allComments.Data)
		require.Len(t, allComments.Data.Comments, 2)
		assert.Equal(t, "First comment before the cutoff.", allComments.Data.Comments[0].Body)
		assert.Equal(t, "Second comment after the cutoff.", allComments.Data.Comments[1].Body)
		assert.Equal(t, bootstrap.User.Username, allComments.Data.Comments[0].User.Username)
		assert.NotZero(t, allComments.Data.Comments[0].ID)
		assert.NotEmpty(t, allComments.Data.Comments[0].CreatedAt)
		assert.Equal(t, 100, allComments.Data.Max)

		cutoffComments := callIssueCommentPage(t, session, map[string]any{
			"owner":  bootstrap.User.Username,
			"repo":   bootstrap.Repository,
			"number": bootstrap.CommentIssueNumber,
			"since":  bootstrap.SecondCommentAt,
		})
		require.NotNil(t, cutoffComments.Data)
		require.Len(t, cutoffComments.Data.Comments, 1)
		assert.Equal(t, "Second comment after the cutoff.", cutoffComments.Data.Comments[0].Body)
		assert.Equal(t, bootstrap.SecondCommentAt, cutoffComments.Data.Since)

		invalidSince := callIssueCommentPage(t, session, map[string]any{
			"owner":  bootstrap.User.Username,
			"repo":   bootstrap.Repository,
			"number": bootstrap.CommentIssueNumber,
			"since":  "not-a-timestamp",
		})
		require.NotNil(t, invalidSince.Error)
		assert.Equal(t, "INVALID_ARGUMENT", invalidSince.Error.Code)

		cappedComments := callIssueCommentPage(t, session, map[string]any{
			"owner":        bootstrap.User.Username,
			"repo":         bootstrap.Repository,
			"number":       bootstrap.CommentIssueNumber,
			"max_comments": 1,
		})
		require.NotNil(t, cappedComments.Data)
		require.Len(t, cappedComments.Data.Comments, 1)
		assert.Equal(t, 1, cappedComments.Data.Max)
		assert.True(t, cappedComments.Meta.Truncated)
		require.NotEmpty(t, cappedComments.Meta.Warnings)
		assert.Contains(t, cappedComments.Meta.Warnings[0], "max_comments limit of 1")
	})

	assertNoSecrets(t, mcpLogs, bootstrap.User.Token)

	t.Log("Verifying that an invalid since never reaches Gogs.")
	gogsLogs, err := runCommand(commandContext, "read Gogs issues E2E logs", "docker", "logs", environment.container)
	require.NoError(t, err)
	assertNoSecrets(t, gogsLogs, bootstrap.User.Token)
	assert.NotContains(t, string(gogsLogs), "since=not-a-timestamp")

	t.Log("Removing all issues E2E resources.")
	environment.cleanup(t)
	assertResourcesRemoved(t, environment)
}

func parseIssuesBootstrap(t *testing.T, output []byte) issuesBootstrapResult {
	t.Helper()
	lines := bytes.Split(output, []byte("\n"))
	for index := len(lines) - 1; index >= 0; index-- {
		line := bytes.TrimSpace(lines[index])
		if len(line) == 0 {
			continue
		}
		var result issuesBootstrapResult
		if err := json.Unmarshal(line, &result); err == nil && result.User.Token != "" {
			return result
		}
	}
	require.FailNow(t, "The issues bootstrap command did not return an identity.")
	return issuesBootstrapResult{}
}

func callIssuePage(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) issuePage {
	t.Helper()
	result := callE2ETool(t, session, name, arguments)
	require.False(t, result.IsError)
	var response issuePageResponse
	decodeStructuredContent(t, result.StructuredContent, &response)
	require.NotNil(t, response.Data)
	return *response.Data
}

func callIssue(t *testing.T, session *mcp.ClientSession, arguments map[string]any) issueResponse {
	t.Helper()
	result := callE2ETool(t, session, "get_issue", arguments)
	var response issueResponse
	decodeStructuredContent(t, result.StructuredContent, &response)
	return response
}

func callIssueCommentPage(t *testing.T, session *mcp.ClientSession, arguments map[string]any) issueCommentPageResponse {
	t.Helper()
	result := callE2ETool(t, session, "list_issue_comments", arguments)
	var response issueCommentPageResponse
	decodeStructuredContent(t, result.StructuredContent, &response)
	return response
}

func findIssueSummary(t *testing.T, issues []issueSummary, number int64) issueSummary {
	t.Helper()
	for _, issue := range issues {
		if issue.Number == number {
			return issue
		}
	}
	require.FailNow(t, "Issue was not listed.", number)
	return issueSummary{}
}

func assertIssueNumbers(t *testing.T, issues []issueSummary, expected []int64) {
	t.Helper()
	numbers := make([]int64, len(issues))
	for index, issue := range issues {
		numbers[index] = issue.Number
	}
	assert.ElementsMatch(t, expected, numbers)
}

func TestIssueWriting(t *testing.T) {
	environment, projectRoot, identifier := prepareSmokeEnvironment(t)
	commandContext, cancelCommands := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelCommands()

	_, err := runCommand(commandContext, "create the issues E2E network", "docker", "network", "create", environment.network)
	require.NoError(t, err)
	dataDir := filepath.Join(environment.tempDir, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "log"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "app.ini"), []byte(gogsConfig(identifier)), 0o600))

	t.Log("Creating the issues E2E database with an owner and a read-only collaborator.")
	bootstrapOutput, err := runSensitiveCommand(
		commandContext,
		"bootstrap the Gogs issues E2E database",
		"run",
		"--rm",
		"--name", environment.bootstrapContainer,
		"--network", environment.network,
		"--volume", dataDir+":/data:Z",
		"--entrypoint", "/app/gogs-e2e-issues",
		environment.image,
		"--config", "/data/app.ini",
	)
	require.NoError(t, err)
	bootstrap := parseIssuesBootstrap(t, bootstrapOutput)
	require.NotEmpty(t, bootstrap.User.Token)
	require.NotEmpty(t, bootstrap.Reader.Token)

	t.Log("Starting Gogs for the issue writing scenario.")
	_, err = runCommand(
		commandContext,
		"start the Gogs issues E2E container",
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

	writeEnv := []string{"GOGS_MCP_WRITE_ENABLED=true"}

	t.Log("Verifying that the default tool list excludes create_issue.")
	readerLogs := useMCPClient(t, projectRoot, baseURL, bootstrap.Reader.Token, func(session *mcp.ClientSession) {
		openPage := callIssuePage(t, session, "list_issues", map[string]any{
			"owner": bootstrap.User.Username,
			"repo":  bootstrap.Repository,
		})
		assertIssueNumbers(t, openPage.Issues, []int64{bootstrap.OpenIssueNumber, bootstrap.CommentIssueNumber})
	})
	assertNoSecrets(t, readerLogs, bootstrap.Reader.Token, bootstrap.User.Token)

	t.Log("Verifying create_issue as a read-only collaborator with writing enabled.")
	readerWriteLogs := useMCPClientWithOptions(t, projectRoot, baseURL, bootstrap.Reader.Token, "", writeEnv, e2eWritingToolNames, func(session *mcp.ClientSession) {
		created := callCreatedIssue(t, session, map[string]any{
			"owner": bootstrap.User.Username,
			"repo":  bootstrap.Repository,
			"title": "Reader-reported parser crash",
			"body":  "Steps to reproduce the crash.",
		})
		require.NotNil(t, created.Data)
		assert.Equal(t, "open", created.Data.State)
		assert.Equal(t, bootstrap.Reader.Username, created.Data.User.Username)
		assert.Equal(t,
			fmt.Sprintf("%s/%s/%s/issues/%d", baseURL, bootstrap.User.Username, bootstrap.Repository, created.Data.Number),
			created.Data.WebURL)

		detail := callIssue(t, session, map[string]any{
			"owner":  bootstrap.User.Username,
			"repo":   bootstrap.Repository,
			"number": created.Data.Number,
		})
		require.NotNil(t, detail.Data)
		assert.Equal(t, "Reader-reported parser crash", detail.Data.Title)
		assert.Equal(t, "Steps to reproduce the crash.", detail.Data.Body)
		assert.Equal(t, "open", detail.Data.State)
		assert.Equal(t, created.Data.WebURL, detail.Data.WebURL)

		denied := callE2ETool(t, session, "create_issue", map[string]any{
			"owner":  bootstrap.User.Username,
			"repo":   bootstrap.Repository,
			"title":  "Denied managed fields",
			"labels": []any{"bug"},
		})
		var deniedResponse issueResponse
		decodeStructuredContent(t, denied.StructuredContent, &deniedResponse)
		require.NotNil(t, deniedResponse.Error)
		assert.Equal(t, "PERMISSION_DENIED", deniedResponse.Error.Code)
	})
	assertNoSecrets(t, readerWriteLogs, bootstrap.Reader.Token, bootstrap.User.Token)

	t.Log("Verifying create_issue with administrative fields as the owner.")
	ownerLogs := useMCPClientWithOptions(t, projectRoot, baseURL, bootstrap.User.Token, "", writeEnv, e2eWritingToolNames, func(session *mcp.ClientSession) {
		created := callCreatedIssue(t, session, map[string]any{
			"owner":     bootstrap.User.Username,
			"repo":      bootstrap.Repository,
			"title":     "Owner-planned parser rework",
			"assignee":  bootstrap.User.Username,
			"labels":    []any{bootstrap.LabelName},
			"milestone": bootstrap.MilestoneTitle,
		})
		require.NotNil(t, created.Data)
		assert.Equal(t, "open", created.Data.State)

		detail := callIssue(t, session, map[string]any{
			"owner":  bootstrap.User.Username,
			"repo":   bootstrap.Repository,
			"number": created.Data.Number,
		})
		require.NotNil(t, detail.Data)
		require.Len(t, detail.Data.Labels, 1)
		assert.Equal(t, bootstrap.LabelName, detail.Data.Labels[0].Name)
		require.NotNil(t, detail.Data.Milestone)
		assert.Equal(t, bootstrap.MilestoneTitle, detail.Data.Milestone.Title)
		require.NotNil(t, detail.Data.Assignee)
		assert.Equal(t, bootstrap.User.Username, detail.Data.Assignee.Username)

		unknown := callE2ETool(t, session, "create_issue", map[string]any{
			"owner":  bootstrap.User.Username,
			"repo":   bootstrap.Repository,
			"title":  "Unknown label reference",
			"labels": []any{"ghost-label"},
		})
		var unknownResponse issueResponse
		decodeStructuredContent(t, unknown.StructuredContent, &unknownResponse)
		require.NotNil(t, unknownResponse.Error)
		assert.Equal(t, "INVALID_ARGUMENT", unknownResponse.Error.Code)
	})
	assertNoSecrets(t, ownerLogs, bootstrap.User.Token)

	t.Log("Removing all issue writing E2E resources.")
	environment.cleanup(t)
	assertResourcesRemoved(t, environment)
}

// callCreatedIssue runs create_issue and decodes its issue payload.
func callCreatedIssue(t *testing.T, session *mcp.ClientSession, arguments map[string]any) issueResponse {
	t.Helper()
	result := callE2ETool(t, session, "create_issue", arguments)
	require.False(t, result.IsError)
	var response issueResponse
	decodeStructuredContent(t, result.StructuredContent, &response)
	return response
}

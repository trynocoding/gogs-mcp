package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"gogs-mcp/internal/gogs"
	"gogs-mcp/internal/snapshot"

	cockroacherrors "github.com/cockroachdb/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeClient struct {
	user               gogs.User
	userErr            error
	userCalls          int
	repositories       []gogs.Repository
	listErr            error
	repository         gogs.Repository
	getErr             error
	listCalls          int
	getCalls           int
	requestedOwner     string
	requestedRepo      string
	directoryEntries   []gogs.ContentEntry
	directoryRef       string
	directoryErr       error
	directoryCalls     int
	directoryPath      string
	file               gogs.FileContent
	fileErr            error
	fileCalls          int
	filePath           string
	contentRef         string
	branches           []gogs.Branch
	branch             gogs.Branch
	branchErr          error
	branchCalls        int
	branchName         string
	commits            []gogs.Commit
	commitLimit        int
	commitErr          error
	commitCalls        int
	commit             gogs.Commit
	commitSHA          string
	resolvedSHA        string
	resolveErr         error
	resolveCalls       int
	resolveRef         string
	archiveCalls       int
	archiveSHA         string
	archiveBody        func() (io.ReadCloser, error)
	issueSummaries     []gogs.IssueSummary
	issueNextPage      int
	listIssuesErr      error
	listIssuesCalls    int
	listIssuesState    string
	listIssuesPage     int
	issue              gogs.Issue
	getIssueErr        error
	getIssueCalls      int
	getIssueNumber     int64
	issueComments      []gogs.IssueComment
	commentsErr        error
	commentsCalls      int
	commentsNumber     int64
	commentsSince      string
	repoLabels         []gogs.RepositoryLabel
	repoLabelErr       error
	repoMilestones     []gogs.RepositoryMilestone
	repoMilestoneErr   error
	userExists         bool
	userExistsErr      error
	userExistsCalls    int
	createdIssue       gogs.Issue
	createIssueErr     error
	createIssueCalls   int
	createdOptions     gogs.CreateIssueOptions
	updatedIssue       gogs.Issue
	updateIssueErr     error
	updateIssueCalls   int
	updatedNumber      int64
	updatedOptions     gogs.UpdateIssueOptions
	createdComment     gogs.IssueComment
	createCommentErr   error
	createCommentCalls int
	commentedNumber    int64
	commentedBody      string
	pullSummaries      []gogs.PullRequestSummary
	pullTotal          int
	listPullsErr       error
	listPullsCalls     int
	listPullsState     string
	listPullsLimit     int
	pull               gogs.PullRequest
	getPullErr         error
	getPullCalls       int
	getPullNumber      int64
	pullDiff           gogs.PullRequestDiff
	pullDiffErr        error
	pullDiffCalls      int
	pullDiffNumber     int64
	pullDiffBaseRef    string
	pullDiffPaths      []string
	pullDiffMaxBytes   int
}

func (c *fakeClient) GetAuthenticatedUser(context.Context) (gogs.User, error) {
	c.userCalls++
	return c.user, c.userErr
}

func (c *fakeClient) ListRepositories(context.Context) ([]gogs.Repository, error) {
	c.listCalls++
	return c.repositories, c.listErr
}

func (c *fakeClient) GetRepository(_ context.Context, owner, repo string) (gogs.Repository, error) {
	c.getCalls++
	c.requestedOwner = owner
	c.requestedRepo = repo
	return c.repository, c.getErr
}

func (c *fakeClient) ListDirectory(_ context.Context, _, _, repositoryPath, ref string) ([]gogs.ContentEntry, string, error) {
	c.directoryCalls++
	c.directoryPath = repositoryPath
	c.contentRef = ref
	return c.directoryEntries, c.directoryRef, c.directoryErr
}

func (c *fakeClient) GetFile(_ context.Context, _, _, repositoryPath, ref string) (gogs.FileContent, error) {
	c.fileCalls++
	c.filePath = repositoryPath
	c.contentRef = ref
	return c.file, c.fileErr
}

func (c *fakeClient) ListBranches(context.Context, string, string) ([]gogs.Branch, error) {
	c.branchCalls++
	return c.branches, c.branchErr
}

func (c *fakeClient) GetBranch(_ context.Context, _, _, branch string) (gogs.Branch, error) {
	c.branchCalls++
	c.branchName = branch
	return c.branch, c.branchErr
}

func (c *fakeClient) ListCommits(_ context.Context, _, _ string, limit int) ([]gogs.Commit, error) {
	c.commitCalls++
	c.commitLimit = limit
	return c.commits, c.commitErr
}

func (c *fakeClient) GetCommit(_ context.Context, _, _, sha string) (gogs.Commit, error) {
	c.commitCalls++
	c.commitSHA = sha
	return c.commit, c.commitErr
}

func (c *fakeClient) ResolveCommitSHA(_ context.Context, _, _, ref string) (string, error) {
	c.resolveCalls++
	c.resolveRef = ref
	return c.resolvedSHA, c.resolveErr
}

func (c *fakeClient) DownloadArchive(_ context.Context, _, _, sha string) (io.ReadCloser, error) {
	c.archiveCalls++
	c.archiveSHA = sha
	if c.archiveBody != nil {
		return c.archiveBody()
	}
	return nil, cockroacherrors.New("unexpected archive download")
}

func (c *fakeClient) ListIssues(_ context.Context, _, _, state string, page int) ([]gogs.IssueSummary, int, error) {
	c.listIssuesCalls++
	c.listIssuesState = state
	c.listIssuesPage = page
	return c.issueSummaries, c.issueNextPage, c.listIssuesErr
}

func (c *fakeClient) GetIssue(_ context.Context, _, _ string, number int64) (gogs.Issue, error) {
	c.getIssueCalls++
	c.getIssueNumber = number
	return c.issue, c.getIssueErr
}

func (c *fakeClient) ListIssueComments(_ context.Context, _, _ string, number int64, since string) ([]gogs.IssueComment, error) {
	c.commentsCalls++
	c.commentsNumber = number
	c.commentsSince = since
	return c.issueComments, c.commentsErr
}

func (c *fakeClient) ListRepositoryLabels(_ context.Context, _, _ string) ([]gogs.RepositoryLabel, error) {
	return c.repoLabels, c.repoLabelErr
}

func (c *fakeClient) ListRepositoryMilestones(_ context.Context, _, _ string) ([]gogs.RepositoryMilestone, error) {
	return c.repoMilestones, c.repoMilestoneErr
}

func (c *fakeClient) UserExists(_ context.Context, _ string) (bool, error) {
	c.userExistsCalls++
	return c.userExists, c.userExistsErr
}

func (c *fakeClient) CreateIssue(_ context.Context, _, _ string, options gogs.CreateIssueOptions) (gogs.Issue, error) {
	c.createIssueCalls++
	c.createdOptions = options
	return c.createdIssue, c.createIssueErr
}

func (c *fakeClient) UpdateIssue(_ context.Context, _, _ string, number int64, options gogs.UpdateIssueOptions) (gogs.Issue, error) {
	c.updateIssueCalls++
	c.updatedNumber = number
	c.updatedOptions = options
	return c.updatedIssue, c.updateIssueErr
}

func (c *fakeClient) CreateIssueComment(_ context.Context, _, _ string, number int64, body string) (gogs.IssueComment, error) {
	c.createCommentCalls++
	c.commentedNumber = number
	c.commentedBody = body
	return c.createdComment, c.createCommentErr
}

func (c *fakeClient) ListPullRequests(_ context.Context, _, _, state string, limit int) ([]gogs.PullRequestSummary, int, error) {
	c.listPullsCalls++
	c.listPullsState = state
	c.listPullsLimit = limit
	return c.pullSummaries, c.pullTotal, c.listPullsErr
}

func (c *fakeClient) GetPullRequest(_ context.Context, _, _ string, number int64) (gogs.PullRequest, error) {
	c.getPullCalls++
	c.getPullNumber = number
	return c.pull, c.getPullErr
}

func (c *fakeClient) GetPullRequestDiff(_ context.Context, _, _ string, number int64, baseRef string, paths []string, maxBytes int) (gogs.PullRequestDiff, error) {
	c.pullDiffCalls++
	c.pullDiffNumber = number
	c.pullDiffBaseRef = baseRef
	c.pullDiffPaths = paths
	c.pullDiffMaxBytes = maxBytes
	return c.pullDiff, c.pullDiffErr
}

func TestServerNegotiatesAndReturnsAuthenticatedUser(t *testing.T) {
	session := connectTestClient(t, &fakeClient{user: gogs.User{
		ID:       42,
		Username: "alice",
		FullName: "Alice Example",
		Email:    "alice@example.test",
	}})

	list, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, list.Tools, 17)
	tool := findTool(t, list.Tools, "get_authenticated_user")
	require.NotNil(t, tool.Annotations)
	assert.True(t, tool.Annotations.ReadOnlyHint)
	assert.True(t, tool.Annotations.IdempotentHint)
	require.NotNil(t, tool.Annotations.DestructiveHint)
	assert.False(t, *tool.Annotations.DestructiveHint)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_authenticated_user",
		Arguments: map[string]any{},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var output ToolResponse[AuthenticatedUser]
	decodeStructuredContent(t, result.StructuredContent, &output)
	require.NotNil(t, output.Data)
	assert.Equal(t, int64(42), output.Data.ID)
	assert.Equal(t, "alice", output.Data.Username)
	assert.Equal(t, "Alice Example", output.Data.FullName)
	assert.Equal(t, "alice@example.test", output.Data.Email)
	assert.NotEmpty(t, output.Meta.RequestID)
	require.NotEmpty(t, result.Content)
	textContent, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, textContent.Text, "\"username\":\"alice\"")
}

func TestServerReturnsStructuredAuthenticationError(t *testing.T) {
	session := connectTestClient(t, &fakeClient{userErr: &gogs.Error{
		Code:      gogs.CodeAuthenticationFailed,
		Message:   "Gogs rejected the configured credentials.",
		Retryable: false,
	}})

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_authenticated_user",
		Arguments: map[string]any{},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)

	var output ToolResponse[AuthenticatedUser]
	decodeStructuredContent(t, result.StructuredContent, &output)
	assert.Nil(t, output.Data)
	require.NotNil(t, output.Error)
	assert.Equal(t, "AUTHENTICATION_FAILED", output.Error.Code)
	assert.False(t, output.Error.Retryable)
	assert.NotEmpty(t, output.Meta.RequestID)
}

func TestDefaultToolListContainsNoWriteTools(t *testing.T) {
	session := connectTestClient(t, &fakeClient{})
	list, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	names := make([]string, 0, len(list.Tools))
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	assert.NotContains(t, names, "create_issue")
	assert.NotContains(t, names, "update_issue")
	assert.NotContains(t, names, "create_issue_comment")
}

func TestAuthenticatedUserToolRejectsUnknownInput(t *testing.T) {
	session := connectTestClient(t, &fakeClient{})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_authenticated_user",
		Arguments: map[string]any{
			"unexpected": true,
		},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func connectTestClient(t *testing.T, serverClient Client) *mcp.ClientSession {
	t.Helper()
	return connectTestClientWithSnapshots(t, serverClient, nil)
}

func connectWritingTestClient(t *testing.T, serverClient Client) *mcp.ClientSession {
	t.Helper()
	return connectTestClientWithDefaults(t, serverClient, nil, true)
}

func connectTestClientWithSnapshots(t *testing.T, serverClient Client, snapshots *snapshot.Manager, search ...SearchDefaults) *mcp.ClientSession {
	t.Helper()
	return connectTestClientWithDefaults(t, serverClient, snapshots, false, search...)
}

func connectTestClientWithDefaults(t *testing.T, serverClient Client, snapshots *snapshot.Manager, writeEnabled bool, search ...SearchDefaults) *mcp.ClientSession {
	t.Helper()
	defaults := DefaultSearchDefaults()
	if len(search) > 0 {
		defaults = search[0]
	}
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := New(serverClient, snapshots, slog.New(slog.NewTextHandler(io.Discard, nil)), defaults, writeEnabled)
	serverSession, err := server.MCP().Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, serverSession.Close())
	})

	client := mcp.NewClient(
		&mcp.Implementation{Name: "gogs-mcp-test", Version: "test"},
		nil,
	)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, clientSession.Close())
	})
	return clientSession
}

func findTool(t *testing.T, tools []*mcp.Tool, name string) *mcp.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	require.FailNow(t, "Tool was not registered.", name)
	return nil
}

func decodeStructuredContent(t *testing.T, source, destination any) {
	t.Helper()
	encoded, err := json.Marshal(source)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, destination))
}

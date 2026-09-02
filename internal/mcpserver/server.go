package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"gogs-mcp/internal/gogs"
	"gogs-mcp/internal/snapshot"
	"gogs-mcp/internal/version"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Client interface {
	GetAuthenticatedUser(context.Context) (gogs.User, error)
	ListRepositories(context.Context) ([]gogs.Repository, error)
	GetRepository(context.Context, string, string) (gogs.Repository, error)
	ListDirectory(context.Context, string, string, string, string) ([]gogs.ContentEntry, string, error)
	GetFile(context.Context, string, string, string, string) (gogs.FileContent, error)
	ListBranches(context.Context, string, string) ([]gogs.Branch, error)
	GetBranch(context.Context, string, string, string) (gogs.Branch, error)
	ListCommits(context.Context, string, string, int) ([]gogs.Commit, error)
	GetCommit(context.Context, string, string, string) (gogs.Commit, error)
	ResolveCommitSHA(context.Context, string, string, string) (string, error)
	DownloadArchive(context.Context, string, string, string) (io.ReadCloser, error)
	ListIssues(context.Context, string, string, string, int) ([]gogs.IssueSummary, int, error)
	GetIssue(context.Context, string, string, int64) (gogs.Issue, error)
	ListIssueComments(context.Context, string, string, int64, string) ([]gogs.IssueComment, error)
	ListPullRequests(context.Context, string, string, string, int) ([]gogs.PullRequestSummary, int, error)
	GetPullRequest(context.Context, string, string, int64) (gogs.PullRequest, error)
	GetPullRequestDiff(context.Context, string, string, int64, string, []string, int) (gogs.PullRequestDiff, error)
	ListRepositoryLabels(context.Context, string, string) ([]gogs.RepositoryLabel, error)
	ListRepositoryMilestones(context.Context, string, string) ([]gogs.RepositoryMilestone, error)
	UserExists(context.Context, string) (bool, error)
	CreateIssue(context.Context, string, string, gogs.CreateIssueOptions) (gogs.Issue, error)
	UpdateIssue(context.Context, string, string, int64, gogs.UpdateIssueOptions) (gogs.Issue, error)
	CreateIssueComment(context.Context, string, string, int64, string) (gogs.IssueComment, error)
}

type Server struct {
	mcp         *mcp.Server
	schemaCache *mcp.SchemaCache
}

type emptyInput struct{}

var fallbackRequestID atomic.Uint64

// serverOptions carries the optional construction settings of a server.
type serverOptions struct {
	schemaCache *mcp.SchemaCache
}

// Option customizes one optional construction setting of a server.
type Option func(*serverOptions)

// WithSchemaCache shares one schema cache across several servers. The http
// transport builds one server per authenticated user, and the cache removes
// the repeated JSON schema resolution of every tool registration.
func WithSchemaCache(cache *mcp.SchemaCache) Option {
	return func(options *serverOptions) {
		options.schemaCache = cache
	}
}

// New assembles the MCP server. The snapshot manager enables search_code and
// may be nil, in which case search_code reports that search is unavailable.
// The search defaults bound the per-search timeout and file size. Write tools
// are only registered when writeEnabled is set.
func New(client Client, snapshots *snapshot.Manager, logger *slog.Logger, search SearchDefaults, writeEnabled bool, options ...Option) *Server {
	resolved := serverOptions{}
	for _, option := range options {
		option(&resolved)
	}
	server := mcp.NewServer(
		&mcp.Implementation{Name: "gogs-mcp", Version: version.Version},
		&mcp.ServerOptions{
			Capabilities: &mcp.ServerCapabilities{},
			Instructions: "Use the available tools to read data from the configured Gogs instance.",
			Logger:       logger,
			SchemaCache:  resolved.schemaCache,
		},
	)

	destructive := false
	openWorld := true
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_authenticated_user",
		Description: "Return the Gogs user associated with the configured personal access token.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: &destructive,
			IdempotentHint:  true,
			OpenWorldHint:   &openWorld,
			ReadOnlyHint:    true,
			Title:           "Get authenticated user",
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, ToolResponse[AuthenticatedUser], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "get_authenticated_user")
		user, err := client.GetAuthenticatedUser(ctx)
		if err != nil {
			classified := gogs.AsError(err)
			return &mcp.CallToolResult{IsError: true}, ToolResponse[AuthenticatedUser]{
				Error: &ToolError{
					Code:      string(classified.Code),
					Message:   classified.Message,
					Retryable: classified.Retryable,
				},
				Meta: ResponseMeta{RequestID: requestID},
			}, nil
		}

		output := AuthenticatedUser{
			ID:       user.ID,
			Username: user.Username,
			FullName: user.FullName,
			Email:    user.Email,
		}
		return nil, ToolResponse[AuthenticatedUser]{
			Data: &output,
			Meta: ResponseMeta{RequestID: requestID},
		}, nil
	})
	registerRepositoryTools(server, client)
	registerContentTools(server, client)
	registerGitTools(server, client)
	registerPullTools(server, client)
	registerSearchTools(server, client, snapshots, &identityCache{}, search)
	registerIssueTools(server, client, writeEnabled)

	return &Server{
		mcp:         server,
		schemaCache: resolved.schemaCache,
	}
}

func (s *Server) MCP() *mcp.Server {
	return s.mcp
}

func (s *Server) Run(ctx context.Context, reader io.ReadCloser, writer io.WriteCloser) error {
	return s.mcp.Run(ctx, &mcp.IOTransport{Reader: reader, Writer: writer})
}

func newRequestID() string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err == nil {
		return hex.EncodeToString(random[:])
	}
	sequence := fallbackRequestID.Add(1)
	return time.Now().UTC().Format("20060102T150405.000000000") + "-" + hex.EncodeToString([]byte{
		byte(sequence >> 56),
		byte(sequence >> 48),
		byte(sequence >> 40),
		byte(sequence >> 32),
		byte(sequence >> 24),
		byte(sequence >> 16),
		byte(sequence >> 8),
		byte(sequence),
	})
}

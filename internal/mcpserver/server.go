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
	"gogs-mcp/internal/version"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Client interface {
	GetAuthenticatedUser(context.Context) (gogs.User, error)
	ListRepositories(context.Context) ([]gogs.Repository, error)
	GetRepository(context.Context, string, string) (gogs.Repository, error)
	ListDirectory(context.Context, string, string, string, string) ([]gogs.ContentEntry, string, error)
	GetFile(context.Context, string, string, string, string) (gogs.FileContent, error)
}

type Server struct {
	mcp *mcp.Server
}

type emptyInput struct{}

var fallbackRequestID atomic.Uint64

func New(client Client, logger *slog.Logger) *Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "gogs-mcp", Version: version.Version},
		&mcp.ServerOptions{
			Capabilities: &mcp.ServerCapabilities{},
			Instructions: "Use the available tools to read data from the configured Gogs instance.",
			Logger:       logger,
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

	return &Server{mcp: server}
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

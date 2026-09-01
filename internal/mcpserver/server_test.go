package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"gogs-mcp/internal/gogs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeClient struct {
	user             gogs.User
	userErr          error
	repositories     []gogs.Repository
	listErr          error
	repository       gogs.Repository
	getErr           error
	listCalls        int
	getCalls         int
	requestedOwner   string
	requestedRepo    string
	directoryEntries []gogs.ContentEntry
	directoryRef     string
	directoryErr     error
	directoryCalls   int
	directoryPath    string
	file             gogs.FileContent
	fileErr          error
	fileCalls        int
	filePath         string
	contentRef       string
}

func (c *fakeClient) GetAuthenticatedUser(context.Context) (gogs.User, error) {
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

func TestServerNegotiatesAndReturnsAuthenticatedUser(t *testing.T) {
	session := connectTestClient(t, &fakeClient{user: gogs.User{
		ID:       42,
		Username: "alice",
		FullName: "Alice Example",
		Email:    "alice@example.test",
	}})

	list, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, list.Tools, 6)
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
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := New(serverClient, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

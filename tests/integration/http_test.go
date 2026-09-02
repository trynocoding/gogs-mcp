//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogs-mcp/internal/httpserver"
	"gogs-mcp/internal/mcpserver"
	"gogs-mcp/internal/securelog"
	"gogs-mcp/internal/snapshot"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	httpAliceToken = "http-integration-alice-token"
	httpBobToken   = "http-integration-bob-token"
)

// multiUserGogs answers /api/v1/user from the bearer token and counts the
// calls, so the tests can observe both the identity and the caching.
func multiUserGogs(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var userRequests int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/user" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&userRequests, 1)
		writer.Header().Set("Content-Type", "application/json")
		switch request.Header.Get("Authorization") {
		case "token " + httpAliceToken:
			_, _ = fmt.Fprint(writer, `{"id":1,"username":"alice","full_name":"Alice","email":"alice@example.test"}`)
		case "token " + httpBobToken:
			_, _ = fmt.Fprint(writer, `{"id":2,"username":"bob","full_name":"Bob","email":"bob@example.test"}`)
		default:
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(writer, `{"message":"token is required"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server, &userRequests
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newHTTPTransportFixture assembles the production HTTP transport against a
// fake Gogs, so the tests exercise the full middleware chain.
func newHTTPTransportFixture(t *testing.T, gogsServer *httptest.Server, logs *lockedBuffer) *httpserver.Server {
	t.Helper()
	parsed, err := url.Parse(gogsServer.URL)
	require.NoError(t, err)
	snapshots, err := snapshot.NewManager(t.TempDir(), parsed.String(), snapshot.DefaultLimits(), snapshot.DefaultEviction())
	require.NoError(t, err)
	server, err := httpserver.New(httpserver.Options{
		Addr:           "127.0.0.1:0",
		Endpoint:       "/mcp",
		TokenHeader:    "Authorization",
		UserCacheTTL:   time.Minute,
		MaxUsers:       8,
		BaseURL:        parsed,
		UserAgent:      "gogs-mcp/integration",
		HTTPTimeout:    5 * time.Second,
		CacheRoot:      t.TempDir(),
		CacheTTL:       time.Hour,
		CacheMaxBytes:  1 << 20,
		SearchDefaults: mcpserver.DefaultSearchDefaults(),
		Snapshots:      snapshots,
		LogDestination: securelog.NewWriter(logs),
		LogLevel:       "debug",
	})
	require.NoError(t, err)
	return server
}

// bearerTransport adds one Gogs credential header to every request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	if t.token != "" {
		cloned.Header.Set("Authorization", "token "+t.token)
	}
	if t.base == nil {
		t.base = http.DefaultTransport
	}
	return t.base.RoundTrip(cloned)
}

// connectOverHTTP opens an MCP session against the transport endpoint with
// one token, mirroring how MCP clients connect to a central deployment.
func connectOverHTTP(t *testing.T, endpointURL, token string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint:             endpointURL,
		HTTPClient:           &http.Client{Transport: &bearerTransport{token: token}},
		DisableStandaloneSSE: true,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-http-test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func postMCP(t *testing.T, handler http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postMCPWithHeader(t, handler, "token "+token, body)
}

// postMCPWithHeader sends one raw JSON-RPC request with the exact credential
// header value, so malformed values reach the server as written.
func postMCPWithHeader(t *testing.T, handler http.Handler, headerValue, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if headerValue != "" {
		request.Header.Set("Authorization", headerValue)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestHTTPTransportIsolatesUsers(t *testing.T) {
	gogsServer, userRequests := multiUserGogs(t)
	logs := &lockedBuffer{}
	server := newHTTPTransportFixture(t, gogsServer, logs)
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)

	alice := connectOverHTTP(t, endpoint.URL+"/mcp", httpAliceToken)
	bob := connectOverHTTP(t, endpoint.URL+"/mcp", httpBobToken)

	tools, err := alice.ListTools(context.Background(), nil)
	require.NoError(t, err)
	names := make([]string, len(tools.Tools))
	for index, tool := range tools.Tools {
		names[index] = tool.Name
	}
	assert.ElementsMatch(t, []string{
		"get_authenticated_user",
		"list_repositories",
		"search_repositories",
		"get_repository",
		"list_directory",
		"get_file",
		"list_branches",
		"get_branch",
		"list_commits",
		"get_commit",
		"search_code",
		"list_issues",
		"get_issue",
		"list_issue_comments",
		"list_pull_requests",
		"get_pull_request",
		"get_pull_request_diff",
	}, names)

	for _, testCase := range []struct {
		session  *mcp.ClientSession
		username string
	}{
		{alice, "alice"},
		{bob, "bob"},
		// A second session with a known token reuses the cached identity.
		{connectOverHTTP(t, endpoint.URL+"/mcp", httpAliceToken), "alice"},
	} {
		result := callTool(t, testCase.session, "get_authenticated_user", map[string]any{})
		require.False(t, result.IsError)
		var response mcpserver.ToolResponse[mcpserver.AuthenticatedUser]
		decode(t, result.StructuredContent, &response)
		require.NotNil(t, response.Data)
		assert.Equal(t, testCase.username, response.Data.Username)
	}

	// Five Gogs calls in total: one identity probe per user plus one
	// /api/v1/user read per get_authenticated_user tool call. The MCP
	// handshake requests are served from the user cache.
	assert.Equal(t, int32(5), atomic.LoadInt32(userRequests),
		"the handshake must not add Gogs probes beyond one per user")
	assert.NotContains(t, logs.String(), httpAliceToken)
	assert.NotContains(t, logs.String(), httpBobToken)
}

func TestHTTPTransportRejectsMissingAndInvalidTokens(t *testing.T) {
	gogsServer, userRequests := multiUserGogs(t)
	logs := &lockedBuffer{}
	server := newHTTPTransportFixture(t, gogsServer, logs)
	handler := server.Handler()

	for _, testCase := range []struct {
		name   string
		header string
		secret string
	}{
		{name: "no header", header: ""},
		{name: "scheme only", header: "token "},
		{name: "unknown", header: "token http-integration-unknown-token", secret: "http-integration-unknown-token"},
	} {
		recorder := postMCPWithHeader(t, handler, testCase.header, `{}`)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code, testCase.name)
		assert.Equal(t, `Bearer realm="gogs-mcp"`, recorder.Header().Get("WWW-Authenticate"), testCase.name)
		assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"), testCase.name)
		if testCase.secret != "" {
			assert.NotContains(t, recorder.Body.String(), testCase.secret, testCase.name)
		}
	}

	// The negative cache must stop repeated probes of a rejected token.
	for i := 0; i < 3; i++ {
		recorder := postMCP(t, handler, "http-integration-unknown-token", `{}`)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(userRequests),
		"rejected tokens must not reach Gogs again until the negative cache expires")
}

func TestHTTPTransportRejectsUnsupportedMethods(t *testing.T) {
	gogsServer, _ := multiUserGogs(t)
	logs := &lockedBuffer{}
	server := newHTTPTransportFixture(t, gogsServer, logs)
	handler := server.Handler()

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		request := httptest.NewRequest(method, "/mcp", nil)
		request.Header.Set("Authorization", "token "+httpAliceToken)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusMethodNotAllowed, recorder.Code, method)
	}
}

func TestHTTPTransportServesHealthProbeWithoutCredentials(t *testing.T) {
	gogsServer, _ := multiUserGogs(t)
	logs := &lockedBuffer{}
	server := newHTTPTransportFixture(t, gogsServer, logs)
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, recorder.Code)
}

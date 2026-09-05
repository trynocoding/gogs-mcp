package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogs-mcp/internal/gogs"
	"gogs-mcp/internal/mcpserver"
	"gogs-mcp/internal/securelog"
	"gogs-mcp/internal/snapshot"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	aliceToken = "alice-personal-access-token"
	bobToken   = "bob-personal-access-token"
)

// fakeGogs answers /api/v1/user from the bearer token and counts the calls.
func fakeGogs(t *testing.T) (*url.URL, *int32) {
	t.Helper()
	var userRequests int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/user" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&userRequests, 1)
		switch request.Header.Get("Authorization") {
		case "token " + aliceToken:
			writeBody(t, writer, `{"id":1,"username":"alice","full_name":"Alice","email":"alice@example.test"}`)
		case "token " + bobToken:
			writeBody(t, writer, `{"id":2,"username":"bob","full_name":"Bob","email":"bob@example.test"}`)
		default:
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"message":"token is required"}`))
		}
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	return parsed, &userRequests
}

func writeBody(t *testing.T, writer http.ResponseWriter, body string) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	_, err := writer.Write([]byte(body))
	assert.NoError(t, err)
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

type fixture struct {
	server       *Server
	handler      http.Handler
	logs         *lockedBuffer
	userRequests *int32
}

func newFixture(t *testing.T, mutate func(*Options)) *fixture {
	t.Helper()
	baseURL, userRequests := fakeGogs(t)

	logs := &lockedBuffer{}
	snapshots, err := snapshot.NewManager(t.TempDir(), baseURL.String(), snapshot.DefaultLimits(), snapshot.DefaultEviction())
	require.NoError(t, err)

	options := Options{
		Addr:           "127.0.0.1:0",
		Endpoint:       "/mcp",
		TokenHeader:    "Authorization",
		UserCacheTTL:   time.Minute,
		MaxUsers:       8,
		BaseURL:        baseURL,
		UserAgent:      "gogs-mcp/test",
		HTTPTimeout:    5 * time.Second,
		CacheRoot:      t.TempDir(),
		CacheTTL:       time.Hour,
		CacheMaxBytes:  1 << 20,
		SearchDefaults: mcpserver.SearchDefaults{Timeout: 5 * time.Second, MaxFileBytes: 1 << 20},
		Snapshots:      snapshots,
		LogDestination: securelog.NewWriter(logs),
		LogLevel:       "debug",
	}
	if mutate != nil {
		mutate(&options)
	}
	server, err := New(options)
	require.NoError(t, err)
	return &fixture{
		server:       server,
		handler:      server.Handler(),
		logs:         logs,
		userRequests: userRequests,
	}
}

// tokenTransport injects one Gogs credential into every request.
type tokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t *tokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	if t.token != "" {
		cloned.Header.Set("Authorization", "token "+t.token)
	}
	return t.base.RoundTrip(cloned)
}

// connect opens an MCP session against the fixture with one token.
func (f *fixture) connect(t *testing.T, ctx context.Context, token string) *mcp.ClientSession {
	t.Helper()
	endpoint := httptest.NewServer(f.handler)
	t.Cleanup(endpoint.Close)

	transport := &mcp.StreamableClientTransport{
		Endpoint:             endpoint.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: &tokenTransport{token: token, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "httpserver-test", Version: "0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// callUserTool invokes get_authenticated_user and returns its data object.
func callUserTool(t *testing.T, ctx context.Context, session *mcp.ClientSession) map[string]any {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_authenticated_user", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError, "the tool call must succeed")
	content, ok := result.StructuredContent.(map[string]any)
	require.True(t, ok, "structured content must be an object")
	data, ok := content["data"].(map[string]any)
	require.True(t, ok, "structured content must carry data")
	return data
}

// postMCP sends one raw JSON-RPC request with the given credentials.
func postMCP(t *testing.T, handler http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		request.Header.Set("Authorization", "token "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

const userCallBody = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_authenticated_user","arguments":{}}}`

func TestExtractToken(t *testing.T) {
	testCases := map[string]struct {
		value    string
		token    string
		accepted bool
	}{
		"gogs scheme":       {value: "token abc", token: "abc", accepted: true},
		"bearer scheme":     {value: "Bearer abc", token: "abc", accepted: true},
		"scheme mixed case": {value: "TOKEN abc", token: "abc", accepted: true},
		"bare value":        {value: "abc", token: "abc", accepted: true},
		"empty":             {value: "", accepted: false},
		"scheme only":       {value: "Bearer ", accepted: false},
		"too long":          {value: strings.Repeat("a", 4097), accepted: false},
	}
	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			token, ok := extractToken(testCase.value)
			assert.Equal(t, testCase.accepted, ok)
			if testCase.accepted {
				assert.Equal(t, testCase.token, token)
			}
		})
	}
}

func TestMissingOrUnknownTokenIsUnauthorized(t *testing.T) {
	fixture := newFixture(t, nil)

	testCases := map[string]string{
		"no header":     "",
		"empty value":   " ",
		"scheme only":   "token ",
		"unknown token": "unknown-personal-access-token",
	}
	for name, token := range testCases {
		t.Run(name, func(t *testing.T) {
			recorder := postMCP(t, fixture.handler, token, `{}`)
			assert.Equal(t, http.StatusUnauthorized, recorder.Code)
			assert.Equal(t, `Bearer realm="gogs-mcp"`, recorder.Header().Get("WWW-Authenticate"))
			assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

			var payload headerError
			require.NoError(t, json.NewDecoder(recorder.Body).Decode(&payload))
			assert.Equal(t, "AUTHENTICATION_FAILED", payload.Error.Code)
			assert.NotContains(t, recorder.Body.String(), "unknown-personal-access-token")
		})
	}
}

func TestHandlerServesToolsPerUser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fixture := newFixture(t, nil)
	alice := fixture.connect(t, ctx, aliceToken)
	bob := fixture.connect(t, ctx, bobToken)

	aliceTools, err := alice.ListTools(ctx, nil)
	require.NoError(t, err)
	toolNames := map[string]bool{}
	for _, tool := range aliceTools.Tools {
		toolNames[tool.Name] = true
	}
	assert.True(t, toolNames["get_authenticated_user"])
	assert.True(t, toolNames["list_repositories"])

	assert.Equal(t, "alice", callUserTool(t, ctx, alice)["username"])
	assert.Equal(t, "bob", callUserTool(t, ctx, bob)["username"])

	// A second session with a known token reuses the cached user.
	aliceAgain := fixture.connect(t, ctx, aliceToken)
	assert.Equal(t, "alice", callUserTool(t, ctx, aliceAgain)["username"])
}

func TestUserCacheRootIsPrivate(t *testing.T) {
	fixture := newFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session := fixture.connect(t, ctx, aliceToken)
	callUserTool(t, ctx, session)

	entries, err := os.ReadDir(filepath.Join(fixture.server.options.CacheRoot, "users"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	info, err := entries[0].Info()
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
		"the per-user cache root must stay private to the server process")
}

func TestInvalidTokenProbesGogsOnce(t *testing.T) {
	fixture := newFixture(t, nil)

	for i := 0; i < 3; i++ {
		recorder := postMCP(t, fixture.handler, "rejected-personal-access-token", `{}`)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(fixture.userRequests), "the negative cache must stop repeated probes")
}

func TestKnownTokenProbesGogsOncePerResolution(t *testing.T) {
	fixture := newFixture(t, nil)

	for i := 0; i < 3; i++ {
		recorder := postMCP(t, fixture.handler, aliceToken, userCallBody)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	}
	// One probe resolves the user; every tool call then reads the user
	// directly. The requests served from the cache never probe again.
	assert.Equal(t, int32(4), atomic.LoadInt32(fixture.userRequests))
}

func TestResolveSingleFlightsConcurrentRequests(t *testing.T) {
	fixture := newFixture(t, nil)
	hash := tokenHash(aliceToken)

	entries := make([]*userEntry, 10)
	var group sync.WaitGroup
	for i := 0; i < len(entries); i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			entry, err := fixture.server.resolve(context.Background(), aliceToken, hash)
			assert.NoError(t, err)
			entries[i] = entry
		}()
	}
	group.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(fixture.userRequests),
		"concurrent resolutions of one token must probe Gogs exactly once")
	for _, entry := range entries {
		assert.Same(t, entries[0], entry)
	}
}

func TestHandlerRejectsGetAndDelete(t *testing.T) {
	fixture := newFixture(t, nil)

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		request := httptest.NewRequest(method, "/mcp", nil)
		request.Header.Set("Authorization", "token "+aliceToken)
		recorder := httptest.NewRecorder()
		fixture.handler.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusMethodNotAllowed, recorder.Code, method)
	}
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	fixture := newFixture(t, nil)

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusOK, recorder.Code)
}

func TestLogsNeverContainTokens(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fixture := newFixture(t, nil)
	session := fixture.connect(t, ctx, aliceToken)
	_ = callUserTool(t, ctx, session)

	assert.NotContains(t, fixture.logs.String(), aliceToken)
	assert.Contains(t, fixture.logs.String(), `"username":"alice"`)
}

func TestUserCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := newUserCache(2, time.Minute)

	cache.put(&userEntry{tokenHash: "first", user: gogsUser(1)})
	cache.put(&userEntry{tokenHash: "second", user: gogsUser(2)})
	_, ok := cache.get("first")
	require.True(t, ok, "first must still be cached")
	cache.put(&userEntry{tokenHash: "third", user: gogsUser(3)})

	_, ok = cache.get("second")
	assert.False(t, ok, "the least recently used entry must be evicted")
	_, ok = cache.get("first")
	assert.True(t, ok)
	assert.Equal(t, 2, cache.length())
}

func TestUserCacheExpiresEntries(t *testing.T) {
	cache := newUserCache(4, time.Minute)
	entry, _ := cache.put(&userEntry{tokenHash: "hash", user: gogsUser(5)})
	entry.expiresAt = time.Now().Add(-time.Nanosecond)

	_, ok := cache.get("hash")
	assert.False(t, ok)
	assert.Equal(t, 0, cache.length())
}

func TestUserCachePutKeepsExistingEntry(t *testing.T) {
	cache := newUserCache(4, time.Minute)
	first, evicted := cache.put(&userEntry{tokenHash: "hash", user: gogsUser(6)})
	duplicate, evictedAgain := cache.put(&userEntry{tokenHash: "hash", user: gogsUser(6)})
	assert.Same(t, first, duplicate)
	assert.Empty(t, evicted)
	assert.Empty(t, evictedAgain)
	assert.Equal(t, 1, cache.length())
}

func TestInvalidTokensExpireAndResetOnOverflow(t *testing.T) {
	negative := newInvalidTokens(2, time.Minute)
	negative.add("a")
	negative.add("b")
	assert.True(t, negative.has("a"))

	negative.add("c")
	assert.False(t, negative.has("a"), "overflow must reset the set")
	assert.True(t, negative.has("c"))

	expiring := newInvalidTokens(4, time.Millisecond)
	expiring.add("x")
	time.Sleep(2 * time.Millisecond)
	assert.False(t, expiring.has("x"))
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	options := Options{
		Addr:         "127.0.0.1:8080",
		BaseURL:      nil,
		CacheRoot:    "relative/path",
		UserCacheTTL: time.Minute,
		MaxUsers:     8,
	}
	_, err := New(options)
	require.Error(t, err)
}

func TestNewRejectsReservedEndpoint(t *testing.T) {
	options := Options{
		Addr:      "127.0.0.1:8080",
		BaseURL:   &url.URL{Scheme: "http", Host: "127.0.0.1:3000"},
		CacheRoot: t.TempDir(),
		Endpoint:  "/healthz",
	}
	_, err := New(options)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")
}

func TestRunFailsWithListenError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	occupied := listener.Addr().String()

	fixture := newFixture(t, func(options *Options) { options.Addr = occupied })
	err = fixture.server.Run(context.Background())
	require.Error(t, err)
	var listenError *ListenError
	require.ErrorAs(t, err, &listenError)
	assert.Equal(t, occupied, listenError.Addr)
}

func TestRunServesAndShutsDown(t *testing.T) {
	fixture := newFixture(t, nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	fixture.server.options.Addr = address

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fixture.server.Run(ctx) }()

	require.Eventually(t, func() bool {
		response, err := http.Get("http://" + address + "/healthz")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run must return after the context is canceled")
	}
}

func gogsUser(id int64) gogs.User {
	return gogs.User{ID: id, Username: fmt.Sprintf("user-%d", id)}
}

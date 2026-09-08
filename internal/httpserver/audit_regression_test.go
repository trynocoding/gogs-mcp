package httpserver

import (
	"context"
	"gogs-mcp/internal/gogs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserCacheHitsDoNotExtendAuthenticationLifetime(t *testing.T) {
	cache := newUserCache(4, time.Minute)
	entry, _ := cache.put(&userEntry{tokenHash: "token"})
	expires := entry.expiresAt
	for range 20 {
		_, ok := cache.get("token")
		require.True(t, ok)
		assert.Equal(t, expires, entry.expiresAt)
	}
	entry.expiresAt = time.Now().Add(-time.Nanosecond)
	_, ok := cache.get("token")
	assert.False(t, ok)
}

func TestDownstreamUnauthorizedInvalidatesAuthenticatedClient(t *testing.T) {
	var revoked atomic.Bool
	var probes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if revoked.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeBody(t, w, `{"id":1,"username":"alice"}`)
	}))
	defer backend.Close()
	base, err := url.Parse(backend.URL)
	require.NoError(t, err)
	f := newFixture(t, func(o *Options) { o.BaseURL = base })
	session := f.connect(t, context.Background(), aliceToken)
	callUserTool(t, context.Background(), session)
	_, ok := f.server.users.get(tokenHash(aliceToken))
	require.True(t, ok)
	revoked.Store(true)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_authenticated_user", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	_, ok = f.server.users.get(tokenHash(aliceToken))
	assert.False(t, ok)
	count := probes.Load()
	_, err = f.server.resolve(context.Background(), aliceToken, tokenHash(aliceToken))
	require.Error(t, err)
	assert.Equal(t, gogs.CodeAuthenticationFailed, gogs.AsError(err).Code)
	assert.Equal(t, count, probes.Load(), "a known revoked token must not trigger another Gogs request")
}

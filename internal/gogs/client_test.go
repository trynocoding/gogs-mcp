package gogs

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetAuthenticatedUser(t *testing.T) {
	const token = "test-personal-access-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/gogs/api/v1/user", request.URL.Path)
		assert.Equal(t, "token "+token, request.Header.Get("Authorization"))
		assert.Equal(t, "gogs-mcp/test", request.Header.Get("User-Agent"))
		_, err := fmt.Fprint(writer, `{"id":7,"login":"octocat","full_name":"Octo Cat","email":"octo@example.test"}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/gogs/api/v1/", token, "", time.Second)
	user, err := client.GetAuthenticatedUser(context.Background())
	require.NoError(t, err)
	assert.Equal(t, User{
		ID:       7,
		Username: "octocat",
		FullName: "Octo Cat",
		Email:    "octo@example.test",
	}, user)
}

func TestGetAuthenticatedUserClassifiesAuthenticationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "invalid-token", "", time.Second)
	_, err := client.GetAuthenticatedUser(context.Background())
	require.Error(t, err)
	assert.Equal(t, CodeAuthenticationFailed, AsError(err).Code)
	assert.False(t, AsError(err).Retryable)
	assert.NotContains(t, err.Error(), "invalid-token")
}

func TestGetAuthenticatedUserRetriesUnavailableStatus(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) < 3 {
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeResponse(t, writer, `{"id":11,"username":"retried-user"}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", 2*time.Second)
	user, err := client.GetAuthenticatedUser(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "retried-user", user.Username)
	assert.Equal(t, int32(3), requests.Load())
}

func TestGetAuthenticatedUserClassifiesUnavailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())

	client := newTestClient(t, "http://"+address+"/api/v1/", "secret-token", "", 2*time.Second)
	_, err = client.GetAuthenticatedUser(context.Background())
	require.Error(t, err)
	assert.Equal(t, CodeUnavailable, AsError(err).Code)
	assert.True(t, AsError(err).Retryable)
}

func TestGetAuthenticatedUserClassifiesTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		writeResponse(t, writer, "{}")
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", 20*time.Millisecond)
	_, err := client.GetAuthenticatedUser(context.Background())
	require.Error(t, err)
	assert.Equal(t, CodeTimeout, AsError(err).Code)
	assert.True(t, AsError(err).Retryable)
}

func TestGetAuthenticatedUserClassifiesTLSError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeResponse(t, writer, "{}")
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.GetAuthenticatedUser(context.Background())
	require.Error(t, err)
	assert.Equal(t, CodeTLSError, AsError(err).Code)
	assert.False(t, AsError(err).Retryable)
}

func TestCustomCAFile(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeResponse(t, writer, `{"id":9,"username":"trusted-user"}`)
	}))
	defer server.Close()

	certificate, err := x509.ParseCertificate(server.Certificate().Raw)
	require.NoError(t, err)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certificate.Raw,
	}), 0o600))

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", caPath, time.Second)
	user, err := client.GetAuthenticatedUser(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(9), user.ID)
	assert.Equal(t, "trusted-user", user.Username)
}

func TestRedirectDoesNotForwardAuthorizationAcrossOrigins(t *testing.T) {
	var redirectedAuthorization string
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		redirectedAuthorization = request.Header.Get("Authorization")
		writeResponse(t, writer, `{"id":10,"username":"redirected-user"}`)
	}))
	defer destination.Close()

	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "token secret-token", request.Header.Get("Authorization"))
		http.Redirect(writer, request, destination.URL+"/api/v1/user", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client := newTestClient(t, source.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.GetAuthenticatedUser(context.Background())
	require.NoError(t, err)
	assert.Empty(t, redirectedAuthorization)
}

func TestResponseSizeLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeResponse(t, writer, strings.Repeat("x", maxJSONResponseBytes+1))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	_, err := client.GetAuthenticatedUser(context.Background())
	require.Error(t, err)
	assert.Equal(t, CodeResponseTooLarge, AsError(err).Code)
}

func TestClientUsesEnvironmentProxyResolver(t *testing.T) {
	client := newTestClient(t, "https://gogs.example.test/api/v1/", "secret-token", "", time.Second)
	transport, ok := client.http.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.Proxy)
}

func TestClientWritesStructuredRequestLogWithoutCredentials(t *testing.T) {
	const token = "structured-log-secret"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL + "/api/v1/")
	require.NoError(t, err)
	var logs bytes.Buffer
	client, err := NewClient(Options{
		APIRoot:   parsed,
		Token:     token,
		Timeout:   time.Second,
		UserAgent: "gogs-mcp/test",
		Logger:    slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	require.NoError(t, err)

	ctx := WithRequestMetadata(context.Background(), "request-123", "get_authenticated_user")
	_, err = client.GetAuthenticatedUser(ctx)
	require.Error(t, err)
	assert.Contains(t, logs.String(), `"request_id":"request-123"`)
	assert.Contains(t, logs.String(), `"tool":"get_authenticated_user"`)
	assert.Contains(t, logs.String(), `"gogs_host":`)
	assert.Contains(t, logs.String(), `"http_status":401`)
	assert.Contains(t, logs.String(), `"duration_ms":`)
	assert.Contains(t, logs.String(), `"error_code":"AUTHENTICATION_FAILED"`)
	assert.NotContains(t, logs.String(), token)
	assert.NotContains(t, logs.String(), "Authorization")
}

func newTestClient(t *testing.T, apiRoot, token, caFile string, timeout time.Duration) *Client {
	t.Helper()
	parsed, err := url.Parse(apiRoot)
	require.NoError(t, err)
	client, err := NewClient(Options{
		APIRoot:   parsed,
		Token:     token,
		CAFile:    caFile,
		Timeout:   timeout,
		UserAgent: "gogs-mcp/test",
	})
	require.NoError(t, err)
	return client
}

func writeResponse(t *testing.T, writer http.ResponseWriter, body string) {
	t.Helper()
	_, err := fmt.Fprint(writer, body)
	assert.NoError(t, err)
}

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadFromEnvironment(t *testing.T) {
	cfg, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL":          "https://gogs.example.test/",
		"GOGS_TOKEN":             "secret-token",
		"GOGS_MCP_WRITE_ENABLED": "true",
		"GOGS_HTTP_TIMEOUT":      "12s",
		"GOGS_LOG_LEVEL":         "debug",
	}))
	require.NoError(t, err)

	assert.Equal(t, "secret-token", cfg.Token)
	assert.True(t, cfg.WriteEnabled)
	assert.Equal(t, 12*time.Second, cfg.HTTPTimeout)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "https://gogs.example.test/api/v1/", cfg.APIRoot().String())
}

func TestAPIRootPreservesSubpathAndAppendsOnce(t *testing.T) {
	testCases := map[string]string{
		"https://example.test/gogs":        "https://example.test/gogs/api/v1/",
		"https://example.test/gogs/":       "https://example.test/gogs/api/v1/",
		"https://example.test/gogs/api/v1": "https://example.test/gogs/api/v1/",
	}
	for baseURL, expected := range testCases {
		t.Run(baseURL, func(t *testing.T) {
			cfg, err := Load("", environment(map[string]string{
				"GOGS_BASE_URL": baseURL,
				"GOGS_TOKEN":    "secret-token",
			}))
			require.NoError(t, err)
			assert.Equal(t, expected, cfg.APIRoot().String())
		})
	}
}

func TestLoadRejectsConflictingTokenSources(t *testing.T) {
	_, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL":   "https://gogs.example.test",
		"GOGS_TOKEN":      "secret-token",
		"GOGS_TOKEN_FILE": "/private/token",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestLoadRejectsPresentEmptyConflictingTokenSource(t *testing.T) {
	_, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL":   "https://gogs.example.test",
		"GOGS_TOKEN":      "",
		"GOGS_TOKEN_FILE": "/private/token",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestLoadRejectsWideTokenFilePermissions(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("secret-token\n"), 0o644))

	_, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL":   "https://gogs.example.test",
		"GOGS_TOKEN_FILE": tokenPath,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0600")
}

func TestLoadReadsPrivateTokenFile(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("secret-token\n"), 0o600))

	cfg, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL":   "https://gogs.example.test",
		"GOGS_TOKEN_FILE": tokenPath,
	}))
	require.NoError(t, err)
	assert.Equal(t, "secret-token", cfg.Token)
}

func TestLoadRequiresExplicitPlainHTTP(t *testing.T) {
	values := map[string]string{
		"GOGS_BASE_URL": "http://gogs.example.test",
		"GOGS_TOKEN":    "secret-token",
	}
	_, err := Load("", environment(values))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GOGS_ALLOW_INSECURE_HTTP")

	values["GOGS_ALLOW_INSECURE_HTTP"] = "true"
	_, err = Load("", environment(values))
	require.NoError(t, err)
}

func TestEnvironmentTokenOverridesConfigTokenFile(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("file-token\n"), 0o600))
	configPath := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"base_url": "https://gogs.example.test",
		"token_file": "`+tokenPath+`"
	}`), 0o600))

	cfg, err := Load(configPath, environment(map[string]string{
		"GOGS_TOKEN": "environment-token",
	}))
	require.NoError(t, err)
	assert.Equal(t, "environment-token", cfg.Token)
	assert.Empty(t, cfg.TokenFile)
}

func TestLoadRejectsUnknownConfigField(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"base_url": "https://gogs.example.test",
		"token": "must-not-be-accepted"
	}`), 0o600))

	_, err := Load(configPath, environment(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}

func environment(values map[string]string) LookupEnv {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestSearchAndCacheLimitsDefaults(t *testing.T) {
	cfg, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL": "https://gogs.example.test/",
		"GOGS_TOKEN":    "secret-token",
	}))
	require.NoError(t, err)

	assert.Equal(t, int64(2<<30), cfg.CacheMaxBytes)
	assert.Equal(t, 24*time.Hour, cfg.CacheTTL)
	assert.Equal(t, 30*time.Second, cfg.SearchTimeout)
	assert.Equal(t, int64(1<<20), cfg.MaxFileBytes)
}

func TestSearchAndCacheLimitsOverrides(t *testing.T) {
	cfg, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL":            "https://gogs.example.test/",
		"GOGS_TOKEN":               "secret-token",
		"GOGS_MCP_CACHE_MAX_BYTES": "1073741824",
		"GOGS_MCP_CACHE_TTL":       "1h",
		"GOGS_MCP_SEARCH_TIMEOUT":  "90s",
		"GOGS_MCP_MAX_FILE_BYTES":  "262144",
	}))
	require.NoError(t, err)

	assert.Equal(t, int64(1<<30), cfg.CacheMaxBytes)
	assert.Equal(t, time.Hour, cfg.CacheTTL)
	assert.Equal(t, 90*time.Second, cfg.SearchTimeout)
	assert.Equal(t, int64(1<<18), cfg.MaxFileBytes)
}

func TestLoadRejectsInvalidSearchAndCacheLimits(t *testing.T) {
	cases := map[string]string{
		"GOGS_MCP_CACHE_MAX_BYTES": "0",
		"GOGS_MCP_CACHE_TTL":       "-5m",
		"GOGS_MCP_SEARCH_TIMEOUT":  "not-a-duration",
		"GOGS_MCP_MAX_FILE_BYTES":  "0",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load("", environment(map[string]string{
				"GOGS_BASE_URL": "https://gogs.example.test/",
				"GOGS_TOKEN":    "secret-token",
				name:            value,
			}))
			require.Error(t, err)
		})
	}
}

func TestLoadRejectsSearchTimeoutAbovePerRequestCap(t *testing.T) {
	_, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL":           "https://gogs.example.test/",
		"GOGS_TOKEN":              "secret-token",
		"GOGS_MCP_SEARCH_TIMEOUT": "10m",
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "must not exceed")
}

func TestHTTPTransportDefaults(t *testing.T) {
	cfg, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL":      "https://gogs.example.test/",
		"GOGS_MCP_TRANSPORT": "http",
		"GOGS_MCP_HTTP_ADDR": "127.0.0.1:8080",
	}))
	require.NoError(t, err)

	assert.Equal(t, TransportHTTP, cfg.Transport)
	assert.Equal(t, "127.0.0.1:8080", cfg.HTTPAddr)
	assert.Equal(t, "/mcp", cfg.HTTPEndpoint)
	assert.Equal(t, "Authorization", cfg.HTTPTokenHeader)
	assert.Equal(t, 5*time.Minute, cfg.HTTPUserCacheTTL)
	assert.Equal(t, 128, cfg.HTTPMaxUsers)
	assert.False(t, cfg.HTTPJSONResponse)
}

func TestHTTPTransportAllowsMissingToken(t *testing.T) {
	cfg, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL":      "https://gogs.example.test/",
		"GOGS_MCP_TRANSPORT": "http",
		"GOGS_MCP_HTTP_ADDR": "[::1]:9090",
	}))
	require.NoError(t, err)
	assert.Empty(t, cfg.Token)
	assert.Empty(t, cfg.TokenFile)
}

func TestStdioTransportStillRequiresToken(t *testing.T) {
	_, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL": "https://gogs.example.test/",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GOGS_TOKEN")

	cfg, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL": "https://gogs.example.test/",
		"GOGS_TOKEN":    "secret-token",
	}))
	require.NoError(t, err)
	assert.Equal(t, TransportStdio, cfg.Transport)
}

func TestLoadRejectsInvalidTransportSettings(t *testing.T) {
	testCases := []struct {
		name    string
		value   string
		invalid string
	}{
		{"missing_addr", "http", "GOGS_MCP_HTTP_ADDR"},
		{"bad_transport", "socket", "GOGS_MCP_TRANSPORT"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			values := map[string]string{
				"GOGS_BASE_URL": "https://gogs.example.test/",
			}
			if testCase.value == "http" {
				values["GOGS_MCP_TRANSPORT"] = "http"
			} else {
				values["GOGS_MCP_TRANSPORT"] = testCase.value
			}
			if testCase.name != "missing_addr" {
				values["GOGS_MCP_HTTP_ADDR"] = "127.0.0.1:8080"
			}
			_, err := Load("", environment(values))
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.invalid)
		})
	}
}

func TestLoadRejectsMalformedHTTPAddress(t *testing.T) {
	_, err := Load("", environment(map[string]string{
		"GOGS_BASE_URL":      "https://gogs.example.test/",
		"GOGS_MCP_TRANSPORT": "http",
		"GOGS_MCP_HTTP_ADDR": "127.0.0.1",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GOGS_MCP_HTTP_ADDR")
}

func TestLoadRejectsInvalidHTTPSettings(t *testing.T) {
	testCases := map[string]struct{ name, value string }{
		"endpoint_relative":      {"GOGS_MCP_HTTP_ENDPOINT", "mcp"},
		"endpoint_root":          {"GOGS_MCP_HTTP_ENDPOINT", "/"},
		"endpoint_traversal":     {"GOGS_MCP_HTTP_ENDPOINT", "/a/../mcp"},
		"header_invalid_chars":   {"GOGS_MCP_HTTP_TOKEN_HEADER", "X Gogs Token"},
		"header_reserved_host":   {"GOGS_MCP_HTTP_TOKEN_HEADER", "Host"},
		"header_reserved_case":   {"GOGS_MCP_HTTP_TOKEN_HEADER", "mcp-session-id"},
		"user_cache_ttl_zero":    {"GOGS_MCP_HTTP_USER_CACHE_TTL", "0s"},
		"user_cache_ttl_too_big": {"GOGS_MCP_HTTP_USER_CACHE_TTL", "2h"},
		"max_users_zero":         {"GOGS_MCP_HTTP_MAX_USERS", "0"},
		"max_users_too_big":      {"GOGS_MCP_HTTP_MAX_USERS", "2000"},
	}
	for name, setting := range testCases {
		t.Run(name, func(t *testing.T) {
			_, err := Load("", environment(map[string]string{
				"GOGS_BASE_URL": "https://gogs.example.test/",
				"GOGS_TOKEN":    "secret-token",
				setting.name:    setting.value,
			}))
			require.Error(t, err)
		})
	}
}

func TestHTTPTransportOverridesFromEnvironmentAndFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"base_url": "https://gogs.example.test",
		"transport": "http",
		"http_addr": "127.0.0.1:1",
		"http_endpoint": "/gogs-mcp",
		"http_token_header": "X-Gogs-Token",
		"http_user_cache_ttl": "10m",
		"http_max_users": 16,
		"http_json_response": true
	}`), 0o600))

	cfg, err := Load(configPath, environment(map[string]string{
		"GOGS_MCP_HTTP_ADDR": "0.0.0.0:2",
	}))
	require.NoError(t, err)

	assert.Equal(t, TransportHTTP, cfg.Transport)
	assert.Equal(t, "0.0.0.0:2", cfg.HTTPAddr)
	assert.Equal(t, "/gogs-mcp", cfg.HTTPEndpoint)
	assert.Equal(t, "X-Gogs-Token", cfg.HTTPTokenHeader)
	assert.Equal(t, 10*time.Minute, cfg.HTTPUserCacheTTL)
	assert.Equal(t, 16, cfg.HTTPMaxUsers)
	assert.True(t, cfg.HTTPJSONResponse)
}

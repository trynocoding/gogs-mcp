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

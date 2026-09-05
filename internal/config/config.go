package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

const (
	defaultHTTPTimeout   = 30 * time.Second
	defaultCacheMaxBytes = int64(2 << 30)
	defaultCacheTTL      = 24 * time.Hour
	defaultSearchTimeout = 30 * time.Second
	defaultMaxFileBytes  = int64(1 << 20)
	// maxSearchTimeout mirrors the per-request search cap so that a server
	// default can never exceed what one search may run for.
	maxSearchTimeout = 5 * time.Minute
	maxConfigBytes   = 1 << 20
	maxTokenBytes    = 8 << 10

	defaultHTTPEndpoint     = "/mcp"
	defaultHTTPTokenHeader  = "Authorization"
	defaultHTTPUserCacheTTL = 5 * time.Minute
	defaultHTTPMaxUsers     = 128
	maxHTTPUserCacheTTL     = time.Hour
	maxHTTPMaxUsers         = 1024
)

// Transport selects how the MCP server talks to its client.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportHTTP  Transport = "http"
)

type Config struct {
	BaseURL           *url.URL
	Token             string
	TokenFile         string
	CAFile            string
	AllowInsecureHTTP bool
	WriteEnabled      bool
	HTTPTimeout       time.Duration
	LogLevel          string
	// CacheDir overrides the snapshot cache root. It must be an absolute path.
	CacheDir string
	// CacheMaxBytes bounds the snapshot cache size per Gogs user.
	CacheMaxBytes int64
	// CacheTTL evicts snapshots untouched for this long.
	CacheTTL time.Duration
	// SearchTimeout bounds each code search.
	SearchTimeout time.Duration
	// MaxFileBytes skips files larger than this during code search.
	MaxFileBytes int64
	// Transport selects between the stdio and http MCP transports.
	Transport Transport
	// HTTPAddr is the listen address of the http transport, for example
	// "127.0.0.1:8080". It is required for the http transport.
	HTTPAddr string
	// HTTPEndpoint is the exact request path served by the http transport.
	HTTPEndpoint string
	// HTTPTokenHeader names the request header that carries the Gogs
	// personal access token of each caller.
	HTTPTokenHeader string
	// HTTPUserCacheTTL bounds how long an authenticated Gogs user stays
	// resolved between requests of the http transport.
	HTTPUserCacheTTL time.Duration
	// HTTPMaxUsers bounds how many Gogs users the http transport keeps
	// resolved at the same time.
	HTTPMaxUsers int
	// HTTPJSONResponse answers http transport requests with plain JSON
	// instead of an event stream.
	HTTPJSONResponse bool
}

type fileConfig struct {
	BaseURL           string `json:"base_url"`
	TokenFile         string `json:"token_file"`
	CAFile            string `json:"ca_file"`
	AllowInsecureHTTP *bool  `json:"allow_insecure_http"`
	WriteEnabled      *bool  `json:"write_enabled"`
	HTTPTimeout       string `json:"http_timeout"`
	CacheDir          string `json:"cache_dir"`
	CacheMaxBytes     int64  `json:"cache_max_bytes"`
	CacheTTL          string `json:"cache_ttl"`
	SearchTimeout     string `json:"search_timeout"`
	MaxFileBytes      int64  `json:"max_file_bytes"`
	LogLevel          string `json:"log_level"`
	Transport         string `json:"transport"`
	HTTPAddr          string `json:"http_addr"`
	HTTPEndpoint      string `json:"http_endpoint"`
	HTTPTokenHeader   string `json:"http_token_header"`
	HTTPUserCacheTTL  string `json:"http_user_cache_ttl"`
	HTTPMaxUsers      int    `json:"http_max_users"`
	HTTPJSONResponse  *bool  `json:"http_json_response"`
}

type LookupEnv func(string) (string, bool)

func Load(configPath string, lookup LookupEnv) (Config, error) {
	return load(configPath, lookup, true)
}

// LoadForMaintenance loads the configuration for cache maintenance commands,
// which may run without credentials when a user ID is given on the command
// line. Every other validation still applies.
func LoadForMaintenance(configPath string, lookup LookupEnv) (Config, error) {
	return load(configPath, lookup, false)
}

func load(configPath string, lookup LookupEnv, requireStdioToken bool) (Config, error) {
	cfg := Config{
		HTTPTimeout:      defaultHTTPTimeout,
		LogLevel:         "info",
		CacheMaxBytes:    defaultCacheMaxBytes,
		CacheTTL:         defaultCacheTTL,
		SearchTimeout:    defaultSearchTimeout,
		MaxFileBytes:     defaultMaxFileBytes,
		HTTPEndpoint:     defaultHTTPEndpoint,
		HTTPTokenHeader:  defaultHTTPTokenHeader,
		HTTPUserCacheTTL: defaultHTTPUserCacheTTL,
		HTTPMaxUsers:     defaultHTTPMaxUsers,
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}

	if configPath != "" {
		fileValues, err := readFile(configPath)
		if err != nil {
			return Config{}, err
		}
		if err := applyFile(&cfg, fileValues); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnvironment(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := validate(&cfg, requireStdioToken); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) APIRoot() *url.URL {
	apiRoot := *c.BaseURL
	cleaned := strings.TrimSuffix(path.Clean("/"+apiRoot.Path), "/")
	if !strings.HasSuffix(cleaned, "/api/v1") {
		cleaned += "/api/v1"
	}
	apiRoot.Path = cleaned + "/"
	apiRoot.RawPath = ""
	return &apiRoot
}

func readFile(configPath string) (fileConfig, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return fileConfig{}, errors.Wrap(err, "open config file")
	}

	limited := io.LimitReader(file, maxConfigBytes+1)
	contents, readErr := io.ReadAll(limited)
	closeErr := file.Close()
	if readErr != nil {
		return fileConfig{}, errors.Wrap(readErr, "read config file")
	}
	if closeErr != nil {
		return fileConfig{}, errors.Wrap(closeErr, "close config file")
	}
	if len(contents) > maxConfigBytes {
		return fileConfig{}, errors.New("config file exceeds 1 MiB")
	}

	var values fileConfig
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&values); err != nil {
		return fileConfig{}, errors.Wrap(err, "decode config file")
	}
	if err := ensureEOF(decoder); err != nil {
		return fileConfig{}, err
	}
	return values, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return errors.Wrap(err, "decode trailing config data")
	}
	return errors.New("config file contains multiple JSON values")
}

func applyFile(cfg *Config, values fileConfig) error {
	if values.BaseURL != "" {
		parsed, err := url.Parse(values.BaseURL)
		if err != nil {
			return errors.Wrap(err, "parse base_url")
		}
		cfg.BaseURL = parsed
	}
	cfg.TokenFile = values.TokenFile
	cfg.CAFile = values.CAFile
	if values.AllowInsecureHTTP != nil {
		cfg.AllowInsecureHTTP = *values.AllowInsecureHTTP
	}
	if values.WriteEnabled != nil {
		cfg.WriteEnabled = *values.WriteEnabled
	}
	if values.HTTPTimeout != "" {
		timeout, err := time.ParseDuration(values.HTTPTimeout)
		if err != nil {
			return errors.Wrap(err, "parse http_timeout")
		}
		cfg.HTTPTimeout = timeout
	}
	if values.LogLevel != "" {
		cfg.LogLevel = values.LogLevel
	}
	if values.CacheDir != "" {
		cfg.CacheDir = values.CacheDir
	}
	if values.CacheMaxBytes != 0 {
		cfg.CacheMaxBytes = values.CacheMaxBytes
	}
	if values.CacheTTL != "" {
		ttl, err := time.ParseDuration(values.CacheTTL)
		if err != nil {
			return errors.Wrap(err, "parse cache_ttl")
		}
		cfg.CacheTTL = ttl
	}
	if values.SearchTimeout != "" {
		timeout, err := time.ParseDuration(values.SearchTimeout)
		if err != nil {
			return errors.Wrap(err, "parse search_timeout")
		}
		cfg.SearchTimeout = timeout
	}
	if values.MaxFileBytes != 0 {
		cfg.MaxFileBytes = values.MaxFileBytes
	}
	if values.Transport != "" {
		cfg.Transport = Transport(values.Transport)
	}
	if values.HTTPAddr != "" {
		cfg.HTTPAddr = values.HTTPAddr
	}
	if values.HTTPEndpoint != "" {
		cfg.HTTPEndpoint = values.HTTPEndpoint
	}
	if values.HTTPTokenHeader != "" {
		cfg.HTTPTokenHeader = values.HTTPTokenHeader
	}
	if values.HTTPUserCacheTTL != "" {
		ttl, err := time.ParseDuration(values.HTTPUserCacheTTL)
		if err != nil {
			return errors.Wrap(err, "parse http_user_cache_ttl")
		}
		cfg.HTTPUserCacheTTL = ttl
	}
	if values.HTTPMaxUsers != 0 {
		cfg.HTTPMaxUsers = values.HTTPMaxUsers
	}
	if values.HTTPJSONResponse != nil {
		cfg.HTTPJSONResponse = *values.HTTPJSONResponse
	}
	return nil
}

func applyEnvironment(cfg *Config, lookup LookupEnv) error {
	if value, ok := lookup("GOGS_BASE_URL"); ok {
		parsed, err := url.Parse(value)
		if err != nil {
			return errors.Wrap(err, "parse GOGS_BASE_URL")
		}
		cfg.BaseURL = parsed
	}
	token, hasToken := lookup("GOGS_TOKEN")
	tokenFile, hasTokenFile := lookup("GOGS_TOKEN_FILE")
	if hasToken && hasTokenFile {
		return errors.New("GOGS_TOKEN and GOGS_TOKEN_FILE are mutually exclusive")
	}
	if hasToken {
		cfg.Token = token
		if !hasTokenFile {
			cfg.TokenFile = ""
		}
	}
	if hasTokenFile {
		cfg.TokenFile = tokenFile
		if !hasToken {
			cfg.Token = ""
		}
	}
	if value, ok := lookup("GOGS_CA_FILE"); ok {
		cfg.CAFile = value
	}
	if err := applyBoolEnv(&cfg.AllowInsecureHTTP, "GOGS_ALLOW_INSECURE_HTTP", lookup); err != nil {
		return err
	}
	if err := applyBoolEnv(&cfg.WriteEnabled, "GOGS_MCP_WRITE_ENABLED", lookup); err != nil {
		return err
	}
	if value, ok := lookup("GOGS_HTTP_TIMEOUT"); ok {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return errors.Wrap(err, "parse GOGS_HTTP_TIMEOUT")
		}
		cfg.HTTPTimeout = timeout
	}
	if value, ok := lookup("GOGS_LOG_LEVEL"); ok {
		cfg.LogLevel = value
	}
	if value, ok := lookup("GOGS_MCP_CACHE_DIR"); ok {
		cfg.CacheDir = value
	}
	if value, ok := lookup("GOGS_MCP_CACHE_MAX_BYTES"); ok {
		bytes, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return errors.Wrap(err, "parse GOGS_MCP_CACHE_MAX_BYTES")
		}
		cfg.CacheMaxBytes = bytes
	}
	if value, ok := lookup("GOGS_MCP_CACHE_TTL"); ok {
		ttl, err := time.ParseDuration(value)
		if err != nil {
			return errors.Wrap(err, "parse GOGS_MCP_CACHE_TTL")
		}
		cfg.CacheTTL = ttl
	}
	if value, ok := lookup("GOGS_MCP_SEARCH_TIMEOUT"); ok {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return errors.Wrap(err, "parse GOGS_MCP_SEARCH_TIMEOUT")
		}
		cfg.SearchTimeout = timeout
	}
	if value, ok := lookup("GOGS_MCP_MAX_FILE_BYTES"); ok {
		bytes, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return errors.Wrap(err, "parse GOGS_MCP_MAX_FILE_BYTES")
		}
		cfg.MaxFileBytes = bytes
	}
	if value, ok := lookup("GOGS_MCP_TRANSPORT"); ok {
		cfg.Transport = Transport(value)
	}
	if value, ok := lookup("GOGS_MCP_HTTP_ADDR"); ok {
		cfg.HTTPAddr = value
	}
	if value, ok := lookup("GOGS_MCP_HTTP_ENDPOINT"); ok {
		cfg.HTTPEndpoint = value
	}
	if value, ok := lookup("GOGS_MCP_HTTP_TOKEN_HEADER"); ok {
		cfg.HTTPTokenHeader = value
	}
	if value, ok := lookup("GOGS_MCP_HTTP_USER_CACHE_TTL"); ok {
		ttl, err := time.ParseDuration(value)
		if err != nil {
			return errors.Wrap(err, "parse GOGS_MCP_HTTP_USER_CACHE_TTL")
		}
		cfg.HTTPUserCacheTTL = ttl
	}
	if value, ok := lookup("GOGS_MCP_HTTP_MAX_USERS"); ok {
		users, err := strconv.Atoi(value)
		if err != nil {
			return errors.Wrap(err, "parse GOGS_MCP_HTTP_MAX_USERS")
		}
		cfg.HTTPMaxUsers = users
	}
	if err := applyBoolEnv(&cfg.HTTPJSONResponse, "GOGS_MCP_HTTP_JSON_RESPONSE", lookup); err != nil {
		return err
	}
	return nil
}

func applyBoolEnv(destination *bool, name string, lookup LookupEnv) error {
	value, ok := lookup(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return errors.Wrapf(err, "parse %s", name)
	}
	*destination = parsed
	return nil
}

func validate(cfg *Config, requireStdioToken bool) error {
	if cfg.BaseURL == nil {
		return errors.New("GOGS_BASE_URL is required")
	}
	if cfg.BaseURL.Scheme != "https" && cfg.BaseURL.Scheme != "http" {
		return errors.New("GOGS_BASE_URL must use http or https")
	}
	if cfg.BaseURL.Host == "" {
		return errors.New("GOGS_BASE_URL must include a host")
	}
	if cfg.BaseURL.User != nil || cfg.BaseURL.RawQuery != "" || cfg.BaseURL.Fragment != "" {
		return errors.New("GOGS_BASE_URL must not include credentials, query parameters, or a fragment")
	}
	if cfg.BaseURL.Scheme == "http" && !cfg.AllowInsecureHTTP {
		return errors.New("plain HTTP requires GOGS_ALLOW_INSECURE_HTTP=true")
	}
	switch cfg.Transport {
	case "", TransportStdio:
		cfg.Transport = TransportStdio
	case TransportHTTP:
		if cfg.HTTPAddr == "" {
			return errors.New("GOGS_MCP_HTTP_ADDR is required for the http transport")
		}
		if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
			return errors.Wrap(err, "parse GOGS_MCP_HTTP_ADDR")
		}
	default:
		return errors.Newf("GOGS_MCP_TRANSPORT %q is invalid; use stdio or http", string(cfg.Transport))
	}
	if cfg.Token != "" && cfg.TokenFile != "" {
		return errors.New("GOGS_TOKEN and GOGS_TOKEN_FILE are mutually exclusive")
	}
	if cfg.Token == "" && cfg.TokenFile == "" {
		if cfg.Transport == TransportStdio && requireStdioToken {
			return errors.New("exactly one of GOGS_TOKEN or GOGS_TOKEN_FILE is required")
		}
		// The http transport authenticates every request with a header
		// credential instead; a configured token is still allowed here so
		// that verify and cache clean can run against the same deployment.
	}
	if cfg.TokenFile != "" {
		token, err := readTokenFile(cfg.TokenFile)
		if err != nil {
			return err
		}
		cfg.Token = token
	}
	if cfg.Transport == TransportStdio && requireStdioToken && strings.TrimSpace(cfg.Token) == "" {
		return errors.New("Gogs token must not be empty")
	}
	if strings.ContainsAny(cfg.Token, "\r\n") {
		return errors.New("Gogs token must be a single line")
	}
	if cfg.HTTPTimeout <= 0 {
		return errors.New("GOGS_HTTP_TIMEOUT must be greater than zero")
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("GOGS_LOG_LEVEL %q is invalid", cfg.LogLevel)
	}
	if cfg.CAFile != "" {
		info, err := os.Stat(cfg.CAFile)
		if err != nil {
			return errors.Wrap(err, "inspect CA file")
		}
		if !info.Mode().IsRegular() {
			return errors.New("GOGS_CA_FILE must be a regular file")
		}
	}
	if cfg.CacheDir != "" && !filepath.IsAbs(cfg.CacheDir) {
		return errors.New("GOGS_MCP_CACHE_DIR must be an absolute path")
	}
	if cfg.CacheMaxBytes <= 0 {
		return errors.New("GOGS_MCP_CACHE_MAX_BYTES must be greater than zero")
	}
	if cfg.CacheTTL <= 0 {
		return errors.New("GOGS_MCP_CACHE_TTL must be greater than zero")
	}
	if cfg.SearchTimeout <= 0 {
		return errors.New("GOGS_MCP_SEARCH_TIMEOUT must be greater than zero")
	}
	if cfg.SearchTimeout > maxSearchTimeout {
		return errors.Newf("GOGS_MCP_SEARCH_TIMEOUT must not exceed %s", maxSearchTimeout)
	}
	if cfg.MaxFileBytes <= 0 {
		return errors.New("GOGS_MCP_MAX_FILE_BYTES must be greater than zero")
	}
	if err := validateHTTPSettings(cfg); err != nil {
		return err
	}
	return nil
}

// validateHTTPSettings applies the http transport rules to whichever values
// are configured. The rules run for every transport so that a mistyped http
// setting never hides behind an unused transport.
func validateHTTPSettings(cfg *Config) error {
	if !strings.HasPrefix(cfg.HTTPEndpoint, "/") || cfg.HTTPEndpoint == "/" || path.Clean(cfg.HTTPEndpoint) != cfg.HTTPEndpoint {
		return errors.Newf("GOGS_MCP_HTTP_ENDPOINT %q must be one absolute request path", cfg.HTTPEndpoint)
	}
	// The http handler registers /healthz as an unauthenticated probe before
	// the configured endpoint, and a duplicate ServeMux registration panics;
	// the reserved path is therefore rejected with a readable error here.
	if cfg.HTTPEndpoint == "/healthz" {
		return errors.Newf("GOGS_MCP_HTTP_ENDPOINT %q is reserved for the unauthenticated health probe", cfg.HTTPEndpoint)
	}
	if !isValidHeaderName(cfg.HTTPTokenHeader) || reservedHeader(cfg.HTTPTokenHeader) {
		return errors.Newf("GOGS_MCP_HTTP_TOKEN_HEADER %q is not usable as a credential header", cfg.HTTPTokenHeader)
	}
	if cfg.HTTPUserCacheTTL <= 0 || cfg.HTTPUserCacheTTL > maxHTTPUserCacheTTL {
		return errors.Newf("GOGS_MCP_HTTP_USER_CACHE_TTL must be between just above zero and %s", maxHTTPUserCacheTTL)
	}
	if cfg.HTTPMaxUsers <= 0 || cfg.HTTPMaxUsers > maxHTTPMaxUsers {
		return errors.Newf("GOGS_MCP_HTTP_MAX_USERS must be between 1 and %d", maxHTTPMaxUsers)
	}
	return nil
}

// isValidHeaderName reports whether the name is a valid RFC 7230 header
// field name, so the credential header survives every HTTP client and proxy.
func isValidHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", character):
		default:
			return false
		}
	}
	return true
}

// reservedHeader lists headers whose meaning is owned by HTTP or the MCP
// streamable transport, which a credential header must never shadow.
func reservedHeader(name string) bool {
	reserved := []string{
		"Host", "Content-Length", "Content-Type", "Transfer-Encoding",
		"Accept", "Cookie", "Expect", "Connection",
		"Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id",
	}
	for _, candidate := range reserved {
		if strings.EqualFold(name, candidate) {
			return true
		}
	}
	return false
}

func readTokenFile(tokenPath string) (string, error) {
	info, err := os.Lstat(tokenPath)
	if err != nil {
		return "", errors.Wrap(err, "inspect token file")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("GOGS_TOKEN_FILE must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("GOGS_TOKEN_FILE permissions must not be wider than 0600")
	}
	if info.Size() > maxTokenBytes {
		return "", errors.New("GOGS_TOKEN_FILE exceeds 8 KiB")
	}
	contents, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", errors.Wrap(err, "read token file")
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(contents), "\n"), "\r"), nil
}

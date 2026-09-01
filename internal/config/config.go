package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

const (
	defaultHTTPTimeout = 30 * time.Second
	maxConfigBytes     = 1 << 20
	maxTokenBytes      = 8 << 10
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
}

type fileConfig struct {
	BaseURL           string `json:"base_url"`
	TokenFile         string `json:"token_file"`
	CAFile            string `json:"ca_file"`
	AllowInsecureHTTP *bool  `json:"allow_insecure_http"`
	WriteEnabled      *bool  `json:"write_enabled"`
	HTTPTimeout       string `json:"http_timeout"`
	LogLevel          string `json:"log_level"`
}

type LookupEnv func(string) (string, bool)

func Load(configPath string, lookup LookupEnv) (Config, error) {
	cfg := Config{
		HTTPTimeout: defaultHTTPTimeout,
		LogLevel:    "info",
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
	if err := validate(&cfg); err != nil {
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

func validate(cfg *Config) error {
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
	if cfg.Token != "" && cfg.TokenFile != "" {
		return errors.New("GOGS_TOKEN and GOGS_TOKEN_FILE are mutually exclusive")
	}
	if cfg.Token == "" && cfg.TokenFile == "" {
		return errors.New("exactly one of GOGS_TOKEN or GOGS_TOKEN_FILE is required")
	}
	if cfg.TokenFile != "" {
		token, err := readTokenFile(cfg.TokenFile)
		if err != nil {
			return err
		}
		cfg.Token = token
	}
	if strings.TrimSpace(cfg.Token) == "" {
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
	return nil
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

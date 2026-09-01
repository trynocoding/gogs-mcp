package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"gogs-mcp/internal/config"
	"gogs-mcp/internal/gogs"
	"gogs-mcp/internal/mcpserver"
	"gogs-mcp/internal/securelog"
	"gogs-mcp/internal/snapshot"
	"gogs-mcp/internal/version"

	"github.com/cockroachdb/errors"
)

const (
	exitOK         = 0
	exitInternal   = 1
	exitConfig     = 2
	exitConnection = 3
)

type diagnostic struct {
	OK      bool                         `json:"ok"`
	BaseURL string                       `json:"base_url,omitempty"`
	User    *mcpserver.AuthenticatedUser `json:"user,omitempty"`
	Error   *diagnosticError             `json:"error,omitempty"`
}

type diagnosticError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error {
	return nil
}

func Run(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	lookup config.LookupEnv,
) int {
	if len(args) == 0 {
		writeUsage(stderr)
		return exitConfig
	}

	switch args[0] {
	case "serve":
		return runServe(ctx, args[1:], stdin, stdout, stderr, lookup)
	case "verify":
		return runVerify(ctx, args[1:], stdout, stderr, lookup)
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		writeUsage(stdout)
		return exitOK
	default:
		writeText(stderr, "Unknown command %q.\n", args[0])
		writeUsage(stderr)
		return exitConfig
	}
}

func runServe(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	lookup config.LookupEnv,
) int {
	configPath, ok := parseConfigFlag("serve", args, stderr)
	if !ok {
		return exitConfig
	}
	cfg, err := config.Load(configPath, lookup)
	if err != nil {
		writeText(stderr, "Configuration error: %s.\n", err)
		return exitConfig
	}

	logger := securelog.New(stderr, cfg.LogLevel, cfg.Token, "token "+cfg.Token)
	client, err := gogs.NewClient(gogs.Options{
		APIRoot:   cfg.APIRoot(),
		Token:     cfg.Token,
		CAFile:    cfg.CAFile,
		Timeout:   cfg.HTTPTimeout,
		UserAgent: "gogs-mcp/" + version.Version,
		Logger:    logger,
	})
	if err != nil {
		logger.Error("Could not initialize the Gogs client.", "error", err)
		return exitInternal
	}

	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			logger.Error("Could not determine the snapshot cache directory.", "error", err)
			return exitConfig
		}
		cacheDir = filepath.Join(userCache, "gogs-mcp")
	}
	snapshots, err := snapshot.NewManager(cacheDir, cfg.BaseURL.String(), snapshot.DefaultLimits())
	if err != nil {
		logger.Error("Could not initialize the snapshot cache.", "error", err)
		return exitConfig
	}

	server := mcpserver.New(client, snapshots, logger)
	if err := server.Run(ctx, io.NopCloser(stdin), nopWriteCloser{stdout}); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("The MCP server stopped unexpectedly.", "error", err)
		return exitInternal
	}
	return exitOK
}

func runVerify(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	lookup config.LookupEnv,
) int {
	configPath, ok := parseConfigFlag("verify", args, stderr)
	if !ok {
		return exitConfig
	}
	cfg, err := config.Load(configPath, lookup)
	if err != nil {
		writeJSON(stdout, diagnostic{
			OK: false,
			Error: &diagnosticError{
				Code:    "CONFIG_INVALID",
				Message: "The Gogs MCP configuration is invalid.",
			},
		})
		return exitConfig
	}

	client, err := gogs.NewClient(gogs.Options{
		APIRoot:   cfg.APIRoot(),
		Token:     cfg.Token,
		CAFile:    cfg.CAFile,
		Timeout:   cfg.HTTPTimeout,
		UserAgent: "gogs-mcp/" + version.Version,
	})
	if err != nil {
		writeJSON(stdout, diagnostic{
			OK: false,
			Error: &diagnosticError{
				Code:    "CONFIG_INVALID",
				Message: "The HTTP client configuration is invalid.",
			},
		})
		return exitConfig
	}

	user, err := client.GetAuthenticatedUser(ctx)
	if err != nil {
		classified := gogs.AsError(err)
		writeJSON(stdout, diagnostic{
			OK:      false,
			BaseURL: cfg.BaseURL.Redacted(),
			Error: &diagnosticError{
				Code:      string(classified.Code),
				Message:   classified.Message,
				Retryable: classified.Retryable,
			},
		})
		return exitConnection
	}

	writeJSON(stdout, diagnostic{
		OK:      true,
		BaseURL: cfg.BaseURL.Redacted(),
		User: &mcpserver.AuthenticatedUser{
			ID:       user.ID,
			Username: user.Username,
			FullName: user.FullName,
			Email:    user.Email,
		},
	})
	return exitOK
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "Print machine-readable JSON.")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return exitConfig
	}

	info := version.Info{
		Version:   version.Version,
		Commit:    version.Commit,
		BuildTime: version.BuildTime,
		GoVersion: runtime.Version(),
	}
	if *asJSON {
		writeJSON(stdout, info)
		return exitOK
	}
	writeText(stdout, "gogs-mcp %s (%s, %s, %s).\n", info.Version, info.Commit, info.BuildTime, info.GoVersion)
	return exitOK
}

func parseConfigFlag(command string, args []string, stderr io.Writer) (string, bool) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "Read non-sensitive configuration from this JSON file.")
	if err := flags.Parse(args); err != nil {
		return "", false
	}
	if flags.NArg() != 0 {
		writeText(stderr, "%s does not accept positional arguments.\n", command)
		return "", false
	}
	return *configPath, true
}

func writeJSON(writer io.Writer, value any) {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return
	}
}

func writeUsage(writer io.Writer) {
	writeText(writer, "Usage: gogs-mcp <serve|verify|version> [options].\n")
}

func writeText(writer io.Writer, format string, arguments ...any) {
	if _, err := fmt.Fprintf(writer, format, arguments...); err != nil {
		return
	}
}

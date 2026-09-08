// Package httpserver serves the MCP tools over Streamable HTTP. Every
// request carries the caller's Gogs personal access token in a header, and
// the server resolves that token to an isolated per-user tool server with
// its own Gogs client and cache directories.
package httpserver

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gogs-mcp/internal/gogs"
	"gogs-mcp/internal/mcpserver"
	"gogs-mcp/internal/securelog"
	"gogs-mcp/internal/snapshot"

	"github.com/cockroachdb/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultServerEndpoint     = "/mcp"
	defaultServerTokenHeader  = "Authorization"
	healthzEndpoint           = "/healthz"
	defaultUserCacheTTL       = 5 * time.Minute
	defaultMaxUsers           = 128
	minUserCacheTTL           = time.Second
	maxUserCacheTTL           = time.Hour
	minMaxUsers               = 1
	maxMaxUsers               = 1024
	resolveConcurrency        = 8
	invalidTokenCacheCapacity = 4096
)

// Options configures the HTTP transport. BaseURL is the Gogs instance every
// caller talks to; CacheRoot hosts one isolated cache subtree per user.
type Options struct {
	Addr         string
	Endpoint     string
	TokenHeader  string
	UserCacheTTL time.Duration
	MaxUsers     int
	JSONResponse bool

	BaseURL       *url.URL
	CAFile        string
	UserAgent     string
	HTTPTimeout   time.Duration
	CacheRoot     string
	CacheTTL      time.Duration
	CacheMaxBytes int64

	SearchDefaults mcpserver.SearchDefaults
	WriteEnabled   bool
	Snapshots      *snapshot.Manager
	LogDestination *securelog.Writer
	LogLevel       string
}

// Server is the Streamable HTTP transport of the MCP tools.
type Server struct {
	options    Options
	apiRoot    *url.URL
	schemas    *mcp.SchemaCache
	users      *userCache
	invalid    *invalidTokens
	builds     map[string]*buildCall
	buildsMu   sync.Mutex
	slots      chan struct{}
	baseLogger *slog.Logger
}

// ListenError reports that the configured listen address is unusable. It is
// a configuration problem, not a runtime failure.
type ListenError struct {
	Addr string
	Err  error
}

func (e *ListenError) Error() string {
	return "listen " + e.Addr + ": " + e.Err.Error()
}

func (e *ListenError) Unwrap() error {
	return e.Err
}

// New validates the options without listening. Call Run or Handler next.
func New(options Options) (*Server, error) {
	if options.Addr == "" {
		return nil, errors.New("the HTTP listen address is required")
	}
	if _, _, err := net.SplitHostPort(options.Addr); err != nil {
		return nil, errors.Wrap(err, "parse the HTTP listen address")
	}
	if options.Endpoint == "" {
		options.Endpoint = defaultServerEndpoint
	}
	// config.Load rejects the same value, but New owns the mux registration
	// and can be built without going through the config, so the reservation
	// is repeated here: a duplicate /healthz registration would only surface
	// as a ServeMux panic on the first request.
	if options.Endpoint == healthzEndpoint {
		return nil, errors.Newf("the endpoint %q is reserved for the unauthenticated health probe", options.Endpoint)
	}
	if options.TokenHeader == "" {
		options.TokenHeader = defaultServerTokenHeader
	}
	if options.UserCacheTTL == 0 {
		options.UserCacheTTL = defaultUserCacheTTL
	}
	if options.MaxUsers == 0 {
		options.MaxUsers = defaultMaxUsers
	}
	if options.UserCacheTTL < minUserCacheTTL || options.UserCacheTTL > maxUserCacheTTL {
		return nil, errors.Newf("the user cache TTL must be between %s and %s", minUserCacheTTL, maxUserCacheTTL)
	}
	if options.MaxUsers < minMaxUsers || options.MaxUsers > maxMaxUsers {
		return nil, errors.Newf("the user cache capacity must be between %d and %d", minMaxUsers, maxMaxUsers)
	}
	if options.BaseURL == nil {
		return nil, errors.New("the Gogs base URL is required")
	}
	if !filepath.IsAbs(options.CacheRoot) {
		return nil, errors.New("the cache root must be an absolute path")
	}
	if options.Snapshots == nil {
		return nil, errors.New("the snapshot manager is required")
	}
	if options.LogDestination == nil {
		return nil, errors.New("the log destination is required")
	}

	apiRoot := *options.BaseURL
	apiRoot.RawQuery = ""
	apiRoot.Fragment = ""
	// Mirror config.APIRoot: the client expects the API root path, ending in
	// /api/v1, while the options carry the plain Gogs base URL.
	cleaned := strings.TrimSuffix(path.Clean("/"+apiRoot.Path), "/")
	if !strings.HasSuffix(cleaned, "/api/v1") {
		cleaned += "/api/v1"
	}
	apiRoot.Path = cleaned + "/"
	apiRoot.RawPath = ""

	return &Server{
		options:    options,
		apiRoot:    &apiRoot,
		schemas:    mcp.NewSchemaCache(),
		users:      newUserCache(options.MaxUsers, options.UserCacheTTL),
		invalid:    newInvalidTokens(invalidTokenCacheCapacity, options.UserCacheTTL),
		builds:     make(map[string]*buildCall),
		slots:      make(chan struct{}, resolveConcurrency),
		baseLogger: securelog.NewLogger(options.LogDestination, options.LogLevel),
	}, nil
}

// Handler returns the root HTTP handler: an unauthenticated health probe and
// the authenticated MCP endpoint.
func (s *Server) Handler() http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(s.streamableServer, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: s.options.JSONResponse,
		Logger:       s.baseLogger,
	})
	mux := http.NewServeMux()
	mux.HandleFunc(healthzEndpoint, s.handleHealthz)
	mux.Handle(s.options.Endpoint, s.withAccessLog(s.withAuthentication(streamable)))
	return mux
}

// Run listens on the configured address until the context is canceled.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.options.Addr)
	if err != nil {
		return &ListenError{Addr: s.options.Addr, Err: err}
	}
	defer func() { _ = listener.Close() }()

	if address, ok := listener.Addr().(*net.TCPAddr); ok && !address.IP.IsLoopback() {
		s.baseLogger.Warn("The HTTP transport serves plaintext traffic; terminate TLS with a reverse proxy.")
	}
	s.baseLogger.Info("The HTTP transport is listening.",
		"address", listener.Addr().String(),
		"endpoint", s.options.Endpoint,
	)

	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return errors.Wrap(err, "shut down the HTTP transport")
		}
		return nil
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.Wrap(err, "serve the HTTP transport")
	}
}

func (s *Server) handleHealthz(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("ok\n"))
}

// streamableServer hands the SDK the tool server of the authenticated user.
// The authentication middleware guarantees an identity for every request
// that reaches the SDK, so returning nil is unreachable defense.
func (s *Server) streamableServer(request *http.Request) *mcp.Server {
	entry, ok := identityFrom(request.Context())
	if !ok {
		return nil
	}
	return entry.server.MCP()
}

// newClient builds a Gogs client for one token. The probe client that only
// validates the token has no cache directory and no per-user logger yet.
func (s *Server) newClient(token, cacheDir string, logger *slog.Logger) (*gogs.Client, error) {
	return gogs.NewClient(gogs.Options{
		APIRoot:       s.apiRoot,
		Token:         token,
		CAFile:        s.options.CAFile,
		Timeout:       s.options.HTTPTimeout,
		UserAgent:     s.options.UserAgent,
		CacheDir:      cacheDir,
		Snapshots:     s.options.Snapshots,
		CacheTTL:      s.options.CacheTTL,
		CacheMaxBytes: s.options.CacheMaxBytes,
		Logger:        logger,
	})
}

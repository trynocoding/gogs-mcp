package gogs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/cockroachdb/errors"
)

const maxJSONResponseBytes = 8 << 20

type Options struct {
	APIRoot   *url.URL
	Token     string
	CAFile    string
	Timeout   time.Duration
	UserAgent string
	Logger    *slog.Logger
}

type Client struct {
	apiRoot   *url.URL
	token     string
	userAgent string
	http      *http.Client
	logger    *slog.Logger
}

type requestMetadata struct {
	requestID string
	tool      string
}

type requestMetadataKey struct{}

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
}

type userResponse struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Login    string `json:"login"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
}

func NewClient(options Options) (*Client, error) {
	if options.APIRoot == nil {
		return nil, errors.New("API root is required")
	}

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport has an unexpected type")
	}
	transport = transport.Clone()
	transport.Proxy = http.ProxyFromEnvironment

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if transport.TLSClientConfig != nil {
		tlsConfig = transport.TLSClientConfig.Clone()
		if tlsConfig.MinVersion == 0 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	if options.CAFile != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, errors.Wrap(err, "load system CA pool")
		}
		pem, err := os.ReadFile(options.CAFile)
		if err != nil {
			return nil, errors.Wrap(err, "read custom CA file")
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("GOGS_CA_FILE contains no valid certificates")
		}
		tlsConfig.RootCAs = roots
	}
	transport.TLSClientConfig = tlsConfig

	client := &http.Client{
		Transport: transport,
		Timeout:   options.Timeout,
	}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 0 && !sameOrigin(request.URL, via[0].URL) {
			request.Header.Del("Authorization")
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Client{
		apiRoot:   cloneURL(options.APIRoot),
		token:     options.Token,
		userAgent: options.UserAgent,
		http:      client,
		logger:    logger,
	}, nil
}

func WithRequestMetadata(ctx context.Context, requestID, tool string) context.Context {
	return context.WithValue(ctx, requestMetadataKey{}, requestMetadata{
		requestID: requestID,
		tool:      tool,
	})
}

func (c *Client) GetAuthenticatedUser(ctx context.Context) (User, error) {
	var response userResponse
	if err := c.getJSON(ctx, &response, "user"); err != nil {
		return User{}, err
	}
	username := response.Username
	if username == "" {
		username = response.Login
	}
	return User{
		ID:       response.ID,
		Username: username,
		FullName: response.FullName,
		Email:    response.Email,
	}, nil
}

func (c *Client) getJSON(ctx context.Context, destination any, pathSegments ...string) error {
	return c.getJSONWithQuery(ctx, destination, nil, pathSegments...)
}

func (c *Client) getJSONWithQuery(ctx context.Context, destination any, query url.Values, pathSegments ...string) error {
	requestURL := c.apiRoot.JoinPath(escapedPathSegments(pathSegments)...)
	requestURL.RawQuery = query.Encode()

	var lastError error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := waitForRetry(ctx, attempt); err != nil {
				return classifyTransportError(err)
			}
		}
		started := time.Now()
		status, body, err := c.getOnce(ctx, requestURL)
		if err != nil {
			var gogsError *Error
			if errors.As(err, &gogsError) {
				c.logRequest(ctx, status, time.Since(started), gogsError.Code)
				return gogsError
			}
			classified := classifyTransportError(err)
			c.logRequest(ctx, status, time.Since(started), classified.Code)
			if classified.Code == CodeTLSError || classified.Code == CodeTimeout || attempt == 2 {
				return classified
			}
			lastError = classified
			continue
		}
		if status < 200 || status >= 300 {
			classified := classifyStatus(status)
			c.logRequest(ctx, status, time.Since(started), classified.Code)
			if (status == 502 || status == 503 || status == 504) && attempt < 2 {
				lastError = classified
				continue
			}
			return classified
		}
		if err := json.Unmarshal(body, destination); err != nil {
			c.logRequest(ctx, status, time.Since(started), CodeGogsError)
			return &Error{
				Code:    CodeGogsError,
				Message: "Gogs returned invalid JSON.",
				cause:   err,
			}
		}
		c.logRequest(ctx, status, time.Since(started), "")
		return nil
	}
	return lastError
}

func (c *Client) logRequest(ctx context.Context, status int, duration time.Duration, code ErrorCode) {
	metadata, _ := ctx.Value(requestMetadataKey{}).(requestMetadata)
	c.logger.InfoContext(ctx, "Gogs request completed.",
		"request_id", metadata.requestID,
		"tool", metadata.tool,
		"gogs_host", c.apiRoot.Host,
		"http_status", status,
		"duration_ms", duration.Milliseconds(),
		"error_code", string(code),
	)
}

func (c *Client) getOnce(ctx context.Context, requestURL *url.URL) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return 0, nil, errors.Wrap(err, "create Gogs request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "token "+c.token)
	request.Header.Set("User-Agent", c.userAgent)

	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, err
	}

	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxJSONResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return 0, nil, errors.Wrap(readErr, "read Gogs response")
	}
	if closeErr != nil {
		return 0, nil, errors.Wrap(closeErr, "close Gogs response")
	}
	if len(body) > maxJSONResponseBytes {
		return 0, nil, &Error{
			Code:    CodeResponseTooLarge,
			Message: "The Gogs response exceeded the 8 MiB limit.",
		}
	}
	return response.StatusCode, body, nil
}

func waitForRetry(ctx context.Context, attempt int) error {
	delays := [...]time.Duration{100 * time.Millisecond, 300 * time.Millisecond}
	baseDelay := delays[attempt-1]
	jitterRange := int64(baseDelay / 5)
	jitter := time.Duration(rand.Int64N(2*jitterRange+1) - jitterRange)
	timer := time.NewTimer(baseDelay + jitter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func sameOrigin(left, right *url.URL) bool {
	return left.Scheme == right.Scheme && left.Host == right.Host
}

func cloneURL(value *url.URL) *url.URL {
	cloned := *value
	return &cloned
}

package gogs

import (
	"bytes"
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
	_, err := c.getJSONWithHeaders(ctx, destination, query, pathSegments...)
	return err
}

// getJSONWithHeaders behaves like getJSONWithQuery and additionally returns
// the response headers of the successful request, for example to follow
// pagination Link headers.
func (c *Client) getJSONWithHeaders(ctx context.Context, destination any, query url.Values, pathSegments ...string) (http.Header, error) {
	requestURL := c.apiRoot.JoinPath(escapedPathSegments(pathSegments)...)
	requestURL.RawQuery = query.Encode()

	var lastError error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := waitForRetry(ctx, attempt); err != nil {
				return nil, classifyTransportError(err)
			}
		}
		started := time.Now()
		status, header, body, err := c.getOnce(ctx, requestURL)
		if err != nil {
			var gogsError *Error
			if errors.As(err, &gogsError) {
				c.logRequest(ctx, status, time.Since(started), gogsError.Code)
				return nil, gogsError
			}
			classified := classifyTransportError(err)
			c.logRequest(ctx, status, time.Since(started), classified.Code)
			if classified.Code == CodeTLSError || classified.Code == CodeTimeout || attempt == 2 {
				return nil, classified
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
			return nil, classified
		}
		if err := json.Unmarshal(body, destination); err != nil {
			c.logRequest(ctx, status, time.Since(started), CodeGogsError)
			return nil, &Error{
				Code:    CodeGogsError,
				Message: "Gogs returned invalid JSON.",
				cause:   err,
			}
		}
		c.logRequest(ctx, status, time.Since(started), "")
		return header, nil
	}
	return nil, lastError
}

// postJSON sends one POST request with a JSON payload and decodes the JSON
// response. Unlike the GET helpers it never retries: a write that may have
// reached Gogs must not be repeated, so every failure is classified as
// either a definitive rejection (nothing was written) or
// WRITE_OUTCOME_UNKNOWN (the result must be queried before retrying).
func (c *Client) postJSON(ctx context.Context, destination any, payload any, pathSegments ...string) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return errors.Wrap(err, "encode Gogs request payload")
	}
	requestURL := c.apiRoot.JoinPath(escapedPathSegments(pathSegments)...)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(payloadBytes))
	if err != nil {
		return errors.Wrap(err, "create Gogs request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "token "+c.token)
	request.Header.Set("User-Agent", c.userAgent)

	started := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		classified := classifyWriteTransportError(err)
		c.logRequest(ctx, 0, time.Since(started), classified.Code)
		return classified
	}

	status := response.StatusCode
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maxJSONResponseBytes+1))
	closeErr := response.Body.Close()
	if status >= 400 && status < 500 {
		// A client error is a definitive rejection: nothing was written,
		// even when the response body cannot be decoded.
		classified := classifyStatus(status)
		c.logRequest(ctx, status, time.Since(started), classified.Code)
		return classified
	}
	var cause error
	if readErr != nil {
		cause = readErr
	} else if closeErr != nil {
		cause = closeErr
	}
	if cause != nil || len(responseBody) > maxJSONResponseBytes || status < 200 || status >= 300 {
		classified := &Error{
			Code:    CodeWriteOutcomeUnknown,
			Message: "The write was sent to Gogs but its outcome is unknown; check the result before retrying.",
			cause:   cause,
		}
		c.logRequest(ctx, status, time.Since(started), classified.Code)
		return classified
	}
	if err := json.Unmarshal(responseBody, destination); err != nil {
		classified := &Error{
			Code:    CodeWriteOutcomeUnknown,
			Message: "The write was accepted by Gogs but the response was lost; check the result before retrying.",
			cause:   err,
		}
		c.logRequest(ctx, status, time.Since(started), classified.Code)
		return classified
	}
	c.logRequest(ctx, status, time.Since(started), "")
	return nil
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

func (c *Client) getOnce(ctx context.Context, requestURL *url.URL) (int, http.Header, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return 0, nil, nil, errors.Wrap(err, "create Gogs request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "token "+c.token)
	request.Header.Set("User-Agent", c.userAgent)

	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, nil, err
	}

	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxJSONResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return 0, nil, nil, errors.Wrap(readErr, "read Gogs response")
	}
	if closeErr != nil {
		return 0, nil, nil, errors.Wrap(closeErr, "close Gogs response")
	}
	if len(body) > maxJSONResponseBytes {
		return 0, nil, nil, &Error{
			Code:    CodeResponseTooLarge,
			Message: "The Gogs response exceeded the 8 MiB limit.",
		}
	}
	return response.StatusCode, response.Header, body, nil
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

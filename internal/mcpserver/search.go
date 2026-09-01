package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"gogs-mcp/internal/gogs"
	"gogs-mcp/internal/snapshot"

	"github.com/cockroachdb/errors"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maximumSearchQueryLength = 1000

// SearchDefaults carries the server-configured bounds for search_code.
type SearchDefaults struct {
	Timeout      time.Duration
	MaxFileBytes int64
}

// DefaultSearchDefaults matches the documented search limits.
func DefaultSearchDefaults() SearchDefaults {
	return SearchDefaults{
		Timeout:      snapshot.DefaultSearchTimeout,
		MaxFileBytes: snapshot.DefaultMaxFileBytes,
	}
}

type searchCodeInput struct {
	Owner          string   `json:"owner"`
	Repo           string   `json:"repo"`
	Query          string   `json:"query"`
	Ref            string   `json:"ref,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	CaseSensitive  *bool    `json:"case_sensitive,omitempty"`
	ContextLines   *int     `json:"context_lines,omitempty"`
	MaxResults     int      `json:"max_results,omitempty"`
	Include        []string `json:"include,omitempty"`
	Exclude        []string `json:"exclude,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// searchRequest is a fully resolved search: every optional input has been
// defaulted and validated, so the snapshot layer only re-checks invariants.
type searchRequest struct {
	options snapshot.Options
	mode    string
	timeout time.Duration
}

// resolveSearchRequest validates the tool input before any snapshot work
// starts, so that an invalid query or glob never triggers a download.
func resolveSearchRequest(input searchCodeInput, defaults SearchDefaults) (searchRequest, error) {
	mode := input.Mode
	if mode == "" {
		mode = snapshot.ModeLiteral
	}
	caseSensitive := true
	if input.CaseSensitive != nil {
		caseSensitive = *input.CaseSensitive
	}
	matcher, err := snapshot.Compile(mode, input.Query, caseSensitive)
	if err != nil {
		return searchRequest{}, err
	}

	request := searchRequest{
		mode: mode,
		options: snapshot.Options{
			Matcher:      matcher,
			ContextLines: snapshot.DefaultContextLines,
			MaxResults:   snapshot.DefaultMaxResults,
			MaxFileBytes: defaults.MaxFileBytes,
			Include:      input.Include,
			Exclude:      input.Exclude,
		},
		timeout: defaults.Timeout,
	}
	if input.ContextLines != nil {
		request.options.ContextLines = *input.ContextLines
	}
	if input.MaxResults != 0 {
		request.options.MaxResults = input.MaxResults
	}
	if input.TimeoutSeconds != 0 {
		// The integer is validated before the duration conversion so that a
		// huge value cannot wrap into a small positive duration.
		if input.TimeoutSeconds < 0 || input.TimeoutSeconds > int(snapshot.MaxSearchTimeout/time.Second) {
			return searchRequest{}, errors.Newf("timeout_seconds must be between 1 and %d", int(snapshot.MaxSearchTimeout/time.Second))
		}
		request.timeout = time.Duration(input.TimeoutSeconds) * time.Second
	}
	if request.options.ContextLines < 0 || request.options.ContextLines > snapshot.MaxContextLines {
		return searchRequest{}, errors.Newf("context_lines must be between 0 and %d", snapshot.MaxContextLines)
	}
	if request.options.MaxResults < 1 || request.options.MaxResults > snapshot.MaxResultsLimit {
		return searchRequest{}, errors.Newf("max_results must be between 1 and %d", snapshot.MaxResultsLimit)
	}
	if err := snapshot.ValidatePathPatterns(request.options.Include, request.options.Exclude); err != nil {
		return searchRequest{}, err
	}
	return request, nil
}

// identityCache memoizes the authenticated user ID for the process lifetime.
// Failures are not cached so that a transient Gogs error is retried by the
// next search instead of poisoning the cache key.
type identityCache struct {
	mu     sync.Mutex
	userID int64
	loaded bool
}

func (c *identityCache) userIDFor(ctx context.Context, client Client) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return c.userID, nil
	}
	user, err := client.GetAuthenticatedUser(ctx)
	if err != nil {
		return 0, err
	}
	c.userID = user.ID
	c.loaded = true
	return c.userID, nil
}

func registerSearchTools(server *mcp.Server, client Client, snapshots *snapshot.Manager, identities *identityCache, search SearchDefaults) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_code",
		Description: "Search repository file content in an immutable snapshot of a branch, tag, or commit. The query is a case-sensitive literal string by default, or a regular expression with mode=regex; binary, non-UTF-8, and oversized files are skipped and reported in warnings.",
		Annotations: readOnlyAnnotations("Search code"),
		InputSchema: searchCodeInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input searchCodeInput) (*mcp.CallToolResult, ToolResponse[SearchPage], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "search_code")

		if snapshots == nil {
			result, response := searchCodeError(requestID, &gogs.Error{
				Code:    gogs.CodeGogsError,
				Message: "Code search is not available because the snapshot cache is not configured.",
			})
			return result, response, nil
		}

		request, err := resolveSearchRequest(input, search)
		if err != nil {
			result, response := searchCodeError(requestID, &gogs.Error{Code: gogs.CodeInvalidArgument, Message: err.Error()})
			return result, response, nil
		}

		userID, err := identities.userIDFor(ctx, client)
		if err != nil {
			result, response := searchCodeError(requestID, err)
			return result, response, nil
		}

		commitSHA, err := client.ResolveCommitSHA(ctx, input.Owner, input.Repo, input.Ref)
		if err != nil {
			result, response := searchCodeError(requestID, err)
			return result, response, nil
		}

		result, err := snapshots.Ensure(ctx, snapshot.Key{
			UserID:    userID,
			Owner:     input.Owner,
			Repo:      input.Repo,
			CommitSHA: commitSHA,
		}, func(downloadContext context.Context) (io.ReadCloser, error) {
			return client.DownloadArchive(downloadContext, input.Owner, input.Repo, commitSHA)
		})
		if err != nil {
			result, response := searchCodeError(requestID, mapSnapshotError(err))
			return result, response, nil
		}
		defer result.Release()

		// The timeout scopes only the search itself; the download and
		// extraction keep their own HTTP and size bounds.
		searchContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), request.timeout)
		defer cancel()
		found, stats, err := snapshot.Search(searchContext, result.Dir, request.options)
		if err != nil {
			result, response := searchCodeError(requestID, mapSnapshotError(err))
			return result, response, nil
		}
		if stats.TimedOut && len(found) == 0 {
			result, response := searchCodeError(requestID, &gogs.Error{
				Code:      gogs.CodeSearchTimeout,
				Message:   "The search timed out before finding any match.",
				Retryable: true,
			})
			return result, response, nil
		}

		matches, dropped := searchMatchesWithin(found, maximumFileTextBytes)
		meta := ResponseMeta{RequestID: requestID, CacheHit: result.CacheHit}
		if stats.TimedOut {
			meta.Truncated = true
			meta.Warnings = append(meta.Warnings, "The search timed out; results are partial.")
		}
		if stats.Truncated {
			meta.Truncated = true
			meta.Warnings = append(meta.Warnings, fmt.Sprintf("The search stopped at the max_results limit of %d matches.", request.options.MaxResults))
		}
		if stats.SkippedBinary > 0 {
			meta.Warnings = append(meta.Warnings, fmt.Sprintf("Skipped %d binary or non-UTF-8 files.", stats.SkippedBinary))
		}
		if stats.SkippedOversized > 0 {
			meta.Warnings = append(meta.Warnings, skippedOversizedWarning(stats.SkippedOversized, request.options.MaxFileBytes))
		}
		if dropped {
			meta.Truncated = true
			meta.Warnings = append(meta.Warnings, "Search results reached the 64 KiB structured output limit.")
		}
		page := SearchPage{
			Query:     input.Query,
			Ref:       input.Ref,
			CommitSHA: commitSHA,
			Mode:      request.mode,
			Matches:   matches,
		}
		return nil, ToolResponse[SearchPage]{Data: &page, Meta: meta}, nil
	})
}

func searchCodeError(requestID string, err error) (*mcp.CallToolResult, ToolResponse[SearchPage]) {
	classified := gogs.AsError(err)
	return &mcp.CallToolResult{IsError: true}, ToolResponse[SearchPage]{
		Error: mapToolError(classified),
		Meta:  ResponseMeta{RequestID: requestID},
	}
}

// mapSnapshotError translates local snapshot and search failures into the
// shared error taxonomy. Download failures arrive already classified as Gogs
// errors and pass through unchanged.
func mapSnapshotError(err error) error {
	switch {
	case errors.Is(err, snapshot.ErrArchiveTooLarge):
		return &gogs.Error{Code: gogs.CodeResponseTooLarge, Message: "The compressed archive exceeds the 128 MiB limit."}
	case errors.Is(err, snapshot.ErrSnapshotTooLarge):
		return &gogs.Error{Code: gogs.CodeResponseTooLarge, Message: "The decompressed archive exceeds the 512 MiB limit."}
	case errors.Is(err, snapshot.ErrTooManyEntries):
		return &gogs.Error{Code: gogs.CodeResponseTooLarge, Message: "The archive contains more than 20000 entries."}
	case errors.Is(err, snapshot.ErrUnsafeArchiveEntry):
		return &gogs.Error{Code: gogs.CodeArchiveUnsafe, Message: "The archive contains entries that cannot be extracted safely."}
	case errors.Is(err, snapshot.ErrCacheCapacityExceeded):
		return &gogs.Error{Code: gogs.CodeCacheCapacityExceeded, Message: "The snapshot cache is full and cannot make room for a new snapshot.", Retryable: true}
	case errors.Is(err, snapshot.ErrInvalidOptions):
		return &gogs.Error{Code: gogs.CodeInvalidArgument, Message: err.Error()}
	}
	return err
}

// humanBytes renders a byte limit for warnings.
func humanBytes(value int64) string {
	const mib = 1 << 20
	if value >= mib && value%mib == 0 {
		return fmt.Sprintf("%d MiB", value/mib)
	}
	return fmt.Sprintf("%d bytes", value)
}

func skippedOversizedWarning(count int, limit int64) string {
	if count == 1 {
		return fmt.Sprintf("Skipped 1 file larger than %s.", humanBytes(limit))
	}
	return fmt.Sprintf("Skipped %d files larger than %s.", count, humanBytes(limit))
}

// searchMatchesWithin maps snapshot matches onto the tool output shape until
// the encoded matches reach the shared structured output budget.
func searchMatchesWithin(found []snapshot.Match, maximum int) ([]SearchMatch, bool) {
	matches := make([]SearchMatch, 0, min(len(found), 64))
	encoded := 0
	dropped := false
	for _, match := range found {
		mapped := mapSearchMatch(match)
		payload, err := json.Marshal(mapped)
		if err != nil {
			dropped = true
			break
		}
		separator := 0
		if len(matches) > 0 {
			separator = 1
		}
		if encoded+separator+len(payload) > maximum {
			dropped = true
			break
		}
		encoded += separator + len(payload)
		matches = append(matches, mapped)
	}
	return matches, dropped
}

func mapSearchMatch(match snapshot.Match) SearchMatch {
	context := make([]SearchContextLine, 0, len(match.Context))
	for _, line := range match.Context {
		context = append(context, SearchContextLine{Line: line.Number, Text: line.Text})
	}
	return SearchMatch{
		Path:     match.Path,
		Line:     match.Line,
		Column:   match.Column,
		LineText: match.LineText,
		Context:  context,
	}
}

func searchCodeInputSchema() *jsonschema.Schema {
	properties := repositoryReferenceProperties()
	properties["query"] = &jsonschema.Schema{
		Type:        "string",
		Description: "Text to search for: a literal string by default, or a regular expression when mode is regex.",
		MinLength:   intPointer(1),
		MaxLength:   intPointer(maximumSearchQueryLength),
	}
	properties["mode"] = &jsonschema.Schema{
		Type:        "string",
		Description: "How the query is interpreted: literal text or a regular expression. Defaults to literal.",
		Enum:        []any{snapshot.ModeLiteral, snapshot.ModeRegex},
		Default:     json.RawMessage(`"literal"`),
	}
	properties["case_sensitive"] = &jsonschema.Schema{
		Type:        "boolean",
		Description: "Whether the query matches case exactly. Defaults to true.",
		Default:     json.RawMessage("true"),
	}
	properties["context_lines"] = &jsonschema.Schema{
		Type:        "integer",
		Description: fmt.Sprintf("Context lines shown before and after each match, from 0 through %d. Defaults to %d.", snapshot.MaxContextLines, snapshot.DefaultContextLines),
		Default:     json.RawMessage(strconv.Itoa(snapshot.DefaultContextLines)),
		Minimum:     floatPointer(0),
		Maximum:     floatPointer(snapshot.MaxContextLines),
	}
	properties["max_results"] = &jsonschema.Schema{
		Type:        "integer",
		Description: fmt.Sprintf("Maximum number of matches to return, from 1 through %d. Defaults to %d.", snapshot.MaxResultsLimit, snapshot.DefaultMaxResults),
		Default:     json.RawMessage(strconv.Itoa(snapshot.DefaultMaxResults)),
		Minimum:     floatPointer(1),
		Maximum:     floatPointer(snapshot.MaxResultsLimit),
	}
	maximumTimeoutSeconds := float64(snapshot.MaxSearchTimeout / time.Second)
	properties["timeout_seconds"] = &jsonschema.Schema{
		Type:        "integer",
		Description: fmt.Sprintf("Search time limit in seconds, from 1 through %d. Defaults to the configured server limit.", int(maximumTimeoutSeconds)),
		Minimum:     floatPointer(1),
		Maximum:     &maximumTimeoutSeconds,
	}
	properties["include"] = &jsonschema.Schema{
		Type:        "array",
		Description: "Only search paths matching at least one of these glob patterns, for example src/*.go or *.md. Patterns use / separators and must stay inside the repository.",
		Items:       &jsonschema.Schema{Type: "string", MinLength: intPointer(1)},
	}
	properties["exclude"] = &jsonschema.Schema{
		Type:        "array",
		Description: "Skip paths matching any of these glob patterns, for example vendor/*.",
		Items:       &jsonschema.Schema{Type: "string", MinLength: intPointer(1)},
	}
	return objectSchema(properties, []string{"owner", "repo", "query"})
}

func floatPointer(value float64) *float64 {
	return &value
}

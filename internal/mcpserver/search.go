package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"gogs-mcp/internal/gogs"
	"gogs-mcp/internal/snapshot"

	"github.com/cockroachdb/errors"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maximumSearchQueryLength = 1000

type searchCodeInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Query string `json:"query"`
	Ref   string `json:"ref,omitempty"`
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

func registerSearchTools(server *mcp.Server, client Client, snapshots *snapshot.Manager, identities *identityCache) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_code",
		Description: "Search repository file content with a case-sensitive literal query inside an immutable snapshot of a branch, tag, or commit.",
		Annotations: readOnlyAnnotations("Search code"),
		InputSchema: searchCodeInputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input searchCodeInput) (*mcp.CallToolResult, ToolResponse[SearchPage], error) {
		requestID := newRequestID()
		ctx = gogs.WithRequestMetadata(ctx, requestID, "search_code")

		if snapshots == nil {
			return searchCodeError(requestID, &gogs.Error{
				Code:    gogs.CodeGogsError,
				Message: "Code search is not available because the snapshot cache is not configured.",
			})
		}

		userID, err := identities.userIDFor(ctx, client)
		if err != nil {
			return searchCodeError(requestID, err)
		}

		commitSHA, err := client.ResolveCommitSHA(ctx, input.Owner, input.Repo, input.Ref)
		if err != nil {
			return searchCodeError(requestID, err)
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
			return searchCodeError(requestID, mapSnapshotError(err))
		}

		found, err := snapshot.Search(ctx, result.Dir, input.Query)
		if err != nil {
			return searchCodeError(requestID, err)
		}

		matches, dropped := searchMatchesWithin(found, maximumFileTextBytes)
		meta := ResponseMeta{RequestID: requestID, CacheHit: result.CacheHit}
		if dropped {
			meta.Truncated = true
			meta.Warnings = append(meta.Warnings, "Search results reached the 64 KiB structured output limit.")
		}
		page := SearchPage{
			Query:     input.Query,
			Ref:       input.Ref,
			CommitSHA: commitSHA,
			Mode:      "literal",
			Matches:   matches,
		}
		return nil, ToolResponse[SearchPage]{Data: &page, Meta: meta}, nil
	})
}

func searchCodeError(requestID string, err error) (*mcp.CallToolResult, ToolResponse[SearchPage], error) {
	classified := gogs.AsError(err)
	return &mcp.CallToolResult{IsError: true}, ToolResponse[SearchPage]{
		Error: mapToolError(classified),
		Meta:  ResponseMeta{RequestID: requestID},
	}, nil
}

// mapSnapshotError translates local snapshot failures into the shared error
// taxonomy. Download failures arrive already classified as Gogs errors and
// pass through unchanged.
func mapSnapshotError(err error) error {
	switch {
	case errors.Is(err, snapshot.ErrArchiveTooLarge):
		return &gogs.Error{Code: gogs.CodeResponseTooLarge, Message: "The compressed archive exceeds the 128 MiB limit."}
	case errors.Is(err, snapshot.ErrSnapshotTooLarge):
		return &gogs.Error{Code: gogs.CodeResponseTooLarge, Message: "The decompressed archive exceeds the 512 MiB limit."}
	case errors.Is(err, snapshot.ErrTooManyEntries):
		return &gogs.Error{Code: gogs.CodeResponseTooLarge, Message: "The archive contains more than 20000 entries."}
	case errors.Is(err, snapshot.ErrUnsafeArchiveEntry):
		return &gogs.Error{Code: gogs.CodeGogsError, Message: "The archive contains entries that cannot be extracted safely."}
	}
	return err
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
		Description: "Case-sensitive literal text to search for.",
		MinLength:   intPointer(1),
		MaxLength:   intPointer(maximumSearchQueryLength),
	}
	return objectSchema(properties, []string{"owner", "repo", "query"})
}

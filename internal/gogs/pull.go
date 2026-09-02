package gogs

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
)

// Pull request listing limits shared with the tool layer.
const (
	defaultPullLimit = 30
	maximumPullLimit = 100
	// defaultDiffMaxBytes bounds the rendered diff of get_pull_request_diff.
	defaultDiffMaxBytes = 256 * 1024
	// maximumDiffPaths bounds the path filter of get_pull_request_diff.
	maximumDiffPaths = 100
)

// PullRequestState values accepted by the pull request listing.
const (
	PullStateOpen   = "open"
	PullStateClosed = "closed"
	PullStateAll    = "all"
)

// PullRequestSummary is the compact pull request representation used by
// listings; the potentially large body is intentionally left out.
type PullRequestSummary struct {
	Number      int64
	Title       string
	State       string
	User        User
	NumComments int
	CreatedAt   string
	UpdatedAt   string
	HeadSHA     string
}

// PullRequest is the full pull request representation exposed to tools.
// Gogs v0.14.2 has no pull request API, so the metadata comes from the
// underlying issue and the head ref. The base branch cannot be recovered
// from the server; BaseRef carries the repository default branch with
// BaseRefAssumed set to true.
type PullRequest struct {
	Number         int64
	Title          string
	Body           string
	State          string
	User           User
	Labels         []IssueLabel
	NumComments    int
	CreatedAt      string
	UpdatedAt      string
	WebURL         string
	HeadSHA        string
	BaseRef        string
	BaseRefAssumed bool
}

// PullRequestDiff is the merge-base diff of one pull request. The merge
// state describes the whole pull request even when paths filtered the diff.
type PullRequestDiff struct {
	Number             int64
	BaseRef            string
	BaseRefAssumed     bool
	MergeBase          string
	Diff               string
	Files              []DiffFileStat
	Commits            []PullCommit
	Truncated          bool
	MergeState         string
	MergeConflictPaths []string
}

// ListPullRequests returns the most recent pull requests of a repository in
// the given state, newest first, together with the total number of pull
// request heads the repository currently advertises. Every listed entry
// costs one issue lookup, so the result is bounded by limit rather than
// paged: Gogs v0.14.2 has no server-side pull request listing.
func (c *Client) ListPullRequests(ctx context.Context, owner, repo, state string, limit int) ([]PullRequestSummary, int, error) {
	switch state {
	case PullStateOpen, PullStateClosed, PullStateAll:
	default:
		return nil, 0, &Error{
			Code:    CodeInvalidArgument,
			Message: "The state must be one of open, closed, or all.",
		}
	}
	if limit <= 0 {
		limit = defaultPullLimit
	}
	limit = min(limit, maximumPullLimit)

	refs, err := c.listPullRefs(ctx, owner, repo)
	if err != nil {
		return nil, 0, err
	}

	summaries := make([]PullRequestSummary, 0, limit)
	// refs arrive in ascending order; walk them backwards so the newest
	// pull requests come first.
	for index := len(refs) - 1; index >= 0 && len(summaries) < limit; index-- {
		ref := refs[index]
		issue, err := c.GetIssue(ctx, owner, repo, ref.Number)
		var notFound *Error
		if errors.As(err, &notFound) && notFound.Code == CodeResourceNotFoundOrForbidden {
			// The ref outlived its issue or is not visible to the token.
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if state != PullStateAll && issue.State != state {
			continue
		}
		summaries = append(summaries, PullRequestSummary{
			Number:      issue.Number,
			Title:       issue.Title,
			State:       issue.State,
			User:        issue.User,
			NumComments: issue.NumComments,
			CreatedAt:   issue.CreatedAt,
			UpdatedAt:   issue.UpdatedAt,
			HeadSHA:     ref.HeadSHA,
		})
	}
	return summaries, len(refs), nil
}

// GetPullRequest returns one pull request by number. A missing or
// inaccessible issue reports the shared not-found code; an issue without a
// pull request head ref is rejected as an argument error.
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int64) (PullRequest, error) {
	ref, err := c.pullHeadRef(ctx, owner, repo, number)
	if err != nil {
		return PullRequest{}, err
	}
	issue, err := c.GetIssue(ctx, owner, repo, number)
	if err != nil {
		return PullRequest{}, err
	}
	repository, err := c.GetRepository(ctx, owner, repo)
	if err != nil {
		return PullRequest{}, err
	}
	return PullRequest{
		Number:         issue.Number,
		Title:          issue.Title,
		Body:           issue.Body,
		State:          issue.State,
		User:           issue.User,
		Labels:         issue.Labels,
		NumComments:    issue.NumComments,
		CreatedAt:      issue.CreatedAt,
		UpdatedAt:      issue.UpdatedAt,
		WebURL:         issue.WebURL,
		HeadSHA:        ref.HeadSHA,
		BaseRef:        repository.DefaultBranch,
		BaseRefAssumed: true,
	}, nil
}

// GetPullRequestDiff returns the merge-base diff of one pull request. An
// empty baseRef resolves to the repository default branch, which is only an
// assumption because Gogs v0.14.2 does not expose the target branch of a
// pull request; callers can override it through baseRef. A non-empty paths
// restricts the rendered diff to the matching files, while the merge state
// always describes the whole pull request.
func (c *Client) GetPullRequestDiff(ctx context.Context, owner, repo string, number int64, baseRef string, paths []string, maxBytes int) (PullRequestDiff, error) {
	if len(paths) > maximumDiffPaths {
		return PullRequestDiff{}, &Error{
			Code:    CodeInvalidArgument,
			Message: fmt.Sprintf("The path filter accepts at most %d paths.", maximumDiffPaths),
		}
	}
	if _, err := c.pullHeadRef(ctx, owner, repo, number); err != nil {
		return PullRequestDiff{}, err
	}
	assumed := false
	if baseRef == "" {
		repository, err := c.GetRepository(ctx, owner, repo)
		if err != nil {
			return PullRequestDiff{}, err
		}
		baseRef = repository.DefaultBranch
		assumed = true
		if baseRef == "" {
			return PullRequestDiff{}, &Error{
				Code:    CodeInvalidArgument,
				Message: "The repository has no default branch; pass base_ref explicitly.",
			}
		}
	}
	if maxBytes <= 0 {
		maxBytes = defaultDiffMaxBytes
	}

	engine, err := c.pullEngine(ctx)
	if err != nil {
		return PullRequestDiff{}, err
	}
	diff, err := engine.DiffPull(ctx, c.gitCloneURL(owner, repo), number, baseRef, paths, maxBytes)
	if err != nil {
		return PullRequestDiff{}, err
	}
	return PullRequestDiff{
		Number:             number,
		BaseRef:            baseRef,
		BaseRefAssumed:     assumed,
		MergeBase:          diff.MergeBase,
		Diff:               diff.Diff,
		Files:              diff.Files,
		Commits:            diff.Commits,
		Truncated:          diff.Truncated,
		MergeState:         diff.MergeState,
		MergeConflictPaths: diff.MergeConflictPaths,
	}, nil
}

// CleanPullCache drops every cached git object gathered for pull request
// diffs. It is a no-op when the pull request engine was never used.
func (c *Client) CleanPullCache() error {
	c.pullMu.Lock()
	defer c.pullMu.Unlock()
	if c.pull == nil {
		return nil
	}
	return c.pull.CleanCache()
}

// listPullRefs returns every pull request head of a repository.
func (c *Client) listPullRefs(ctx context.Context, owner, repo string) ([]PullRef, error) {
	engine, err := c.pullEngine(ctx)
	if err != nil {
		return nil, err
	}
	return engine.ListPullRefs(ctx, c.gitCloneURL(owner, repo))
}

// pullHeadRef resolves the refs/pull/{number}/head entry of one pull
// request. Issues without one are not pull requests.
func (c *Client) pullHeadRef(ctx context.Context, owner, repo string, number int64) (PullRef, error) {
	if number <= 0 {
		return PullRef{}, &Error{Code: CodeInvalidArgument, Message: "The pull request number must be positive."}
	}
	refs, err := c.listPullRefs(ctx, owner, repo)
	if err != nil {
		return PullRef{}, err
	}
	index := slices.IndexFunc(refs, func(ref PullRef) bool { return ref.Number == number })
	if index < 0 {
		return PullRef{}, &Error{
			Code:    CodeInvalidArgument,
			Message: fmt.Sprintf("Issue #%d has no pull request head ref and is not a pull request.", number),
		}
	}
	return refs[index], nil
}

// pullEngine lazily builds the git-backed pull request engine. The first
// call resolves the token owner, whose username authenticates the git
// requests. A failed attempt is retried on the next call.
func (c *Client) pullEngine(ctx context.Context) (*PullEngine, error) {
	c.pullMu.Lock()
	defer c.pullMu.Unlock()
	if c.pull != nil {
		return c.pull, nil
	}
	if c.cacheDir == "" {
		return nil, &Error{
			Code:    CodeInvalidArgument,
			Message: "Pull request tools require a cache directory; set GOGS_MCP_CACHE_DIR.",
		}
	}
	user, err := c.GetAuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	engine, err := NewPullEngine(PullEngineOptions{
		CacheDir:      c.cacheDir,
		Username:      user.Username,
		Token:         c.token,
		CacheTTL:      c.cacheTTL,
		CacheMaxBytes: c.cacheMaxBytes,
		Logger:        c.logger,
	})
	if err != nil {
		return nil, &Error{Code: CodeInternal, Message: "Could not initialize the pull request engine.", cause: err}
	}
	c.pull = engine
	return engine, nil
}

// gitCloneURL turns the API root into the HTTP clone URL of a repository:
// http://host/owner/repo.git
func (c *Client) gitCloneURL(owner, repo string) *url.URL {
	root := *c.apiRoot
	root.Path = strings.TrimSuffix(strings.TrimRight(root.Path, "/"), "/api/v1")
	root.RawQuery, root.Fragment = "", ""
	return root.JoinPath(owner, repo+".git")
}

package gogs

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
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
	// BaseCommits counts the commits the base branch carries since the
	// merge base; it stays zero while the base has not moved.
	BaseCommits int
}

// ListPullRequests returns the most recent pull requests of a repository in
// the given state, newest first, together with the total number of pull
// request heads the repository currently advertises and a continuation cursor.
// Each call examines at most 100 issue records, including filtered entries.
func (c *Client) ListPullRequests(ctx context.Context, owner, repo, state string, limit int, before int64) ([]PullRequestSummary, int, int64, error) {
	if state != PullStateOpen && state != PullStateClosed && state != PullStateAll {
		return nil, 0, 0, &Error{Code: CodeInvalidArgument, Message: "The state must be open, closed, or all."}
	}
	if before < 0 {
		return nil, 0, 0, &Error{Code: CodeInvalidArgument, Message: "before must be a positive PR number."}
	}
	if limit <= 0 {
		limit = defaultPullLimit
	}
	limit = min(limit, maximumPullLimit)
	timeout := c.http.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	refs, err := c.listPullRefs(ctx, owner, repo)
	if err != nil {
		return nil, 0, 0, err
	}
	return c.scanPullRefs(ctx, owner, repo, state, limit, before, refs)
}

// scanPullRefs uses the last examined number as a stable continuation boundary,
// even when filtering returns no matches. Each call reads at most 100 issues.
func (c *Client) scanPullRefs(ctx context.Context, owner, repo, state string, limit int, before int64, refs []PullRef) ([]PullRequestSummary, int, int64, error) {
	summaries := make([]PullRequestSummary, 0, limit)
	scanned := 0
	var next int64
	for index := len(refs) - 1; index >= 0; index-- {
		ref := refs[index]
		if before > 0 && ref.Number >= before {
			continue
		}
		if scanned >= 100 || len(summaries) >= limit {
			return summaries, len(refs), next, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, classifyTransportError(err)
		}
		scanned++
		next = ref.Number
		issue, err := c.GetIssue(ctx, owner, repo, ref.Number)
		if err != nil {
			if AsError(err).Code == CodeResourceNotFoundOrForbidden {
				continue
			}
			return nil, 0, 0, err
		}
		if state != PullStateAll && issue.State != state {
			continue
		}
		summaries = append(summaries, PullRequestSummary{Number: issue.Number, Title: issue.Title, State: issue.State, User: issue.User, NumComments: issue.NumComments, CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt, HeadSHA: ref.HeadSHA})
	}
	return summaries, len(refs), 0, nil
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
	c.invalidateAuthentication(err)
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
		BaseCommits:        diff.BaseCommits,
	}, nil
}

// CleanPullCache drops every cached git object gathered for pull request
// diffs. The pull request engine is built lazily, so the cache directory is
// cleaned directly when the engine was never used.
func (c *Client) CleanPullCache() error {
	c.pullMu.Lock()
	defer c.pullMu.Unlock()
	if c.pull != nil {
		return c.pull.CleanCache()
	}
	return CleanPullCacheDir(c.cacheDir)
}

// listPullRefs returns every pull request head of a repository.
func (c *Client) listPullRefs(ctx context.Context, owner, repo string) ([]PullRef, error) {
	engine, err := c.pullEngine(ctx)
	if err != nil {
		return nil, err
	}
	refs, err := engine.ListPullRefs(ctx, c.gitCloneURL(owner, repo))
	c.invalidateAuthentication(err)
	return refs, err
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
			Code: CodeInvalidArgument,
			// The wording avoids claiming that an issue with this number
			// exists: on an empty repository, no issue and no pull ref does.
			Message: fmt.Sprintf("Number %d has no pull request head ref and is not a pull request.", number),
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
	cacheDir := c.cacheDir
	if c.snapshots != nil {
		cacheDir, err = c.snapshots.UserRoot(user.ID)
		if err != nil {
			return nil, err
		}
	}
	engine, err := NewPullEngine(PullEngineOptions{
		CacheDir:      cacheDir,
		Username:      user.Username,
		Token:         c.token,
		CABundle:      c.caBundle,
		Timeout:       c.http.Timeout,
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

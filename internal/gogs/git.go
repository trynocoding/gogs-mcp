package gogs

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

// CommitPerson describes a git-level author or committer.
type CommitPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

// Commit is the stable commit representation exposed to tools.
type Commit struct {
	SHA        string       `json:"sha"`
	Message    string       `json:"message"`
	WebURL     string       `json:"web_url"`
	Author     CommitPerson `json:"author"`
	Committer  CommitPerson `json:"committer"`
	ParentSHAs []string     `json:"parent_shas,omitempty"`
}

// Branch is the stable branch representation exposed to tools.
type Branch struct {
	Name    string `json:"name"`
	HeadSHA string `json:"head_sha"`
}

type branchResponse struct {
	Name   string `json:"name"`
	Commit *struct {
		ID string `json:"id"`
	} `json:"commit"`
}

type commitResponse struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  *struct {
		Message   string                `json:"message"`
		Author    *commitPersonResponse `json:"author"`
		Committer *commitPersonResponse `json:"committer"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

type commitPersonResponse struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

func mapBranch(response branchResponse) Branch {
	var headSHA string
	if response.Commit != nil {
		headSHA = response.Commit.ID
	}
	return Branch{
		Name:    response.Name,
		HeadSHA: headSHA,
	}
}

// ListBranches returns every branch of the repository with its head commit SHA.
func (c *Client) ListBranches(ctx context.Context, owner, repo string) ([]Branch, error) {
	var response []branchResponse
	if err := c.getJSON(ctx, &response, "repos", owner, repo, "branches"); err != nil {
		return nil, err
	}
	branches := make([]Branch, len(response))
	for index, entry := range response {
		branches[index] = mapBranch(entry)
	}
	return branches, nil
}

// GetBranch returns a single branch. Branch names may contain slashes.
func (c *Client) GetBranch(ctx context.Context, owner, repo, branch string) (Branch, error) {
	if err := ValidateBranchName(branch); err != nil {
		return Branch{}, err
	}
	var response branchResponse
	if err := c.getJSON(ctx, &response, branchPathSegments(owner, repo, branch)...); err != nil {
		return Branch{}, err
	}
	return mapBranch(response), nil
}

// ListCommits returns the most recent commits of the default branch. Gogs v0.14.2
// starts at HEAD and only supports limiting the count, not paging or refs.
func (c *Client) ListCommits(ctx context.Context, owner, repo string, limit int) ([]Commit, error) {
	query := make(url.Values)
	query.Set("pageSize", strconv.Itoa(limit))
	var response []commitResponse
	if err := c.getJSONWithQuery(ctx, &response, query, "repos", owner, repo, "commits"); err != nil {
		return nil, err
	}
	commits := make([]Commit, len(response))
	for index, entry := range response {
		commits[index] = mapCommit(entry)
	}
	return commits, nil
}

// GetCommit returns a single commit by commit SHA. Git accepts full SHAs,
// short SHAs, and other revision syntax, so only path-injection shapes are
// rejected locally; unknown revisions are resolved by Gogs into a 404.
func (c *Client) GetCommit(ctx context.Context, owner, repo, sha string) (Commit, error) {
	if err := ValidateCommitRevision(sha); err != nil {
		return Commit{}, err
	}
	var response commitResponse
	if err := c.getJSON(ctx, &response, "repos", owner, repo, "commits", sha); err != nil {
		return Commit{}, err
	}
	return mapCommit(response), nil
}

func mapCommit(response commitResponse) Commit {
	commit := Commit{
		SHA:        response.SHA,
		WebURL:     response.HTMLURL,
		ParentSHAs: make([]string, len(response.Parents)),
	}
	if response.Commit != nil {
		commit.Message = response.Commit.Message
		if response.Commit.Author != nil {
			commit.Author = CommitPerson(*response.Commit.Author)
		}
		if response.Commit.Committer != nil {
			commit.Committer = CommitPerson(*response.Commit.Committer)
		}
	}
	for index, parent := range response.Parents {
		commit.ParentSHAs[index] = parent.SHA
	}
	return commit
}

// ValidateBranchName rejects branch names that cannot be safely placed into the
// branches API path. Slash-separated names are allowed and escaped per segment.
func ValidateBranchName(branch string) error {
	if branch == "" {
		return &Error{
			Code:    CodeValidationFailed,
			Message: "The branch name is required.",
		}
	}
	if strings.ContainsRune(branch, '\x00') || strings.Contains(branch, "\\") {
		return &Error{
			Code:    CodeValidationFailed,
			Message: "The branch name is invalid.",
		}
	}
	for _, segment := range strings.Split(branch, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return &Error{
				Code:    CodeValidationFailed,
				Message: "The branch name is invalid.",
			}
		}
	}
	return nil
}

func branchPathSegments(owner, repo, branch string) []string {
	segments := []string{"repos", owner, repo, "branches"}
	return append(segments, strings.Split(branch, "/")...)
}

// ValidateCommitRevision rejects revision strings that cannot be safely placed
// into the commits API path. A bare "." or ".." segment is cleaned away during
// URL construction, which would retarget the request at a different endpoint,
// so they are rejected before any request leaves this process. Slashes and
// NUL are rejected to keep the revision a single path segment. Every other
// unknown revision is left for Gogs to resolve into a 404.
func ValidateCommitRevision(sha string) error {
	if sha == "" {
		return &Error{
			Code:    CodeValidationFailed,
			Message: "The commit SHA is required.",
		}
	}
	if sha == "." || sha == ".." || strings.ContainsAny(sha, "\\/\x00") {
		return &Error{
			Code:    CodeValidationFailed,
			Message: "The commit SHA is invalid.",
		}
	}
	return nil
}

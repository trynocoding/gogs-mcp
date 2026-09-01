package gogs

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/cockroachdb/errors"
)

// ResolveCommitSHA resolves an empty ref, branch name, tag name, or 7 to 40
// character hex revision into the full commit SHA that an immutable snapshot
// must be pinned to. The fallback order mirrors the Gogs archive download
// order: branches first, then tags, then revisions.
func (c *Client) ResolveCommitSHA(ctx context.Context, owner, repo, ref string) (string, error) {
	if ref == "" {
		repository, err := c.GetRepository(ctx, owner, repo)
		if err != nil {
			return "", err
		}
		if repository.DefaultBranch == "" {
			return "", &Error{
				Code:    CodeResourceNotFoundOrForbidden,
				Message: "The repository has no default branch to resolve.",
			}
		}
		return c.branchHeadSHA(ctx, owner, repo, repository.DefaultBranch)
	}

	sha, err := c.branchHeadSHA(ctx, owner, repo, ref)
	if err == nil {
		return sha, nil
	}
	if AsError(err).Code != CodeResourceNotFoundOrForbidden {
		return "", err
	}

	tags, err := c.ListTags(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	for _, tag := range tags {
		if tag.Name == ref && tag.CommitSHA != "" {
			return tag.CommitSHA, nil
		}
	}

	if isHexRevision(ref) {
		commit, err := c.GetCommit(ctx, owner, repo, ref)
		if err == nil {
			return commit.SHA, nil
		}
		if AsError(err).Code != CodeResourceNotFoundOrForbidden {
			return "", err
		}
	}

	return "", &Error{
		Code:    CodeResourceNotFoundOrForbidden,
		Message: "The ref cannot be resolved to a commit: no branch, tag, or revision matched.",
	}
}

func (c *Client) branchHeadSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	found, err := c.GetBranch(ctx, owner, repo, branch)
	if err != nil {
		return "", err
	}
	if found.HeadSHA == "" {
		return "", &Error{
			Code:    CodeGogsError,
			Message: "Gogs did not report a head commit for the branch.",
		}
	}
	return found.HeadSHA, nil
}

func isHexRevision(ref string) bool {
	if len(ref) < 7 || len(ref) > 40 {
		return false
	}
	for _, character := range ref {
		switch {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'f':
		case character >= 'A' && character <= 'F':
		default:
			return false
		}
	}
	return true
}

// ValidateArchiveSHA guards the archive path against revisions that URL path
// cleaning could retarget. Only a resolved full-length commit SHA may be
// downloaded.
func ValidateArchiveSHA(sha string) error {
	if len(sha) == 40 && isHexRevision(sha) {
		return nil
	}
	return &Error{
		Code:    CodeValidationFailed,
		Message: "The archive must be requested with a full 40-character commit SHA.",
	}
}

// DownloadArchive returns the raw tar.gz archive body of the given commit.
// The caller owns the reader and must close it. Unlike the JSON helpers the
// body is streamed without buffering, and transport failures are not retried
// because a partially consumed stream cannot be replayed.
func (c *Client) DownloadArchive(ctx context.Context, owner, repo, sha string) (io.ReadCloser, error) {
	if err := ValidateArchiveSHA(sha); err != nil {
		return nil, err
	}
	requestURL := c.apiRoot.JoinPath(escapedPathSegments([]string{"repos", owner, repo, "archive", sha + ".tar.gz"})...)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, errors.Wrap(err, "create Gogs request")
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("Authorization", "token "+c.token)
	request.Header.Set("User-Agent", c.userAgent)

	started := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		classified := classifyTransportError(err)
		c.logRequest(ctx, 0, time.Since(started), classified.Code)
		return nil, classified
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		classified := classifyStatus(response.StatusCode)
		c.logRequest(ctx, response.StatusCode, time.Since(started), classified.Code)
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxJSONResponseBytes))
		_ = response.Body.Close()
		return nil, classified
	}
	c.logRequest(ctx, response.StatusCode, time.Since(started), "")
	return response.Body, nil
}

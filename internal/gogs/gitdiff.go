package gogs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// PullRef is one pull request head advertised through the refs/pull/{index}/head
// convention that Gogs maintains on the base repository. The ref exists from
// the moment a pull request is opened and is refreshed whenever new commits
// are pushed to its head branch.
type PullRef struct {
	Number  int64
	HeadSHA string
}

// DiffFileStat summarizes one file of a pull request diff.
type DiffFileStat struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	IsBinary  bool   `json:"is_binary"`
}

// PullCommit is one commit reachable from a pull request head but not from
// the merge base. The walk is first-parent, which covers the common case of
// a linear head branch.
type PullCommit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

// PullDiff is the merge-base diff of one pull request plus everything an
// AI reviewer needs to navigate it.
type PullDiff struct {
	MergeBase string         `json:"merge_base"`
	Diff      string         `json:"diff"`
	Files     []DiffFileStat `json:"files"`
	Commits   []PullCommit   `json:"commits"`
	Truncated bool           `json:"truncated"`
}

// PullEngineOptions configures the pull request engine.
type PullEngineOptions struct {
	CacheDir string
	Username string
	Token    string
	Timeout  time.Duration
	Logger   *slog.Logger
}

// PullEngine computes pull request data over the native git protocol of the
// base repository: refs enumeration, merge-base resolution, and unified
// diffs. Fetched objects are cached under CacheDir, so repeated calls only
// transfer what changed.
type PullEngine struct {
	cacheDir string
	username string
	token    string
	timeout  time.Duration
	logger   *slog.Logger

	mu sync.Mutex
}

// NewPullEngine validates the options and returns a ready engine.
func NewPullEngine(options PullEngineOptions) (*PullEngine, error) {
	if options.CacheDir == "" {
		return nil, errors.New("cache directory is required")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &PullEngine{
		cacheDir: options.CacheDir,
		username: options.Username,
		token:    options.Token,
		timeout:  timeout,
		logger:   logger,
	}, nil
}

// CleanCache removes every cached repository. It is the git counterpart of
// the snapshot cache cleanup behind "gogs-mcp cache clean".
func (e *PullEngine) CleanCache() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return os.RemoveAll(filepath.Join(e.cacheDir, "pull"))
}

// ListPullRefs returns every pull request head that the repository currently
// advertises, ordered by pull request number.
func (e *PullEngine) ListPullRefs(ctx context.Context, cloneURL *url.URL) ([]PullRef, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{cloneURL.String()},
	})
	refs, err := remote.ListContext(ctx, &git.ListOptions{
		Auth: e.authFor(cloneURL),
	})
	if err != nil {
		return nil, classifyGitError(err)
	}

	pullRefs := make([]PullRef, 0)
	for _, ref := range refs {
		number, ok := parsePullRef(ref.Name().String())
		if !ok {
			continue
		}
		pullRefs = append(pullRefs, PullRef{
			Number:  number,
			HeadSHA: ref.Hash().String(),
		})
	}
	return pullRefs, nil
}

// DiffPull fetches the pull request head and the base ref into the local
// cache, then returns their merge-base diff together with the changed files
// and the pull request commits.
func (e *PullEngine) DiffPull(ctx context.Context, cloneURL *url.URL, number int64, baseRef string, maxBytes int) (*PullDiff, error) {
	if number <= 0 {
		return nil, &Error{Code: CodeInvalidArgument, Message: "The pull request number must be positive."}
	}
	if baseRef == "" {
		return nil, &Error{Code: CodeInvalidArgument, Message: "The base ref is required."}
	}

	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	e.mu.Lock()
	defer e.mu.Unlock()

	repo, err := e.cachedRepo(cloneURL)
	if err != nil {
		return nil, err
	}
	if err := e.fetchRefs(ctx, repo, cloneURL, number, baseRef); err != nil {
		return nil, err
	}

	headHash, err := resolveCachedRef(repo, fmt.Sprintf("refs/gogs-mcp/pull/%d/head", number))
	if err != nil {
		return nil, err
	}
	baseHash, err := resolveCachedRef(repo, "refs/gogs-mcp/base")
	if err != nil {
		return nil, err
	}

	headCommit, err := repo.CommitObject(headHash)
	if err != nil {
		return nil, &Error{Code: CodeGogsError, Message: "Could not read the pull request head commit.", cause: err}
	}
	baseCommit, err := repo.CommitObject(baseHash)
	if err != nil {
		return nil, &Error{Code: CodeGogsError, Message: "Could not read the base ref commit.", cause: err}
	}

	mergeBases, err := headCommit.MergeBase(baseCommit)
	if err != nil {
		return nil, &Error{Code: CodeGogsError, Message: "Could not compute the merge base.", cause: err}
	}
	if len(mergeBases) == 0 {
		return nil, &Error{
			Code:    CodeInvalidArgument,
			Message: fmt.Sprintf("The pull request head has no common history with %q.", baseRef),
		}
	}
	mergeBase := mergeBases[0]

	commits, err := commitsSince(repo, mergeBase.Hash, headHash)
	if err != nil {
		return nil, err
	}

	mergeBaseTree, err := mergeBase.Tree()
	if err != nil {
		return nil, &Error{Code: CodeGogsError, Message: "Could not read the merge base tree.", cause: err}
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return nil, &Error{Code: CodeGogsError, Message: "Could not read the pull request head tree.", cause: err}
	}

	changes, err := object.DiffTreeWithOptions(ctx, mergeBaseTree, headTree, &object.DiffTreeOptions{
		DetectRenames: true,
		RenameScore:   50,
	})
	if err != nil {
		return nil, &Error{Code: CodeGogsError, Message: "Could not diff the pull request trees.", cause: err}
	}
	patch, err := changes.Patch()
	if err != nil {
		return nil, &Error{Code: CodeGogsError, Message: "Could not build the pull request patch.", cause: err}
	}

	return buildPullDiff(patch, commits, mergeBase.Hash.String(), maxBytes)
}

// buildPullDiff renders the patch text and collects the per-file stats. The
// stats are always complete; the diff text stops after maxBytes. A
// non-positive maxBytes means no limit.
func buildPullDiff(patch *object.Patch, commits []PullCommit, mergeBase string, maxBytes int) (*PullDiff, error) {
	filePatches := patch.FilePatches()
	files := make([]DiffFileStat, 0, len(filePatches))
	for _, filePatch := range filePatches {
		from, to := filePatch.Files()
		files = append(files, DiffFileStat{
			Path:      diffPath(from, to),
			Status:    diffStatus(from, to),
			Additions: countChunkLines(filePatch, fdiff.Add),
			Deletions: countChunkLines(filePatch, fdiff.Delete),
			IsBinary:  filePatch.IsBinary(),
		})
	}

	var buffer bytes.Buffer
	if err := patch.Encode(&buffer); err != nil {
		return nil, &Error{Code: CodeGogsError, Message: "Could not render the pull request diff.", cause: err}
	}
	truncated := false
	if maxBytes > 0 && buffer.Len() > maxBytes {
		truncateToLastLine(&buffer, maxBytes)
		truncated = true
	}

	return &PullDiff{
		MergeBase: mergeBase,
		Diff:      buffer.String(),
		Files:     files,
		Commits:   commits,
		Truncated: truncated,
	}, nil
}

// cachedRepo opens the bare cache repository for the clone URL, recreating
// it whenever it is missing or unusable. The caller must hold e.mu.
func (e *PullEngine) cachedRepo(cloneURL *url.URL) (*git.Repository, error) {
	sum := sha256.Sum256([]byte(cloneURL.String()))
	repoPath := filepath.Join(e.cacheDir, "pull", hex.EncodeToString(sum[:8])+".git")

	repo, openErr := git.PlainOpen(repoPath)
	if openErr == nil {
		return repo, nil
	}
	if !errors.Is(openErr, git.ErrRepositoryNotExists) {
		// An unreadable cache is not worth reporting; rebuild it from scratch.
		e.logger.Warn("Discarding the unusable pull cache repository.", "path", repoPath, "error", openErr)
	}
	if err := os.RemoveAll(repoPath); err != nil {
		return nil, &Error{Code: CodeInternal, Message: "Could not reset the pull cache repository.", cause: err}
	}
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		return nil, &Error{Code: CodeInternal, Message: "Could not create the pull cache directory.", cause: err}
	}
	repo, err := git.PlainInit(repoPath, true)
	if err != nil {
		return nil, &Error{Code: CodeInternal, Message: "Could not initialize the pull cache repository.", cause: err}
	}
	return repo, nil
}

// fetchRefs mirrors the pull request head and the base ref into stable local
// names, so the diff never depends on remote-side ref layout. The mirror
// names are per-repository, and a later call for another pull request
// overwrites the base mirror with its own base ref.
func (e *PullEngine) fetchRefs(ctx context.Context, repo *git.Repository, cloneURL *url.URL, number int64, baseRef string) error {
	const remoteName = "gogs-mcp"
	if _, err := repo.Remote(remoteName); err != nil {
		if _, err := repo.CreateRemote(&config.RemoteConfig{
			Name: remoteName,
			URLs: []string{cloneURL.String()},
		}); err != nil {
			return &Error{Code: CodeInternal, Message: "Could not configure the pull cache remote.", cause: err}
		}
	}
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: remoteName,
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("+refs/pull/%d/head:refs/gogs-mcp/pull/%d/head", number, number)),
			config.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/gogs-mcp/base", baseRef)),
		},
		Auth:  e.authFor(cloneURL),
		Force: true,
		Tags:  git.NoTags,
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	if err != nil {
		// go-git reports an unresolvable refspec source as
		// NoMatchingRefSpecError, which is not an errors.Is match for
		// plumbing.ErrReferenceNotFound, so both are checked.
		var missingRef git.NoMatchingRefSpecError
		if errors.As(err, &missingRef) || errors.Is(err, plumbing.ErrReferenceNotFound) {
			return &Error{
				Code:    CodeInvalidArgument,
				Message: fmt.Sprintf("Could not fetch the pull request refs; the base ref %q may not exist.", baseRef),
				cause:   err,
			}
		}
		return classifyGitError(err)
	}
	return nil
}

func (e *PullEngine) authFor(cloneURL *url.URL) transport.AuthMethod {
	if cloneURL.Scheme != "http" && cloneURL.Scheme != "https" {
		return nil
	}
	if e.username == "" || e.token == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: e.username, Password: e.token}
}

// classifyGitError maps a go-git failure onto the shared error taxonomy.
func classifyGitError(err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Message: "The Gogs git request timed out.", Retryable: true, cause: err}
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return &Error{Code: CodeTimeout, Message: "The Gogs git request timed out.", Retryable: true, cause: err}
	}
	if errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed) ||
		errors.Is(err, transport.ErrInvalidAuthMethod) {
		return &Error{
			Code:    CodeAuthenticationFailed,
			Message: "Git authentication failed; check the configured token and its permissions.",
			cause:   err,
		}
	}
	if errors.Is(err, transport.ErrRepositoryNotFound) {
		return &Error{
			Code:    CodeResourceNotFoundOrForbidden,
			Message: "The repository does not exist over the git protocol, or the token cannot see it.",
			cause:   err,
		}
	}
	return &Error{
		Code:    CodeUnavailable,
		Message: "Could not reach Gogs over the git protocol.",
		cause:   err,
	}
}

// parsePullRef accepts "refs/pull/{index}/head" and reports the index.
func parsePullRef(ref string) (int64, bool) {
	const prefix = "refs/pull/"
	if !strings.HasPrefix(ref, prefix) || !strings.HasSuffix(ref, "/head") {
		return 0, false
	}
	number, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(ref, prefix), "/head"), 10, 64)
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

func resolveCachedRef(repo *git.Repository, ref string) (plumbing.Hash, error) {
	resolved, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		return plumbing.ZeroHash, &Error{
			Code:    CodeGogsError,
			Message: fmt.Sprintf("Could not resolve the ref %q in the pull cache.", ref),
			cause:   err,
		}
	}
	return *resolved, nil
}

// commitsSince walks the history from head back to, but not including, the
// merge base.
func commitsSince(repo *git.Repository, mergeBase, head plumbing.Hash) ([]PullCommit, error) {
	if mergeBase == head {
		return nil, nil
	}
	iterator, err := repo.Log(&git.LogOptions{From: head})
	if err != nil {
		return nil, &Error{Code: CodeGogsError, Message: "Could not walk the pull request history.", cause: err}
	}
	defer iterator.Close()

	commits := make([]PullCommit, 0)
	for {
		commit, err := iterator.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &Error{Code: CodeGogsError, Message: "Could not walk the pull request history.", cause: err}
		}
		if commit.Hash == mergeBase {
			break
		}
		message, _, _ := strings.Cut(commit.Message, "\n")
		commits = append(commits, PullCommit{
			SHA:     commit.Hash.String(),
			Message: message,
			Author:  commit.Author.Name,
			Date:    commit.Author.When.UTC().Format(time.RFC3339),
		})
	}
	return commits, nil
}

func diffPath(from, to fdiff.File) string {
	if from == nil {
		return to.Path()
	}
	return from.Path()
}

func diffStatus(from, to fdiff.File) string {
	switch {
	case from == nil:
		return "added"
	case to == nil:
		return "deleted"
	case from.Path() != to.Path():
		return "renamed"
	default:
		return "modified"
	}
}

func countChunkLines(filePatch fdiff.FilePatch, kind fdiff.Operation) int {
	count := 0
	for _, chunk := range filePatch.Chunks() {
		if chunk.Type() == kind {
			count += strings.Count(chunk.Content(), "\n")
		}
	}
	return count
}

// truncateToLastLine cuts the buffer to at most maxBytes without leaving a
// partial line behind.
func truncateToLastLine(buffer *bytes.Buffer, maxBytes int) {
	raw := buffer.Bytes()
	cut := min(maxBytes, len(raw))
	for cut > 0 && raw[cut-1] != '\n' {
		cut--
	}
	buffer.Truncate(cut)
}

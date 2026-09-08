package gogs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gogs-mcp/internal/diskcache"

	"github.com/cockroachdb/errors"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/filesystem"
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

// Merge states reported in PullDiff.MergeState. The state is a heuristic
// computed from the local object cache; only a real merge can decide the
// outcome.
const (
	MergeStateFastForward = "fast_forward" // the base has not moved since the merge base, so the merge cannot conflict
	MergeStateDiverged    = "diverged"     // both sides moved on, but no file was touched by both
	MergeStateConflicting = "conflicting"  // both sides touched the same files; a merge may conflict
)

// PullDiff is the merge-base diff of one pull request plus everything an
// AI reviewer needs to navigate it.
type PullDiff struct {
	MergeBase          string         `json:"merge_base"`
	Diff               string         `json:"diff"`
	Files              []DiffFileStat `json:"files"`
	Commits            []PullCommit   `json:"commits"`
	Truncated          bool           `json:"truncated"`
	MergeState         string         `json:"merge_state"`
	MergeConflictPaths []string       `json:"merge_conflict_paths,omitempty"`
	// BaseCommits counts the commits the base branch carries since the
	// merge base. A large count together with an assumed base ref suggests
	// the pull request targets a different branch.
	BaseCommits int `json:"base_commits"`
}

// PullEngineOptions configures the pull request engine.
type PullEngineOptions struct {
	CacheDir string
	Username string
	Token    string
	CABundle []byte
	Timeout  time.Duration
	// CacheTTL expires diff cache repositories untouched for this long;
	// non-positive values fall back to defaultPullCacheTTL.
	CacheTTL time.Duration
	// CacheMaxBytes keeps the diff cache within this size: the least
	// recently used repositories are evicted once the total exceeds it, and
	// a repository that alone exceeds it is dropped instead of cached.
	// Non-positive values fall back to defaultPullCacheMaxBytes.
	CacheMaxBytes int64
	Logger        *slog.Logger
}

// PullEngine computes pull request data over the native git protocol of the
// base repository: refs enumeration, merge-base resolution, and unified
// diffs. Fetched objects are cached under CacheDir, so repeated calls only
// transfer what changed.
type PullEngine struct {
	cacheDir      string
	username      string
	token         string
	caBundle      []byte
	timeout       time.Duration
	cacheTTL      time.Duration
	cacheMaxBytes int64
	logger        *slog.Logger
}

// Defaults for the diff cache bounds; they mirror the snapshot cache limits.
const (
	defaultPullCacheTTL      = 24 * time.Hour
	defaultPullCacheMaxBytes = int64(2 << 30)
)

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
	cacheTTL := options.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = defaultPullCacheTTL
	}
	cacheMaxBytes := options.CacheMaxBytes
	if cacheMaxBytes <= 0 {
		cacheMaxBytes = defaultPullCacheMaxBytes
	}
	return &PullEngine{
		cacheDir:      options.CacheDir,
		username:      options.Username,
		token:         options.Token,
		timeout:       timeout,
		caBundle:      append([]byte(nil), options.CABundle...),
		cacheTTL:      cacheTTL,
		cacheMaxBytes: cacheMaxBytes,
		logger:        logger,
	}, nil
}

// CleanCache removes every cached repository. It is the git counterpart of
// the snapshot cache cleanup behind "gogs-mcp cache clean".
func (e *PullEngine) CleanCache() error {
	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()
	return diskcache.RemoveTree(ctx, e.pullCacheRoot(), e.cacheDir)
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
		Auth:     e.authFor(cloneURL),
		CABundle: e.caBundle,
	})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		// An empty repository advertises no refs, which go-git surfaces as an
		// error; an empty pull request listing is the faithful answer.
		return []PullRef{}, nil
	}
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
	// The remote advertisement is a map iteration in go-git, so the refs
	// arrive in arbitrary order; callers walk the slice expecting ascending
	// numbers.
	sort.Slice(pullRefs, func(i, j int) bool { return pullRefs[i].Number < pullRefs[j].Number })
	return pullRefs, nil
}

// DiffPull fetches the pull request head and the base ref into the local
// cache, then returns their merge-base diff together with the changed files
// and the pull request commits. A non-empty paths restricts the rendered diff
// to the matching files; the file stats follow the same filter while the
// merge state always describes the whole pull request.
func (e *PullEngine) DiffPull(ctx context.Context, cloneURL *url.URL, number int64, baseRef string, paths []string, maxBytes int) (*PullDiff, error) {
	if number <= 0 {
		return nil, &Error{Code: CodeInvalidArgument, Message: "The pull request number must be positive."}
	}
	if baseRef == "" {
		return nil, &Error{Code: CodeInvalidArgument, Message: "The base ref is required."}
	}
	filters := normalizePathFilters(paths)

	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	writer, err := diskcache.Acquire(ctx, e.cacheDir, e.cacheDir+".writer", false)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer writer.Close()
	repoPath := e.repoPathFor(cloneURL)
	entry, err := diskcache.Acquire(ctx, e.cacheDir, repoPath, false)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer entry.Close()
	budget := &diskcache.Budget{Root: e.cacheDir, Keep: repoPath, MaxBytes: e.cacheMaxBytes, TTL: e.cacheTTL}
	if err := budget.MakeRoom(0); err != nil {
		return nil, classifyGitError(err)
	}
	repo, err := e.cachedRepo(cloneURL, budget)
	if err != nil {
		if errors.Is(err, diskcache.ErrCapacity) {
			_ = os.RemoveAll(repoPath)
		}
		return nil, err
	}
	if err := e.fetchRefs(ctx, repo, cloneURL, number, baseRef); err != nil {
		if errors.Is(err, diskcache.ErrCapacity) {
			_ = os.RemoveAll(repoPath)
		}
		return nil, err
	}
	touchCache(repoPath)
	if err := budget.MakeRoom(0); err != nil {
		_ = os.RemoveAll(repoPath)
		return nil, classifyGitError(err)
	}

	headHash, err := resolveCachedRef(repo, fmt.Sprintf("refs/gogs-mcp/pull/%d/head", number))
	if err != nil {
		return nil, err
	}
	baseHash, err := resolveCachedRef(repo, "refs/gogs-mcp/base")
	if err != nil {
		return nil, err
	}

	mergeBases, err := boundedMergeBases(ctx, repo, headHash, baseHash)
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

	if len(mergeBases) == 0 {
		return nil, &Error{
			Code:    CodeInvalidArgument,
			Message: fmt.Sprintf("The pull request head has no common history with %q.", baseRef),
		}
	}
	mergeBase := mergeBases[0]

	commits, err := commitsSince(ctx, repo, mergeBase.Hash, headHash)
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
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return nil, &Error{Code: CodeGogsError, Message: "Could not read the base ref tree.", cause: err}
	}

	headChanges, err := object.DiffTreeWithOptions(ctx, mergeBaseTree, headTree, &object.DiffTreeOptions{
		DetectRenames: false,
		RenameScore:   50,
	})
	if err != nil {
		return nil, &Error{Code: CodeGogsError, Message: "Could not diff the pull request trees.", cause: err}
	}

	mergeState, conflictPaths, baseCommits, err := e.mergeStateFor(ctx, repo, mergeBase.Hash, baseHash, mergeBaseTree, baseTree, headChanges)
	if err != nil {
		return nil, err
	}

	if len(filters) > 0 {
		headChanges = renameCandidates(headChanges, filters)
	}
	if err := validatePatchWork(ctx, headChanges); err != nil {
		return nil, err
	}
	headChanges, err = object.DetectRenames(headChanges, &object.DiffTreeOptions{DetectRenames: true, RenameScore: 50})
	if err != nil {
		return nil, classifyGitError(err)
	}
	if len(filters) > 0 {
		headChanges = filterChanges(headChanges, filters)
	}
	touchCache(repoPath)
	diff, err := buildBoundedPullDiff(ctx, headChanges, commits, mergeBase.Hash.String(), maxBytes)
	if err != nil {
		return nil, err
	}

	diff.MergeState = mergeState
	diff.MergeConflictPaths = conflictPaths
	diff.BaseCommits = baseCommits
	return diff, nil
}

// pullCacheRoot is the directory holding the bare cache repositories.
func (e *PullEngine) pullCacheRoot() string {
	return filepath.Join(e.cacheDir, ".pull")
}

// CleanPullCacheDir removes the pull request git cache below a cache
// directory. It lets cache maintenance reach the pull cache of a user whose
// pull request engine was never constructed.
func CleanPullCacheDir(dir string) error {
	if dir == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := diskcache.RemoveTree(ctx, filepath.Join(dir, ".pull"), dir); err != nil {
		return err
	}
	return diskcache.RemoveTree(ctx, filepath.Join(dir, "pull"), dir)
}

// repoPathFor derives the stable cache path of a clone URL. The caller holds the shared user writer lock.
func (e *PullEngine) repoPathFor(cloneURL *url.URL) string {
	sum := sha256.Sum256([]byte(cloneURL.String()))
	return filepath.Join(e.pullCacheRoot(), hex.EncodeToString(sum[:8])+".git")
}

// touchCache marks a cache repository as freshly used, which drives the TTL
// sweep. Fetch failures leave the old timestamp in place, so an unusable
// repository ages out.
func touchCache(path string) {
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// dirSize returns the on-disk size of a directory tree, counting unreadable
// entries as zero.
func dirSize(root string) int64 {
	var total int64
	//nolint:errcheck // a size estimate must not fail on unreadable entries
	filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			//nolint:nilerr // unreadable entries contribute zero to the estimate
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// normalizePathFilters trims the path filters and drops the empty ones. A
// nil result means no filtering.
func normalizePathFilters(paths []string) []string {
	filters := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		path = strings.TrimSuffix(path, "/")
		if path != "" {
			filters = append(filters, path)
		}
	}
	if len(filters) == 0 {
		return nil
	}
	return filters
}

// filterChanges keeps the changes whose source or destination path matches
// one of the filters.
// renameCandidates keeps the opposite side of a possible rename even when
// only the old or new path was requested. These candidates share the same
// input budget as the selected files; unrelated modifications are excluded.
func renameCandidates(changes object.Changes, filters []string) object.Changes {
	selected := filterChanges(changes, filters)
	var needAdded, needDeleted bool
	for _, change := range selected {
		needDeleted = needDeleted || change.From.Name == ""
		needAdded = needAdded || change.To.Name == ""
	}
	result := make(object.Changes, 0, len(selected))
	for _, change := range changes {
		if matchAnyPath(change.From.Name, filters) || matchAnyPath(change.To.Name, filters) || (needAdded && change.From.Name == "") || (needDeleted && change.To.Name == "") {
			result = append(result, change)
		}
	}
	return result
}

func filterChanges(changes object.Changes, filters []string) object.Changes {
	kept := make(object.Changes, 0, len(changes))
	for _, change := range changes {
		if matchAnyPath(change.From.Name, filters) || matchAnyPath(change.To.Name, filters) {
			kept = append(kept, change)
		}
	}
	return kept
}

// matchAnyPath reports whether the path matches one of the filters exactly or
// as a directory prefix.
func matchAnyPath(path string, filters []string) bool {
	if path == "" {
		return false
	}
	for _, filter := range filters {
		if path == filter || strings.HasPrefix(path, filter+"/") {
			return true
		}
	}
	return false
}

// mergeStateFor compares the two sides of the pull request against the merge
// base and also reports how many commits the base branch carries since it.
// Only a real merge can decide the outcome, so the conflicting state reports
// the files that both sides touched.
func (e *PullEngine) mergeStateFor(ctx context.Context, repo *git.Repository, mergeBase, base plumbing.Hash, mergeBaseTree, baseTree *object.Tree, headChanges object.Changes) (string, []string, int, error) {
	baseCommits, err := commitsSince(ctx, repo, mergeBase, base)
	if err != nil {
		return "", nil, 0, err
	}
	if len(baseCommits) == 0 {
		return MergeStateFastForward, nil, 0, nil
	}

	baseChanges, err := object.DiffTreeWithOptions(ctx, mergeBaseTree, baseTree, &object.DiffTreeOptions{
		DetectRenames: true,
		RenameScore:   50,
	})
	if err != nil {
		return "", nil, 0, &Error{Code: CodeGogsError, Message: "Could not diff the base ref tree.", cause: err}
	}

	headPaths := touchedPaths(headChanges)
	var conflicts []string
	for path := range touchedPaths(baseChanges) {
		if headPaths[path] {
			conflicts = append(conflicts, path)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return MergeStateConflicting, conflicts, len(baseCommits), nil
	}
	return MergeStateDiverged, nil, len(baseCommits), nil
}

// touchedPaths collects the source and destination paths of the changes.
func touchedPaths(changes object.Changes) map[string]bool {
	paths := make(map[string]bool, len(changes))
	for _, change := range changes {
		if change.From.Name != "" {
			paths[change.From.Name] = true
		}
		if change.To.Name != "" {
			paths[change.To.Name] = true
		}
	}
	return paths
}

// buildPullDiff renders the patch text and collects the per-file stats. The
// stats are always complete; the diff text stops after maxBytes. A
// non-positive maxBytes means no limit. Submodule changes only surface in
// the stats: go-git's unified encoder emits no hunk for a gitlink, so their
// entries report zero line counts with is_binary set while the diff text
// stays silent about them.
func buildPullDiff(changes object.Changes, patch *object.Patch, commits []PullCommit, mergeBase string, maxBytes int) (*PullDiff, error) {
	filePatches := patch.FilePatches()
	// getPatchContext appends exactly one file patch per tree change, in
	// order; this guard is a cheap check on that invariant, because its
	// failure would otherwise panic on the index below after a future
	// go-git upgrade.
	if len(filePatches) != len(changes) {
		return nil, &Error{Code: CodeInternal, Message: "The pull request patch does not match its tree changes."}
	}
	files := make([]DiffFileStat, 0, len(filePatches))
	for index, filePatch := range filePatches {
		files = append(files, DiffFileStat{
			Path:      diffEntryPath(changes[index]),
			Status:    diffChangeStatus(changes[index]),
			Additions: countChunkLines(filePatch, fdiff.Add),
			Deletions: countChunkLines(filePatch, fdiff.Delete),
			IsBinary:  filePatch.IsBinary(),
		})
	}

	buffer := limitedDiffWriter{maximum: maxBytes}
	if err := patch.Encode(&buffer); err != nil && !errors.Is(err, errDiffLimit) {
		return nil, &Error{Code: CodeGogsError, Message: "Could not render the pull request diff.", cause: err}
	}
	truncated := false
	if buffer.truncated {
		truncateToLastLine(&buffer.Buffer, maxBytes)
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
// it whenever it is missing, unusable, or cannot be made private. The
// repository is private to the process (0700): the fetched objects carry
// repository data that only the authenticated token was meant to see, and
// the 0700 mode gates access even to the world-readable pack files git
// writes. The caller holds the shared user writer lock.
func (e *PullEngine) cachedRepo(cloneURL *url.URL, budget *diskcache.Budget) (*git.Repository, error) {
	repoPath := e.repoPathFor(cloneURL)

	fs := &diskcache.Filesystem{Filesystem: osfs.New(repoPath), Budget: budget}
	store := filesystem.NewStorageWithOptions(fs, cache.NewObjectLRUDefault(), filesystem.Options{LargeObjectThreshold: 1 << 20})
	repo, openErr := git.Open(store, nil)
	if openErr == nil {
		// Repositories created by earlier versions may still be group or
		// world readable; tighten them, because an actively used repository
		// never ages out through the TTL sweep.
		err := os.Chmod(repoPath, 0o700)
		if err == nil {
			return repo, nil
		}
		// Serving objects from a path that cannot be made private would
		// expose repository data, so the repository is discarded; the
		// rebuild below either replaces it with a private one or fails.
		e.logger.Warn("Discarding the pull cache repository whose permissions could not be tightened.", "path", repoPath, "error", err)
	} else if !errors.Is(openErr, git.ErrRepositoryNotExists) {
		// An unreadable cache is not worth reporting; rebuild it from scratch.
		e.logger.Warn("Discarding the unusable pull cache repository.", "path", repoPath, "error", openErr)
	}
	if err := os.RemoveAll(repoPath); err != nil {
		return nil, &Error{Code: CodeInternal, Message: "Could not reset the pull cache repository.", cause: err}
	}
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		return nil, &Error{Code: CodeInternal, Message: "Could not create the pull cache directory.", cause: err}
	}
	repo, err := git.Init(store, nil)
	if err != nil {
		return nil, classifyGitError(err)
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
		Auth:     e.authFor(cloneURL),
		CABundle: e.caBundle,
		Force:    true,
		Tags:     git.NoTags,
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
	if errors.Is(err, diskcache.ErrCapacity) {
		return &Error{Code: CodeCacheCapacityExceeded, Message: "The shared user cache is full.", Retryable: true, cause: err}
	}
	if classified := classifyTransportError(err); classified.Code == CodeTLSError || classified.Code == CodeCanceled {
		return classified
	}
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
func commitsSince(ctx context.Context, repo *git.Repository, mergeBase, head plumbing.Hash) ([]PullCommit, error) {
	if mergeBase == head {
		return nil, nil
	}
	excluded, err := reachableCommits(ctx, repo, mergeBase, nil)
	if err != nil {
		return nil, err
	}
	included, err := reachableCommits(ctx, repo, head, excluded)
	if err != nil {
		return nil, err
	}
	commits := make([]PullCommit, 0, len(included))
	for _, commit := range included {
		message, _, _ := strings.Cut(commit.Message, "\n")
		commits = append(commits, PullCommit{SHA: commit.Hash.String(), Message: message, Author: commit.Author.Name, Date: commit.Author.When.UTC().Format(time.RFC3339)})
	}
	sort.Slice(commits, func(i, j int) bool {
		if commits[i].Date != commits[j].Date {
			return commits[i].Date > commits[j].Date
		}
		return commits[i].SHA < commits[j].SHA
	})
	return commits, nil
}

// reachableCommits bounds graph traversal and excludes the complete base history.
func reachableCommits(ctx context.Context, repo *git.Repository, head plumbing.Hash, excluded map[plumbing.Hash]*object.Commit) (map[plumbing.Hash]*object.Commit, error) {
	const maximumHistoryCommits = 100000
	seen := make(map[plumbing.Hash]*object.Commit)
	var bytesRead int64
	pending := []plumbing.Hash{head}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, classifyTransportError(err)
		}
		hash := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[hash] != nil || excluded[hash] != nil {
			continue
		}
		if len(seen) >= maximumHistoryCommits {
			return nil, &Error{Code: CodeResponseTooLarge, Message: "The Git history exceeds the 100000-commit traversal limit."}
		}
		encoded, err := repo.Storer.EncodedObject(plumbing.CommitObject, hash)
		if err != nil {
			return nil, &Error{Code: CodeGogsError, Message: "Could not read a history commit.", cause: err}
		}
		bytesRead += encoded.Size()
		if encoded.Size() > 1<<20 || bytesRead > 64<<20 {
			return nil, &Error{Code: CodeResponseTooLarge, Message: "Git history exceeds 1 MiB per commit or 64 MiB of commit data."}
		}
		commit, err := object.DecodeCommit(repo.Storer, encoded)
		if err != nil {
			return nil, &Error{Code: CodeGogsError, Message: "Could not walk the pull request history.", cause: err}
		}
		seen[hash] = commit
		pending = append(pending, commit.ParentHashes...)
	}
	return seen, nil
}

// diffEntryPath derives the reported path from the tree change alone. A
// rename surfaces under its new name, which matches the filter matching of
// filterChanges and the b/ side of the diff header. A submodule change
// carries no readable file on either side, because gitlink entries have no
// file content, so the tree change is the only source either way.
func diffEntryPath(change *object.Change) string {
	if change.To != (object.ChangeEntry{}) {
		return change.To.Name
	}
	return change.From.Name
}

// diffChangeStatus maps a tree change onto the same status vocabulary the
// file-based diffs report.
func diffChangeStatus(change *object.Change) string {
	switch {
	case change.From == (object.ChangeEntry{}):
		return "added"
	case change.To == (object.ChangeEntry{}):
		return "deleted"
	case change.From.Name != change.To.Name:
		return "renamed"
	default:
		return "modified"
	}
}

func countChunkLines(filePatch fdiff.FilePatch, kind fdiff.Operation) int {
	count := 0
	for _, chunk := range filePatch.Chunks() {
		if chunk.Type() == kind {
			content := chunk.Content()
			count += strings.Count(content, "\n")
			// The last line of a chunk may lack a trailing newline, and it is
			// still a changed line.
			if content != "" && !strings.HasSuffix(content, "\n") {
				count++
			}
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

var errDiffLimit = errors.New("diff output limit reached")

type limitedDiffWriter struct {
	bytes.Buffer
	maximum   int
	truncated bool
}

func (w *limitedDiffWriter) Write(p []byte) (int, error) {
	if w.maximum <= 0 {
		return w.Buffer.Write(p)
	}
	remaining := w.maximum - w.Len()
	if len(p) <= remaining {
		return w.Buffer.Write(p)
	}
	n, _ := w.Buffer.Write(p[:remaining])
	w.truncated = true
	return n, errDiffLimit
}

func validatePatchWork(ctx context.Context, changes object.Changes) error {
	const maxChangedFiles = 2000
	const maxBlobBytes = 1 << 20
	const maxInputBytes = 8 << 20
	if len(changes) > maxChangedFiles {
		return &Error{Code: CodeResponseTooLarge, Message: "The diff exceeds 2000 changed files; narrow paths."}
	}
	var total int64
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return classifyTransportError(err)
		}
		from, to, err := change.Files()
		if err != nil {
			return classifyGitError(err)
		}
		for _, file := range []*object.File{from, to} {
			if file == nil {
				continue
			}
			total += file.Size
			if file.Size > maxBlobBytes || total > maxInputBytes {
				return &Error{Code: CodeResponseTooLarge, Message: "Diff inputs exceed 1 MiB per blob or 8 MiB total; narrow paths or inspect file metadata."}
			}
		}
	}
	return nil
}

func buildBoundedPullDiff(ctx context.Context, changes object.Changes, commits []PullCommit, base string, maxBytes int) (*PullDiff, error) {
	result := &PullDiff{MergeBase: base, Commits: commits, Files: []DiffFileStat{}}
	output := limitedDiffWriter{maximum: maxBytes}
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return nil, classifyTransportError(err)
		}
		patch, err := change.PatchContext(ctx)
		if err != nil {
			return nil, classifyGitError(err)
		}
		for _, file := range patch.FilePatches() {
			result.Files = append(result.Files, DiffFileStat{Path: diffEntryPath(change), Status: diffChangeStatus(change), Additions: countChunkLines(file, fdiff.Add), Deletions: countChunkLines(file, fdiff.Delete), IsBinary: file.IsBinary()})
		}
		if !output.truncated {
			if err := patch.Encode(&output); err != nil && !errors.Is(err, errDiffLimit) {
				return nil, classifyGitError(err)
			}
		}
	}
	if output.truncated {
		truncateToLastLine(&output.Buffer, maxBytes)
	}
	result.Diff = output.String()
	result.Truncated = output.truncated
	return result, nil
}

func boundedMergeBases(ctx context.Context, repo *git.Repository, head, base plumbing.Hash) ([]*object.Commit, error) {
	left, err := reachableCommits(ctx, repo, head, nil)
	if err != nil {
		return nil, err
	}
	right, err := reachableCommits(ctx, repo, base, nil)
	if err != nil {
		return nil, err
	}
	common := make(map[plumbing.Hash]*object.Commit)
	for hash, commit := range left {
		if right[hash] != nil {
			common[hash] = commit
		}
	}
	ancestors := make(map[plumbing.Hash]bool)
	for _, commit := range common {
		if err := ctx.Err(); err != nil {
			return nil, classifyTransportError(err)
		}
		for _, parent := range commit.ParentHashes {
			ancestors[parent] = true
		}
	}
	var bases []*object.Commit
	for hash, commit := range common {
		if !ancestors[hash] {
			bases = append(bases, commit)
		}
	}
	sort.Slice(bases, func(i, j int) bool { return bases[i].Hash.String() < bases[j].Hash.String() })
	return bases, nil
}

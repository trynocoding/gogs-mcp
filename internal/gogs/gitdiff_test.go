package gogs

import (
	"bytes"
	"context"
	"gogs-mcp/internal/diskcache"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildPullFixture creates a local repository that imitates the server side
// of a pull request: a main branch, one commit on a head branch, and the
// refs/pull/1/head ref that Gogs pushes onto the base repository. It returns
// the repository URL, the main commit, and the pull request head.
func buildPullFixture(t *testing.T) (*url.URL, plumbing.Hash, plumbing.Hash) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)

	signature := &object.Signature{Name: "Reader", Email: "reader@example.com", When: time.Now()}
	writeFile := func(name, content string) {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		_, err := worktree.Add(name)
		require.NoError(t, err)
	}

	writeFile("file.txt", "alpha\nbeta\ngamma\n")
	writeFile("old.txt", "goes away\n")
	mainHash, err := worktree.Commit("Base state", &git.CommitOptions{Author: signature, AllowEmptyCommits: true})
	require.NoError(t, err)

	// go-git initializes HEAD to master; align the fixture with the Gogs
	// default branch name.
	require.NoError(t, worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("main"),
		Create: true,
	}))

	require.NoError(t, worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("pr-branch"),
		Create: true,
	}))
	writeFile("file.txt", "alpha\nbeta v2\ngamma\n")
	require.NoError(t, os.Remove(filepath.Join(dir, "old.txt")))
	_, err = worktree.Remove("old.txt")
	require.NoError(t, err)
	// The content lacks a trailing newline on purpose: the added line must
	// still count as one addition.
	writeFile("new.txt", "arrives")
	headHash, err := worktree.Commit("Pull change", &git.CommitOptions{Author: signature, AllowEmptyCommits: true})
	require.NoError(t, err)

	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.ReferenceName("refs/pull/1/head"), headHash)))

	return &url.URL{Scheme: "file", Path: dir}, mainHash, headHash
}

func newTestPullEngine(t *testing.T) *PullEngine {
	t.Helper()
	engine, err := NewPullEngine(PullEngineOptions{
		CacheDir: t.TempDir(),
		Timeout:  30 * time.Second,
	})
	require.NoError(t, err)
	return engine
}

func TestListPullRefsFindsPullHeads(t *testing.T) {
	fixtureURL, _, headHash := buildPullFixture(t)
	engine := newTestPullEngine(t)

	refs, err := engine.ListPullRefs(context.Background(), fixtureURL)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, int64(1), refs[0].Number)
	assert.Equal(t, headHash.String(), refs[0].HeadSHA)
}

func TestListPullRefsReturnsEmptyForEmptyRepository(t *testing.T) {
	emptyDir := t.TempDir()
	_, err := git.PlainInit(emptyDir, false)
	require.NoError(t, err)
	engine := newTestPullEngine(t)

	refs, err := engine.ListPullRefs(context.Background(), &url.URL{Scheme: "file", Path: emptyDir})
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestListPullRefsOrdersPullHeads(t *testing.T) {
	fixtureURL, mainHash, headHash := buildPullFixture(t)
	repo, err := git.PlainOpen(fixtureURL.Path)
	require.NoError(t, err)
	// Seed extra heads out of order, including the lexicographic trap of a
	// two-digit number.
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.ReferenceName("refs/pull/10/head"), headHash)))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.ReferenceName("refs/pull/2/head"), mainHash)))
	engine := newTestPullEngine(t)

	refs, err := engine.ListPullRefs(context.Background(), fixtureURL)
	require.NoError(t, err)
	numbers := make([]int64, 0, len(refs))
	for _, ref := range refs {
		numbers = append(numbers, ref.Number)
	}
	assert.Equal(t, []int64{1, 2, 10}, numbers)
}

func TestDiffPullReportsRenameUnderNewPath(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)

	signature := &object.Signature{Name: "Reader", Email: "reader@example.com", When: time.Now()}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.txt"), []byte("rename me\n"), 0o644))
	_, err = worktree.Add("old.txt")
	require.NoError(t, err)
	_, err = worktree.Commit("Before rename", &git.CommitOptions{Author: signature})
	require.NoError(t, err)
	// go-git initializes HEAD to master; align the fixture with the base ref
	// the test fetches.
	require.NoError(t, worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("main"),
		Create: true,
	}))
	require.NoError(t, worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("pr-branch"),
		Create: true,
	}))
	_, err = worktree.Move("old.txt", "new.txt")
	require.NoError(t, err)
	headHash, err := worktree.Commit("Rename old.txt", &git.CommitOptions{Author: signature})
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.ReferenceName("refs/pull/1/head"), headHash)))
	engine := newTestPullEngine(t)

	for _, filters := range [][]string{nil, {"new.txt"}, {"old.txt"}} {
		diff, err := engine.DiffPull(context.Background(), &url.URL{Scheme: "file", Path: dir}, 1, "main", filters, 0)
		require.NoError(t, err)
		require.Len(t, diff.Files, 1)
		assert.Equal(t, "new.txt", diff.Files[0].Path)
		assert.Equal(t, "renamed", diff.Files[0].Status)
	}
	// Similarity-based renames must also retain both sides of a path filter.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("rename me\nextra\n"), 0600))
	_, err = worktree.Add("new.txt")
	require.NoError(t, err)
	edited, err := worktree.Commit("Edit renamed file", &git.CommitOptions{Author: signature})
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference("refs/pull/1/head", edited)))
	diff, err := engine.DiffPull(context.Background(), &url.URL{Scheme: "file", Path: dir}, 1, "main", []string{"new.txt"}, 0)
	require.NoError(t, err)
	require.Len(t, diff.Files, 1)
	assert.Equal(t, "renamed", diff.Files[0].Status)

}

func TestDiffPullProducesStandardUnifiedDiff(t *testing.T) {
	fixtureURL, mainHash, headHash := buildPullFixture(t)
	engine := newTestPullEngine(t)

	diff, err := engine.DiffPull(context.Background(), fixtureURL, 1, "main", nil, 0)
	require.NoError(t, err)

	assert.Equal(t, mainHash.String(), diff.MergeBase)
	assert.False(t, diff.Truncated)
	// The base branch has not moved past the merge base.
	assert.Equal(t, MergeStateFastForward, diff.MergeState)
	assert.Zero(t, diff.BaseCommits)

	require.Len(t, diff.Files, 3)
	// Entries follow the lexicographic order of the unified diff.
	assert.Equal(t, DiffFileStat{Path: "file.txt", Status: "modified", Additions: 1, Deletions: 1}, diff.Files[0])
	assert.Equal(t, DiffFileStat{Path: "new.txt", Status: "added", Additions: 1}, diff.Files[1])
	assert.Equal(t, DiffFileStat{Path: "old.txt", Status: "deleted", Deletions: 1}, diff.Files[2])

	assert.Contains(t, diff.Diff, "diff --git a/file.txt b/file.txt")
	assert.Contains(t, diff.Diff, "@@ -1,3 +1,3 @@")
	assert.Contains(t, diff.Diff, "-beta\n+beta v2")

	require.Len(t, diff.Commits, 1)
	assert.Equal(t, "Pull change", diff.Commits[0].Message)
	assert.Equal(t, headHash.String(), diff.Commits[0].SHA)
}

func TestDiffPullTruncatesAtByteLimit(t *testing.T) {
	fixtureURL, _, _ := buildPullFixture(t)
	engine := newTestPullEngine(t)

	diff, err := engine.DiffPull(context.Background(), fixtureURL, 1, "main", nil, 120)
	require.NoError(t, err)
	assert.True(t, diff.Truncated)
	assert.LessOrEqual(t, len(diff.Diff), 120)
	assert.True(t, strings.HasSuffix(diff.Diff, "\n"), "truncation must not leave a partial line")
	// The file stats are unaffected by the diff text truncation.
	assert.Len(t, diff.Files, 3)
}

func TestDiffPullRejectsBadArguments(t *testing.T) {
	fixtureURL, _, _ := buildPullFixture(t)
	engine := newTestPullEngine(t)
	ctx := context.Background()

	_, err := engine.DiffPull(ctx, fixtureURL, 0, "main", nil, 0)
	require.Error(t, err)
	assert.Equal(t, CodeInvalidArgument, AsError(err).Code)

	_, err = engine.DiffPull(ctx, fixtureURL, 1, "", nil, 0)
	require.Error(t, err)
	assert.Equal(t, CodeInvalidArgument, AsError(err).Code)

	_, err = engine.DiffPull(ctx, fixtureURL, 1, "no-such-branch", nil, 0)
	require.Error(t, err)
	assert.Equal(t, CodeInvalidArgument, AsError(err).Code)
}

func TestDiffPullRejectsRepositoryAboveCacheLimit(t *testing.T) {
	fixtureURL, _, _ := buildPullFixture(t)
	engine, err := NewPullEngine(PullEngineOptions{
		CacheDir:      t.TempDir(),
		CacheMaxBytes: 1, // even the smallest fixture fetches more than one byte
	})
	require.NoError(t, err)
	ctx := context.Background()

	diff, err := engine.DiffPull(ctx, fixtureURL, 1, "main", nil, 0)
	require.Error(t, err)
	assert.Nil(t, diff)
	assert.Equal(t, CodeCacheCapacityExceeded, AsError(err).Code)
	assert.True(t, AsError(err).Retryable, "raising the limit must make the call retryable")

	// The oversized repository must not linger until the next call.
	entries, err := os.ReadDir(filepath.Join(engine.cacheDir, ".pull"))
	require.NoError(t, err)
	assert.Empty(t, entries, "a repository above the limit must not be cached")
}

func TestDiffPullKeepsAggregateWithinCacheLimit(t *testing.T) {
	urlA, _, _ := buildPullFixture(t)
	urlB, _, _ := buildPullFixture(t)
	ctx := context.Background()

	// The probe measures the fixture sizes, so the limit below fits either
	// repository alone but not both together.
	probe := newTestPullEngine(t)
	_, err := probe.DiffPull(ctx, urlA, 1, "main", nil, 0)
	require.NoError(t, err)
	_, err = probe.DiffPull(ctx, urlB, 1, "main", nil, 0)
	require.NoError(t, err)
	limit := max(dirSize(probe.repoPathFor(urlA)), dirSize(probe.repoPathFor(urlB)))

	engine, err := NewPullEngine(PullEngineOptions{CacheDir: t.TempDir(), CacheMaxBytes: limit})
	require.NoError(t, err)

	_, err = engine.DiffPull(ctx, urlA, 1, "main", nil, 0)
	require.NoError(t, err)
	// Fetching B pushes the total above the limit; the older repository A
	// must be evicted instead of breaching the bound.
	_, err = engine.DiffPull(ctx, urlB, 1, "main", nil, 0)
	require.NoError(t, err)

	entries, err := os.ReadDir(filepath.Join(engine.cacheDir, ".pull"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "the cache must keep only the freshly fetched repository")
	assert.Equal(t, filepath.Base(engine.repoPathFor(urlB)), entries[0].Name())
	assert.LessOrEqual(t, dirSize(filepath.Join(engine.cacheDir, ".pull")), limit)
}

func TestDiffPullReusesCacheRepository(t *testing.T) {
	fixtureURL, _, _ := buildPullFixture(t)
	engine, err := NewPullEngine(PullEngineOptions{CacheDir: t.TempDir()})
	require.NoError(t, err)
	ctx := context.Background()

	_, err = engine.DiffPull(ctx, fixtureURL, 1, "main", nil, 0)
	require.NoError(t, err)
	entries, err := os.ReadDir(filepath.Join(engine.cacheDir, ".pull"))
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// A second call must not create another cache repository.
	_, err = engine.DiffPull(ctx, fixtureURL, 1, "main", nil, 0)
	require.NoError(t, err)
	entries, err = os.ReadDir(filepath.Join(engine.cacheDir, ".pull"))
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	require.NoError(t, engine.CleanCache())
	entries, err = os.ReadDir(engine.cacheDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, ".locks", entries[0].Name())
}

func TestDiffPullCreatesPrivateCacheRepository(t *testing.T) {
	fixtureURL, _, _ := buildPullFixture(t)
	engine := newTestPullEngine(t)

	_, err := engine.DiffPull(context.Background(), fixtureURL, 1, "main", nil, 0)
	require.NoError(t, err)

	repoPath := engine.repoPathFor(fixtureURL)
	info, err := os.Stat(repoPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
		"the cache repository holds fetched repository objects and must be private")
}

func TestDiffPullTightensLegacyCachePermissions(t *testing.T) {
	fixtureURL, _, _ := buildPullFixture(t)
	engine := newTestPullEngine(t)
	ctx := context.Background()

	_, err := engine.DiffPull(ctx, fixtureURL, 1, "main", nil, 0)
	require.NoError(t, err)
	repoPath := engine.repoPathFor(fixtureURL)
	// Imitate a repository created before the cache became private; an
	// actively used repository never ages out through the TTL sweep.
	require.NoError(t, os.Chmod(repoPath, 0o755))

	_, err = engine.DiffPull(ctx, fixtureURL, 1, "main", nil, 0)
	require.NoError(t, err)
	info, err := os.Stat(repoPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestParsePullRef(t *testing.T) {
	testCases := map[string]struct {
		ref    string
		number int64
		ok     bool
	}{
		"head ref":        {ref: "refs/pull/12/head", number: 12, ok: true},
		"single digit":    {ref: "refs/pull/1/head", number: 1, ok: true},
		"not a pull ref":  {ref: "refs/heads/main"},
		"other ref dir":   {ref: "refs/tags/1/head"},
		"missing head":    {ref: "refs/pull/12"},
		"not a number":    {ref: "refs/pull/abc/head"},
		"zero number":     {ref: "refs/pull/0/head"},
		"negative number": {ref: "refs/pull/-3/head"},
	}
	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			number, ok := parsePullRef(testCase.ref)
			assert.Equal(t, testCase.ok, ok)
			assert.Equal(t, testCase.number, number)
		})
	}
}

func TestTruncateToLastLine(t *testing.T) {
	testCases := map[string]struct {
		content  string
		maxBytes int
		expected string
	}{
		"whole buffer fits":   {content: "a\nbb\nccc\n", maxBytes: 10, expected: "a\nbb\nccc\n"},
		"exact buffer length": {content: "aaaa\nbbbb\n", maxBytes: 10, expected: "aaaa\nbbbb\n"},
		"cut to line start":   {content: "aaaa\nbbbb\ncccc\n", maxBytes: 9, expected: "aaaa\n"},
		"first line too long": {content: "abcdefgh\nx\n", maxBytes: 3, expected: ""},
	}
	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			buffer := new(bytes.Buffer)
			buffer.WriteString(testCase.content)
			truncateToLastLine(buffer, testCase.maxBytes)
			assert.Equal(t, testCase.expected, buffer.String())
			assert.LessOrEqual(t, buffer.Len(), testCase.maxBytes)
		})
	}
}

// buildDivergedPullFixture extends the pull request fixture with a commit on
// the base branch, so the merge base no longer equals the base tip. When
// baseTouchesCommon is set, the base commit rewrites file.txt, which the head
// branch also touches; otherwise the base commit only adds a separate file.
func buildDivergedPullFixture(t *testing.T, baseTouchesCommon bool) *url.URL {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)

	signature := &object.Signature{Name: "Reader", Email: "reader@example.com", When: time.Now()}
	writeFile := func(name, content string) {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		_, err := worktree.Add(name)
		require.NoError(t, err)
	}

	writeFile("file.txt", "one\ntwo\n")
	_, err = worktree.Commit("Base", &git.CommitOptions{Author: signature, AllowEmptyCommits: true})
	require.NoError(t, err)

	require.NoError(t, worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("main"),
		Create: true,
	}))
	require.NoError(t, worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("pr-branch"),
		Create: true,
	}))
	writeFile("file.txt", "one\ntwo v2\n")
	headHash, err := worktree.Commit("Pull change", &git.CommitOptions{Author: signature, AllowEmptyCommits: true})
	require.NoError(t, err)

	require.NoError(t, worktree.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("main")}))
	if baseTouchesCommon {
		writeFile("file.txt", "one\ntwo v3\n")
	} else {
		writeFile("docs.md", "documentation\n")
	}
	_, err = worktree.Commit("Base moves on", &git.CommitOptions{Author: signature, AllowEmptyCommits: true})
	require.NoError(t, err)

	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.ReferenceName("refs/pull/1/head"), headHash)))

	return &url.URL{Scheme: "file", Path: dir}
}

func TestDiffPullReportsMergeStates(t *testing.T) {
	t.Run("fast forward", func(t *testing.T) {
		fixtureURL, _, _ := buildPullFixture(t)
		engine := newTestPullEngine(t)
		diff, err := engine.DiffPull(context.Background(), fixtureURL, 1, "main", nil, 0)
		require.NoError(t, err)
		assert.Equal(t, MergeStateFastForward, diff.MergeState)
		assert.Empty(t, diff.MergeConflictPaths)
		assert.Zero(t, diff.BaseCommits)
	})

	t.Run("diverged", func(t *testing.T) {
		fixtureURL := buildDivergedPullFixture(t, false)
		engine := newTestPullEngine(t)
		diff, err := engine.DiffPull(context.Background(), fixtureURL, 1, "main", nil, 0)
		require.NoError(t, err)
		assert.Equal(t, MergeStateDiverged, diff.MergeState)
		assert.Empty(t, diff.MergeConflictPaths)
		assert.Equal(t, 1, diff.BaseCommits)
	})

	t.Run("conflicting", func(t *testing.T) {
		fixtureURL := buildDivergedPullFixture(t, true)
		engine := newTestPullEngine(t)
		diff, err := engine.DiffPull(context.Background(), fixtureURL, 1, "main", nil, 0)
		require.NoError(t, err)
		assert.Equal(t, MergeStateConflicting, diff.MergeState)
		assert.Equal(t, []string{"file.txt"}, diff.MergeConflictPaths)
		assert.Equal(t, 1, diff.BaseCommits)
	})
}

func TestDiffPullFiltersByPath(t *testing.T) {
	fixtureURL, _, _ := buildPullFixture(t)
	engine := newTestPullEngine(t)
	ctx := context.Background()

	diff, err := engine.DiffPull(ctx, fixtureURL, 1, "main", []string{"file.txt"}, 0)
	require.NoError(t, err)
	require.Len(t, diff.Files, 1)
	assert.Equal(t, "file.txt", diff.Files[0].Path)
	assert.Contains(t, diff.Diff, "diff --git a/file.txt b/file.txt")
	assert.NotContains(t, diff.Diff, "new.txt")
	// The commit list is not filtered, and the merge state keeps describing
	// the whole pull request.
	require.Len(t, diff.Commits, 1)
	assert.Equal(t, MergeStateFastForward, diff.MergeState)

	// A trailing slash selects everything under a directory; the cache
	// repository is stored under a .git suffix directory that must not leak
	// into the results.
	diff, err = engine.DiffPull(ctx, fixtureURL, 1, "main", []string{"old.txt", "docs/"}, 0)
	require.NoError(t, err)
	require.Len(t, diff.Files, 1)
	assert.Equal(t, "old.txt", diff.Files[0].Path)

	diff, err = engine.DiffPull(ctx, fixtureURL, 1, "main", []string{"   ", ""}, 0)
	require.NoError(t, err)
	assert.Len(t, diff.Files, 3, "blank filters must not filter anything")
}

func TestDiffPullWarnsWhenFiltersMatchNothing(t *testing.T) {
	fixtureURL, _, _ := buildPullFixture(t)
	engine := newTestPullEngine(t)

	diff, err := engine.DiffPull(context.Background(), fixtureURL, 1, "main", []string{"absent/"}, 0)
	require.NoError(t, err)
	assert.Empty(t, diff.Files)
	assert.Empty(t, diff.Diff)
	assert.Equal(t, MergeStateFastForward, diff.MergeState)
}

func TestSweepPullCacheEvictsExpiredAndOversizedRepositories(t *testing.T) {
	newCache := func(t *testing.T, ttl time.Duration, maxBytes int64) (*PullEngine, string, string) {
		t.Helper()
		root := filepath.Join(t.TempDir(), ".pull")
		stale := filepath.Join(root, "stale.git")
		fresh := filepath.Join(root, "fresh.git")
		for _, path := range []string{stale, fresh} {
			require.NoError(t, os.MkdirAll(path, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(path, "objects.pack"), []byte("payload"), 0o644))
		}
		engine, err := NewPullEngine(PullEngineOptions{
			CacheDir:      filepath.Dir(root),
			CacheTTL:      ttl,
			CacheMaxBytes: maxBytes,
		})
		require.NoError(t, err)
		return engine, stale, fresh
	}

	t.Run("expired by ttl", func(t *testing.T) {
		engine, stale, fresh := newCache(t, time.Hour, 0)
		old := time.Now().Add(-2 * time.Hour)
		require.NoError(t, os.Chtimes(stale, old, old))
		require.NoError(t, (&diskcache.Budget{Root: engine.cacheDir, Keep: fresh, MaxBytes: engine.cacheMaxBytes, TTL: engine.cacheTTL}).MakeRoom(0))
		assert.NoDirExists(t, stale)
		assert.DirExists(t, fresh)
	})

	t.Run("evicted by capacity oldest first", func(t *testing.T) {
		// Each repository payload is 7 bytes; a limit of 8 fits exactly one.
		engine, stale, fresh := newCache(t, time.Hour, 8)
		// fresh is newer, so stale must go first; the freed bytes then cover
		// the kept repository.
		now := time.Now()
		require.NoError(t, os.Chtimes(stale, now.Add(-time.Minute), now.Add(-time.Minute)))
		require.NoError(t, (&diskcache.Budget{Root: engine.cacheDir, Keep: fresh, MaxBytes: engine.cacheMaxBytes, TTL: engine.cacheTTL}).MakeRoom(0))
		assert.NoDirExists(t, stale)
		assert.DirExists(t, fresh)
	})

	t.Run("active repository prevents overcommit", func(t *testing.T) {
		engine, _, fresh := newCache(t, time.Hour, 1)
		require.ErrorIs(t, (&diskcache.Budget{Root: engine.cacheDir, Keep: fresh, MaxBytes: engine.cacheMaxBytes}).MakeRoom(0), diskcache.ErrCapacity)
		assert.DirExists(t, fresh)
	})

	t.Run("untouched within ttl", func(t *testing.T) {
		engine, stale, fresh := newCache(t, time.Hour, 0)
		require.NoError(t, (&diskcache.Budget{Root: engine.cacheDir, Keep: fresh, MaxBytes: engine.cacheMaxBytes, TTL: engine.cacheTTL}).MakeRoom(0))
		assert.DirExists(t, stale)
	})
}

func TestNewPullEngineDefaultsCacheBounds(t *testing.T) {
	engine, err := NewPullEngine(PullEngineOptions{CacheDir: t.TempDir(), CacheTTL: -time.Second, CacheMaxBytes: -1})
	require.NoError(t, err)
	assert.Equal(t, defaultPullCacheTTL, engine.cacheTTL)
	assert.Equal(t, defaultPullCacheMaxBytes, engine.cacheMaxBytes)
}

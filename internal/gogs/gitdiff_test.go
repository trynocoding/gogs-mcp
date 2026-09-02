package gogs

import (
	"bytes"
	"context"
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
	writeFile("new.txt", "arrives\n")
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

func TestDiffPullProducesStandardUnifiedDiff(t *testing.T) {
	fixtureURL, mainHash, headHash := buildPullFixture(t)
	engine := newTestPullEngine(t)

	diff, err := engine.DiffPull(context.Background(), fixtureURL, 1, "main", 0)
	require.NoError(t, err)

	assert.Equal(t, mainHash.String(), diff.MergeBase)
	assert.False(t, diff.Truncated)

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

	diff, err := engine.DiffPull(context.Background(), fixtureURL, 1, "main", 120)
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

	_, err := engine.DiffPull(ctx, fixtureURL, 0, "main", 0)
	require.Error(t, err)
	assert.Equal(t, CodeInvalidArgument, AsError(err).Code)

	_, err = engine.DiffPull(ctx, fixtureURL, 1, "", 0)
	require.Error(t, err)
	assert.Equal(t, CodeInvalidArgument, AsError(err).Code)

	_, err = engine.DiffPull(ctx, fixtureURL, 1, "no-such-branch", 0)
	require.Error(t, err)
	assert.Equal(t, CodeInvalidArgument, AsError(err).Code)
}

func TestDiffPullReusesCacheRepository(t *testing.T) {
	fixtureURL, _, _ := buildPullFixture(t)
	engine, err := NewPullEngine(PullEngineOptions{CacheDir: t.TempDir()})
	require.NoError(t, err)
	ctx := context.Background()

	_, err = engine.DiffPull(ctx, fixtureURL, 1, "main", 0)
	require.NoError(t, err)
	entries, err := os.ReadDir(filepath.Join(engine.cacheDir, "pull"))
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// A second call must not create another cache repository.
	_, err = engine.DiffPull(ctx, fixtureURL, 1, "main", 0)
	require.NoError(t, err)
	entries, err = os.ReadDir(filepath.Join(engine.cacheDir, "pull"))
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	require.NoError(t, engine.CleanCache())
	entries, err = os.ReadDir(engine.cacheDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
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

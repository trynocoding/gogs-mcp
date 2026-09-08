package gogs

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogs-mcp/internal/diskcache"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPullEngineInheritsPrivateCAAndTimeout(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/user" {
			writeResponse(t, w, `{"id":9,"username":"reader"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
	root, err := url.Parse(server.URL + "/api/v1/")
	require.NoError(t, err)
	client, err := NewClient(Options{APIRoot: root, Token: "secret-token", CAFile: ca, Timeout: 7 * time.Second, CacheDir: t.TempDir()})
	require.NoError(t, err)
	engine, err := client.pullEngine(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 7*time.Second, engine.timeout)
	_, err = engine.ListPullRefs(context.Background(), client.gitCloneURL("owner", "repo"))
	require.Error(t, err)
	assert.Equal(t, CodeResourceNotFoundOrForbidden, AsError(err).Code)
	repo, err := git.PlainInit(t.TempDir(), true)
	require.NoError(t, err)
	err = engine.fetchRefs(context.Background(), repo, client.gitCloneURL("owner", "repo"), 1, "main")
	require.Error(t, err)
	assert.Equal(t, CodeResourceNotFoundOrForbidden, AsError(err).Code)
}

func TestPullCommitsIncludeMergedSideBranch(t *testing.T) {
	u, base, first := buildPullFixture(t)
	repo, err := git.PlainOpen(u.Path)
	require.NoError(t, err)
	parent, err := repo.CommitObject(first)
	require.NoError(t, err)
	signature := object.Signature{Name: "audit", Email: "audit@example.com", When: time.Now()}
	put := func(message string, parents []plumbing.Hash) plumbing.Hash {
		commit := &object.Commit{Author: signature, Committer: signature, Message: message, TreeHash: parent.TreeHash, ParentHashes: parents}
		obj := repo.Storer.NewEncodedObject()
		require.NoError(t, commit.Encode(obj))
		hash, err := repo.Storer.SetEncodedObject(obj)
		require.NoError(t, err)
		return hash
	}
	side := put("side commit", []plumbing.Hash{base})
	head := put("merge side", []plumbing.Hash{first, side})
	commits, err := commitsSince(context.Background(), repo, base, head)
	require.NoError(t, err)
	var got []string
	for _, commit := range commits {
		got = append(got, commit.SHA)
	}
	assert.ElementsMatch(t, []string{first.String(), side.String(), head.String()}, got)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = commitsSince(canceled, repo, base, head)
	require.Error(t, err)
}

func TestIndependentPullEnginesCoordinateSharedRepository(t *testing.T) {
	u, _, _ := buildPullFixture(t)
	cacheDir := t.TempDir()
	first, err := NewPullEngine(PullEngineOptions{CacheDir: cacheDir})
	require.NoError(t, err)
	second, err := NewPullEngine(PullEngineOptions{CacheDir: cacheDir})
	require.NoError(t, err)
	var group sync.WaitGroup
	for index := range 12 {
		group.Go(func() {
			engine, branch := first, "main"
			if index%2 == 1 {
				engine, branch = second, "pr-branch"
			}
			diff, err := engine.DiffPull(context.Background(), u, 1, branch, nil, 64<<10)
			if err != nil {
				t.Errorf("operation failed: %v", err)
				return
			}
			if branch == "main" {
				assert.NotEmpty(t, diff.Files)
			} else {
				assert.Empty(t, diff.Files)
			}
		})
	}
	group.Wait()
	lock, err := diskcache.Acquire(context.Background(), cacheDir, cacheDir+".writer", false)
	require.NoError(t, err)
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = first.DiffPull(ctx, u, 1, "main", nil, 1024)
	require.Error(t, err)
	assert.Equal(t, CodeTimeout, AsError(err).Code)
}

func TestPullListingBoundsScansAndContinuesThroughFilteredPages(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var number int64
		_, err := fmt.Sscanf(r.URL.Path, "/api/v1/repos/owner/repo/issues/%d", &number)
		assert.NoError(t, err)
		writeResponse(t, w, fmt.Sprintf(`{"number":%d,"state":"closed"}`, number))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/api/v1/", "secret-token", "", time.Second)
	refs := make([]PullRef, 250)
	for i := range refs {
		refs[i].Number = int64(i + 1)
	}
	var cursor int64
	for _, expected := range []int{100, 100, 50} {
		calls.Store(0)
		found, total, next, err := client.scanPullRefs(context.Background(), "owner", "repo", "open", 1, cursor, refs)
		require.NoError(t, err)
		assert.Empty(t, found)
		assert.Equal(t, 250, total)
		assert.Equal(t, int32(expected), calls.Load())
		cursor = next
	}
	assert.Zero(t, cursor)
}

func TestBoundedDiffWriterStopsBeforeAllocatingFullPatch(t *testing.T) {
	writer := limitedDiffWriter{maximum: 16}
	_, err := writer.Write(make([]byte, 1024))
	require.ErrorIs(t, err, errDiffLimit)
	assert.Equal(t, 16, writer.Len())
	u, _, _ := buildPullFixture(t)
	repo, err := git.PlainOpen(u.Path)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(u.Path, "huge.txt"), make([]byte, 2<<20), 0600))
	_, err = worktree.Add("huge.txt")
	require.NoError(t, err)
	hash, err := worktree.Commit("huge", &git.CommitOptions{Author: &object.Signature{Name: "reader", Email: "reader@example.com", When: time.Now()}})
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference("refs/pull/1/head", hash)))
	engine := newTestPullEngine(t)
	_, err = engine.DiffPull(context.Background(), u, 1, "main", nil, 1)
	require.Error(t, err)
	assert.Equal(t, CodeResponseTooLarge, AsError(err).Code)
	diff, err := engine.DiffPull(context.Background(), u, 1, "main", []string{"new.txt"}, 1024)
	require.NoError(t, err)
	require.Len(t, diff.Files, 1)
	assert.True(t, sort.SliceIsSorted(diff.Commits, func(i, j int) bool {
		if diff.Commits[i].Date != diff.Commits[j].Date {
			return diff.Commits[i].Date > diff.Commits[j].Date
		}
		return diff.Commits[i].SHA < diff.Commits[j].SHA
	}))
}

func TestCancellationDoesNotMakeUnknownWritesRetryable(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		classified := AsError(classifyWriteTransportError(cause))
		assert.Equal(t, CodeWriteOutcomeUnknown, classified.Code)
		assert.False(t, classified.Retryable)
	}
}

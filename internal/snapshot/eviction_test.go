package snapshot

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newEvictionManager(t *testing.T, eviction Eviction) *Manager {
	t.Helper()
	manager, err := NewManager(t.TempDir(), testInstance, DefaultLimits(), eviction)
	require.NoError(t, err)
	return manager
}

func keyWithSHA(suffix string) Key {
	return Key{UserID: 7, Owner: "owner", Repo: "repo", CommitSHA: strings.Repeat(suffix, 40)}
}

func commitDirOf(t *testing.T, result Result) string {
	t.Helper()
	return filepath.Dir(result.Dir)
}

// backdate sets the commit directory mtime into the past, since it is the
// eviction clock.
func backdate(t *testing.T, commitDir string, age time.Duration) {
	t.Helper()
	past := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(commitDir, past, past))
}

func oneLineArchive(t *testing.T, line string) func(context.Context) (io.ReadCloser, error) {
	t.Helper()
	return tarball(t, regularEntry("file.txt", line+"\n"))
}

func TestEvictionExpiresUntouchedSnapshots(t *testing.T) {
	manager := newEvictionManager(t, Eviction{TTL: time.Hour})
	ctx := context.Background()

	first, err := manager.Ensure(ctx, keyWithSHA("a"), oneLineArchive(t, "first"))
	require.NoError(t, err)
	first.Release()
	backdate(t, commitDirOf(t, first), 2*time.Hour)

	second, err := manager.Ensure(ctx, keyWithSHA("b"), oneLineArchive(t, "second"))
	require.NoError(t, err)
	second.Release()

	_, err = os.Stat(commitDirOf(t, first))
	assert.True(t, os.IsNotExist(err), "the expired snapshot must be removed")
	info, err := os.Stat(commitDirOf(t, second))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestEvictionTouchExtendsTTL(t *testing.T) {
	manager := newEvictionManager(t, Eviction{TTL: time.Hour})
	ctx := context.Background()
	first, err := manager.Ensure(ctx, keyWithSHA("a"), oneLineArchive(t, "first"))
	require.NoError(t, err)
	commitDir := commitDirOf(t, first)
	backdate(t, commitDir, 2*time.Hour)

	// A cache hit touches the snapshot, which must protect it from the sweep
	// triggered by the next miss.
	repeat, err := manager.Ensure(ctx, keyWithSHA("a"), oneLineArchive(t, "first"))
	require.NoError(t, err)
	assert.True(t, repeat.CacheHit)
	repeat.Release()
	first.Release()

	info, err := os.Stat(commitDir)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), info.ModTime(), time.Minute)

	second, err := manager.Ensure(ctx, keyWithSHA("b"), oneLineArchive(t, "second"))
	require.NoError(t, err)
	second.Release()
	_, err = os.Stat(commitDir)
	require.NoError(t, err, "the touched snapshot must survive the sweep")
	_, err = os.Stat(commitDirOf(t, second))
	require.NoError(t, err)
}

func TestEvictionCapacityEvictsLeastRecentlyUsed(t *testing.T) {
	manager := newEvictionManager(t, Eviction{TTL: 0})
	ctx := context.Background()

	first, err := manager.Ensure(ctx, keyWithSHA("a"), oneLineArchive(t, "first"))
	require.NoError(t, err)
	first.Release()
	second, err := manager.Ensure(ctx, keyWithSHA("b"), oneLineArchive(t, "second"))
	require.NoError(t, err)
	second.Release()
	backdate(t, commitDirOf(t, first), 2*time.Hour)
	backdate(t, commitDirOf(t, second), time.Hour)

	// Shrinking the capacity below the current usage forces the next Ensure to
	// evict the least recently used snapshot before downloading.
	manager.eviction.MaxBytes = directorySize(commitDirOf(t, second))*2 + 64

	third, err := manager.Ensure(ctx, keyWithSHA("c"), oneLineArchive(t, "third"))
	require.NoError(t, err)
	third.Release()

	_, err = os.Stat(commitDirOf(t, first))
	assert.True(t, os.IsNotExist(err), "the least recently used snapshot must be evicted")
	_, err = os.Stat(commitDirOf(t, second))
	require.NoError(t, err)
	_, err = os.Stat(commitDirOf(t, third))
	require.NoError(t, err)
}

func TestEvictionCapacityExceededKeepsInUseSnapshot(t *testing.T) {
	manager := newEvictionManager(t, Eviction{TTL: 0})
	ctx := context.Background()

	first, err := manager.Ensure(ctx, keyWithSHA("a"), oneLineArchive(t, "first"))
	require.NoError(t, err)
	commitDir := commitDirOf(t, first)
	manager.eviction.MaxBytes = directorySize(commitDir) - 1

	download, calls := unusedDownload()
	_, err = manager.Ensure(ctx, keyWithSHA("b"), download)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCacheCapacityExceeded)
	assert.Equal(t, 0, *calls, "the download must not start when no room can be made")
	_, err = os.Stat(commitDir)
	require.NoError(t, err, "the in-use snapshot must not be evicted")

	// Releasing the snapshot makes room for a smaller replacement.
	first.Release()
	second, err := manager.Ensure(ctx, keyWithSHA("b"), oneLineArchive(t, "x"))
	require.NoError(t, err)
	second.Release()
}

func TestEvictionTTLKeepsInUseSnapshot(t *testing.T) {
	manager := newEvictionManager(t, Eviction{TTL: time.Hour})
	ctx := context.Background()

	first, err := manager.Ensure(ctx, keyWithSHA("a"), oneLineArchive(t, "first"))
	require.NoError(t, err)
	commitDir := commitDirOf(t, first)
	backdate(t, commitDir, 2*time.Hour)

	second, err := manager.Ensure(ctx, keyWithSHA("b"), oneLineArchive(t, "second"))
	require.NoError(t, err)
	_, err = os.Stat(commitDir)
	require.NoError(t, err, "an in-use snapshot must not be evicted")

	second.Release()
	first.Release()
	third, err := manager.Ensure(ctx, keyWithSHA("c"), oneLineArchive(t, "third"))
	require.NoError(t, err)
	third.Release()
	_, err = os.Stat(commitDir)
	assert.True(t, os.IsNotExist(err), "the released snapshot becomes evictable again")
}

func TestRemoveUserRemovesOnlyThatUser(t *testing.T) {
	manager := newEvictionManager(t, DefaultEviction())
	ctx := context.Background()

	mine, err := manager.Ensure(ctx, keyWithSHA("a"), oneLineArchive(t, "mine"))
	require.NoError(t, err)
	mine.Release()
	otherKey := keyWithSHA("b")
	otherKey.UserID = 8
	other, err := manager.Ensure(ctx, otherKey, oneLineArchive(t, "other"))
	require.NoError(t, err)
	other.Release()

	require.NoError(t, manager.RemoveUser(8))
	_, err = os.Stat(commitDirOf(t, mine))
	require.NoError(t, err)
	_, err = os.Stat(commitDirOf(t, other))
	assert.True(t, os.IsNotExist(err))

	require.NoError(t, manager.RemoveUser(99), "a user without cache is a no-op")
	require.NoError(t, manager.RemoveUser(7))
}

func TestRemoveAllRemovesOnlyInstanceRoots(t *testing.T) {
	manager := newEvictionManager(t, DefaultEviction())
	ctx := context.Background()

	result, err := manager.Ensure(ctx, keyWithSHA("a"), oneLineArchive(t, "content"))
	require.NoError(t, err)
	result.Release()
	root := manager.root

	require.NoError(t, os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep"), 0o600))

	require.NoError(t, manager.RemoveAll())

	_, err = os.Stat(filepath.Dir(result.Dir))
	assert.True(t, os.IsNotExist(err), "the instance root must be removed")
	_, err = os.Stat(filepath.Join(root, "keep.txt"))
	require.NoError(t, err, "unrelated files must survive")
	_, err = os.Stat(filepath.Join(root, "tmp"))
	assert.True(t, os.IsNotExist(err), "temporary data lives within its user namespace")
	require.NoError(t, manager.RemoveAll(), "repeated cleanup is a no-op")
}

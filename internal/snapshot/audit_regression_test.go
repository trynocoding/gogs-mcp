package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gogs-mcp/internal/diskcache"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureRejectsSnapshotAboveQuotaBeforePublication(t *testing.T) {
	manager := newEvictionManager(t, Eviction{MaxBytes: 1, TTL: time.Hour})
	_, err := manager.Ensure(context.Background(), keyWithSHA("a"), oneLineArchive(t, "larger than quota"))
	require.ErrorIs(t, err, ErrCacheCapacityExceeded)
	root, err := manager.userRoot(7)
	require.NoError(t, err)
	size, err := diskcache.Size(root)
	require.NoError(t, err)
	assert.LessOrEqual(t, size, int64(1))
	commit, _, err := manager.directory(keyWithSHA("a"))
	require.NoError(t, err)
	assert.NoDirExists(t, commit)
}

func TestIndependentManagersProtectActiveSnapshots(t *testing.T) {
	first := newEvictionManager(t, Eviction{MaxBytes: 1024, TTL: time.Second})
	result, err := first.Ensure(context.Background(), keyWithSHA("a"), oneLineArchive(t, strings.Repeat("a", 400)))
	require.NoError(t, err)
	second, err := NewManager(first.root, testInstance, DefaultLimits(), Eviction{MaxBytes: 1024, TTL: time.Second})
	require.NoError(t, err)
	backdate(t, commitDirOf(t, result), time.Hour)
	_, err = second.Ensure(context.Background(), keyWithSHA("b"), oneLineArchive(t, strings.Repeat("b", 400)))
	require.ErrorIs(t, err, ErrCacheCapacityExceeded)
	content, err := os.ReadFile(result.Dir + "/file.txt")
	require.NoError(t, err)
	assert.Len(t, content, 401)
	require.Error(t, second.RemoveUser(7), "cleanup must also respect another manager's active reader")
	result.Release()
	replacement, err := second.Ensure(context.Background(), keyWithSHA("b"), oneLineArchive(t, strings.Repeat("b", 400)))
	require.NoError(t, err)
	replacement.Release()
	assert.NoDirExists(t, commitDirOf(t, result))
}

func TestConcurrentSnapshotAdmissionStaysWithinQuota(t *testing.T) {
	manager := newEvictionManager(t, Eviction{MaxBytes: 1024})
	archive := oneLineArchive(t, strings.Repeat("a", 400))
	var group sync.WaitGroup
	for _, suffix := range []string{"a", "b", "c", "d"} {
		group.Go(func() {
			result, err := manager.Ensure(context.Background(), keyWithSHA(suffix), archive)
			if err != nil {
				assert.ErrorIs(t, err, ErrCacheCapacityExceeded)
				return
			}
			defer result.Release()
			root, err := manager.userRoot(7)
			if err != nil {
				t.Errorf("operation failed: %v", err)
				return
			}
			used, err := diskcache.Size(root)
			if err != nil {
				t.Errorf("size failed: %v", err)
				return
			}
			assert.LessOrEqual(t, used, int64(1024))
		})
	}
	group.Wait()
}

func TestRepositoryNamesCannotBypassActiveSnapshotLocks(t *testing.T) {
	for _, name := range []string{"snapshot-demo", "project.git"} {
		t.Run(name, func(t *testing.T) {
			manager := newEvictionManager(t, Eviction{MaxBytes: 1024})
			key := keyWithSHA("a")
			key.Repo = name
			held, err := manager.Ensure(context.Background(), key, oneLineArchive(t, strings.Repeat("a", 400)))
			require.NoError(t, err)
			defer held.Release()
			_, err = manager.Ensure(context.Background(), keyWithSHA("b"), oneLineArchive(t, strings.Repeat("b", 400)))
			require.ErrorIs(t, err, ErrCacheCapacityExceeded)
			require.Error(t, manager.RemoveUser(7))
			data, err := os.ReadFile(held.Dir + "/file.txt")
			require.NoError(t, err)
			assert.Len(t, data, 401)
		})
	}
}

func TestAbandonedExtractionsExpireInUserNamespace(t *testing.T) {
	manager := newEvictionManager(t, DefaultEviction())
	root, err := manager.UserRoot(7)
	require.NoError(t, err)
	other, err := manager.UserRoot(8)
	require.NoError(t, err)
	stale := filepath.Join(root, ".tmp", "snapshot-abandoned")
	fresh := filepath.Join(root, ".tmp", "snapshot-recent")
	untouched := filepath.Join(other, ".tmp", "snapshot-abandoned")
	for _, dir := range []string{stale, fresh, untouched} {
		require.NoError(t, os.MkdirAll(dir, 0700))
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, dir := range []string{stale, untouched} {
		require.NoError(t, os.Chtimes(dir, old, old))
	}
	result, err := manager.Ensure(context.Background(), keyWithSHA("a"), oneLineArchive(t, "ready"))
	require.NoError(t, err)
	defer result.Release()
	assert.NoDirExists(t, stale, "crash leftovers expire after one hour, independent of the 24-hour cache TTL")
	assert.DirExists(t, fresh)
	assert.DirExists(t, untouched, "one user's writer lock cannot authorize cleaning another user")
}

package diskcache

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocksCoordinateIndependentProcesses(t *testing.T) {
	if path := os.Getenv("GOGS_CACHE_LOCK_TEST_PATH"); path != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := Acquire(ctx, filepath.Dir(path), path, false)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		return
	}
	path := filepath.Join(t.TempDir(), "entry")
	lock, err := Acquire(context.Background(), filepath.Dir(path), path, false)
	require.NoError(t, err)
	command := exec.Command(os.Args[0], "-test.run=^TestLocksCoordinateIndependentProcesses$")
	command.Env = append(os.Environ(), "GOGS_CACHE_LOCK_TEST_PATH="+path)
	output, err := command.CombinedOutput()
	lock.Close()
	require.NoError(t, err, string(output))
	lock, err = Acquire(context.Background(), filepath.Dir(path), path, false)
	require.NoError(t, err)
	lock.Close()
}
func TestGitFilesystemRejectsGrowthBeforeDiskWrite(t *testing.T) {
	root := t.TempDir()
	budget := &Budget{Root: root, MaxBytes: 5}
	fs := &Filesystem{Filesystem: osfs.New(root), Budget: budget}
	file, err := fs.Create("data")
	require.NoError(t, err)
	n, err := file.Write([]byte("hello"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	n, err = file.Write([]byte("!"))
	require.ErrorIs(t, err, ErrCapacity)
	assert.Zero(t, n)
	require.ErrorIs(t, file.Truncate(6), ErrCapacity)
	require.NoError(t, file.Close())
	content, err := os.ReadFile(filepath.Join(root, "data"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content))
}

func TestLocksUseCacheVolumeAndSurviveCleanup(t *testing.T) {
	root := t.TempDir()
	// A scratch container has no writable /tmp; only the configured volume exists.
	t.Setenv("TMPDIR", filepath.Join(root, "unavailable", "tmp"))
	identity := filepath.Join(root, "entry")
	lock, err := Acquire(context.Background(), root, identity, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "payload"), []byte("data"), 0600))
	require.NoError(t, RemoveTree(context.Background(), root, root))
	contender, err := TryLock(root, identity, false)
	require.NoError(t, err)
	require.Nil(t, contender, "cleanup must never replace a lock inode held by an existing process")
	lock.Close()
	contender, err = TryLock(root, identity, false)
	require.NoError(t, err)
	require.NotNil(t, contender)
	contender.Close()
	_, err = os.Stat(filepath.Join(root, "payload"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(os.Getenv("TMPDIR"))
	require.True(t, os.IsNotExist(err))
}

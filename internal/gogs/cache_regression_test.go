package gogs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"

	"gogs-mcp/internal/diskcache"
	"gogs-mcp/internal/snapshot"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitAndSnapshotsShareOneUserQuota(t *testing.T) {
	manager, err := snapshot.NewManager(t.TempDir(), "https://gogs.test/", snapshot.DefaultLimits(), snapshot.Eviction{MaxBytes: 4096})
	require.NoError(t, err)
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	body := strings.Repeat("a", 3500)
	require.NoError(t, tarWriter.WriteHeader(&tar.Header{Name: "file.txt", Mode: 0600, Size: int64(len(body))}))
	_, err = io.WriteString(tarWriter, body)
	require.NoError(t, err)
	require.NoError(t, tarWriter.Close())
	require.NoError(t, gzipWriter.Close())
	result, err := manager.Ensure(context.Background(), snapshot.Key{UserID: 7, Owner: "owner", Repo: "repo", CommitSHA: strings.Repeat("a", 40)}, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(archive.Bytes())), nil
	})
	require.NoError(t, err)
	root, err := manager.UserRoot(7)
	require.NoError(t, err)
	engine, err := NewPullEngine(PullEngineOptions{CacheDir: root, CacheMaxBytes: 4096})
	require.NoError(t, err)
	fixture, _, _ := buildPullFixture(t)
	_, err = engine.DiffPull(context.Background(), fixture, 1, "main", nil, 1024)
	require.Error(t, err)
	assert.Equal(t, CodeCacheCapacityExceeded, AsError(err).Code)
	used, err := diskcache.Size(root)
	require.NoError(t, err)
	assert.LessOrEqual(t, used, int64(4096))
	result.Release()
	_, err = engine.DiffPull(context.Background(), fixture, 1, "main", nil, 1024)
	require.NoError(t, err)
	used, err = diskcache.Size(root)
	require.NoError(t, err)
	assert.LessOrEqual(t, used, int64(4096))
}

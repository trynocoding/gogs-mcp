package gogs

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListPullRequestsRejectsUnknownStateBeforeAnyRequest(t *testing.T) {
	// An empty client must never be touched: the state check runs first.
	client := &Client{}
	_, _, _, err := client.ListPullRequests(context.Background(), "owner", "calculator", "merged", 30, 0)
	require.Error(t, err)
	assert.Equal(t, CodeInvalidArgument, AsError(err).Code)
}

func TestGitCloneURLStripsAPIRoot(t *testing.T) {
	testCases := map[string]string{
		"http://127.0.0.1:3000/api/v1":    "http://127.0.0.1:3000/owner/calculator.git",
		"http://127.0.0.1:3000/api/v1/":   "http://127.0.0.1:3000/owner/calculator.git",
		"http://127.0.0.1:3000/api/v1//":  "http://127.0.0.1:3000/owner/calculator.git",
		"https://gogs.example.com/api/v1": "https://gogs.example.com/owner/calculator.git",
	}
	for apiRoot, expected := range testCases {
		parsed, err := url.Parse(apiRoot)
		require.NoError(t, err)
		client := &Client{apiRoot: parsed}
		assert.Equal(t, expected, client.gitCloneURL("owner", "calculator").String(), apiRoot)
	}
}

func TestCleanPullCacheCleansDirectoryWithoutEngine(t *testing.T) {
	cacheDir := t.TempDir()
	pullDir := filepath.Join(cacheDir, "pull")
	require.NoError(t, os.MkdirAll(filepath.Join(pullDir, "abcdef01.git"), 0o700))

	client := &Client{cacheDir: cacheDir}
	require.NoError(t, client.CleanPullCache())

	_, err := os.Lstat(pullDir)
	assert.True(t, os.IsNotExist(err), "the pull cache directory must be removed")
}

func TestCleanPullCacheIsNoOpWithoutCacheDirectory(t *testing.T) {
	client := &Client{}
	assert.NoError(t, client.CleanPullCache())
}

func TestCleanPullCacheDirRejectsEmptyDirectory(t *testing.T) {
	assert.NoError(t, CleanPullCacheDir(""))
}

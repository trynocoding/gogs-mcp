package snapshot

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testInstance = "https://gogs.example.test/"

func newTestManager(t *testing.T, limits Limits) *Manager {
	t.Helper()
	manager, err := NewManager(t.TempDir(), testInstance, limits)
	require.NoError(t, err)
	return manager
}

func testKey() Key {
	return Key{UserID: 7, Owner: "owner", Repo: "repo", CommitSHA: strings.Repeat("a", 40)}
}

// tarball builds an in-memory gzip-compressed tar with the given entries.
type tarEntry struct {
	Header *tar.Header
	Body   []byte
}

func tarball(t *testing.T, entries ...tarEntry) func(context.Context) (io.ReadCloser, error) {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(writer)
	for _, entry := range entries {
		require.NoError(t, archive.WriteHeader(entry.Header))
		if len(entry.Body) > 0 {
			_, err := archive.Write(entry.Body)
			require.NoError(t, err)
		}
	}
	require.NoError(t, archive.Close())
	require.NoError(t, writer.Close())

	body := buffer.Bytes()
	return func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

func regularEntry(name, body string) tarEntry {
	return tarEntry{Header: &tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(body))}, Body: []byte(body)}
}

func dirEntry(name string) tarEntry {
	return tarEntry{Header: &tar.Header{Name: name, Typeflag: tar.TypeDir}}
}

// unusedDownload counts how often a handler tried to download an archive.
func unusedDownload() (func(context.Context) (io.ReadCloser, error), *int) {
	calls := 0
	return func(context.Context) (io.ReadCloser, error) {
		calls++
		return nil, errors.New("unexpected download")
	}, &calls
}

func TestEnsureExtractsSnapshotWithMetadata(t *testing.T) {
	manager := newTestManager(t, DefaultLimits())
	prefix := "repo-" + strings.Repeat("a", 7)
	download := tarball(t,
		dirEntry(prefix+"/"),
		regularEntry(prefix+"/src/main.go", "package main\n"),
		regularEntry(prefix+"/README.md", "# title\n"),
	)

	result, err := manager.Ensure(context.Background(), testKey(), download)
	require.NoError(t, err)
	assert.False(t, result.CacheHit)
	assert.Equal(t, 0, result.SkippedEntries)

	body, err := os.ReadFile(filepath.Join(filepath.Dir(result.Dir), "metadata.json"))
	require.NoError(t, err)
	var metadata Metadata
	require.NoError(t, json.Unmarshal(body, &metadata))
	assert.Equal(t, metadataSchemaVersion, metadata.SchemaVersion)
	assert.Equal(t, int64(7), metadata.UserID)
	assert.Equal(t, "owner/repo", metadata.Repository)
	assert.Equal(t, strings.Repeat("a", 40), metadata.CommitSHA)
	assert.Equal(t, 2, metadata.FileCount)
	assert.Equal(t, int64(len("package main\n")+len("# title\n")), metadata.TotalBytes)
	instanceHash, err := normalizedInstanceHash(testInstance)
	require.NoError(t, err)
	assert.Equal(t, instanceHash, metadata.InstanceHash)
	assert.False(t, metadata.CreatedAt.IsZero())
	assert.False(t, metadata.LastAccessedAt.IsZero())

	content, err := os.ReadFile(filepath.Join(result.Dir, "src", "main.go"))
	require.NoError(t, err)
	assert.Equal(t, "package main\n", string(content))
}

func TestEnsureReusesPublishedSnapshot(t *testing.T) {
	manager := newTestManager(t, DefaultLimits())
	first, err := manager.Ensure(context.Background(), testKey(), tarball(t, regularEntry("file.txt", "content\n")))
	require.NoError(t, err)
	assert.False(t, first.CacheHit)

	download, calls := unusedDownload()
	second, err := manager.Ensure(context.Background(), testKey(), download)
	require.NoError(t, err)
	assert.True(t, second.CacheHit)
	assert.Equal(t, 0, *calls)
	assert.Equal(t, first.Dir, second.Dir)
}

func TestEnsureIsolatesCacheKeys(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root, testInstance, DefaultLimits())
	require.NoError(t, err)
	otherInstance, err := NewManager(root, "https://other.example.test/", DefaultLimits())
	require.NoError(t, err)

	base := testKey()
	variants := []struct {
		name string
		key  Key
	}{
		{"same key", base},
		{"other user", Key{UserID: 8, Owner: "owner", Repo: "repo", CommitSHA: base.CommitSHA}},
		{"other owner", Key{UserID: 7, Owner: "someone", Repo: "repo", CommitSHA: base.CommitSHA}},
		{"other repo", Key{UserID: 7, Owner: "owner", Repo: "fork", CommitSHA: base.CommitSHA}},
		{"other commit", Key{UserID: 7, Owner: "owner", Repo: "repo", CommitSHA: strings.Repeat("b", 40)}},
	}

	directories := make(map[string]bool)
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			download := tarball(t, regularEntry("file.txt", variant.name+"\n"))
			result, err := manager.Ensure(context.Background(), variant.key, download)
			require.NoError(t, err)
			require.False(t, directories[result.Dir], "directory %s is shared across keys", result.Dir)
			directories[result.Dir] = true

			repeat, err := manager.Ensure(context.Background(), variant.key, download)
			require.NoError(t, err)
			assert.True(t, repeat.CacheHit)
			assert.Equal(t, result.Dir, repeat.Dir)
		})
	}

	// The same key on a different instance lands in a different directory.
	download := tarball(t, regularEntry("file.txt", "other instance\n"))
	other, err := otherInstance.Ensure(context.Background(), base, download)
	require.NoError(t, err)
	assert.False(t, directories[other.Dir])
}

func TestEnsureRejectsInvalidKeys(t *testing.T) {
	download, calls := unusedDownload()
	manager := newTestManager(t, DefaultLimits())

	cases := []struct {
		name string
		key  Key
	}{
		{"short sha", Key{UserID: 7, Owner: "owner", Repo: "repo", CommitSHA: "abc1234"}},
		{"non-hex sha", Key{UserID: 7, Owner: "owner", Repo: "repo", CommitSHA: strings.Repeat("g", 40)}},
		{"dot owner", Key{UserID: 7, Owner: ".", Repo: "repo", CommitSHA: strings.Repeat("a", 40)}},
		{"dotdot repo", Key{UserID: 7, Owner: "owner", Repo: "..", CommitSHA: strings.Repeat("a", 40)}},
		{"traversal owner", Key{UserID: 7, Owner: "../escape", Repo: "repo", CommitSHA: strings.Repeat("a", 40)}},
		{"slash in repo", Key{UserID: 7, Owner: "owner", Repo: "a/b", CommitSHA: strings.Repeat("a", 40)}},
		{"zero user", Key{UserID: 0, Owner: "owner", Repo: "repo", CommitSHA: strings.Repeat("a", 40)}},
		{"negative user", Key{UserID: -1, Owner: "owner", Repo: "repo", CommitSHA: strings.Repeat("a", 40)}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := manager.Ensure(context.Background(), testCase.key, download)
			require.Error(t, err)
		})
	}
	assert.Equal(t, 0, *calls)
}

func TestEnsureSkipsSpecialEntries(t *testing.T) {
	manager := newTestManager(t, DefaultLimits())
	archive := tarball(t,
		regularEntry("file.txt", "content\n"),
		tarEntry{Header: &tar.Header{Name: "symlink", Typeflag: tar.TypeSymlink, Linkname: "file.txt"}},
		tarEntry{Header: &tar.Header{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "file.txt"}},
		tarEntry{Header: &tar.Header{Name: "fifo", Typeflag: tar.TypeFifo}},
		tarEntry{Header: &tar.Header{Name: "chardev", Typeflag: tar.TypeChar, Devmajor: 5, Devminor: 1}},
		tarEntry{Header: &tar.Header{Name: "blockdev", Typeflag: tar.TypeBlock, Devmajor: 8, Devminor: 0}},
	)

	result, err := manager.Ensure(context.Background(), testKey(), archive)
	require.NoError(t, err)
	assert.Equal(t, 5, result.SkippedEntries)

	entries, err := os.ReadDir(result.Dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "file.txt", entries[0].Name())
	assert.True(t, entries[0].Type().IsRegular())
}

func TestEnsureRejectsUnsafeArchivePaths(t *testing.T) {
	root := t.TempDir()
	unsafe := []string{
		"/etc/escape",
		"../escape",
		"a/../../escape",
		"normal/../../../escape",
	}
	for _, name := range unsafe {
		t.Run(name, func(t *testing.T) {
			manager, err := NewManager(root, testInstance, DefaultLimits())
			require.NoError(t, err)
			_, err = manager.Ensure(context.Background(), testKey(), tarball(t, regularEntry(name, "evil\n")))
			require.ErrorIs(t, err, ErrUnsafeArchiveEntry)

			// Nothing may be published for a rejected archive.
			commitDir := filepath.Join(root, instanceHashForTest(t), "7", "owner", "repo", strings.Repeat("a", 40))
			_, statErr := os.Stat(commitDir)
			assert.True(t, os.IsNotExist(statErr))
		})
	}
}

func TestSafeJoinRejectsUnmaterializableNames(t *testing.T) {
	destination := string(os.PathSeparator) + filepath.Join("cache", "snapshot")
	unsafe := []string{
		"",
		"has\x00nul",
		"/absolute",
		"..",
		"../escape",
		"deeper/../../escape",
	}
	for _, name := range unsafe {
		_, err := safeJoin(destination, name)
		require.ErrorIs(t, err, ErrUnsafeArchiveEntry, "name %q", name)
	}

	safe := map[string]string{
		"file.txt":            filepath.Join(destination, "file.txt"),
		"dir/sub/file.txt":    filepath.Join(destination, "dir", "sub", "file.txt"),
		"./file.txt":          filepath.Join(destination, "file.txt"),
		"dir/./sub/../f.txt":  filepath.Join(destination, "dir", "f.txt"),
		"repo-abc123/src/x.y": filepath.Join(destination, "repo-abc123", "src", "x.y"),
	}
	for name, expected := range safe {
		actual, err := safeJoin(destination, name)
		require.NoError(t, err, "name %q", name)
		assert.Equal(t, expected, actual)
	}
}

func TestEnsureEnforcesLimits(t *testing.T) {
	t.Run("compressed bytes", func(t *testing.T) {
		manager := newTestManager(t, Limits{MaxCompressedBytes: 32, MaxDecompressedBytes: 1 << 20, MaxEntries: 100})
		_, err := manager.Ensure(context.Background(), testKey(), tarball(t, regularEntry("file.txt", strings.Repeat("x", 4096))))
		require.ErrorIs(t, err, ErrArchiveTooLarge)
	})

	t.Run("decompressed bytes", func(t *testing.T) {
		manager := newTestManager(t, Limits{MaxCompressedBytes: 1 << 20, MaxDecompressedBytes: 16, MaxEntries: 100})
		_, err := manager.Ensure(context.Background(), testKey(), tarball(t, regularEntry("file.txt", strings.Repeat("x", 4096))))
		require.ErrorIs(t, err, ErrSnapshotTooLarge)
	})

	t.Run("entry count", func(t *testing.T) {
		manager := newTestManager(t, Limits{MaxCompressedBytes: 1 << 20, MaxDecompressedBytes: 1 << 20, MaxEntries: 2})
		_, err := manager.Ensure(context.Background(), testKey(), tarball(t,
			regularEntry("one.txt", "1\n"),
			regularEntry("two.txt", "2\n"),
			regularEntry("three.txt", "3\n"),
		))
		require.ErrorIs(t, err, ErrTooManyEntries)
	})
}

func TestEnsureRejectsNonArchiveBodies(t *testing.T) {
	manager := newTestManager(t, DefaultLimits())
	_, err := manager.Ensure(context.Background(), testKey(), func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("this is not gzip")), nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open Gogs archive")
}

func TestEnsurePropagatesDownloadFailure(t *testing.T) {
	manager := newTestManager(t, DefaultLimits())
	_, err := manager.Ensure(context.Background(), testKey(), func(context.Context) (io.ReadCloser, error) {
		return nil, errors.New("boom")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestEnsureCleansUpAfterFailedExtraction(t *testing.T) {
	manager := newTestManager(t, DefaultLimits())
	_, err := manager.Ensure(context.Background(), testKey(), func(context.Context) (io.ReadCloser, error) {
		return nil, errors.New("boom")
	})
	require.Error(t, err)

	entries, readErr := os.ReadDir(filepath.Join(manager.root, "tmp"))
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func TestEnsureConcurrentPublication(t *testing.T) {
	manager := newTestManager(t, DefaultLimits())
	download := tarball(t, regularEntry("file.txt", "content\n"))

	const attempts = 4
	results := make(chan Result, attempts)
	failures := make(chan error, attempts)
	for index := 0; index < attempts; index++ {
		go func() {
			result, err := manager.Ensure(context.Background(), testKey(), download)
			if err != nil {
				failures <- err
				return
			}
			results <- result
		}()
	}

	directories := make(map[string]bool)
	for index := 0; index < attempts; index++ {
		select {
		case err := <-failures:
			t.Fatalf("concurrent ensure failed: %v", err)
		case result := <-results:
			directories[result.Dir] = true
			_, err := os.Stat(filepath.Join(filepath.Dir(result.Dir), "metadata.json"))
			require.NoError(t, err)
			content, err := os.ReadFile(filepath.Join(result.Dir, "file.txt"))
			require.NoError(t, err)
			assert.Equal(t, "content\n", string(content))
		}
	}
	assert.Len(t, directories, 1)
}

func TestSweepStaleTemporaries(t *testing.T) {
	manager := newTestManager(t, DefaultLimits())
	temporaryRoot := filepath.Join(manager.root, "tmp")
	require.NoError(t, os.MkdirAll(filepath.Join(temporaryRoot, "stale"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(temporaryRoot, "fresh"), 0o700))
	stale := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(temporaryRoot, "stale"), stale, stale))

	_, err := manager.Ensure(context.Background(), testKey(), tarball(t, regularEntry("file.txt", "content\n")))
	require.NoError(t, err)

	assert.NoDirExists(t, filepath.Join(temporaryRoot, "stale"))
	assert.DirExists(t, filepath.Join(temporaryRoot, "fresh"))
}

func TestCachePermissions(t *testing.T) {
	// The cache root is nested below the temporary directory so that every
	// asserted path is created by the manager itself.
	root := filepath.Join(t.TempDir(), "cache")
	manager, err := NewManager(root, testInstance, DefaultLimits())
	require.NoError(t, err)
	result, err := manager.Ensure(context.Background(), testKey(), tarball(t,
		regularEntry("top.txt", "content\n"),
		dirEntry("sub/"),
		regularEntry("sub/file.txt", "content\n"),
	))
	require.NoError(t, err)

	paths := []string{
		root,
		filepath.Join(root, "tmp"),
		filepath.Dir(result.Dir),
		result.Dir,
		filepath.Join(result.Dir, "sub"),
		filepath.Join(result.Dir, "sub", "file.txt"),
		filepath.Join(filepath.Dir(result.Dir), "metadata.json"),
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.LessOrEqual(t, int(info.Mode().Perm()), 0o700, path)
	}
}

func TestEnsureFlattensArchivePrefix(t *testing.T) {
	manager := newTestManager(t, DefaultLimits())
	download := tarball(t,
		dirEntry("private-shared/"),
		regularEntry("private-shared/src/version.txt", "feature-version\n"),
	)

	result, err := manager.Ensure(context.Background(), testKey(), download)
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(result.Dir, "src", "version.txt"))
	require.NoError(t, err)
	assert.Equal(t, "feature-version\n", string(content))

	entries, err := os.ReadDir(result.Dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "src", entries[0].Name())
}

func TestEnsureKeepsUnwrappedArchives(t *testing.T) {
	manager := newTestManager(t, DefaultLimits())
	download := tarball(t,
		regularEntry("one.txt", "1\n"),
		regularEntry("two.txt", "2\n"),
	)

	result, err := manager.Ensure(context.Background(), testKey(), download)
	require.NoError(t, err)

	entries, err := os.ReadDir(result.Dir)
	require.NoError(t, err)
	require.Len(t, entries, 2)
}

func TestEnsureKeepsMixedTopLevelEntries(t *testing.T) {
	manager := newTestManager(t, DefaultLimits())
	download := tarball(t,
		regularEntry("top.txt", "root\n"),
		regularEntry("wrapper/nested.txt", "wrapped\n"),
	)

	result, err := manager.Ensure(context.Background(), testKey(), download)
	require.NoError(t, err)

	entries, err := os.ReadDir(result.Dir)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	// A file beside the directory means the archive has no single wrapper,
	// so the directory is kept as-is.
	assert.Equal(t, "top.txt", entries[0].Name())
	assert.Equal(t, "wrapper", entries[1].Name())
}

func instanceHashForTest(t *testing.T) string {
	t.Helper()
	hash, err := normalizedInstanceHash(testInstance)
	require.NoError(t, err)
	return hash
}

func TestNormalizedInstanceHash(t *testing.T) {
	cases := []struct {
		first  string
		second string
		same   bool
	}{
		{"https://gogs.example.test/", "https://GOGS.example.test", true},
		{"http://gogs.example.test:80", "http://gogs.example.test", true},
		{"https://gogs.example.test:443/", "https://gogs.example.test/", true},
		{"https://gogs.example.test/subpath/", "https://gogs.example.test/subpath", true},
		{"https://gogs.example.test", "http://gogs.example.test", false},
		{"https://gogs.example.test", "https://gogs.example.test:8443", false},
		{"https://gogs.example.test/a", "https://gogs.example.test/b", false},
	}
	for _, testCase := range cases {
		first, err := normalizedInstanceHash(testCase.first)
		require.NoError(t, err)
		second, err := normalizedInstanceHash(testCase.second)
		require.NoError(t, err)
		assert.Equal(t, testCase.same, first == second, "%s vs %s", testCase.first, testCase.second)
		assert.Len(t, first, 64)
	}

	for _, broken := range []string{"", "not-a-url"} {
		_, err := normalizedInstanceHash(broken)
		require.Error(t, err, broken)
	}
}

func TestNewManagerRequiresRootAndInstance(t *testing.T) {
	_, err := NewManager("", testInstance, DefaultLimits())
	require.Error(t, err)
	_, err = NewManager(t.TempDir(), "", DefaultLimits())
	require.Error(t, err)
}

func TestSearchFindsLiteralMatches(t *testing.T) {
	root := t.TempDir()
	writeSnapshotFile(t, root, "src/main.go", "package main\n\nfunc main() {\n\tprintln(\"needle here\")\n\tprintln(\"needle twice needle\")\n}\n")
	writeSnapshotFile(t, root, "docs/readme.md", "no match here\n")

	matches, err := Search(context.Background(), root, "needle")
	require.NoError(t, err)
	require.Len(t, matches, 3)

	assert.Equal(t, "src/main.go", matches[0].Path)
	assert.Equal(t, 4, matches[0].Line)
	assert.Equal(t, 11, matches[0].Column)
	assert.Equal(t, "\tprintln(\"needle here\")", matches[0].LineText)
	require.Len(t, matches[0].Context, 4)
	assert.Equal(t, 2, matches[0].Context[0].Number)
	assert.Equal(t, 6, matches[0].Context[3].Number)

	// Results are ordered by path, line, and column; multiple hits on one
	// line produce separate matches.
	assert.Equal(t, 5, matches[1].Line)
	assert.Equal(t, 11, matches[1].Column)
	assert.Equal(t, 24, matches[2].Column)
}

func TestSearchIsCaseSensitive(t *testing.T) {
	root := t.TempDir()
	writeSnapshotFile(t, root, "case.txt", "Needle\nneedle\nNEEDLE\n")

	matches, err := Search(context.Background(), root, "needle")
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, 2, matches[0].Line)
}

func TestSearchContextBoundaries(t *testing.T) {
	root := t.TempDir()
	writeSnapshotFile(t, root, "lines.txt", "line-1\nline-2\nline-3\nline-4\nline-5\nline-6\nline-7\n")

	matches, err := Search(context.Background(), root, "line-4")
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Len(t, matches[0].Context, 4)
	assert.Equal(t, []int{2, 3, 5, 6}, contextNumbers(matches[0]))

	matches, err = Search(context.Background(), root, "line-1")
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Len(t, matches[0].Context, 2)
	assert.Equal(t, []int{2, 3}, contextNumbers(matches[0]))

	matches, err = Search(context.Background(), root, "line-7")
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Len(t, matches[0].Context, 2)
	assert.Equal(t, []int{5, 6}, contextNumbers(matches[0]))
}

func contextNumbers(match Match) []int {
	numbers := make([]int, len(match.Context))
	for index, line := range match.Context {
		numbers[index] = line.Number
	}
	return numbers
}

func TestSearchStopsAtOversizedLines(t *testing.T) {
	root := t.TempDir()
	writeSnapshotFile(t, root, "big.txt", "needle before\n"+strings.Repeat("x", 2*maxLineBytes)+"\nneedle after\n")

	matches, err := Search(context.Background(), root, "needle")
	require.NoError(t, err)
	// The scan stops at the overlong line; earlier content is still searched.
	require.Len(t, matches, 1)
	assert.Equal(t, 1, matches[0].Line)
}

func TestSearchTruncatesLongLines(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("字", maxLineRunes+50)
	writeSnapshotFile(t, root, "long.txt", long+" needle\n")

	matches, err := Search(context.Background(), root, "needle")
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.True(t, strings.HasSuffix(matches[0].LineText, "…"))
	assert.Len(t, []rune(matches[0].LineText), maxLineRunes+1)
}

func TestSearchCapsMatches(t *testing.T) {
	root := t.TempDir()
	var lines []string
	for index := 0; index < 100; index++ {
		lines = append(lines, "needle\n")
	}
	writeSnapshotFile(t, root, "many.txt", strings.Join(lines, ""))

	matches, err := Search(context.Background(), root, "needle")
	require.NoError(t, err)
	assert.Len(t, matches, MaxMatches)
}

func TestSearchRequiresQuery(t *testing.T) {
	root := t.TempDir()
	_, err := Search(context.Background(), root, "")
	require.Error(t, err)
}

func TestSearchEmptySnapshot(t *testing.T) {
	root := t.TempDir()
	matches, err := Search(context.Background(), root, "needle")
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestSearchOnlyRegularFiles(t *testing.T) {
	root := t.TempDir()
	writeSnapshotFile(t, root, "hit.txt", "needle\n")
	require.NoError(t, os.Mkdir(filepath.Join(root, "needle-dir"), 0o700))

	matches, err := Search(context.Background(), root, "needle")
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "hit.txt", matches[0].Path)
	assert.Equal(t, 1, matches[0].Line)
	assert.Equal(t, 1, matches[0].Column)
	assert.Equal(t, "needle", matches[0].LineText)
	assert.Empty(t, matches[0].Context)
}

func writeSnapshotFile(t *testing.T, root, name, body string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
	require.NoError(t, os.WriteFile(target, []byte(body), 0o600))
}

// Package snapshot materializes immutable commit snapshots in a local cache
// and searches them for literal text. Archive contents are treated as
// untrusted input: only plain directories and regular files are ever created,
// and every path is verified to stay inside the snapshot directory.
package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gogs-mcp/internal/diskcache"

	"github.com/cockroachdb/errors"
)

// metadataSchemaVersion is bumped whenever the on-disk metadata layout
// changes in a way that older caches cannot be interpreted against.
const metadataSchemaVersion = 1

// Limits bound how much of a Gogs archive may be processed for one snapshot.
type Limits struct {
	MaxCompressedBytes   int64
	MaxDecompressedBytes int64
	MaxEntries           int
}

// DefaultLimits are the documented archive limits of the server.
func DefaultLimits() Limits {
	return Limits{
		MaxCompressedBytes:   128 << 20,
		MaxDecompressedBytes: 512 << 20,
		MaxEntries:           20000,
	}
}

// Eviction controls how the manager keeps the cache bounded.
type Eviction struct {
	// MaxBytes is the total cache size above which least-recently-used
	// snapshots are removed.
	MaxBytes int64
	// TTL is how long a snapshot may stay untouched before it expires.
	TTL time.Duration
}

// DefaultEviction matches the documented cache limits.
func DefaultEviction() Eviction {
	return Eviction{
		MaxBytes: 2 << 30,
		TTL:      24 * time.Hour,
	}
}

var (
	// ErrArchiveTooLarge reports that the compressed archive exceeded the limit.
	ErrArchiveTooLarge = errors.New("the compressed archive exceeds the size limit")
	// ErrSnapshotTooLarge reports that the decompressed archive exceeded the limit.
	ErrSnapshotTooLarge = errors.New("the decompressed archive exceeds the size limit")
	// ErrTooManyEntries reports that the archive contains more entries than allowed.
	ErrTooManyEntries = errors.New("the archive contains too many entries")
	// ErrUnsafeArchiveEntry reports an archive entry whose path cannot be
	// materialized safely. Nothing from such an archive is published.
	ErrUnsafeArchiveEntry = errors.New("the archive contains an unsafe entry path")
	// ErrCacheCapacityExceeded reports that eviction could not make room for a
	// new snapshot.
	ErrCacheCapacityExceeded = diskcache.ErrCapacity
)

// Key identifies one immutable snapshot. It is scoped to the normalized Gogs
// instance, the authenticated user, the repository, and the commit, so users
// never share extracted content with each other or across instances.
type Key struct {
	UserID    int64
	Owner     string
	Repo      string
	CommitSHA string
}

// Metadata is the metadata.json document stored next to every published
// snapshot. Credentials never appear in it.
type Metadata struct {
	SchemaVersion int       `json:"schema_version"`
	InstanceHash  string    `json:"instance_hash"`
	UserID        int64     `json:"user_id"`
	Repository    string    `json:"repository"`
	CommitSHA     string    `json:"commit_sha"`
	CreatedAt     time.Time `json:"created_at"`
	// LastAccessedAt records publication time for compatibility; directory mtime
	// is the access clock, avoiding metadata rewrites while readers hold shared locks.
	LastAccessedAt time.Time `json:"last_accessed_at"`
	FileCount      int       `json:"file_count"`
	TotalBytes     int64     `json:"total_bytes"`
}

// Manager publishes snapshots atomically under a private cache root and keeps
// the cache bounded through TTL and LRU eviction.
type Manager struct {
	root     string
	instance string
	limits   Limits
	eviction Eviction
}

// NewManager returns a manager that isolates snapshots per instance and user
// under the given cache root. The instance is any Gogs base URL.
func NewManager(root, instance string, limits Limits, eviction Eviction) (*Manager, error) {
	if root == "" {
		return nil, errors.New("cache root is required")
	}
	if instance == "" {
		return nil, errors.New("Gogs instance is required")
	}
	if eviction.MaxBytes < 0 || eviction.TTL < 0 {
		return nil, errors.New("eviction limits must not be negative")
	}
	return &Manager{root: root, instance: instance, limits: limits, eviction: eviction}, nil
}

// Result describes a materialized snapshot.
type Result struct {
	// Dir is the extracted content root, the snapshot/ directory of the cache
	// layout. The commit directory itself also holds metadata.json.
	Dir      string
	CacheHit bool
	// SkippedEntries counts archive entries that were not materialized, such
	// as symlinks, hardlinks, devices, FIFOs, and sockets.
	SkippedEntries int
	// Release marks the snapshot as no longer being read so that eviction may
	// remove it. Callers must invoke it exactly once, typically deferred.
	Release func()
}

// Ensure returns a fully extracted snapshot for the key, downloading and
// extracting it only when no complete snapshot exists yet. The download is
// only invoked on a cache miss. Publication is atomic: a snapshot directory
// appears either complete or not at all, and concurrent callers converge on
// the first published copy. The returned snapshot is marked in use until
// Result.Release is called, protecting it from eviction.
func (m *Manager) Ensure(ctx context.Context, key Key, download func(context.Context) (io.ReadCloser, error)) (Result, error) {
	commitDir, instanceHash, err := m.directory(key)
	if err != nil {
		return Result{}, err
	}
	userRoot, err := m.userRoot(key.UserID)
	if err != nil {
		return Result{}, err
	}
	final := filepath.Join(commitDir, "snapshot")
	read, err := diskcache.Acquire(ctx, userRoot, commitDir, true)
	if err != nil {
		return Result{}, err
	}
	if info, err := os.Stat(final); err == nil && info.IsDir() {
		now := time.Now()
		_ = os.Chtimes(commitDir, now, now)
		return Result{Dir: final, CacheHit: true, Release: func() { read.Close() }}, nil
	}
	read.Close()
	writer, err := diskcache.Acquire(ctx, userRoot, userRoot+".writer", false)
	if err != nil {
		return Result{}, err
	}
	defer writer.Close()
	entry, err := diskcache.Acquire(ctx, userRoot, commitDir, true)
	if err == nil {
		if info, statErr := os.Stat(final); statErr == nil && info.IsDir() {
			return Result{Dir: final, CacheHit: true, Release: func() { entry.Close() }}, nil
		}
		entry.Close()
		entry, err = diskcache.Acquire(ctx, userRoot, commitDir, false)
	}
	if err != nil {
		return Result{}, err
	}
	success := false
	defer func() {
		if !success {
			entry.Close()
		}
	}()
	finish := func(hit bool, skipped int) (Result, error) {
		if err := entry.Share(); err != nil {
			return Result{}, err
		}
		success = true
		return Result{Dir: final, CacheHit: hit, SkippedEntries: skipped, Release: func() { entry.Close() }}, nil
	}
	if info, err := os.Stat(final); err == nil && info.IsDir() {
		return finish(true, 0)
	}
	m.sweepStaleTemporaries(userRoot)
	budget := diskcache.Budget{Root: userRoot, MaxBytes: m.eviction.MaxBytes, TTL: m.eviction.TTL}
	if err := budget.MakeRoom(1); err != nil {
		return Result{}, err
	}
	temporaryRoot := filepath.Join(userRoot, ".tmp")
	if err := os.MkdirAll(temporaryRoot, 0700); err != nil {
		return Result{}, err
	}
	temporary, err := os.MkdirTemp(temporaryRoot, "snapshot-")
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	budget.Keep = temporary
	stats, err := extractArchive(ctx, download, filepath.Join(temporary, "snapshot"), m.limits, budget.Reserve)
	if err != nil {
		return Result{}, err
	}
	if err := writeMetadata(temporary, key, instanceHash, stats, budget.Reserve); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(commitDir), 0700); err != nil {
		return Result{}, err
	}
	if err := os.Rename(temporary, commitDir); err != nil {
		return Result{}, err
	}
	return finish(false, stats.SkippedEntries)
}

func writeMetadata(directory string, key Key, instanceHash string, stats extractionStats, admit ...func(int64) error) error {
	now := time.Now().UTC()
	document, err := json.MarshalIndent(Metadata{
		SchemaVersion:  metadataSchemaVersion,
		InstanceHash:   instanceHash,
		UserID:         key.UserID,
		Repository:     key.Owner + "/" + key.Repo,
		CommitSHA:      key.CommitSHA,
		CreatedAt:      now,
		LastAccessedAt: now,
		FileCount:      stats.FileCount,
		TotalBytes:     stats.TotalBytes,
	}, "", "  ")
	if err != nil {
		return errors.Wrap(err, "encode snapshot metadata")
	}
	if len(admit) > 0 {
		if err := admit[0](int64(len(document) + 1)); err != nil {
			return err
		}
	}
	target := filepath.Join(directory, "metadata.json")
	// Metadata is written last so that a crash never leaves a published
	// snapshot without it.
	if err := os.WriteFile(target, append(document, '\n'), 0o600); err != nil {
		return errors.Wrap(err, "write snapshot metadata")
	}
	return nil
}

// directory builds the per-instance, per-user commit directory for the key.
// Every component is validated so that repository metadata coming from Gogs
// or from tool input can never escape or reshape the cache layout.
func (m *Manager) directory(key Key) (commitDir, instanceHash string, err error) {
	if !isFullCommitSHA(key.CommitSHA) {
		return "", "", errors.New("snapshot commit must be a full 40-character SHA")
	}
	if !isCacheComponent(key.Owner) {
		return "", "", errors.New("snapshot owner contains unsupported characters")
	}
	if !isCacheComponent(key.Repo) {
		return "", "", errors.New("snapshot repository contains unsupported characters")
	}
	if key.UserID <= 0 {
		return "", "", errors.New("snapshot user must be a positive identifier")
	}
	instanceHash, err = normalizedInstanceHash(m.instance)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(
		m.root,
		instanceHash,
		strconv.FormatInt(key.UserID, 10),
		key.Owner,
		key.Repo,
		key.CommitSHA,
	), instanceHash, nil
}

// normalizedInstanceHash reduces a Gogs base URL to a stable identifier of its
// scheme, host, optional port, and path, so that the same instance always maps
// to one cache directory regardless of trailing slashes or casing. Tokens are
// never part of the key.
func normalizedInstanceHash(instance string) (string, error) {
	parsed, err := url.Parse(instance)
	if err != nil {
		return "", errors.Wrap(err, "parse Gogs instance")
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if scheme == "" || host == "" {
		return "", errors.New("Gogs instance must include a scheme and host")
	}
	if port := parsed.Port(); port != "" && !isDefaultPort(scheme, port) {
		host += ":" + port
	}
	if trimmed := strings.Trim(path.Clean("/"+parsed.Path), "/"); trimmed != "" {
		host += "/" + trimmed
	}
	digest := sha256.Sum256([]byte(scheme + "://" + host))
	return hex.EncodeToString(digest[:]), nil
}

func isDefaultPort(scheme, port string) bool {
	return (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
}

func isFullCommitSHA(value string) bool {
	return len(value) == 40 && isHex(value)
}

func isHex(value string) bool {
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'f':
		case character >= 'A' && character <= 'F':
		default:
			return false
		}
	}
	return true
}

// isCacheComponent accepts the characters Gogs uses in owner and repository
// names. A leading dot or dash is rejected because it would create hidden or
// flag-like directories inside the cache.
func isCacheComponent(value string) bool {
	if value == "" || len(value) > 100 || value == "." || value == ".." {
		return false
	}
	for index, character := range value {
		switch {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character == '.' || character == '_' || character == '-':
			if index == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// sweepStaleTemporaries removes leftover extraction directories from processes
// that died mid-extraction. Published snapshots are never touched.
func (m *Manager) sweepStaleTemporaries(userRoot string) {
	// Ensure holds this user's writer lock, so no extraction in this namespace
	// can still be running. Retain legacy cleanup for pre-migration leftovers.
	for _, root := range []string{filepath.Join(userRoot, ".tmp"), filepath.Join(m.root, "tmp")} {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		stale := time.Now().Add(-time.Hour)
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil || info.ModTime().After(stale) {
				continue
			}
			_ = os.RemoveAll(filepath.Join(root, entry.Name()))
		}
	}
}

// extractionStats records what one archive extraction produced.
type extractionStats struct {
	SkippedEntries int
	FileCount      int
	TotalBytes     int64
}

func extractArchive(ctx context.Context, download func(context.Context) (io.ReadCloser, error), destination string, limits Limits, admit ...func(int64) error) (extractionStats, error) {
	if download == nil {
		return extractionStats{}, errors.New("archive download is required")
	}
	body, err := download(ctx)
	if err != nil {
		return extractionStats{}, err
	}
	defer func() {
		_ = body.Close()
	}()

	compressed := newBoundedReader(body, limits.MaxCompressedBytes, ErrArchiveTooLarge)
	gzipReader, err := gzip.NewReader(compressed)
	if err != nil {
		return extractionStats{}, errors.Wrap(err, "open Gogs archive")
	}
	decompressed := newBoundedReader(gzipReader, limits.MaxDecompressedBytes, ErrSnapshotTooLarge)

	var stats extractionStats
	entries := 0
	reader := tar.NewReader(decompressed)
	for {
		if err := ctx.Err(); err != nil {
			return extractionStats{}, errors.Wrap(err, "extract Gogs archive")
		}

		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if err := flattenArchivePrefix(destination); err != nil {
				return extractionStats{}, err
			}
			return stats, nil
		}
		if err != nil {
			return extractionStats{}, errors.Wrap(err, "read Gogs archive")
		}

		// Every archive header counts toward the entry limit so that archives
		// made purely of skipped entries stay bounded as well.
		entries++
		if entries > limits.MaxEntries {
			return extractionStats{}, ErrTooManyEntries
		}

		if !isMaterializable(header.Typeflag) {
			stats.SkippedEntries++
			continue
		}

		target, err := safeJoin(destination, header.Name)
		if err != nil {
			return extractionStats{}, err
		}
		if target == destination {
			// An entry that cleans to the archive root has no materializable
			// path; ignoring it keeps the snapshot directory intact.
			continue
		}
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return extractionStats{}, errors.Wrap(err, "create snapshot directory")
			}
			continue
		}
		if len(admit) > 0 {
			growth := header.Size
			if info, err := os.Stat(target); err == nil {
				growth = max(0, growth-info.Size())
			}
			if err := admit[0](growth); err != nil {
				return extractionStats{}, err
			}
		}
		written, err := writeFile(target, reader)
		if err != nil {
			return extractionStats{}, err
		}
		stats.FileCount++
		stats.TotalBytes += written
	}
}

// isMaterializable reports whether an archive entry may become part of the
// snapshot. Symlinks, hardlinks, devices, FIFOs, sockets, and metadata headers
// never do, regardless of where they point.
func isMaterializable(typeflag byte) bool {
	switch typeflag {
	case tar.TypeDir, tar.TypeReg, tar.TypeGNUSparse:
		return true
	default:
		return false
	}
}

// flattenArchivePrefix removes the single top-level directory that Gogs archives
// wrap their content in, so that snapshot paths match repository paths. An
// archive without such a wrapper is left untouched.
func flattenArchivePrefix(destination string) error {
	entries, err := os.ReadDir(destination)
	if err != nil {
		return errors.Wrap(err, "read snapshot root")
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}
	wrapper := filepath.Join(destination, entries[0].Name())
	nested, err := os.ReadDir(wrapper)
	if err != nil {
		return errors.Wrap(err, "read snapshot wrapper directory")
	}
	for _, entry := range nested {
		source := filepath.Join(wrapper, entry.Name())
		target := filepath.Join(destination, entry.Name())
		// A name clash cannot come from a prefix-wrapped archive and would
		// make the snapshot ambiguous, so it aborts publication.
		if _, err := os.Lstat(target); err == nil {
			return errors.New("snapshot wrapper content clashes with the archive root")
		}
		if err := os.Rename(source, target); err != nil {
			return errors.Wrap(err, "flatten snapshot wrapper directory")
		}
	}
	return os.Remove(wrapper)
}

func writeFile(target string, content io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, errors.Wrap(err, "create snapshot parent directory")
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, errors.Wrap(err, "create snapshot file")
	}
	written, err := io.Copy(file, content)
	if err != nil {
		_ = file.Close()
		return written, errors.Wrap(err, "write snapshot file")
	}
	return written, file.Close()
}

// safeJoin maps an archive entry name onto a path inside the destination.
// Absolute paths, NUL bytes, and names that escape the destination after
// cleaning are rejected instead of being materialized.
func safeJoin(destination, name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') {
		return "", ErrUnsafeArchiveEntry
	}
	if strings.HasPrefix(name, "/") {
		return "", ErrUnsafeArchiveEntry
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", ErrUnsafeArchiveEntry
	}
	target := filepath.Join(destination, filepath.FromSlash(cleaned))
	if !strings.HasPrefix(target, destination+string(os.PathSeparator)) {
		return "", ErrUnsafeArchiveEntry
	}
	return target, nil
}

// boundedReader fails once more than maximum bytes have been read through it,
// terminating oversized archives instead of exhausting disk or memory.
type boundedReader struct {
	reader   io.Reader
	count    int64
	maximum  int64
	exceeded error
}

func newBoundedReader(reader io.Reader, maximum int64, exceeded error) *boundedReader {
	return &boundedReader{reader: reader, maximum: maximum, exceeded: exceeded}
}

func (b *boundedReader) Read(buffer []byte) (int, error) {
	if b.count >= b.maximum {
		return 0, b.exceeded
	}
	if int64(len(buffer)) > b.maximum-b.count {
		buffer = buffer[:b.maximum-b.count]
	}
	read, err := b.reader.Read(buffer)
	b.count += int64(read)
	return read, err
}

// UserRoot returns the shared snapshot and Git cache namespace.
func (m *Manager) UserRoot(userID int64) (string, error) { return m.userRoot(userID) }

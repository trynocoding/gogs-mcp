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
	SchemaVersion  int       `json:"schema_version"`
	InstanceHash   string    `json:"instance_hash"`
	UserID         int64     `json:"user_id"`
	Repository     string    `json:"repository"`
	CommitSHA      string    `json:"commit_sha"`
	CreatedAt      time.Time `json:"created_at"`
	LastAccessedAt time.Time `json:"last_accessed_at"`
	FileCount      int       `json:"file_count"`
	TotalBytes     int64     `json:"total_bytes"`
}

// Manager publishes snapshots atomically under a private cache root.
type Manager struct {
	root     string
	instance string
	limits   Limits
}

// NewManager returns a manager that isolates snapshots per instance and user
// under the given cache root. The instance is any Gogs base URL.
func NewManager(root, instance string, limits Limits) (*Manager, error) {
	if root == "" {
		return nil, errors.New("cache root is required")
	}
	if instance == "" {
		return nil, errors.New("Gogs instance is required")
	}
	return &Manager{root: root, instance: instance, limits: limits}, nil
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
}

// Ensure returns a fully extracted snapshot for the key, downloading and
// extracting it only when no complete snapshot exists yet. The download is
// only invoked on a cache miss. Publication is atomic: a snapshot directory
// appears either complete or not at all, and concurrent callers converge on
// the first published copy.
func (m *Manager) Ensure(ctx context.Context, key Key, download func(context.Context) (io.ReadCloser, error)) (Result, error) {
	commitDir, instanceHash, err := m.directory(key)
	if err != nil {
		return Result{}, err
	}
	final := filepath.Join(commitDir, "snapshot")
	if info, err := os.Stat(final); err == nil && info.IsDir() {
		return Result{Dir: final, CacheHit: true}, nil
	}

	m.sweepStaleTemporaries()

	temporaryRoot := filepath.Join(m.root, "tmp")
	// The whole cache tree stays private to the current user.
	if err := os.MkdirAll(temporaryRoot, 0o700); err != nil {
		return Result{}, errors.Wrap(err, "create snapshot temporary directory")
	}
	temporary, err := os.MkdirTemp(temporaryRoot, "snapshot-")
	if err != nil {
		return Result{}, errors.Wrap(err, "create snapshot temporary directory")
	}

	stats, err := extractArchive(ctx, download, filepath.Join(temporary, "snapshot"), m.limits)
	if err == nil {
		err = writeMetadata(temporary, key, instanceHash, stats)
	}
	if err != nil {
		_ = os.RemoveAll(temporary)
		return Result{}, err
	}

	// Only the parent of the final directory is created here. The rename
	// below must land on a path that does not exist yet, because os.Rename
	// does not replace existing directories on Linux; if a concurrent process
	// published the same snapshot first, the rename fails and the published
	// copy is adopted below.
	if err := os.MkdirAll(filepath.Dir(commitDir), 0o700); err != nil {
		_ = os.RemoveAll(temporary)
		return Result{}, errors.Wrap(err, "create snapshot cache directory")
	}
	if err := os.Rename(temporary, commitDir); err != nil {
		// Another process published the same snapshot first.
		if info, statErr := os.Stat(final); statErr == nil && info.IsDir() {
			_ = os.RemoveAll(temporary)
			return Result{Dir: final, CacheHit: true, SkippedEntries: stats.SkippedEntries}, nil
		}
		_ = os.RemoveAll(temporary)
		return Result{}, errors.Wrap(err, "publish snapshot directory")
	}
	return Result{Dir: final, SkippedEntries: stats.SkippedEntries}, nil
}

func writeMetadata(directory string, key Key, instanceHash string, stats extractionStats) error {
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
func (m *Manager) sweepStaleTemporaries() {
	entries, err := os.ReadDir(filepath.Join(m.root, "tmp"))
	if err != nil {
		return
	}
	stale := time.Now().Add(-time.Hour)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.ModTime().After(stale) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(m.root, "tmp", entry.Name()))
	}
}

// extractionStats records what one archive extraction produced.
type extractionStats struct {
	SkippedEntries int
	FileCount      int
	TotalBytes     int64
}

func extractArchive(ctx context.Context, download func(context.Context) (io.ReadCloser, error), destination string, limits Limits) (extractionStats, error) {
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

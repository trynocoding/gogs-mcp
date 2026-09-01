package snapshot

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/cockroachdb/errors"
)

// touch records the current access on a snapshot so that TTL expiry and LRU
// ordering follow actual use. The commit directory mtime is the eviction
// clock; metadata.json carries the same timestamp for observability.
func (m *Manager) touch(commitDir string) {
	now := time.Now()
	_ = os.Chtimes(commitDir, now, now)
	document, err := os.ReadFile(filepath.Join(commitDir, "metadata.json"))
	if err != nil {
		return
	}
	var metadata Metadata
	if err := json.Unmarshal(document, &metadata); err != nil {
		return
	}
	metadata.LastAccessedAt = now.UTC()
	updated, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(commitDir, "metadata.json"), append(updated, '\n'), 0o600)
}

// retain marks a snapshot as in use until the returned release function is
// called, so that eviction never removes a snapshot mid-read within this
// process.
func (m *Manager) retain(commitDir string) func() {
	m.useMu.Lock()
	m.inUse[commitDir]++
	m.useMu.Unlock()
	return func() {
		m.useMu.Lock()
		defer m.useMu.Unlock()
		if m.inUse[commitDir] <= 1 {
			delete(m.inUse, commitDir)
			return
		}
		m.inUse[commitDir]--
	}
}

func (m *Manager) isInUse(commitDir string) bool {
	m.useMu.Lock()
	defer m.useMu.Unlock()
	return m.inUse[commitDir] > 0
}

// evictExpired removes snapshots of one cache user whose last access is older
// than the TTL. Removal failures are ignored: a snapshot that cannot be
// removed today is left for a future sweep rather than failing the caller.
func (m *Manager) evictExpired(userID int64) error {
	if m.eviction.TTL <= 0 {
		return nil
	}
	userRoot, err := m.userRoot(userID)
	if err != nil {
		return err
	}
	expiry := time.Now().Add(-m.eviction.TTL)
	for _, commitDir := range m.commitDirectories(userRoot) {
		info, err := os.Lstat(commitDir)
		if err != nil || !info.IsDir() || m.isInUse(commitDir) {
			continue
		}
		if info.ModTime().Before(expiry) {
			_ = os.RemoveAll(commitDir)
		}
	}
	return nil
}

// enforceCapacity removes least-recently-used snapshots of one cache user
// until the measured cache size fits the limit. Snapshots held by active
// searches are never removed; when nothing evictable remains and the cache is
// still over the limit, ErrCacheCapacityExceeded aborts before any download.
func (m *Manager) enforceCapacity(userID int64) error {
	if m.eviction.MaxBytes <= 0 {
		return nil
	}
	userRoot, err := m.userRoot(userID)
	if err != nil {
		return err
	}
	total := int64(0)
	type cacheEntry struct {
		dir   string
		bytes int64
		mtime time.Time
	}
	var entries []cacheEntry
	for _, commitDir := range m.commitDirectories(userRoot) {
		info, err := os.Lstat(commitDir)
		if err != nil || !info.IsDir() {
			continue
		}
		size := directorySize(commitDir)
		entries = append(entries, cacheEntry{dir: commitDir, bytes: size, mtime: info.ModTime()})
		total += size
	}
	if total <= m.eviction.MaxBytes {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].mtime.Before(entries[j].mtime)
	})
	for _, entry := range entries {
		if total <= m.eviction.MaxBytes {
			break
		}
		if m.isInUse(entry.dir) {
			continue
		}
		if err := os.RemoveAll(entry.dir); err != nil {
			// A snapshot that cannot be removed frees no space; the next
			// candidate is tried instead.
			continue
		}
		total -= entry.bytes
	}
	if total > m.eviction.MaxBytes {
		return ErrCacheCapacityExceeded
	}
	return nil
}

// commitDirectories lists every commit directory of one cache user, walking
// only the fixed owner/repository/commit levels of the cache layout.
func (m *Manager) commitDirectories(userRoot string) []string {
	var commitDirs []string
	owners, err := os.ReadDir(userRoot)
	if err != nil {
		return nil
	}
	for _, owner := range owners {
		repositories, err := os.ReadDir(filepath.Join(userRoot, owner.Name()))
		if err != nil {
			continue
		}
		for _, repository := range repositories {
			commits, err := os.ReadDir(filepath.Join(userRoot, owner.Name(), repository.Name()))
			if err != nil {
				continue
			}
			for _, commit := range commits {
				commitDirs = append(commitDirs, filepath.Join(userRoot, owner.Name(), repository.Name(), commit.Name()))
			}
		}
	}
	return commitDirs
}

// directorySize is a best-effort estimate; unreadable entries are skipped
// rather than failing the capacity check.
func directorySize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			if info, statErr := entry.Info(); statErr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// userRoot returns the per-instance, per-user directory of the cache layout.
func (m *Manager) userRoot(userID int64) (string, error) {
	if userID <= 0 {
		return "", errors.New("snapshot user must be a positive identifier")
	}
	instanceHash, err := normalizedInstanceHash(m.instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(m.root, instanceHash, strconv.FormatInt(userID, 10)), nil
}

// RemoveUser deletes every snapshot cached for one Gogs user on the current
// instance. It reports success for a user that has no cache yet.
func (m *Manager) RemoveUser(userID int64) error {
	userRoot, err := m.userRoot(userID)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(userRoot); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrap(err, "inspect snapshot cache user directory")
	}
	if err := os.RemoveAll(userRoot); err != nil {
		return errors.Wrap(err, "remove snapshot cache user directory")
	}
	return nil
}

// RemoveAll deletes every per-instance cache root. Only directories named as
// instance hashes are touched, so configuration, tokens, and unrelated files
// inside or outside the cache root are never affected.
func (m *Manager) RemoveAll() error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrap(err, "read snapshot cache root")
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isInstanceHash(entry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.root, entry.Name())); err != nil {
			return errors.Wrap(err, "remove snapshot cache root")
		}
	}
	return nil
}

// isInstanceHash reports whether a cache root entry names a per-instance
// directory rather than the temporary area or unrelated files.
func isInstanceHash(name string) bool {
	return len(name) == 64 && isHex(name)
}

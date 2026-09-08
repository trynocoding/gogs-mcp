package snapshot

import (
	"context"
	"gogs-mcp/internal/diskcache"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/cockroachdb/errors"
)

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
	if err := diskcache.RemoveTree(context.Background(), userRoot, userRoot); err != nil {
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
		instanceRoot := filepath.Join(m.root, entry.Name())
		users, err := os.ReadDir(instanceRoot)
		if err != nil {
			return err
		}
		for _, user := range users {
			if !user.IsDir() {
				continue
			}
			root := filepath.Join(instanceRoot, user.Name())
			if err := diskcache.RemoveTree(context.Background(), root, root); err != nil {
				return err
			}
		}

	}
	return nil
}

// isInstanceHash reports whether a cache root entry names a per-instance
// directory rather than the temporary area or unrelated files.
func isInstanceHash(name string) bool {
	return len(name) == 64 && isHex(name)
}

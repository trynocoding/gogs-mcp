// Package diskcache coordinates cache readers and writers across clients and processes.
package diskcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"golang.org/x/sys/unix"
)

var ErrCapacity = errors.New("the shared user cache has no available capacity")

// Lock files live outside entries, so eviction never replaces a locked inode.
type Lock struct{ file *os.File }

func Acquire(ctx context.Context, lockRoot, path string, shared bool) (*Lock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := TryLock(lockRoot, path, shared)
		if err != nil || lock != nil {
			return lock, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func TryLock(lockRoot, path string, shared bool) (*Lock, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(absolute))
	// The application cache and its lock directory have the same OS owner.
	root := filepath.Join(lockRoot, ".locks")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("cache lock directory must be private")
	}
	fd, err := unix.Open(filepath.Join(root, hex.EncodeToString(sum[:])), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "cache-lock")
	mode := unix.LOCK_EX
	if shared {
		mode = unix.LOCK_SH
	}
	if err := unix.Flock(fd, mode|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() {
	if l != nil && l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}
func (l *Lock) Share() error { return unix.Flock(int(l.file.Fd()), unix.LOCK_SH) }

// Size fails closed on unreadable files instead of understating disk usage.
func Size(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// Budget is used while holding the user's writer lock. Keep is never evicted.
type Budget struct {
	Root     string
	Keep     string
	MaxBytes int64
	TTL      time.Duration
	mu       sync.Mutex
	ready    bool
	free     int64
}

func (b *Budget) MakeRoom(additional int64) error {
	if b.MaxBytes <= 0 && b.TTL <= 0 {
		return nil
	}
	if b.MaxBytes > 0 && additional > b.MaxBytes {
		return ErrCapacity
	}
	total, err := Size(b.Root)
	if err != nil {
		return err
	}
	entries, err := b.entries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		expired := b.TTL > 0 && time.Since(entry.modified) > b.TTL
		if (b.MaxBytes <= 0 || total+additional <= b.MaxBytes) && !expired {
			continue
		}
		if entry.path == b.Keep {
			continue
		}
		lock, err := TryLock(b.Root, entry.path, false)
		if err != nil {
			return err
		}
		if lock == nil {
			continue
		}
		size, err := Size(entry.path)
		if err == nil {
			err = os.RemoveAll(entry.path)
		}
		lock.Close()
		if err != nil {
			return err
		}
		total -= size
	}
	if b.MaxBytes > 0 && total+additional > b.MaxBytes {
		return ErrCapacity
	}
	return nil
}

type entry struct {
	path     string
	modified time.Time
}

func (b *Budget) entries() ([]entry, error) {
	var entries []entry
	err := filepath.WalkDir(b.Root, func(path string, item fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !item.IsDir() || path == b.Root {
			return nil
		}
		relative, err := filepath.Rel(b.Root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(relative, string(filepath.Separator))
		if parts[0] == ".locks" {
			return fs.SkipDir
		}
		// Repository names are untrusted: names such as snapshot-demo or
		// project.git must never be mistaken for the locked cache entry.
		_, metadataErr := os.Stat(filepath.Join(path, "metadata.json"))
		published := len(parts) == 3 && metadataErr == nil
		temporary := len(parts) == 2 && parts[0] == ".tmp" && strings.HasPrefix(item.Name(), "snapshot-")
		gitRoot := filepath.Base(b.Root) == ".pull" || filepath.Base(b.Root) == "pull"
		gitEntry := strings.HasSuffix(item.Name(), ".git") && ((len(parts) == 2 && parts[0] == ".pull") || (len(parts) == 1 && gitRoot))
		if published || temporary || gitEntry {
			info, err := item.Info()
			if err != nil {
				return err
			}
			entries = append(entries, entry{path, info.ModTime()})
			return fs.SkipDir
		}
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].modified.Before(entries[j].modified) })
	return entries, err
}

// Reserve accounts for growth before writes, including in-flight temporary data.
func (b *Budget) Reserve(growth int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.MaxBytes <= 0 {
		return nil
	}
	if !b.ready || growth > b.free {
		if err := b.MakeRoom(growth); err != nil {
			return err
		}
		used, err := Size(b.Root)
		if err != nil {
			return err
		}
		b.free = b.MaxBytes - used
		b.ready = true
	}
	if growth > b.free {
		return ErrCapacity
	}
	b.free -= growth
	return nil
}

// RemoveTree refuses to remove entries held by readers in any process.
func RemoveTree(ctx context.Context, root, writerRoot string) error {
	writer, err := Acquire(ctx, writerRoot, writerRoot+".writer", false)
	if err != nil {
		return err
	}
	defer writer.Close()
	budget := Budget{Root: root}
	entries, err := budget.entries()
	if err != nil {
		return err
	}
	locks := []*Lock{}
	defer func() {
		for _, lock := range locks {
			lock.Close()
		}
	}()
	for _, entry := range entries {
		lock, err := TryLock(writerRoot, entry.path, false)
		if err != nil {
			return err
		}
		if lock == nil {
			return errors.New("cache entries are in use; retry cleanup after active requests finish")
		}
		locks = append(locks, lock)
	}
	if filepath.Clean(root) != filepath.Clean(writerRoot) {
		return os.RemoveAll(root)
	}
	// Retain lock inodes: a waiter may already have opened one before cleanup.
	children, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, child := range children {
		if child.Name() == ".locks" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, child.Name())); err != nil {
			return err
		}
	}
	return nil
}

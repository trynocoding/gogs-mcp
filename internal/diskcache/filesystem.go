package diskcache

import (
	"io"
	"os"
	"sync"

	"github.com/go-git/go-billy/v5"
)

// Filesystem applies the shared disk budget before extending any Git file.
// The caller holds the user writer lock for this filesystem's lifetime.
type Filesystem struct {
	billy.Filesystem
	Budget *Budget
}

func (f *Filesystem) Create(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
}
func (f *Filesystem) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	file, err := f.Filesystem.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat(name)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &budgetFile{File: file, budget: f.Budget, size: info.Size(), appendMode: flag&os.O_APPEND != 0}, nil
}
func (f *Filesystem) TempFile(dir, prefix string) (billy.File, error) {
	file, err := f.Filesystem.TempFile(dir, prefix)
	if err != nil {
		return nil, err
	}
	return &budgetFile{File: file, budget: f.Budget}, nil
}
func (f *Filesystem) Chroot(path string) (billy.Filesystem, error) {
	child, err := f.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return &Filesystem{Filesystem: child, Budget: f.Budget}, nil
}

type budgetFile struct {
	billy.File
	budget     *Budget
	size       int64
	appendMode bool
	mu         sync.Mutex
}

func (f *budgetFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if f.appendMode {
		offset = max(offset, f.size)
	}
	growth := max(0, offset+int64(len(p))-f.size)
	if err := f.budget.Reserve(growth); err != nil {
		return 0, err
	}
	n, err := f.File.Write(p)
	f.size = max(f.size, offset+int64(n))
	return n, err
}
func (f *budgetFile) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.budget.Reserve(max(0, size-f.size)); err != nil {
		return err
	}
	if err := f.File.Truncate(size); err != nil {
		return err
	}
	f.size = size
	return nil
}

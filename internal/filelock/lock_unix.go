//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package filelock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock is an advisory exclusive lock held until Close. The lock file is kept
// on disk so that all processes contend on the same inode.
type Lock struct {
	file *os.File
}

func Acquire(path string) (*Lock, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve lock path %q: %w", path, err)
	}
	file, err := os.OpenFile(absPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", absPath, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock %q: %w", absPath, err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return fmt.Errorf("unlock table file: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close table lock: %w", closeErr)
	}
	return nil
}

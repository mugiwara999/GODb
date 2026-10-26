//go:build solaris

package filelock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Lock is an exclusive fcntl record lock held until Close. The lock file is
// kept on disk so that all processes contend on the same inode.
type Lock struct {
	file  *os.File
	mutex *sync.Mutex
}

var locks sync.Map

func Acquire(path string) (*Lock, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve lock path %q: %w", path, err)
	}
	value, _ := locks.LoadOrStore(absPath, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()

	file, err := os.OpenFile(absPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		mutex.Unlock()
		return nil, fmt.Errorf("open lock file %q: %w", absPath, err)
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: int16(io.SeekStart)}
	for {
		err = syscall.FcntlFlock(file.Fd(), syscall.F_SETLKW, &lock)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = file.Close()
		mutex.Unlock()
		return nil, fmt.Errorf("lock %q: %w", absPath, err)
	}
	return &Lock{file: file, mutex: mutex}, nil
}

func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	var errs []error
	if l.file != nil {
		unlock := syscall.Flock_t{Type: syscall.F_UNLCK, Whence: int16(io.SeekStart)}
		if err := syscall.FcntlFlock(l.file.Fd(), syscall.F_SETLK, &unlock); err != nil {
			errs = append(errs, fmt.Errorf("unlock table file: %w", err))
		}
		if err := l.file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close table lock: %w", err))
		}
		l.file = nil
	}
	if l.mutex != nil {
		l.mutex.Unlock()
		l.mutex = nil
	}
	return errors.Join(errs...)
}

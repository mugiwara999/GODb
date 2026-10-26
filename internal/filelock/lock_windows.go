//go:build windows

package filelock

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"golang.org/x/sys/windows"
)

// Lock is an exclusive byte-range lock held until Close. The lock file is
// kept on disk so that all processes contend on the same path.
type Lock struct {
	file       windows.Handle
	mutex      *sync.Mutex
	overlapped windows.Overlapped
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

	pathPointer, err := windows.UTF16PtrFromString(absPath)
	if err != nil {
		mutex.Unlock()
		return nil, fmt.Errorf("encode lock path %q: %w", absPath, err)
	}
	// Open synchronously so LockFileEx blocks here until the byte range is free.
	file, err := windows.CreateFile(pathPointer, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		mutex.Unlock()
		return nil, fmt.Errorf("open lock file %q: %w", absPath, err)
	}
	overlapped := windows.Overlapped{}
	if err := windows.LockFileEx(file, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		_ = windows.CloseHandle(file)
		mutex.Unlock()
		return nil, fmt.Errorf("lock %q: %w", absPath, err)
	}
	return &Lock{file: file, mutex: mutex, overlapped: overlapped}, nil
}

func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	var errs []error
	if l.file != 0 && l.file != windows.InvalidHandle {
		if err := windows.UnlockFileEx(l.file, 0, 1, 0, &l.overlapped); err != nil {
			errs = append(errs, fmt.Errorf("unlock table file: %w", err))
		}
		if err := windows.CloseHandle(l.file); err != nil {
			errs = append(errs, fmt.Errorf("close table lock: %w", err))
		}
		l.file = 0
	}
	if l.mutex != nil {
		l.mutex.Unlock()
		l.mutex = nil
	}
	return errors.Join(errs...)
}

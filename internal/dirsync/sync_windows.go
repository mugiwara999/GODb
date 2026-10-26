//go:build windows

package dirsync

import (
	"errors"
	"syscall"
)

// Sync asks Windows to flush directory metadata using a write-capable
// directory handle. Some Windows filesystems and configurations reject this
// operation even when the directory is writable. Those known unsupported
// responses are treated as best-effort success; callers must not assume that
// directory-entry changes are durable on those filesystems after power loss.
func Sync(path string) error {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		if isUnsupported(err) {
			return nil
		}
		return err
	}
	defer syscall.CloseHandle(handle)
	if err := syscall.FlushFileBuffers(handle); err != nil && !isUnsupported(err) {
		return err
	}
	return nil
}

func isUnsupported(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) ||
		errors.Is(err, syscall.Errno(1)) || // ERROR_INVALID_FUNCTION
		errors.Is(err, syscall.Errno(6)) || // ERROR_INVALID_HANDLE
		errors.Is(err, syscall.Errno(50)) // ERROR_NOT_SUPPORTED
}

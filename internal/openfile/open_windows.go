//go:build windows

package openfile

import (
	"os"
	"syscall"
)

// OpenReadWrite opens an existing file for read/write access while allowing
// other handles to delete or rename it. This lets index handles survive a
// logical index drop on Windows while table handles refresh their catalogs.
func OpenReadWrite(path string) (*os.File, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = syscall.CloseHandle(handle)
		return nil, syscall.EINVAL
	}
	return file, nil
}

//go:build !windows

package openfile

import "os"

// OpenReadWrite opens an existing file for read/write access.
func OpenReadWrite(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}

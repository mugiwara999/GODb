//go:build !windows

package dirsync

import "os"

// Sync flushes directory metadata through the platform's file-sync API.
func Sync(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

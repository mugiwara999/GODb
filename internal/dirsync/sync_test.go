package dirsync

import (
	"path/filepath"
	"testing"
)

func TestSyncDirectory(t *testing.T) {
	if err := Sync(t.TempDir()); err != nil {
		t.Fatalf("Sync temporary directory: %v", err)
	}
}

func TestSyncMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	if err := Sync(path); err == nil {
		t.Fatal("Sync accepted a missing directory")
	}
}

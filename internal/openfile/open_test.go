package openfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenReadWriteAllowsDeleteWhileOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "open.idx")
	if err := os.WriteFile(path, []byte("index"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := OpenReadWrite(path)
	if err != nil {
		t.Fatalf("OpenReadWrite: %v", err)
	}
	if err := os.Remove(path); err != nil {
		_ = file.Close()
		t.Fatalf("remove while open: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close open file after removal: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat removed file error = %v, want not exist", err)
	}
}

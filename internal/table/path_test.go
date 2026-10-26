package table

import (
	"path/filepath"
	"testing"
)

func TestTablePathUsesDataDirWithoutDotEnv(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	got, err := tablePath("Example")
	if err != nil {
		t.Fatalf("tablePath: %v", err)
	}
	want := filepath.Join(dataDir, "example.bin")
	if got != want {
		t.Fatalf("tablePath = %q, want %q", got, want)
	}
}

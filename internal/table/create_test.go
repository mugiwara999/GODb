package table

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mugiwara999/goDB/internal/btree"
	"github.com/mugiwara999/goDB/internal/filelock"
)

func TestCreateRefusesToOverwriteExistingTable(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	created, err := Create("keep_data", []string{"value"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := created.Insert([]string{"preserved"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := created.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	tablePath, _ := tablePath("keep_data")
	indexPath := btree.IndexPath("keep_data", "id")
	beforeTable, err := os.ReadFile(tablePath)
	if err != nil {
		t.Fatal(err)
	}
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create("keep_data", []string{"different"}); !errors.Is(err, ErrTableExists) {
		t.Fatalf("second Create error = %v, want ErrTableExists", err)
	}
	afterTable, err := os.ReadFile(tablePath)
	if err != nil {
		t.Fatal(err)
	}
	afterIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterTable, beforeTable) || !bytes.Equal(afterIndex, beforeIndex) {
		t.Fatal("rejected Create changed existing table or index data")
	}
}

func TestCreatePublicationFailureCleansPartialFiles(t *testing.T) {
	for _, failureStep := range []string{"after_marker", "after_table_publish", "after_index_publish"} {
		t.Run(failureStep, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DATA_DIR", dataDir)
			name := "create_" + failureStep
			path, err := tablePath(name)
			if err != nil {
				t.Fatal(err)
			}
			lock, err := filelock.Acquire(path + ".lock")
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected create interruption")
			_, err = createTableFiles(name, []string{"value"}, path, func(step string) error {
				if step == failureStep {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("createTableFiles error = %v, want injected failure", err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			indexPath := btree.IndexPath(name, "id")
			for _, path := range []string{path, indexPath, path + ".create", path + ".create-tmp", indexPath + ".create-tmp"} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("staging artifact %q remains (stat error %v)", path, err)
				}
			}
			created, err := Create(name, []string{"value"})
			if err != nil {
				t.Fatalf("Create after injected failure: %v", err)
			}
			if err := created.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenRecoversInterruptedCreateMarker(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	path, err := tablePath("interrupted")
	if err != nil {
		t.Fatal(err)
	}
	indexPath := btree.IndexPath("interrupted", "id")
	for _, filePath := range []string{path, indexPath} {
		if err := os.WriteFile(filePath, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path+".create", createMarkerMagic, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open("interrupted"); err == nil {
		t.Fatal("Open succeeded after recovering an incomplete create")
	}
	for _, filePath := range []string{path, indexPath, path + ".create"} {
		if _, err := os.Lstat(filePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("incomplete artifact %q remains (stat error %v)", filePath, err)
		}
	}
	created, err := Create("interrupted", []string{"value"})
	if err != nil {
		t.Fatalf("Create after recovery: %v", err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
}

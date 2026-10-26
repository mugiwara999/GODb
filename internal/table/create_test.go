package table

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mugiwara999/goDB/internal/btree"
	"github.com/mugiwara999/goDB/internal/filelock"
	"github.com/mugiwara999/goDB/internal/pager"
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

func TestCreateRejectsAmbiguousOrUnaddressableSchema(t *testing.T) {
	for _, tc := range []struct {
		name string
		cols []string
	}{
		{name: "generated primary key name", cols: []string{"id"}},
		{name: "case-insensitive duplicate", cols: []string{"Name", "name"}},
		{name: "reserved keyword", cols: []string{"where"}},
		{name: "invalid identifier", cols: []string{"display name"}},
		{name: "embedded NUL", cols: []string{"na\x00me"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DATA_DIR", dataDir)
			if _, err := Create("invalid_schema", tc.cols); err == nil {
				t.Fatalf("Create(%q) succeeded, want schema validation error", tc.cols)
			}
			entries, err := os.ReadDir(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("Create left files for rejected schema: %v", entries)
			}
		})
	}
}

func TestCreateAndOpenRejectUnaddressableTableNames(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	for _, name := range []string{"../escape", "select", "table name", "bad\x00name"} {
		t.Run(name, func(t *testing.T) {
			if _, err := Create(name, []string{"value"}); err == nil {
				t.Fatalf("Create(%q) succeeded, want invalid identifier error", name)
			}
			if _, err := Open(name); err == nil {
				t.Fatalf("Open(%q) succeeded, want invalid identifier error", name)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dataDir), "escape.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid table name created a file outside DATA_DIR (stat error %v)", err)
	}
}

func TestCreateAndOpenCanonicalizeIdentifiers(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	tbl, err := Create("Users", []string{"Name"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := tbl.GetColumns(); len(got) != 2 || got[0] != "id" || got[1] != "name" {
		t.Fatalf("GetColumns() = %q, want [id name]", got)
	}
	if err := tbl.Insert([]string{"Ada"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := tbl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open("USERS")
	if err != nil {
		t.Fatalf("Open with different identifier case: %v", err)
	}
	defer reopened.Close()
	rows, err := reopened.Select([]string{"name"}, nil)
	if err != nil {
		t.Fatalf("Select with canonical column name: %v", err)
	}
	if len(rows) != 2 || rows[1][0] != "Ada" {
		t.Fatalf("Select rows = %q, want header and Ada", rows)
	}
}

func TestOpenValidatesAndNormalizesStoredColumns(t *testing.T) {
	for _, tc := range []struct {
		name          string
		storedColumn  string
		wantOpenError bool
	}{
		{name: "legacy mixed case", storedColumn: "Name"},
		{name: "duplicate generated id", storedColumn: "id", wantOpenError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DATA_DIR", dataDir)
			tbl, err := Create("stored_schema", []string{"name"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			page, err := tbl.Pager.GetPage(0)
			if err != nil {
				t.Fatalf("GetPage metadata: %v", err)
			}
			const secondColumnOffset = pager.ColNamesOffset + len("id") + 1
			copy(page.Data[secondColumnOffset:], []byte(tc.storedColumn+"\x00"))
			if err := tbl.Pager.Flush(page); err != nil {
				t.Fatalf("Flush metadata: %v", err)
			}
			if err := tbl.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			reopened, err := Open("stored_schema")
			if tc.wantOpenError {
				if !errors.Is(err, pager.ErrMetadataCorrupt) {
					t.Fatalf("Open error = %v, want ErrMetadataCorrupt", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open legacy mixed-case metadata: %v", err)
			}
			defer reopened.Close()
			if got := reopened.GetColumns(); len(got) != 2 || got[1] != "name" {
				t.Fatalf("GetColumns() = %q, want canonical [id name]", got)
			}
		})
	}
}

func TestOpenRejectsTypedSchemaWithNonUint64PrimaryID(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	tbl, err := CreateWithTypes("bad_id_type", []Column{{Name: "age", Type: ColumnUint64}})
	if err != nil {
		t.Fatalf("CreateWithTypes: %v", err)
	}
	page, err := tbl.Pager.GetPage(0)
	if err != nil {
		t.Fatalf("GetPage metadata: %v", err)
	}
	typeOffset := pager.ColNamesOffset + len("id") + 1 + len("age") + 1
	page.Data[typeOffset] = byte(pager.ColumnTypeText)
	if err := tbl.Pager.Flush(page); err != nil {
		t.Fatalf("Flush metadata: %v", err)
	}
	if err := tbl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := Open("bad_id_type"); !errors.Is(err, pager.ErrMetadataCorrupt) {
		t.Fatalf("Open error = %v, want ErrMetadataCorrupt", err)
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

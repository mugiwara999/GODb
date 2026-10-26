package table

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mugiwara999/goDB/internal/btree"
)

func TestDropIndexUnlinkFailureLeavesSafeOrphan(t *testing.T) {
	t.Setenv("DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if err := os.MkdirAll(os.Getenv("DATA_DIR"), 0o755); err != nil {
		t.Fatal(err)
	}
	tbl, err := CreateWithTypes("drop_unlink", []Column{{Name: "age", Type: ColumnUint64}})
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Close()
	if err := tbl.Insert([]string{"42"}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.CreateIndex("age"); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected unlink failure")
	err = tbl.dropIndex("age", func(string) error { return injected }, syncDirectory)
	if !errors.Is(err, injected) {
		t.Fatalf("dropIndex error = %v, want injected unlink failure", err)
	}
	indexPath := btree.IndexPath("drop_unlink", "age")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("index file after failed unlink: %v", err)
	}

	reopened, err := Open("drop_unlink")
	if err != nil {
		t.Fatalf("Open after failed unlink: %v", err)
	}
	defer reopened.Close()
	if reopened.Indexes["age"] != nil {
		t.Fatal("Open loaded an unlisted orphan index")
	}
	rows, err := reopened.Select([]string{"age"}, []ColEq{{ColIdx: 1, Value: "42"}})
	if err != nil || len(rows) != 2 {
		t.Fatalf("scan with orphan index = %q, %v; want one row", rows, err)
	}
	if err := reopened.CreateIndex("age"); err != nil {
		t.Fatalf("recreate index over orphan: %v", err)
	}
	if err := reopened.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after recreate: %v", err)
	}
}

func TestDropIndexDirectorySyncFailureLeavesCatalogCleared(t *testing.T) {
	t.Setenv("DATA_DIR", filepath.Join(t.TempDir(), "data"))
	if err := os.MkdirAll(os.Getenv("DATA_DIR"), 0o755); err != nil {
		t.Fatal(err)
	}
	tbl, err := CreateWithTypes("drop_sync", []Column{{Name: "age", Type: ColumnUint64}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tbl.Insert([]string{"42"}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.CreateIndex("age"); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected directory sync failure")
	err = tbl.dropIndex("age", os.Remove, func(string) error { return injected })
	if !errors.Is(err, injected) {
		t.Fatalf("dropIndex error = %v, want injected directory sync failure", err)
	}
	if err := tbl.Close(); err != nil {
		t.Fatal(err)
	}
	indexPath := btree.IndexPath("drop_sync", "age")
	if _, err := os.Stat(indexPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index file after successful unlink = %v; want absent", err)
	}

	reopened, err := Open("drop_sync")
	if err != nil {
		t.Fatalf("Open after directory sync failure: %v", err)
	}
	defer reopened.Close()
	if reopened.Indexes["age"] != nil {
		t.Fatal("Open restored an index after its catalog bit was cleared")
	}
	rows, err := reopened.Select([]string{"age"}, []ColEq{{ColIdx: 1, Value: "42"}})
	if err != nil || len(rows) != 2 {
		t.Fatalf("scan after directory sync failure = %q, %v; want one row", rows, err)
	}
}

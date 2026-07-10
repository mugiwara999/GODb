package table_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mugiwara999/goDB/internal/table"
)

func setupTableTest(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	envPath := filepath.Join(root, ".env")
	if err := os.WriteFile(envPath, []byte("DATA_DIR="+dataDir+"\n"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	return dataDir
}

func TestTableThousandsOfInsertsAndTombstones(t *testing.T) {
	// Catches multi-page allocation bugs, iterator bugs, and delete/tombstone regressions.
	// These bugs are common because row iterators must skip deleted slots while page growth continues.
	setupTableTest(t)

	tbl, err := table.Create("users", []string{"name", "roll"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })

	const total = 1200
	wantRows := make([][]string, 0, total)
	for i := 0; i < total; i++ {
		row := []string{strconv.Itoa(i), fmt.Sprintf("user-%04d", i)}
		if err := tbl.Insert(row); err != nil {
			t.Fatalf("Insert(%d) failed: %v", i, err)
		}
		wantRows = append(wantRows, row)
	}

	if tbl.Pager.GetNumPages() <= 1 {
		t.Fatalf("GetNumPages = %d, want > 1 after thousands of inserts", tbl.Pager.GetNumPages())
	}

	res, err := tbl.Select([]string{"*"}, nil)
	if err != nil {
		t.Fatalf("Select all failed: %v", err)
	}
	if len(res) != total+1 {
		t.Fatalf("Select returned %d rows, want %d including header", len(res), total+1)
	}
	for i := range tbl.GetColumns() {
		if res[0][i] != tbl.GetColumns()[i] {
			t.Fatalf("header column %d = %q, want %q", i, res[0][i], tbl.GetColumns()[i])
		}
	}
	for i := 0; i < total; i++ {
		if res[i+1][1] != wantRows[i][0] || res[i+1][2] != wantRows[i][1] {
			t.Fatalf("row %d mismatch: got %q want %q", i, res[i+1], wantRows[i])
		}
	}

	deleted := make(map[int]struct{})
	beforePages := tbl.Pager.GetNumPages()
	for i := 0; i < total; i += 10 {
		if err := tbl.Delete([]table.ColEq{{ColIdx: 0, Value: strconv.Itoa(i)}}); err != nil {
			t.Fatalf("Delete(%d) failed: %v", i, err)
		}
		deleted[i] = struct{}{}
	}

	if tbl.Pager.GetNumPages() != beforePages {
		t.Fatalf("GetNumPages changed after tombstone deletes: before=%d after=%d", beforePages, tbl.Pager.GetNumPages())
	}

	res, err = tbl.Select([]string{"*"}, nil)
	if err != nil {
		t.Fatalf("Select after delete failed: %v", err)
	}

	wantCount := total - len(deleted)
	if len(res) != wantCount+1 {
		t.Fatalf("Select after delete returned %d rows, want %d including header", len(res), wantCount+1)
	}

	pos := 1
	for i := 0; i < total; i++ {
		if _, ok := deleted[i]; ok {
			continue
		}
		if res[pos][1] != wantRows[i][0] || res[pos][2] != wantRows[i][1] {
			t.Fatalf("row %d mismatch after delete: got %q want %q", i, res[pos], wantRows[i])
		}
		pos++
	}
}

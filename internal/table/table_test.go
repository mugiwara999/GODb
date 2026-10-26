package table_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mugiwara999/goDB/internal/btree"
	"github.com/mugiwara999/goDB/internal/table"
)

func setupTableTest(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	t.Setenv("DATA_DIR", dataDir)

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
	if err := tbl.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("ValidatePrimaryIndex after deletes: %v", err)
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

func TestRejectedInsertDoesNotConsumeIDOrAllocatePage(t *testing.T) {
	setupTableTest(t)

	tbl, err := table.Create("users", []string{"name"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })

	initialPages := tbl.Pager.GetNumPages()
	if err := tbl.Insert(nil); err == nil {
		t.Fatal("Insert with missing values succeeded, want validation error")
	}
	if err := tbl.Insert([]string{"bad\x00value"}); err == nil {
		t.Fatal("Insert with NUL in value succeeded, want validation error")
	}

	lastID, err := tbl.Pager.LastID()
	if err != nil {
		t.Fatalf("LastID after rejected insert: %v", err)
	}
	if lastID != 0 {
		t.Fatalf("LastID after rejected insert = %d, want 0", lastID)
	}
	if got := tbl.Pager.GetNumPages(); got != initialPages {
		t.Fatalf("page count after rejected insert = %d, want %d", got, initialPages)
	}

	if err := tbl.Insert([]string{"alice"}); err != nil {
		t.Fatalf("valid Insert failed: %v", err)
	}
	rows, err := tbl.Select([]string{"id", "name"}, []table.ColEq{{ColIdx: 0, Value: "0"}})
	if err != nil {
		t.Fatalf("Select inserted row by ID: %v", err)
	}
	if len(rows) != 2 || rows[1][0] != "0" || rows[1][1] != "alice" {
		t.Fatalf("Select by ID = %q, want header and row [0 alice]", rows)
	}
}

func TestIndexInsertFailureRollsBackLiveRowAndKeepsIDsMonotonic(t *testing.T) {
	setupTableTest(t)

	tbl, err := table.Create("rollback_insert", []string{"name"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })

	if err := tbl.Indexes["id"].Close(); err != nil {
		t.Fatalf("close primary index: %v", err)
	}
	if err := tbl.Insert([]string{"unindexed"}); err == nil {
		t.Fatal("Insert succeeded with closed primary index")
	}
	rows, err := tbl.Select([]string{"*"}, nil)
	if err != nil {
		t.Fatalf("Select after failed index insert: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Select after failed index insert returned %d rows, want header only", len(rows))
	}
	lastID, err := tbl.Pager.LastID()
	if err != nil {
		t.Fatalf("LastID after failed index insert: %v", err)
	}
	if lastID != 1 {
		t.Fatalf("LastID after failed index insert = %d, want reserved ID count 1", lastID)
	}

	index, err := btree.OpenIndex("rollback_insert", "id")
	if err != nil {
		t.Fatalf("reopen primary index: %v", err)
	}
	tbl.Indexes["id"] = index
	if err := tbl.Insert([]string{"valid"}); err != nil {
		t.Fatalf("Insert after reopening index: %v", err)
	}
	rows, err = tbl.Select([]string{"*"}, nil)
	if err != nil {
		t.Fatalf("Select after valid insert: %v", err)
	}
	if len(rows) != 2 || rows[1][0] != "1" || rows[1][1] != "valid" {
		t.Fatalf("Select after valid insert = %q, want row [1 valid]", rows)
	}
}

func TestOversizedInsertDoesNotConsumeIDOrAllocatePage(t *testing.T) {
	setupTableTest(t)

	tbl, err := table.Create("users", []string{"name"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })

	initialPages := tbl.Pager.GetNumPages()
	if err := tbl.Insert([]string{string(make([]byte, 4096))}); err == nil {
		t.Fatal("oversized Insert succeeded, want page-full error")
	}
	lastID, err := tbl.Pager.LastID()
	if err != nil {
		t.Fatalf("LastID after oversized insert: %v", err)
	}
	if lastID != 0 {
		t.Fatalf("LastID after oversized insert = %d, want 0", lastID)
	}
	if got := tbl.Pager.GetNumPages(); got != initialPages {
		t.Fatalf("page count after oversized insert = %d, want %d", got, initialPages)
	}
}

func TestVariableLengthUpdatePreservesIDsAndIndex(t *testing.T) {
	setupTableTest(t)

	tbl, err := table.Create("updates", []string{"name"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })

	const rowCount = 20
	for i := 0; i < rowCount; i++ {
		if err := tbl.Insert([]string{"old"}); err != nil {
			t.Fatalf("Insert(%d): %v", i, err)
		}
	}
	if err := tbl.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("validate index before update: %v", err)
	}

	longValue := strings.Repeat("long", 120)
	if err := tbl.Update(nil, []table.UpdateValue{{ColIdx: 1, Value: longValue}}); err != nil {
		t.Fatalf("grow rows: %v", err)
	}
	if err := tbl.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("validate index after row relocation: %v", err)
	}
	rows, err := tbl.Select([]string{"*"}, nil)
	if err != nil {
		t.Fatalf("Select after grow: %v", err)
	}
	if len(rows) != rowCount+1 {
		t.Fatalf("row count after grow = %d, want %d", len(rows)-1, rowCount)
	}
	for _, id := range []string{"0", "10", "19"} {
		got, err := tbl.Select([]string{"*"}, []table.ColEq{{ColIdx: 0, Value: id}})
		if err != nil || len(got) != 2 || got[1][0] != id || got[1][1] != longValue {
			t.Fatalf("Select ID %s after grow = %v, err=%v", id, got, err)
		}
	}

	if err := tbl.Update([]table.ColEq{{ColIdx: 1, Value: longValue}}, []table.UpdateValue{{ColIdx: 1, Value: "x"}}); err != nil {
		t.Fatalf("shrink rows: %v", err)
	}
	rows, err = tbl.Select([]string{"*"}, nil)
	if err != nil || len(rows) != rowCount+1 {
		t.Fatalf("Select after shrink returned %d rows, err=%v", len(rows), err)
	}
	for _, id := range []string{"0", "10", "19"} {
		got, err := tbl.Select([]string{"*"}, []table.ColEq{{ColIdx: 0, Value: id}})
		if err != nil || len(got) != 2 || got[1][0] != id || got[1][1] != "x" {
			t.Fatalf("Select ID %s after shrink = %v, err=%v", id, got, err)
		}
	}

	if err := tbl.Close(); err != nil {
		t.Fatalf("Close before reopen: %v", err)
	}
	tbl, err = table.Open("updates")
	if err != nil {
		t.Fatalf("Open after updates: %v", err)
	}
	if err := tbl.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("validate index after reopen: %v", err)
	}
	got, err := tbl.Select([]string{"*"}, []table.ColEq{{ColIdx: 0, Value: "19"}})
	if err != nil || len(got) != 2 || got[1][1] != "x" {
		t.Fatalf("Select ID 19 after reopen = %v, err=%v", got, err)
	}
}

func TestOversizedUpdateLeavesExistingRowsLive(t *testing.T) {
	setupTableTest(t)

	tbl, err := table.Create("oversized_update", []string{"name"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })
	if err := tbl.Insert([]string{"original"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := tbl.Update(nil, []table.UpdateValue{{ColIdx: 1, Value: strings.Repeat("x", 4096)}}); err == nil {
		t.Fatal("oversized update succeeded")
	}
	rows, err := tbl.Select([]string{"*"}, nil)
	if err != nil || len(rows) != 2 || rows[1][1] != "original" {
		t.Fatalf("row after failed oversized update = %v, err=%v", rows, err)
	}
}

func TestSelectByIDHandlesMissingAndDeletedRows(t *testing.T) {
	setupTableTest(t)

	tbl, err := table.Create("select_deleted", []string{"name"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })
	for _, name := range []string{"first", "second"} {
		if err := tbl.Insert([]string{name}); err != nil {
			t.Fatalf("Insert(%q): %v", name, err)
		}
	}
	if err := tbl.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("validate index before delete: %v", err)
	}
	if err := tbl.Indexes["id"].UpdateRID(1, 1, 999); err != nil {
		t.Fatalf("corrupt ID 1 index RID: %v", err)
	}
	if err := tbl.ValidatePrimaryIndex(); err == nil {
		t.Fatal("ValidatePrimaryIndex accepted an invalid row location")
	}
	if err := tbl.Indexes["id"].UpdateRID(1, 1, 1); err != nil {
		t.Fatalf("restore ID 1 index RID: %v", err)
	}

	if err := tbl.Delete([]table.ColEq{{ColIdx: 0, Value: "0"}}); err != nil {
		t.Fatalf("Delete ID 0: %v", err)
	}
	if err := tbl.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("ValidatePrimaryIndex after delete: %v", err)
	}
	for _, id := range []string{"0", "99"} {
		rows, err := tbl.Select([]string{"id", "name"}, []table.ColEq{{ColIdx: 0, Value: id}})
		if err != nil {
			t.Fatalf("Select ID %s: %v", id, err)
		}
		if len(rows) != 1 || rows[0][0] != "id" || rows[0][1] != "name" {
			t.Fatalf("Select ID %s = %q, want header only", id, rows)
		}
	}
	if err := tbl.Close(); err != nil {
		t.Fatalf("Close before reopen: %v", err)
	}
	tbl, err = table.Open("select_deleted")
	if err != nil {
		t.Fatalf("Open after delete: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })
	if err := tbl.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("ValidatePrimaryIndex after reopen: %v", err)
	}

	if err := tbl.Indexes["id"].UpdateRID(1, 1, 999); err != nil {
		t.Fatalf("corrupt ID 1 index RID: %v", err)
	}
	if _, err := tbl.Select([]string{"*"}, []table.ColEq{{ColIdx: 0, Value: "1"}}); err == nil {
		t.Fatal("Select with invalid slot RID succeeded, want corruption error")
	}
}

func TestOpenRejectsCorruptPrimaryIndex(t *testing.T) {
	dataDir := setupTableTest(t)
	tbl, err := table.Create("corrupt_primary", []string{"name"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := tbl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	indexPath := filepath.Join(dataDir, "corrupt_primary_id.idx")
	if err := os.WriteFile(indexPath, []byte("truncated"), 0o644); err != nil {
		t.Fatalf("truncate primary index: %v", err)
	}
	if opened, err := table.Open("corrupt_primary"); err == nil {
		_ = opened.Close()
		t.Fatal("Open succeeded with a truncated primary index")
	}
}

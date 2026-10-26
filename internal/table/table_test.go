package table_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mugiwara999/goDB/internal/table"
	"github.com/mugiwara999/goDB/internal/wal"
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

func TestRejectMalformedRowsAtTableReadAndMutationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{name: "extra unterminated bytes", payload: []byte("0\x00Ada\x00tail")},
		{name: "extra terminated field", payload: []byte("0\x00Ada\x00extra\x00")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTableTest(t)
			tbl, err := table.Create("malformed", []string{"name"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			defer tbl.Close()
			if err := tbl.Insert([]string{"Ada"}); err != nil {
				t.Fatalf("Insert: %v", err)
			}
			page, err := tbl.Pager.GetPage(1)
			if err != nil {
				t.Fatalf("GetPage: %v", err)
			}
			if err := page.ReplaceRow(0, tc.payload); err != nil {
				t.Fatalf("ReplaceRow malformed payload: %v", err)
			}
			if err := tbl.Pager.Flush(page); err != nil {
				t.Fatalf("Flush malformed payload: %v", err)
			}

			checks := []struct {
				name string
				run  func() error
			}{
				{name: "table scan select", run: func() error { _, err := tbl.Select([]string{"*"}, nil); return err }},
				{name: "indexed select", run: func() error { _, err := tbl.Select([]string{"*"}, []table.ColEq{{ColIdx: 0, Value: "0"}}); return err }},
				{name: "delete", run: func() error { return tbl.Delete(nil) }},
				{name: "update", run: func() error { return tbl.Update(nil, []table.UpdateValue{{ColIdx: 1, Value: "Grace"}}) }},
				{name: "primary index validation", run: tbl.ValidatePrimaryIndex},
			}
			for _, check := range checks {
				if err := check.run(); err == nil || !strings.Contains(err.Error(), "corrupt row") {
					t.Errorf("%s error = %v, want corrupt row error", check.name, err)
				}
			}
		})
	}
}

func TestTypedUint64ColumnsCanonicalizeValidateAndPersist(t *testing.T) {
	setupTableTest(t)
	tbl, err := table.CreateWithTypes("typed_users", []table.Column{
		{Name: "age", Type: table.ColumnUint64},
		{Name: "name", Type: table.ColumnText},
	})
	if err != nil {
		t.Fatalf("CreateWithTypes: %v", err)
	}
	if got, want := tbl.GetColumnTypes(), []table.ColumnType{table.ColumnUint64, table.ColumnUint64, table.ColumnText}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GetColumnTypes() = %v, want %v", got, want)
	}
	if err := tbl.Insert([]string{"00042", "Ada"}); err != nil {
		t.Fatalf("Insert canonicalizable UINT64: %v", err)
	}
	rows, err := tbl.Select([]string{"age", "name"}, []table.ColEq{{ColIdx: 1, Value: "00042"}})
	if err != nil {
		t.Fatalf("Select numeric filter: %v", err)
	}
	if len(rows) != 2 || strings.Join(rows[1], ",") != "42,Ada" {
		t.Fatalf("Select numeric row = %q, want [42 Ada]", rows)
	}
	if err := tbl.Update([]table.ColEq{{ColIdx: 0, Value: "0"}}, []table.UpdateValue{{ColIdx: 1, Value: "00043"}}); err != nil {
		t.Fatalf("Update UINT64: %v", err)
	}
	if err := tbl.Insert([]string{"not-a-number", "Grace"}); err == nil {
		t.Fatal("Insert accepted invalid UINT64 value")
	}
	if err := tbl.Update([]table.ColEq{{ColIdx: 0, Value: "0"}}, []table.UpdateValue{{ColIdx: 1, Value: "18446744073709551616"}}); err == nil {
		t.Fatal("Update accepted overflowing UINT64 value")
	}
	if _, err := tbl.Select([]string{"*"}, []table.ColEq{{ColIdx: 1, Value: "1.5"}}); err == nil {
		t.Fatal("Select accepted invalid UINT64 filter")
	}
	if err := tbl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := table.Open("TYPED_USERS")
	if err != nil {
		t.Fatalf("Open typed table: %v", err)
	}
	defer reopened.Close()
	if got, want := reopened.GetColumnTypes(), []table.ColumnType{table.ColumnUint64, table.ColumnUint64, table.ColumnText}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GetColumnTypes after reopen = %v, want %v", got, want)
	}
	rows, err = reopened.Select([]string{"age", "name"}, nil)
	if err != nil {
		t.Fatalf("Select after reopen: %v", err)
	}
	if len(rows) != 2 || strings.Join(rows[1], ",") != "43,Ada" {
		t.Fatalf("reopened rows = %q, want [43 Ada]", rows)
	}
	if err := reopened.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("ValidatePrimaryIndex: %v", err)
	}
}

func TestTypedRowCorruptionIsRejectedByReadersAndValidator(t *testing.T) {
	setupTableTest(t)
	tbl, err := table.CreateWithTypes("typed_corrupt", []table.Column{
		{Name: "age", Type: table.ColumnUint64},
		{Name: "name", Type: table.ColumnText},
	})
	if err != nil {
		t.Fatalf("CreateWithTypes: %v", err)
	}
	defer tbl.Close()
	if err := tbl.Insert([]string{"42", "Ada"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	page, err := tbl.Pager.GetPage(1)
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	if err := page.ReplaceRow(0, []byte("0\x00not-a-number\x00Ada\x00")); err != nil {
		t.Fatalf("ReplaceRow: %v", err)
	}
	if err := tbl.Pager.Flush(page); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	checks := []struct {
		name string
		run  func() error
	}{
		{name: "scan select", run: func() error { _, err := tbl.Select([]string{"*"}, nil); return err }},
		{name: "indexed select", run: func() error { _, err := tbl.Select([]string{"*"}, []table.ColEq{{ColIdx: 0, Value: "0"}}); return err }},
		{name: "update", run: func() error { return tbl.Update(nil, []table.UpdateValue{{ColIdx: 2, Value: "Grace"}}) }},
		{name: "primary index validation", run: tbl.ValidatePrimaryIndex},
	}
	for _, check := range checks {
		if err := check.run(); err == nil || !strings.Contains(err.Error(), "corrupt row") {
			t.Errorf("%s error = %v, want corrupt row error", check.name, err)
		}
	}
}

func TestTypedUint64SecondaryIndexTracksMutationsAndReopens(t *testing.T) {
	setupTableTest(t)
	tbl, err := table.CreateWithTypes("indexed_people", []table.Column{
		{Name: "age", Type: table.ColumnUint64},
		{Name: "name", Type: table.ColumnText},
	})
	if err != nil {
		t.Fatalf("CreateWithTypes: %v", err)
	}
	if err := tbl.Insert([]string{"42", "Ada"}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Insert([]string{"00042", "Grace"}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Insert([]string{"50", "Lin"}); err != nil {
		t.Fatal(err)
	}
	otherHandle, err := table.Open("indexed_people")
	if err != nil {
		t.Fatalf("Open second handle: %v", err)
	}
	defer otherHandle.Close()
	if err := tbl.CreateIndex("AGE"); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err := tbl.CreateIndex("age"); !errors.Is(err, table.ErrIndexExists) {
		t.Fatalf("duplicate CreateIndex error = %v, want ErrIndexExists", err)
	}
	if err := tbl.CreateIndex("name"); err == nil {
		t.Fatal("CreateIndex accepted TEXT column")
	}
	if err := tbl.CreateIndex("missing"); err == nil {
		t.Fatal("CreateIndex accepted missing column")
	}
	if err := tbl.CreateIndex("id"); err == nil {
		t.Fatal("CreateIndex accepted generated primary key")
	}
	if err := otherHandle.Insert([]string{"42", "Katherine"}); err != nil {
		t.Fatalf("Insert from handle opened before index creation: %v", err)
	}
	rows, err := tbl.Select([]string{"age", "name"}, []table.ColEq{{ColIdx: 1, Value: "00042"}})
	if err != nil {
		t.Fatalf("indexed Select: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("indexed rows = %q, want header plus three age 42 rows", rows)
	}
	if err := tbl.Update([]table.ColEq{{ColIdx: 2, Value: "Ada"}}, []table.UpdateValue{{ColIdx: 1, Value: "00050"}}); err != nil {
		t.Fatalf("Update indexed key: %v", err)
	}
	rows, err = tbl.Select([]string{"name"}, []table.ColEq{{ColIdx: 1, Value: "50"}})
	if err != nil {
		t.Fatalf("Select updated key: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows for age 50 = %q, want header plus Lin and Ada", rows)
	}
	if err := tbl.Delete([]table.ColEq{{ColIdx: 2, Value: "Grace"}}); err != nil {
		t.Fatalf("Delete indexed row: %v", err)
	}
	rows, err = tbl.Select([]string{"name"}, []table.ColEq{{ColIdx: 1, Value: "42"}})
	if err != nil {
		t.Fatalf("Select after delete: %v", err)
	}
	if len(rows) != 2 || rows[1][0] != "Katherine" {
		t.Fatalf("remaining age 42 rows = %q, want only Katherine", rows)
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes: %v", err)
	}
	if err := tbl.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := table.Open("indexed_people")
	if err != nil {
		t.Fatalf("Open after index creation: %v", err)
	}
	defer reopened.Close()
	rows, err = reopened.Select([]string{"age", "name"}, []table.ColEq{{ColIdx: 1, Value: "50"}})
	if err != nil {
		t.Fatalf("indexed Select after reopen: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("reopened age 50 rows = %q, want header plus two rows", rows)
	}
	if err := reopened.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after reopen: %v", err)
	}
}

func TestSecondaryIndexTracksRelocatedRows(t *testing.T) {
	setupTableTest(t)
	tbl, err := table.CreateWithTypes("indexed_relocation", []table.Column{
		{Name: "code", Type: table.ColumnUint64},
		{Name: "payload", Type: table.ColumnText},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Close()
	for i := 0; i < 20; i++ {
		if err := tbl.Insert([]string{strconv.Itoa(i % 2), strings.Repeat("x", 175)}); err != nil {
			t.Fatalf("Insert(%d): %v", i, err)
		}
	}
	if err := tbl.CreateIndex("code"); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	before, err := tbl.Indexes["id"].Find(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := tbl.Update([]table.ColEq{{ColIdx: 0, Value: "0"}}, []table.UpdateValue{{ColIdx: 2, Value: strings.Repeat("y", 3000)}}); err != nil {
		t.Fatalf("Update with row relocation: %v", err)
	}
	after, err := tbl.Indexes["id"].Find(0)
	if err != nil {
		t.Fatal(err)
	}
	if before.GetPageID() == after.GetPageID() {
		t.Fatalf("row did not relocate: location remained page %d", before.GetPageID())
	}
	rows, err := tbl.Select([]string{"payload"}, []table.ColEq{{ColIdx: 1, Value: "0"}})
	if err != nil {
		t.Fatalf("indexed Select after relocation: %v", err)
	}
	if len(rows) != 11 {
		t.Fatalf("indexed rows after relocation = %d, want header plus 10 rows", len(rows))
	}
	foundLarge := false
	for _, row := range rows[1:] {
		if row[0] == strings.Repeat("y", 3000) {
			foundLarge = true
		}
	}
	if !foundLarge {
		t.Fatal("relocated row is missing from secondary-index results")
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after relocation: %v", err)
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

func TestInsertRejectsClosedIndexBeforeChangingTable(t *testing.T) {
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
	if lastID != 0 {
		t.Fatalf("LastID after failed index insert = %d, want rollback to 0", lastID)
	}

	if err := tbl.Pager.Close(); err != nil {
		t.Fatalf("close stale pager: %v", err)
	}
	tbl, err = table.Open("rollback_insert")
	if err != nil {
		t.Fatalf("reopen after rejected insert: %v", err)
	}
	if err := tbl.Insert([]string{"valid"}); err != nil {
		t.Fatalf("Insert after reopen: %v", err)
	}
	rows, err = tbl.Select([]string{"*"}, nil)
	if err != nil {
		t.Fatalf("Select after valid insert: %v", err)
	}
	if len(rows) != 2 || rows[1][0] != "0" || rows[1][1] != "valid" {
		t.Fatalf("Select after valid insert = %q, want row [0 valid]", rows)
	}
}

func TestOpenRecoversUncommittedTableAndIndexChanges(t *testing.T) {
	setupTableTest(t)
	tbl, err := table.Create("wal_recovery", []string{"name"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dataPath := tbl.Path
	journal, err := wal.Begin(dataPath + ".wal")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tbl.Pager.SetJournal(journal)
	tbl.Indexes["id"].SetJournal(journal)
	if err := tbl.Pager.SetLastID(41); err != nil {
		t.Fatalf("set uncommitted last ID: %v", err)
	}
	if err := tbl.Indexes["id"].Insert(41, 1, 0); err != nil {
		t.Fatalf("insert uncommitted index key: %v", err)
	}
	if err := journal.CloseUncommitted(); err != nil {
		t.Fatalf("simulate crash: %v", err)
	}
	if err := tbl.Close(); err != nil {
		t.Fatalf("close stale handles: %v", err)
	}

	reopened, err := table.Open("wal_recovery")
	if err != nil {
		t.Fatalf("Open after crash: %v", err)
	}
	defer reopened.Close()
	lastID, err := reopened.Pager.LastID()
	if err != nil {
		t.Fatalf("LastID after recovery: %v", err)
	}
	if lastID != 0 {
		t.Fatalf("LastID after recovery = %d, want 0", lastID)
	}
	if _, err := reopened.Indexes["id"].Find(41); err == nil {
		t.Fatal("uncommitted index key survived recovery")
	}
	if err := reopened.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("ValidatePrimaryIndex after recovery: %v", err)
	}
}

func TestDeleteRejectsClosedIndexBeforeChangingTable(t *testing.T) {
	setupTableTest(t)
	tbl, err := table.Create("rollback_delete", []string{"name"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })
	if err := tbl.Insert([]string{"kept"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := tbl.Indexes["id"].Close(); err != nil {
		t.Fatalf("close primary index: %v", err)
	}
	if err := tbl.Delete([]table.ColEq{{ColIdx: 1, Value: "kept"}}); err == nil {
		t.Fatal("Delete succeeded with closed primary index")
	}
	rows, err := tbl.Select([]string{"*"}, nil)
	if err != nil {
		t.Fatalf("Select after failed Delete: %v", err)
	}
	if len(rows) != 2 || rows[1][0] != "0" || rows[1][1] != "kept" {
		t.Fatalf("row after failed Delete = %q, want [0 kept]", rows)
	}
	if err := tbl.Pager.Close(); err != nil {
		t.Fatalf("close stale pager: %v", err)
	}
	tbl, err = table.Open("rollback_delete")
	if err != nil {
		t.Fatalf("reopen after rejected Delete: %v", err)
	}
	if err := tbl.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("ValidatePrimaryIndex after reopen: %v", err)
	}
}

func TestConcurrentWritersOnSeparateHandlesSerializeAndRefresh(t *testing.T) {
	setupTableTest(t)
	first, err := table.Create("shared_writers", []string{"value"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := table.Open("shared_writers")
	if err != nil {
		_ = first.Close()
		t.Fatalf("Open second handle: %v", err)
	}
	defer first.Close()
	defer second.Close()

	const insertsPerHandle = 50
	var workers sync.WaitGroup
	errCh := make(chan error, 2)
	for handleID, tbl := range []*table.Table{first, second} {
		workers.Add(1)
		go func(handleID int, tbl *table.Table) {
			defer workers.Done()
			for i := 0; i < insertsPerHandle; i++ {
				if err := tbl.Insert([]string{fmt.Sprintf("%d-%d", handleID, i)}); err != nil {
					errCh <- err
					return
				}
			}
			errCh <- nil
		}(handleID, tbl)
	}
	workers.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent insert: %v", err)
		}
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first handle: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second handle: %v", err)
	}
	reopened, err := table.Open("shared_writers")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	rows, err := reopened.Select([]string{"*"}, nil)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got, want := len(rows)-1, 2*insertsPerHandle; got != want {
		t.Fatalf("live rows = %d, want %d", got, want)
	}
	if err := reopened.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("ValidatePrimaryIndex: %v", err)
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

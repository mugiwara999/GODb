package table_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

func TestTypedSecondaryIndexesTrackMutationsAndReopens(t *testing.T) {
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
	if err := tbl.CreateIndex("name"); err != nil {
		t.Fatalf("CreateIndex(TEXT): %v", err)
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

func TestDropSecondaryIndexRefreshesOlderHandlesAndFallsBackToScan(t *testing.T) {
	setupTableTest(t)
	tbl, err := table.CreateWithTypes("drop_index", []table.Column{
		{Name: "age", Type: table.ColumnUint64},
		{Name: "name", Type: table.ColumnText},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tbl.Insert([]string{"42", "Ada"}); err != nil {
		t.Fatal(err)
	}
	oldHandle, err := table.Open("drop_index")
	if err != nil {
		t.Fatal(err)
	}
	defer oldHandle.Close()
	if err := tbl.CreateIndex("age"); err != nil {
		t.Fatal(err)
	}
	if err := oldHandle.Insert([]string{"42", "Grace"}); err != nil {
		t.Fatalf("old-handle insert after create: %v", err)
	}
	if err := tbl.DropIndex("age"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	if tbl.Indexes["age"] != nil {
		t.Fatal("DropIndex left the index open on the dropping handle")
	}
	rows, err := tbl.Select([]string{"name"}, []table.ColEq{{ColIdx: 1, Value: "42"}})
	if err != nil || len(rows) != 3 {
		t.Fatalf("Select after drop = %q, %v; want two rows", rows, err)
	}
	if oldHandle.Indexes["age"] == nil {
		t.Fatal("old handle did not retain its pre-refresh index fixture")
	}
	rows, err = oldHandle.Select([]string{"name"}, []table.ColEq{{ColIdx: 1, Value: "42"}})
	if err != nil || len(rows) != 3 {
		t.Fatalf("old-handle Select after drop = %q, %v; want two rows", rows, err)
	}
	if oldHandle.Indexes["age"] != nil {
		t.Fatal("old handle did not discard the dropped index before reading")
	}
	if err := oldHandle.Insert([]string{"50", "Lin"}); err != nil {
		t.Fatalf("old-handle insert after drop: %v", err)
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after drop: %v", err)
	}
	if err := tbl.DropIndex("age"); !errors.Is(err, table.ErrIndexNotFound) {
		t.Fatalf("second DropIndex error = %v, want ErrIndexNotFound", err)
	}
	if err := tbl.DropIndex("id"); err == nil {
		t.Fatal("DropIndex accepted generated ID")
	}
	if err := tbl.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := table.Open("drop_index")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Indexes["age"] != nil {
		t.Fatal("Open restored a dropped index")
	}
	rows, err = reopened.Select([]string{"name"}, []table.ColEq{{ColIdx: 1, Value: "50"}})
	if err != nil || len(rows) != 2 || rows[1][0] != "Lin" {
		t.Fatalf("scan after reopen = %q, %v; want Lin", rows, err)
	}
}

func TestCloseSerializesWithSelectAndIsIdempotent(t *testing.T) {
	setupTableTest(t)
	tbl, err := table.Create("close_concurrent", []string{"value"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tbl.Close() })
	if err := tbl.Insert([]string{"kept"}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	selectErr := make(chan error, 1)
	validateErr := make(chan error, 1)
	closeErr := make(chan error, 1)
	go func() {
		<-start
		rows, err := tbl.Select([]string{"value"}, nil)
		if err == nil && (len(rows) != 2 || rows[1][0] != "kept") {
			selectErr <- fmt.Errorf("Select returned %q", rows)
			return
		}
		selectErr <- err
	}()
	go func() {
		<-start
		validateErr <- tbl.ValidateIndexes()
	}()
	go func() {
		<-start
		closeErr <- tbl.Close()
	}()
	close(start)
	if err := <-closeErr; err != nil {
		t.Fatalf("concurrent Close: %v", err)
	}
	if err := <-selectErr; err != nil && !errors.Is(err, table.ErrTableClosed) {
		t.Fatalf("concurrent Select error = %v, want success or ErrTableClosed", err)
	}
	if err := <-validateErr; err != nil && !errors.Is(err, table.ErrTableClosed) {
		t.Fatalf("concurrent ValidateIndexes error = %v, want success or ErrTableClosed", err)
	}
	if err := tbl.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := tbl.Insert([]string{"after close"}); !errors.Is(err, table.ErrTableClosed) {
		t.Fatalf("Insert after Close error = %v, want ErrTableClosed", err)
	}
	if _, err := tbl.Select([]string{"*"}, nil); !errors.Is(err, table.ErrTableClosed) {
		t.Fatalf("Select after Close error = %v, want ErrTableClosed", err)
	}
	if err := tbl.ValidateIndexes(); !errors.Is(err, table.ErrTableClosed) {
		t.Fatalf("ValidateIndexes after Close error = %v, want ErrTableClosed", err)
	}
}

func TestSelectWithMultipleIndexesReturnsSameRowsInAnyFilterOrder(t *testing.T) {
	setupTableTest(t)
	tbl, err := table.CreateWithTypes("multi_index_select", []table.Column{
		{Name: "account", Type: table.ColumnUint64},
		{Name: "code", Type: table.ColumnUint64},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Close()
	for i := 0; i < 40; i++ {
		if err := tbl.Insert([]string{"1", strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range []string{"account", "code"} {
		if err := tbl.CreateIndex(column); err != nil {
			t.Fatalf("CreateIndex(%s): %v", column, err)
		}
	}
	filters := []table.ColEq{{ColIdx: 1, Value: "1"}, {ColIdx: 2, Value: "27"}}
	for _, filterOrder := range [][]table.ColEq{filters, {filters[1], filters[0]}} {
		rows, err := tbl.Select([]string{"account", "code"}, filterOrder)
		if err != nil {
			t.Fatalf("Select(%v): %v", filterOrder, err)
		}
		if len(rows) != 2 || rows[1][0] != "1" || rows[1][1] != "27" {
			t.Fatalf("Select(%v) = %q; want the same single row", filterOrder, rows)
		}
	}
	missingFilters := []table.ColEq{{ColIdx: 1, Value: "1"}, {ColIdx: 2, Value: "999"}}
	for _, filterOrder := range [][]table.ColEq{missingFilters, {missingFilters[1], missingFilters[0]}} {
		rows, err := tbl.Select([]string{"account", "code"}, filterOrder)
		if err != nil || len(rows) != 1 {
			t.Fatalf("Select missing match with %v = %q, %v; want header only", filterOrder, rows, err)
		}
	}
}

func TestSelectRangeMatchesScanAndTracksIndexedMutations(t *testing.T) {
	setupTableTest(t)
	tbl, err := table.CreateWithTypes("range_select", []table.Column{
		{Name: "age", Type: table.ColumnUint64},
		{Name: "team", Type: table.ColumnText},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Close()
	for i := 0; i <= 20; i++ {
		team := "red"
		if i%2 == 0 {
			team = "blue"
		}
		if err := tbl.Insert([]string{strconv.Itoa(i), team}); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range [][]string{{"10", "blue"}, {"10", "blue"}, {"11", "blue"}} {
		if err := tbl.Insert(row); err != nil {
			t.Fatal(err)
		}
	}
	lo5, hi10 := uint64(5), uint64(10)
	lo5b, hi6 := uint64(5), uint64(6)
	hi2 := uint64(2)
	lo8, hi12 := uint64(8), uint64(12)
	cases := []struct {
		name    string
		rangeBy table.Uint64Range
		filters []table.ColEq
	}{
		{name: "inclusive bounds", rangeBy: table.Uint64Range{ColIdx: 1, Lower: &lo5, LowerInclusive: true, Upper: &hi10, UpperInclusive: true}},
		{name: "exclusive empty interval", rangeBy: table.Uint64Range{ColIdx: 1, Lower: &lo5b, LowerInclusive: false, Upper: &hi6, UpperInclusive: false}},
		{name: "unbounded lower", rangeBy: table.Uint64Range{ColIdx: 1, Upper: &hi2, UpperInclusive: true}},
		{name: "combined equality", rangeBy: table.Uint64Range{ColIdx: 1, Lower: &lo8, LowerInclusive: true, Upper: &hi12, UpperInclusive: true}, filters: []table.ColEq{{ColIdx: 2, Value: "blue"}}},
	}
	before := make(map[string][][]string)
	for _, tc := range cases {
		rows, err := tbl.SelectRange([]string{"age", "team"}, tc.rangeBy, tc.filters)
		if err != nil {
			t.Fatalf("scan %s: %v", tc.name, err)
		}
		before[tc.name] = rows
	}
	if err := tbl.CreateIndex("age"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		rows, err := tbl.SelectRange([]string{"age", "team"}, tc.rangeBy, tc.filters)
		if err != nil {
			t.Fatalf("indexed %s: %v", tc.name, err)
		}
		sortRows := func(rows [][]string) []string {
			keys := make([]string, len(rows))
			for i, row := range rows {
				keys[i] = strings.Join(row, "\x00")
			}
			sort.Strings(keys)
			return keys
		}
		if !reflect.DeepEqual(sortRows(rows), sortRows(before[tc.name])) {
			t.Fatalf("scan and index differ for %s: scan=%q indexed=%q", tc.name, before[tc.name], rows)
		}
	}
	if err := tbl.Update([]table.ColEq{{ColIdx: 1, Value: "10"}, {ColIdx: 2, Value: "blue"}}, []table.UpdateValue{{ColIdx: 1, Value: "12"}}); err != nil {
		t.Fatalf("Update range key: %v", err)
	}
	if err := tbl.Delete([]table.ColEq{{ColIdx: 1, Value: "11"}}); err != nil {
		t.Fatalf("Delete range key: %v", err)
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after range mutations: %v", err)
	}
	if err := tbl.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := table.Open("range_select")
	if err != nil {
		t.Fatalf("Open after range mutations: %v", err)
	}
	defer reopened.Close()
	rows, err := reopened.SelectRange([]string{"age", "team"}, cases[0].rangeBy, nil)
	if err != nil {
		t.Fatalf("SelectRange after reopen: %v", err)
	}
	for _, row := range rows[1:] {
		age, _ := strconv.ParseUint(row[0], 10, 64)
		if age < 5 || age > 10 {
			t.Fatalf("range after reopen returned out-of-range age %q", row[0])
		}
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

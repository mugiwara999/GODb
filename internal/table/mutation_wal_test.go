package table

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mugiwara999/goDB/internal/btree"
)

func TestMutationRollbackAfterPartialTableAndIndexWrites(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	tbl, err := Create("partial_writes", []string{"value"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer tbl.Close()

	injected := errors.New("injected failure after partial writes")
	err = tbl.mutate(func() error {
		if err := tbl.Pager.SetLastID(1); err != nil {
			return err
		}
		if err := tbl.Indexes["id"].Insert(0, 1, 0); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("mutate error = %v, want injected failure", err)
	}

	lastID, err := tbl.Pager.LastID()
	if err != nil {
		t.Fatalf("LastID after rollback: %v", err)
	}
	if lastID != 0 {
		t.Fatalf("LastID after rollback = %d, want 0", lastID)
	}
	if _, err := tbl.Indexes["id"].Find(0); !errors.Is(err, btree.ErrKeyNotFound) {
		t.Fatalf("index lookup after rollback error = %v, want key not found", err)
	}
	if err := tbl.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("ValidatePrimaryIndex after rollback: %v", err)
	}
}

func TestSecondaryIndexErrorRollsBackTableMutation(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	tbl, err := CreateWithTypes("secondary_rollback", []Column{
		{Name: "age", Type: ColumnUint64},
		{Name: "name", Type: ColumnText},
	})
	if err != nil {
		t.Fatalf("CreateWithTypes: %v", err)
	}
	defer tbl.Close()
	if err := tbl.Insert([]string{"42", "Ada"}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Insert([]string{"42", "Lin"}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.CreateIndex("age"); err != nil {
		t.Fatal(err)
	}
	secondary := tbl.Indexes["age"]
	_, leaf, err := secondary.FindLeaf(42)
	if err != nil {
		t.Fatal(err)
	}
	marker := leaf.GetRID(0)
	posting, err := secondary.GetPage(marker.GetPageID())
	if err != nil {
		t.Fatal(err)
	}
	originalPosting := posting.Data
	posting.SetPostingCount(0)
	if err := secondary.WritePage(posting); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Insert([]string{"42", "Grace"}); err == nil {
		t.Fatal("Insert succeeded despite corrupt secondary posting page")
	}
	posting.Data = originalPosting
	if err := tbl.Indexes["age"].WritePage(posting); err != nil {
		t.Fatalf("restore secondary posting page: %v", err)
	}
	rows, err := tbl.Select([]string{"name"}, []ColEq{{ColIdx: 1, Value: "42"}})
	if err != nil {
		t.Fatalf("Select after rollback: %v", err)
	}
	if len(rows) != 3 || rows[1][0] != "Ada" || rows[2][0] != "Lin" {
		t.Fatalf("rows after rollback = %q, want Ada and Lin", rows)
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after rollback: %v", err)
	}
}

func TestRangeUpdateRollbackRestoresRowsAndIndexes(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	tbl, err := CreateWithTypes("range_update_rollback", []Column{
		{Name: "age", Type: ColumnUint64},
		{Name: "name", Type: ColumnText},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Close()
	for _, row := range [][]string{{"10", "A"}, {"20", "B"}, {"30", "C"}} {
		if err := tbl.Insert(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := tbl.CreateIndex("age"); err != nil {
		t.Fatal(err)
	}
	low, high := uint64(10), uint64(30)
	rangeBy := Uint64Range{ColIdx: 1, Lower: &low, LowerInclusive: true, Upper: &high, UpperInclusive: false}
	injected := errors.New("injected failure after range update")
	err = tbl.mutate(func() error {
		if err := tbl.update(nil, []UpdateValue{{ColIdx: 1, Value: "99"}}, &rangeBy); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("range update mutation error = %v, want injected failure", err)
	}
	for _, value := range []string{"10", "20"} {
		rows, err := tbl.Select([]string{"name"}, []ColEq{{ColIdx: 1, Value: value}})
		if err != nil || len(rows) != 2 {
			t.Fatalf("Select age %s after rollback = %q, %v; want one row", value, rows, err)
		}
	}
	rows, err := tbl.Select([]string{"name"}, []ColEq{{ColIdx: 1, Value: "99"}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("Select rolled-back age 99 = %q, %v; want header only", rows, err)
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after range rollback: %v", err)
	}
}

func TestRangeMutationRejectsStaleSecondaryIndexCandidate(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	tbl, err := CreateWithTypes("stale_range_candidate", []Column{
		{Name: "age", Type: ColumnUint64},
		{Name: "name", Type: ColumnText},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Close()
	if err := tbl.Insert([]string{"10", "Ada"}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.CreateIndex("age"); err != nil {
		t.Fatal(err)
	}
	rowIt := tbl.Pager.RowIterator()
	if rowData, err := rowIt.Next(); err != nil || rowData == nil {
		t.Fatalf("read inserted row location: data=%v err=%v", rowData, err)
	}
	info := rowIt.GetCurrentInfo()
	ridPage, ridSlot := uint32(info.PageID), uint16(info.SlotID)
	secondary := tbl.Indexes["age"]
	if err := secondary.Insert(999, ridPage, ridSlot); err != nil {
		t.Fatal(err)
	}
	point := uint64(999)
	err = tbl.UpdateRange(Uint64Range{ColIdx: 1, Lower: &point, LowerInclusive: true, Upper: &point, UpperInclusive: true}, nil,
		[]UpdateValue{{ColIdx: 2, Value: "Grace"}})
	if err == nil {
		t.Fatal("UpdateRange succeeded with a stale range index entry")
	}
	// mutate refreshes the table's index handles after rollback.
	if err := tbl.Indexes["age"].Delete(999); err != nil {
		t.Fatalf("remove injected stale index entry: %v", err)
	}
	rows, err := tbl.Select([]string{"name"}, []ColEq{{ColIdx: 1, Value: "10"}})
	if err != nil || len(rows) != 2 || rows[1][0] != "Ada" {
		t.Fatalf("row after rejected stale candidate = %q, %v; want Ada", rows, err)
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after stale candidate cleanup: %v", err)
	}
}

func TestRangeMutationsFallBackToTableScan(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	tbl, err := CreateWithTypes("range_mutation_scan", []Column{
		{Name: "age", Type: ColumnUint64},
		{Name: "name", Type: ColumnText},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Close()
	for _, row := range [][]string{{"10", "A"}, {"20", "B"}, {"30", "C"}} {
		if err := tbl.Insert(row); err != nil {
			t.Fatal(err)
		}
	}
	lower, upper := uint64(10), uint64(20)
	if err := tbl.UpdateRange(Uint64Range{ColIdx: 1, Lower: &lower, LowerInclusive: true, Upper: &upper, UpperInclusive: true}, nil,
		[]UpdateValue{{ColIdx: 2, Value: "updated"}}); err != nil {
		t.Fatalf("UpdateRange without secondary index: %v", err)
	}
	deleteLower := uint64(30)
	if err := tbl.DeleteRange(Uint64Range{ColIdx: 1, Lower: &deleteLower, LowerInclusive: true}, nil); err != nil {
		t.Fatalf("DeleteRange without secondary index: %v", err)
	}
	rows, err := tbl.Select([]string{"age", "name"}, nil)
	if err != nil || len(rows) != 3 || rows[1][1] != "updated" || rows[2][1] != "updated" {
		t.Fatalf("rows after scan mutations = %q, %v; want two updated rows", rows, err)
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after scan mutations: %v", err)
	}
}

func TestRangeMutationCandidateLimitFallsBackToScan(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	tbl, err := CreateWithTypes("range_candidate_limit", []Column{
		{Name: "age", Type: ColumnUint64},
		{Name: "name", Type: ColumnText},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Close()
	for i := 0; i <= maxRangeMutationIndexCandidates; i++ {
		if err := tbl.Insert([]string{"7", "old"}); err != nil {
			t.Fatalf("Insert(%d): %v", i, err)
		}
	}
	if err := tbl.CreateIndex("age"); err != nil {
		t.Fatal(err)
	}
	lower, upper := uint64(7), uint64(7)
	selectedRange := Uint64Range{ColIdx: 1, Lower: &lower, LowerInclusive: true, Upper: &upper, UpperInclusive: true}
	candidates, indexed, err := tbl.rangeMutationCandidates(selectedRange, nil, "test")
	if err != nil || indexed || len(candidates) != 0 {
		t.Fatalf("rangeMutationCandidates = (%d candidates, indexed=%t, err=%v); want scan fallback", len(candidates), indexed, err)
	}
	if err := tbl.UpdateRange(selectedRange, nil, []UpdateValue{{ColIdx: 2, Value: "new"}}); err != nil {
		t.Fatalf("UpdateRange through scan fallback: %v", err)
	}
	rows, err := tbl.SelectRange([]string{"name"}, selectedRange, nil)
	if err != nil || len(rows) != maxRangeMutationIndexCandidates+2 {
		t.Fatalf("SelectRange returned %d rows, err=%v; want header and %d updates", len(rows), err, maxRangeMutationIndexCandidates+1)
	}
	for i, row := range rows[1:] {
		if row[0] != "new" {
			t.Fatalf("row %d has name %q, want new", i, row[0])
		}
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after scan fallback: %v", err)
	}
}

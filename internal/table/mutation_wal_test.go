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

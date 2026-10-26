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

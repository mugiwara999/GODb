package btree

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/mugiwara999/goDB/internal/wal"
)

func TestIndexWritePageDetectsShortWriteAndJournalRestoresPage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	index, err := NewIndex("wal_short_write", "id")
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer index.Close()
	page, err := index.GetPage(1)
	if err != nil {
		t.Fatal(err)
	}
	original := page.Data
	journal, err := wal.Begin(index.path + ".wal")
	if err != nil {
		t.Fatal(err)
	}
	index.SetJournal(journal)
	index.writeAtFn = func(data []byte, offset int64) (int, error) {
		return index.file.WriteAt(data[:len(data)/2], offset)
	}
	page.Data[len(page.Data)-1] ^= 0xff
	if err := index.WritePage(page); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WritePage error = %v, want io.ErrShortWrite", err)
	}
	if err := journal.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	index.SetJournal(nil)
	index.writeAtFn = nil
	restored, err := index.GetPage(1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Data[:], original[:]) {
		t.Fatal("partial index write was not restored")
	}
}

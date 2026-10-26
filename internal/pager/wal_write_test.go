package pager

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mugiwara999/goDB/internal/wal"
)

func TestFlushPartialWriteRollsBackFromJournalBeforeImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "table.bin")
	original := bytes.Repeat([]byte{0x3c}, PAGE_SIZE)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	pg, err := OpenPager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	page, err := pg.GetPage(0)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := wal.Begin(path + ".wal")
	if err != nil {
		t.Fatal(err)
	}
	pg.SetJournal(journal)
	injected := errors.New("injected partial page write")
	pg.writeFn = func(file *os.File, page *Page) error {
		_, err := file.WriteAt(page.Data[:PAGE_SIZE/2], 0)
		if err != nil {
			return err
		}
		return injected
	}
	page.Data[PAGE_SIZE-1] ^= 0xff
	if err := pg.Flush(page); !errors.Is(err, injected) {
		t.Fatalf("Flush error = %v, want injected write error", err)
	}
	if err := journal.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	pg.SetJournal(nil)
	pg.writeFn = nil
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("partial pager write was not restored")
	}
}

package wal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverRestoresBeforeImagesAndOriginalFileSize(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "table.db")
	journalPath := filepath.Join(dir, "table.db.wal")
	original := bytes.Repeat([]byte{0x2a}, 2*4096)
	if err := os.WriteFile(dataPath, original, 0o644); err != nil {
		t.Fatalf("write original data: %v", err)
	}
	dataFile, err := os.OpenFile(dataPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open data: %v", err)
	}
	j, err := Begin(journalPath)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	changedPage := bytes.Repeat([]byte{0x55}, 4096)
	if err := j.BeforePageWrite(dataPath, dataFile, 1, 4096); err != nil {
		t.Fatalf("journal existing page: %v", err)
	}
	if _, err := dataFile.WriteAt(changedPage, 4096); err != nil {
		t.Fatalf("write changed page: %v", err)
	}
	newPage := bytes.Repeat([]byte{0x66}, 4096)
	if err := j.BeforePageWrite(dataPath, dataFile, 2, 4096); err != nil {
		t.Fatalf("journal new page: %v", err)
	}
	if _, err := dataFile.WriteAt(newPage, 2*4096); err != nil {
		t.Fatalf("write new page: %v", err)
	}
	if err := j.CloseUncommitted(); err != nil {
		t.Fatalf("simulate process exit: %v", err)
	}
	if err := dataFile.Close(); err != nil {
		t.Fatalf("close changed data: %v", err)
	}

	if err := Recover(journalPath); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("read recovered data: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("recovered file differs from original: got %d bytes, want %d", len(got), len(original))
	}
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("journal still exists after recovery: stat error=%v", err)
	}
}

func TestCommitKeepsDataAndRemovesJournal(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "index.idx")
	journalPath := filepath.Join(dir, "index.idx.wal")
	original := bytes.Repeat([]byte{0x11}, 4096)
	if err := os.WriteFile(dataPath, original, 0o644); err != nil {
		t.Fatalf("write original data: %v", err)
	}
	dataFile, err := os.OpenFile(dataPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open data: %v", err)
	}
	defer dataFile.Close()
	j, err := Begin(journalPath)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	changed := bytes.Repeat([]byte{0x77}, 4096)
	if err := j.BeforePageWrite(dataPath, dataFile, 0, 4096); err != nil {
		t.Fatalf("journal page: %v", err)
	}
	if _, err := dataFile.WriteAt(changed, 0); err != nil {
		t.Fatalf("write changed page: %v", err)
	}
	if err := j.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("read committed data: %v", err)
	}
	if !bytes.Equal(got, changed) {
		t.Fatal("committed data was reverted")
	}
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("journal still exists after commit: stat error=%v", err)
	}
}

func TestRecoverRejectsCorruptJournalWithoutChangingData(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "table.db")
	journalPath := filepath.Join(dir, "table.db.wal")
	original := bytes.Repeat([]byte{0x34}, 4096)
	if err := os.WriteFile(dataPath, original, 0o644); err != nil {
		t.Fatalf("write original data: %v", err)
	}
	dataFile, err := os.OpenFile(dataPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open data: %v", err)
	}
	j, err := Begin(journalPath)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := j.BeforePageWrite(dataPath, dataFile, 0, 4096); err != nil {
		t.Fatalf("journal page: %v", err)
	}
	if _, err := dataFile.WriteAt(bytes.Repeat([]byte{0x45}, 4096), 0); err != nil {
		t.Fatalf("write changed page: %v", err)
	}
	if err := j.CloseUncommitted(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	if err := dataFile.Close(); err != nil {
		t.Fatalf("close data: %v", err)
	}
	logData, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	logData[len(logData)-1] ^= 0xff
	if err := os.WriteFile(journalPath, logData, 0o600); err != nil {
		t.Fatalf("corrupt journal: %v", err)
	}
	if err := Recover(journalPath); err == nil {
		t.Fatal("Recover accepted a checksum-corrupt journal")
	}
	got, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("read data after failed recovery: %v", err)
	}
	if bytes.Equal(got, original) {
		t.Fatal("corrupted journal unexpectedly changed data during recovery")
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("corrupt journal was removed: %v", err)
	}
}

func TestRecoverRejectsJournalTargetsOutsideItsDirectory(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	outsideDir := filepath.Join(root, "outside")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(outsideDir, "important.db")
	original := bytes.Repeat([]byte{0x21}, 4096)
	if err := os.WriteFile(dataPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	dataFile, err := os.OpenFile(dataPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(dataDir, "table.db.wal")
	j, err := Begin(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.BeforePageWrite(dataPath, dataFile, 0, 4096); err != nil {
		t.Fatal(err)
	}
	if _, err := dataFile.WriteAt(bytes.Repeat([]byte{0x32}, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if err := j.CloseUncommitted(); err != nil {
		t.Fatal(err)
	}
	if err := dataFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Recover(journalPath); err == nil {
		t.Fatal("Recover accepted a journal targeting another directory")
	}
	got, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, original) {
		t.Fatal("rejected recovery unexpectedly changed target data")
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("rejected journal was removed: %v", err)
	}
}

func TestRecoverTreatsTornCommitRecordAsUncommitted(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "table.db")
	journalPath := filepath.Join(dir, "table.db.wal")
	original := bytes.Repeat([]byte{0x18}, 4096)
	if err := os.WriteFile(dataPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	dataFile, err := os.OpenFile(dataPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	j, err := Begin(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.BeforePageWrite(dataPath, dataFile, 0, 4096); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Repeat([]byte{0x29}, 4096)
	if _, err := dataFile.WriteAt(changed, 0); err != nil {
		t.Fatal(err)
	}
	if err := dataFile.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := j.appendRecord(recordCommit, nil); err != nil {
		t.Fatal(err)
	}
	if err := j.CloseUncommitted(); err != nil {
		t.Fatal(err)
	}
	if err := dataFile.Close(); err != nil {
		t.Fatal(err)
	}
	logData, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, logData[:len(logData)-1], 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Recover(journalPath); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("torn commit record was treated as committed")
	}
}

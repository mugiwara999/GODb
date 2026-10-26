package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func appendJournalTestRecord(data []byte, kind byte, payload []byte) []byte {
	var header [5]byte
	header[0] = kind
	binary.LittleEndian.PutUint32(header[1:], uint32(len(payload)))
	data = append(data, header[:]...)
	data = append(data, payload...)
	var checksum [4]byte
	binary.LittleEndian.PutUint32(checksum[:], crc32.ChecksumIEEE(data[len(data)-5-len(payload):]))
	return append(data, checksum[:]...)
}

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

func TestInjectedJournalFailuresFailClosedOrRestoreOriginalPage(t *testing.T) {
	for _, point := range []string{"record_page_before", "sync_data_file", "record_commit"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			dataPath := filepath.Join(dir, "table.db")
			journalPath := dataPath + ".wal"
			original := bytes.Repeat([]byte{0x4a}, 4096)
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
			injected := errors.New("injected WAL I/O failure")
			j.fault = func(at string) error {
				if at == point {
					return injected
				}
				return nil
			}
			beforeErr := j.BeforePageWrite(dataPath, dataFile, 0, 4096)
			if point == "record_page_before" {
				if !errors.Is(beforeErr, injected) {
					t.Fatalf("BeforePageWrite error = %v, want injected failure", beforeErr)
				}
			} else {
				if beforeErr != nil {
					t.Fatalf("BeforePageWrite: %v", beforeErr)
				}
				changed := bytes.Repeat([]byte{0x5b}, 4096)
				if _, err := dataFile.WriteAt(changed, 0); err != nil {
					t.Fatal(err)
				}
				if err := j.Commit(); !errors.Is(err, injected) {
					t.Fatalf("Commit error = %v, want injected failure", err)
				}
			}
			if err := j.Rollback(); err != nil {
				t.Fatalf("Rollback: %v", err)
			}
			if err := dataFile.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(dataPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("data differs from original after %s failure", point)
			}
		})
	}
}

func TestRecoveryCanBeRetriedAfterInjectedRestoreFailures(t *testing.T) {
	for _, point := range []string{"restore_page", "truncate_file"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			dataPath := filepath.Join(dir, "table.db")
			journalPath := dataPath + ".wal"
			original := bytes.Repeat([]byte{0x71}, 4096)
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
			pageID := uint32(0)
			if point == "truncate_file" {
				pageID = 1
			}
			if err := j.BeforePageWrite(dataPath, dataFile, pageID, 4096); err != nil {
				t.Fatal(err)
			}
			changed := bytes.Repeat([]byte{0x82}, 4096)
			if _, err := dataFile.WriteAt(changed, int64(pageID)*4096); err != nil {
				t.Fatal(err)
			}
			if err := j.CloseUncommitted(); err != nil {
				t.Fatal(err)
			}
			if err := dataFile.Close(); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected recovery failure")
			fault := func(at string, file *os.File, data []byte, offset int64) error {
				if at != point {
					return nil
				}
				if point == "restore_page" {
					_, err := file.WriteAt(data[:len(data)/2], offset)
					if err != nil {
						return err
					}
				}
				return injected
			}
			if err := recoverWith(journalPath, fault); !errors.Is(err, injected) {
				t.Fatalf("first recovery error = %v, want injected failure", err)
			}
			if _, err := os.Stat(journalPath); err != nil {
				t.Fatalf("journal removed after failed recovery: %v", err)
			}
			if err := Recover(journalPath); err != nil {
				t.Fatalf("retry recovery: %v", err)
			}
			got, err := os.ReadFile(dataPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatal("retry recovery did not restore original file")
			}
		})
	}
}

func TestReadLogRecordSizeLimit(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, strings.Repeat("x", maxRecordSize-13-len(dir)))
	payload := make([]byte, 12+len(path))
	binary.LittleEndian.PutUint32(payload[:4], uint32(len(path)))
	copy(payload[12:], path)
	validLog := append([]byte(nil), journalMagic...)
	validLog = appendJournalTestRecord(validLog, recordFileStart, payload)
	validPath := filepath.Join(dir, "max-record.wal")
	if err := os.WriteFile(validPath, validLog, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLog(validPath); err != nil {
		t.Fatalf("readLog rejected a record at the size limit: %v", err)
	}

	tooLargeHeader := make([]byte, 5)
	tooLargeHeader[0] = recordFileStart
	binary.LittleEndian.PutUint32(tooLargeHeader[1:], maxRecordSize+1)
	tooLargeLog := append(append([]byte(nil), journalMagic...), tooLargeHeader...)
	tooLargePath := filepath.Join(dir, "oversized-record.wal")
	if err := os.WriteFile(tooLargePath, tooLargeLog, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLog(tooLargePath); err == nil {
		t.Fatal("readLog accepted a record over the size limit")
	}
}

func FuzzReadLogDoesNotPanic(f *testing.F) {
	path := filepath.Join(os.TempDir(), "godb-wal-fuzz-target")
	fileStart := make([]byte, 12+len(path))
	binary.LittleEndian.PutUint32(fileStart[:4], uint32(len(path)))
	binary.LittleEndian.PutUint64(fileStart[4:12], 4096)
	copy(fileStart[12:], path)
	valid := append([]byte(nil), journalMagic...)
	valid = appendJournalTestRecord(valid, recordFileStart, fileStart)
	valid = appendJournalTestRecord(valid, recordPageBefore, encodePageBefore(path, 0, 16, bytes.Repeat([]byte{0xa5}, 16)))
	f.Add(valid)
	f.Add(valid[:len(valid)-1]) // torn checksum
	badChecksum := append([]byte(nil), valid...)
	badChecksum[len(badChecksum)-1] ^= 0xff
	f.Add(badChecksum)
	oversized := make([]byte, len(journalMagic)+5)
	copy(oversized, journalMagic)
	oversized[len(journalMagic)] = recordFileStart
	binary.LittleEndian.PutUint32(oversized[len(journalMagic)+1:], maxRecordSize+1)
	f.Add(oversized)
	unknownRecord := append([]byte(nil), journalMagic...)
	f.Add(appendJournalTestRecord(unknownRecord, 0xff, nil))
	committedThenMore := append([]byte(nil), valid...)
	committedThenMore = appendJournalTestRecord(committedThenMore, recordCommit, nil)
	committedThenMore = appendJournalTestRecord(committedThenMore, recordCommit, nil)
	f.Add(committedThenMore)

	fuzzPath := filepath.Join(f.TempDir(), "journal.wal")
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 64<<10 {
			input = input[:64<<10]
		}
		if err := os.WriteFile(fuzzPath, input, 0o600); err != nil {
			t.Fatalf("write fuzz journal: %v", err)
		}
		_, _ = readLog(fuzzPath)
	})
}

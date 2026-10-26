package table

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mugiwara999/goDB/internal/btree"
	"github.com/mugiwara999/goDB/internal/filelock"
	"github.com/mugiwara999/goDB/internal/wal"
)

var ErrIndexExists = errors.New("index already exists")

// CreateIndex builds a non-unique index for a UINT64 column. It scans a stable
// table image under the same process and filesystem locks used by mutations,
// writes to a staging path, and publishes the complete index with one rename.
func (t *Table) CreateIndex(column string) error {
	if t == nil || t.Pager == nil {
		return fmt.Errorf("create index: table is not open")
	}
	t.mutationMu.Lock()
	defer t.mutationMu.Unlock()
	if t.poisoned {
		return fmt.Errorf("create index on table %q: table must be reopened after a failed rollback", t.Name)
	}
	canonical, err := canonicalIdentifier(column, "column")
	if err != nil {
		return err
	}
	columnIndex := -1
	for i, name := range t.cols {
		if name == canonical {
			columnIndex = i
			break
		}
	}
	if columnIndex < 0 {
		return fmt.Errorf("create index on table %q: column %q does not exist", t.Name, canonical)
	}
	if columnIndex == 0 {
		return fmt.Errorf("create index on table %q: generated primary key already has a unique index", t.Name)
	}
	if t.colTypes[columnIndex] != ColumnUint64 {
		return fmt.Errorf("create index on table %q: column %q must have UINT64 type", t.Name, canonical)
	}
	if t.Indexes[canonical] != nil {
		return fmt.Errorf("create index on table %q column %q: %w", t.Name, canonical, ErrIndexExists)
	}

	lock, err := filelock.Acquire(t.Path + ".lock")
	if err != nil {
		return fmt.Errorf("lock table %q for index creation: %w", t.Name, err)
	}
	defer lock.Close()
	if err := wal.Recover(t.Path + ".wal"); err != nil {
		return fmt.Errorf("recover table %q before index creation: %w", t.Name, err)
	}
	if err := t.Pager.Refresh(); err != nil {
		return fmt.Errorf("refresh table %q before index creation: %w", t.Name, err)
	}
	if err := t.refreshIndexes(); err != nil {
		return fmt.Errorf("refresh indexes for table %q: %w", t.Name, err)
	}
	if t.Indexes[canonical] != nil {
		return fmt.Errorf("create index on table %q column %q: %w", t.Name, canonical, ErrIndexExists)
	}

	finalPath := btree.IndexPath(t.Name, canonical)
	if _, err := os.Stat(finalPath); err == nil {
		return fmt.Errorf("create index on table %q column %q: %w", t.Name, canonical, ErrIndexExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect index path %q: %w", finalPath, err)
	}
	tempPath := finalPath + ".build-tmp"
	if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale index staging file %q: %w", tempPath, err)
	}
	index, err := btree.NewIndexAt(t.Name, canonical, tempPath)
	if err != nil {
		return err
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	rowIt := t.Pager.RowIterator()
	for {
		data, err := rowIt.Next()
		if err != nil {
			_ = index.Close()
			return fmt.Errorf("scan table %q while building index %q: %w", t.Name, canonical, err)
		}
		if data == nil {
			break
		}
		row, err := t.decodeRow(data)
		if err != nil {
			_ = index.Close()
			return fmt.Errorf("scan table %q while building index %q: corrupt row: %w", t.Name, canonical, err)
		}
		key, err := strconv.ParseUint(row[columnIndex], 10, 64)
		if err != nil {
			_ = index.Close()
			return fmt.Errorf("scan table %q while building index %q: invalid UINT64 value: %w", t.Name, canonical, err)
		}
		info := rowIt.GetCurrentInfo()
		if err := index.InsertDuplicate(key, uint32(info.PageID), uint16(info.SlotID)); err != nil {
			_ = index.Close()
			return fmt.Errorf("insert row into index %q: %w", canonical, err)
		}
	}
	if err := index.Sync(); err != nil {
		_ = index.Close()
		return fmt.Errorf("sync staged index %q: %w", canonical, err)
	}
	if err := index.Close(); err != nil {
		return fmt.Errorf("close staged index %q: %w", canonical, err)
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return fmt.Errorf("publish index %q: %w", canonical, err)
	}
	removeTemp = false
	if err := syncDirectory(filepath.Dir(finalPath)); err != nil {
		_ = os.Remove(finalPath)
		_ = syncDirectory(filepath.Dir(finalPath))
		return fmt.Errorf("sync index directory for %q: %w", canonical, err)
	}
	published, err := btree.OpenIndex(t.Name, canonical)
	if err != nil {
		_ = os.Remove(finalPath)
		_ = syncDirectory(filepath.Dir(finalPath))
		return fmt.Errorf("open published index %q: %w", canonical, err)
	}
	t.Indexes[canonical] = published
	return nil
}

func (t *Table) refreshIndexes() error {
	for columnIndex, column := range t.cols {
		if columnIndex > 0 && t.colTypes[columnIndex] != ColumnUint64 {
			continue
		}
		if index := t.Indexes[column]; index != nil {
			if err := index.Refresh(); err != nil {
				return fmt.Errorf("refresh index %q: %w", column, err)
			}
			continue
		}
		path := btree.IndexPath(t.Name, column)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect index %q: %w", column, err)
		}
		index, err := btree.OpenIndex(t.Name, column)
		if err != nil {
			return fmt.Errorf("open index %q: %w", column, err)
		}
		index.SetJournal(nil)
		t.Indexes[column] = index
	}
	return nil
}

func (t *Table) maintainSecondaryIndexes(oldRow, newRow []string, oldPage uint32, oldSlot uint16, newPage uint32, newSlot uint16) error {
	for columnIndex := 1; columnIndex < len(t.cols); columnIndex++ {
		index := t.Indexes[t.cols[columnIndex]]
		if index == nil || (oldRow[columnIndex] == newRow[columnIndex] && oldPage == newPage && oldSlot == newSlot) {
			continue
		}
		oldKey, err := strconv.ParseUint(oldRow[columnIndex], 10, 64)
		if err != nil {
			return fmt.Errorf("read old index key for column %q: %w", t.cols[columnIndex], err)
		}
		newKey, err := strconv.ParseUint(newRow[columnIndex], 10, 64)
		if err != nil {
			return fmt.Errorf("read new index key for column %q: %w", t.cols[columnIndex], err)
		}
		if err := index.DeleteRID(oldKey, oldPage, oldSlot); err != nil {
			return fmt.Errorf("delete old index entry for column %q: %w", t.cols[columnIndex], err)
		}
		if err := index.InsertDuplicate(newKey, newPage, newSlot); err != nil {
			return fmt.Errorf("insert new index entry for column %q: %w", t.cols[columnIndex], err)
		}
	}
	return nil
}

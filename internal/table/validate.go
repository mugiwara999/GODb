package table

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/mugiwara999/goDB/internal/pager"
)

// ValidatePrimaryIndex checks that every primary-index entry points to a live
// row with the same ID and that every live row has the matching index entry.
// Deleted rows leave stale entries until index deletion is implemented, so
// those entries are reported as consistency errors.
func (t *Table) ValidatePrimaryIndex() error {
	if t == nil || t.Pager == nil {
		return fmt.Errorf("validate primary index: table is not open")
	}
	index := t.Indexes["id"]
	if index == nil {
		return fmt.Errorf("validate primary index for table %q: missing primary key index", t.Name)
	}
	if err := index.Validate(); err != nil {
		return fmt.Errorf("validate primary index for table %q: %w", t.Name, err)
	}

	type rowLocation struct {
		pageID int
		slotID int
	}
	entries := make(map[uint64]rowLocation)
	leaf, err := index.FindLeftMostLeaf()
	if err != nil {
		return fmt.Errorf("validate primary index for table %q: find leftmost leaf: %w", t.Name, err)
	}
	visitedLeaves := make(map[uint32]bool)
	for leaf != nil {
		if visitedLeaves[leaf.ID] {
			return fmt.Errorf("validate primary index for table %q: leaf chain cycles at page %d", t.Name, leaf.ID)
		}
		visitedLeaves[leaf.ID] = true
		for i := 0; i < leaf.GetNumKeys(); i++ {
			key := leaf.GetKey(i)
			rid := leaf.GetRID(i)
			location := rowLocation{pageID: int(rid.GetPageID()), slotID: int(rid.GetSlotID())}
			if previous, exists := entries[key]; exists {
				return fmt.Errorf("validate primary index for table %q: duplicate key %d at (%d,%d) and (%d,%d)", t.Name, key, previous.pageID, previous.slotID, location.pageID, location.slotID)
			}
			entries[key] = location

			page, err := t.Pager.GetPage(location.pageID)
			if err != nil {
				return fmt.Errorf("validate primary index for table %q: key %d points to page %d: %w", t.Name, key, location.pageID, err)
			}
			data, err := page.GetRow(location.slotID)
			if errors.Is(err, pager.ErrDeletedSlot) {
				return fmt.Errorf("validate primary index for table %q: key %d points to deleted row (%d,%d): %w", t.Name, key, location.pageID, location.slotID, err)
			}
			if err != nil {
				return fmt.Errorf("validate primary index for table %q: key %d points to invalid row (%d,%d): %w", t.Name, key, location.pageID, location.slotID, err)
			}
			row := DeserializeRow(data)
			if len(row) != len(t.cols) {
				return fmt.Errorf("validate primary index for table %q: key %d points to row with %d values, expected %d", t.Name, key, len(row), len(t.cols))
			}
			rowID, err := strconv.ParseUint(row[0], 10, 64)
			if err != nil || rowID != key {
				return fmt.Errorf("validate primary index for table %q: key %d points to row ID %q", t.Name, key, row[0])
			}
		}

		next := leaf.NextLeaf()
		if next == 0 {
			break
		}
		leaf, err = index.GetPage(next)
		if err != nil {
			return fmt.Errorf("validate primary index for table %q: read leaf %d: %w", t.Name, next, err)
		}
	}

	rowIt := t.Pager.RowIterator()
	for {
		data, err := rowIt.Next()
		if err != nil {
			return fmt.Errorf("validate primary index for table %q: scan rows: %w", t.Name, err)
		}
		if data == nil {
			break
		}
		row := DeserializeRow(data)
		if len(row) != len(t.cols) {
			return fmt.Errorf("validate primary index for table %q: live row has %d values, expected %d", t.Name, len(row), len(t.cols))
		}
		id, err := strconv.ParseUint(row[0], 10, 64)
		if err != nil {
			return fmt.Errorf("validate primary index for table %q: invalid live row ID %q: %w", t.Name, row[0], err)
		}
		location, exists := entries[id]
		if !exists {
			return fmt.Errorf("validate primary index for table %q: live row ID %d is missing from the index", t.Name, id)
		}
		info := rowIt.GetCurrentInfo()
		if location.pageID != info.PageID || location.slotID != info.SlotID {
			return fmt.Errorf("validate primary index for table %q: ID %d points to (%d,%d), live row is at (%d,%d)", t.Name, id, location.pageID, location.slotID, info.PageID, info.SlotID)
		}
	}
	return nil
}

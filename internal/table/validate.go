package table

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/mugiwara999/goDB/internal/pager"
)

// ValidatePrimaryIndex checks that every primary-index entry points to a live
// row with the same ID and that every live row has the matching index entry.
func (t *Table) ValidatePrimaryIndex() error {
	if t == nil || t.Pager == nil {
		return fmt.Errorf("validate primary index: table is not open")
	}
	t.mutationMu.Lock()
	defer t.mutationMu.Unlock()
	if t.closed {
		return fmt.Errorf("validate primary index for table %q: %w", t.Name, ErrTableClosed)
	}
	return t.validatePrimaryIndex()
}

func (t *Table) validatePrimaryIndex() error {
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
			key, err := leaf.GetKey(i)
			if err != nil {
				return fmt.Errorf("validate primary index for table %q: read key from leaf %d: %w", t.Name, leaf.ID, err)
			}
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
			row, err := t.decodeRow(data)
			if err != nil {
				return fmt.Errorf("validate primary index for table %q: key %d points to corrupt row: %w", t.Name, key, err)
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
		row, err := t.decodeRow(data)
		if err != nil {
			return fmt.Errorf("validate primary index for table %q: corrupt live row: %w", t.Name, err)
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

// ValidateIndexes checks each open secondary index against the live
// rows, including both missing and stale key/RID associations.
func (t *Table) ValidateIndexes() error {
	if t == nil || t.Pager == nil {
		return fmt.Errorf("validate indexes: table is not open")
	}
	t.mutationMu.Lock()
	defer t.mutationMu.Unlock()
	if t.closed {
		return fmt.Errorf("validate indexes for table %q: %w", t.Name, ErrTableClosed)
	}
	if err := t.validatePrimaryIndex(); err != nil {
		return err
	}
	type rowLocation struct {
		page uint32
		slot uint16
	}
	for columnIndex := 1; columnIndex < len(t.cols); columnIndex++ {
		column := t.cols[columnIndex]
		index := t.Indexes[column]
		if index == nil {
			continue
		}
		if err := index.Validate(); err != nil {
			return fmt.Errorf("validate secondary index %q for table %q: %w", column, t.Name, err)
		}
		expected := make(map[uint64]map[rowLocation]struct{})
		rowIt := t.Pager.RowIterator()
		for {
			data, err := rowIt.Next()
			if err != nil {
				return fmt.Errorf("validate secondary index %q: scan rows: %w", column, err)
			}
			if data == nil {
				break
			}
			row, err := t.decodeRow(data)
			if err != nil {
				return fmt.Errorf("validate secondary index %q: corrupt row: %w", column, err)
			}
			key, err := t.indexKey(t.colTypes[columnIndex], row[columnIndex])
			if err != nil {
				return fmt.Errorf("validate secondary index %q: invalid key %q: %w", column, row[columnIndex], err)
			}
			info := rowIt.GetCurrentInfo()
			location := rowLocation{page: uint32(info.PageID), slot: uint16(info.SlotID)}
			if expected[key] == nil {
				expected[key] = make(map[rowLocation]struct{})
			}
			expected[key][location] = struct{}{}
		}

		leaf, err := index.FindLeftMostLeaf()
		if err != nil {
			return fmt.Errorf("validate secondary index %q: find first leaf: %w", column, err)
		}
		visitedKeys := make(map[uint64]struct{})
		for leaf != nil {
			for i := 0; i < leaf.GetNumKeys(); i++ {
				key, err := leaf.GetKey(i)
				if err != nil {
					return fmt.Errorf("validate secondary index %q: read key from leaf %d: %w", column, leaf.ID, err)
				}
				wanted, exists := expected[key]
				if !exists {
					return fmt.Errorf("validate secondary index %q: unexpected key %d", column, key)
				}
				if _, exists := visitedKeys[key]; exists {
					return fmt.Errorf("validate secondary index %q: key %d appears more than once", column, key)
				}
				visitedKeys[key] = struct{}{}
				got, err := index.FindAll(key)
				if err != nil {
					return fmt.Errorf("validate secondary index %q key %d: %w", column, key, err)
				}
				if len(got) != len(wanted) {
					return fmt.Errorf("validate secondary index %q key %d has %d RIDs, expected %d", column, key, len(got), len(wanted))
				}
				for _, rid := range got {
					location := rowLocation{page: rid.GetPageID(), slot: rid.GetSlotID()}
					if _, exists := wanted[location]; !exists {
						return fmt.Errorf("validate secondary index %q key %d has stale RID (%d,%d)", column, key, location.page, location.slot)
					}
				}
			}
			next := leaf.NextLeaf()
			if next == 0 {
				break
			}
			leaf, err = index.GetPage(next)
			if err != nil {
				return fmt.Errorf("validate secondary index %q: read leaf %d: %w", column, next, err)
			}
		}
		if len(visitedKeys) != len(expected) {
			return fmt.Errorf("validate secondary index %q has %d keys, expected %d", column, len(visitedKeys), len(expected))
		}
	}
	return nil
}

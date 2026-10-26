package table

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/mugiwara999/goDB/internal/btree"
	"github.com/mugiwara999/goDB/internal/pager"
)

type ColEq struct {
	ColIdx int
	Value  string
}

type UpdateValue struct {
	ColIdx int
	Value  string
}

func SerializeRow(values []string) []byte {
	buf := make([]byte, 0)
	for _, v := range values {
		buf = append(buf, []byte(v)...)
		buf = append(buf, 0)
	}
	return buf
}

func DeserializeRow(data []byte) []string {
	values := make([]string, 0)
	accumulator := make([]byte, 0)
	for _, b := range data {
		if b == 0 {
			values = append(values, string(accumulator))
			accumulator = accumulator[:0]
			continue
		}
		accumulator = append(accumulator, b)
	}
	return values
}

func (t *Table) Insert(values []string) error {
	if len(values)+1 != len(t.cols) {
		return fmt.Errorf("insert into table %q: expected %d values, got %d", t.Name, len(t.cols)-1, len(values))
	}
	for _, value := range values {
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("insert into table %q: values cannot contain NUL", t.Name)
		}
	}
	if t.Indexes["id"] == nil {
		return fmt.Errorf("insert into table %q: missing primary key index", t.Name)
	}

	lastID, err := t.Pager.LastID()

	if err != nil {
		return fmt.Errorf("insert into table %q: %w", t.Name, err)
	}

	if lastID == math.MaxUint64 {
		return fmt.Errorf("insert into table %q: ID space exhausted", t.Name)
	}
	lastID++

	var lastPage *pager.Page

	values = append([]string{fmt.Sprintf("%d", lastID-1)}, values...)
	rowData := SerializeRow(values)
	if len(rowData) > pager.PAGE_SIZE-11 {
		return fmt.Errorf("insert into table %q: row requires %d bytes, maximum is %d", t.Name, len(rowData), pager.PAGE_SIZE-11)
	}
	// Reserve the ID before storing the row so a failed write cannot silently reuse it.
	if err := t.Pager.SetLastID(lastID); err != nil {
		return fmt.Errorf("insert into table %q: %w", t.Name, err)
	}
	if t.Pager.GetNumPages() <= 1 {
		lastPage, err = t.Pager.NewPage()
		if err != nil {
			return fmt.Errorf("insert into table %q: %w", t.Name, err)
		}
	} else {
		lastPage, err = t.Pager.GetPage(t.Pager.GetNumPages() - 1)
		if err != nil {
			return fmt.Errorf("insert into table %q: %w", t.Name, err)
		}

		if !lastPage.CanFit(len(rowData)) {
			lastPage, err = t.Pager.NewPage()
			if err != nil {
				return fmt.Errorf("insert into table %q: %w", t.Name, err)
			}
		}
	}

	if err := lastPage.AddRow(rowData); err != nil {
		return fmt.Errorf("insert into table %q: %w", t.Name, err)
	}

	if err := t.Pager.Flush(lastPage); err != nil {
		return fmt.Errorf("insert into table %q: %w", t.Name, err)
	}

	slotID, err := lastPage.GetNumSlots()

	slotID--

	if err != nil {
		return fmt.Errorf("insert into table %q: %w", t.Name, err)
	}

	err = t.Indexes["id"].Insert(lastID-1, uint32(lastPage.ID), uint16(slotID))

	if err != nil {
		indexErr := fmt.Errorf("insert row ID %d into primary index: %w", lastID-1, err)
		if rollbackErr := lastPage.DeleteRow(slotID); rollbackErr != nil {
			return errors.Join(indexErr, fmt.Errorf("mark unindexed row deleted: %w", rollbackErr))
		}
		if rollbackErr := t.Pager.Flush(lastPage); rollbackErr != nil {
			return errors.Join(indexErr, fmt.Errorf("persist unindexed row rollback: %w", rollbackErr))
		}
		return indexErr
	}

	// TODO: It requires values[] to be someStruct[] with types
	// for _, idx := range t.Indexes {
	// 	name := idx.Name()
	// 	if name == "id" {
	// 		continue
	// 	}
	//
	// 	colIdx := -1
	//
	// 	for i, v := range t.cols {
	// 		if name == v {
	// 			idx.Insert(uint64(values[i]), uint32(lastPage.ID), uint16(slotID))
	// 		}
	// 	}
	//
	// }
	return nil
}

func (t *Table) Select(columns []string, colEquals []ColEq) ([][]string, error) {
	result := make([][]string, 0)

	if len(columns) > 0 && columns[0] == "*" {
		columns = t.GetColumns()
	}

	if len(columns) == 0 {
		return nil, fmt.Errorf("select from table %q: no columns requested", t.Name)
	}

	colMap := make(map[string]int)
	for i, col := range t.cols {
		colMap[col] = i
	}

	colIdxs := make([]int, 0, len(columns))
	for _, col := range columns {
		idx, ok := colMap[col]
		if !ok {
			return nil, fmt.Errorf("select from table %q: column %q does not exist", t.Name, col)
		}
		colIdxs = append(colIdxs, idx)
	}

	result = append(result, columns)

	for _, v := range colEquals {
		if v.ColIdx < 0 || v.ColIdx >= len(t.cols) {
			return nil, fmt.Errorf("select from table %q: filter column index %d is out of range for table with %d columns", t.Name, v.ColIdx, len(t.cols))
		}

		if v.ColIdx == 0 {
			id, err := strconv.ParseUint(v.Value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("select from table %q: invalid id value %q: %w", t.Name, v.Value, err)
			}
			// since id is unique, we can use the index to find the row directly
			index := t.Indexes["id"]
			if index == nil {
				return nil, fmt.Errorf("select from table %q: missing primary key index", t.Name)
			}
			rid, err := index.Find(id)
			if errors.Is(err, btree.ErrKeyNotFound) {
				return result, nil
			}
			if err != nil {
				return nil, fmt.Errorf("select from table %q: %w", t.Name, err)
			}
			page, err := t.Pager.GetPage(int(rid.GetPageID()))
			if err != nil {
				return nil, fmt.Errorf("select from table %q: %w", t.Name, err)
			}
			rowData, err := page.GetRow(int(rid.GetSlotID()))
			if errors.Is(err, pager.ErrDeletedSlot) {
				return result, nil
			}
			if err != nil {
				return nil, fmt.Errorf("select from table %q: %w", t.Name, err)
			}
			row := DeserializeRow(rowData)
			match := true

			for _, v := range colEquals {
				if v.ColIdx < 0 || v.ColIdx >= len(row) {
					return nil, fmt.Errorf("select from table %q: filter column index %d is out of range for row with %d values", t.Name, v.ColIdx, len(row))
				}
				if row[v.ColIdx] != v.Value {
					match = false
					break
				}
			}

			if match {
				res := make([]string, 0, len(colIdxs))
				for _, idx := range colIdxs {
					res = append(res, row[idx])
				}
				result = append(result, res)
			}
			return result, nil

		}
	}

	rowIt := t.Pager.RowIterator()
	for {
		rowData, err := rowIt.Next()
		if err != nil {
			return nil, fmt.Errorf("select from table %q: %w", t.Name, err)
		}
		if rowData == nil {
			break
		}

		row := DeserializeRow(rowData)
		if len(row) < len(t.cols) {
			return nil, fmt.Errorf("select from table %q: corrupt row has %d values, expected %d", t.Name, len(row), len(t.cols))
		}

		match := true
		for _, v := range colEquals {
			if v.ColIdx < 0 || v.ColIdx >= len(row) {
				return nil, fmt.Errorf("select from table %q: filter column index %d is out of range for row with %d values", t.Name, v.ColIdx, len(row))
			}
			if row[v.ColIdx] != v.Value {
				match = false
				break
			}
		}

		if match {
			res := make([]string, 0, len(colIdxs))
			for _, idx := range colIdxs {
				res = append(res, row[idx])
			}
			result = append(result, res)
		}
	}

	return result, nil
}

func (t *Table) Delete(filters []ColEq) error {
	index := t.Indexes["id"]
	if index == nil {
		return fmt.Errorf("delete from table %q: missing primary key index", t.Name)
	}
	rowIt := t.Pager.RowIterator()

	for {
		rowData, err := rowIt.Next()
		if err != nil {
			return fmt.Errorf("delete from table %q: %w", t.Name, err)
		}
		if rowData == nil {
			break
		}

		row := DeserializeRow(rowData)
		if len(row) < len(t.cols) {
			return fmt.Errorf("delete from table %q: corrupt row has %d values, expected %d", t.Name, len(row), len(t.cols))
		}

		match := true
		for _, v := range filters {
			if v.ColIdx < 0 || v.ColIdx >= len(row) {
				return fmt.Errorf("delete from table %q: filter column index %d is out of range for row with %d values", t.Name, v.ColIdx, len(row))
			}
			if row[v.ColIdx] != v.Value {
				match = false
				break
			}
		}

		if match {
			id, err := strconv.ParseUint(row[0], 10, 64)
			if err != nil {
				return fmt.Errorf("delete from table %q: invalid row ID %q: %w", t.Name, row[0], err)
			}
			matchInfo := rowIt.GetCurrentInfo()
			page, err := t.Pager.GetPage(matchInfo.PageID)
			if err != nil {
				return fmt.Errorf("delete from table %q page %d slot %d: %w", t.Name, matchInfo.PageID, matchInfo.SlotID, err)
			}

			if err := page.DeleteRow(matchInfo.SlotID); err != nil {
				return fmt.Errorf("delete from table %q page %d slot %d: %w", t.Name, matchInfo.PageID, matchInfo.SlotID, err)
			}

			if err := t.Pager.Flush(page); err != nil {
				return fmt.Errorf("delete from table %q page %d slot %d: %w", t.Name, matchInfo.PageID, matchInfo.SlotID, err)
			}
			if err := index.Delete(id); err != nil {
				return fmt.Errorf("delete from table %q primary index key %d: %w", t.Name, id, err)
			}
		}
	}

	return nil
}

// TODO:
// 2. Mutation during iteration in Update
//
// When the new row has a different length, you delete the old row and call
// t.Insert(row). The insertion adds a new row at the end of the file (possibly
// on the current page if there’s room). The iterator’s pageID may still be
// that same page, and after the insertion the numSlots increases. On the next
// Next() call, the iterator sees the new slot and processes the same logical
// row again – leading to double updates.
// This is a classic problem: mutating a collection while iterating over it.
// For a learning DB, you could:
//
//	Collect all changes in a first pass and apply them in a second pass, or
//
//	Immediately mark the old row as deleted and insert the new one, but make
//	the iterator use a snapshot of page IDs / slot counts at the start.
//	Worth understanding and fixing.

func (t *Table) Update(filters []ColEq, toUpdate []UpdateValue) error {
	for _, filter := range filters {
		if filter.ColIdx < 0 || filter.ColIdx >= len(t.cols) {
			return fmt.Errorf("update table %q: filter column index %d is out of range", t.Name, filter.ColIdx)
		}
	}
	for _, update := range toUpdate {
		if update.ColIdx <= 0 || update.ColIdx >= len(t.cols) {
			return fmt.Errorf("update table %q: update column index %d is invalid", t.Name, update.ColIdx)
		}
		if strings.ContainsRune(update.Value, 0) {
			return fmt.Errorf("update table %q: values cannot contain NUL", t.Name)
		}
	}

	type change struct {
		info   pager.RowItInfo
		data   []byte
		id     uint64
		oldRID *btree.RID
	}
	var changes []change
	rowIt := t.Pager.RowIterator()

	for {
		rowData, err := rowIt.Next()
		if err != nil {
			return fmt.Errorf("update table %q: %w", t.Name, err)
		}
		if rowData == nil {
			break
		}

		row := DeserializeRow(rowData)
		if len(row) != len(t.cols) {
			return fmt.Errorf("update table %q: corrupt row has %d values, expected %d", t.Name, len(row), len(t.cols))
		}

		match := true
		for _, v := range filters {
			if v.ColIdx < 0 || v.ColIdx >= len(row) {
				return fmt.Errorf("update table %q: filter column index %d is out of range for row with %d values", t.Name, v.ColIdx, len(row))
			}
			if row[v.ColIdx] != v.Value {
				match = false
				break
			}
		}

		if !match {
			continue
		}

		updated := append([]string(nil), row...)
		for _, v := range toUpdate {
			updated[v.ColIdx] = v.Value
		}

		data := SerializeRow(updated)
		if len(data) > pager.PAGE_SIZE-11 {
			return fmt.Errorf("update table %q: %w", t.Name, pager.ErrPageFull)
		}
		id, err := strconv.ParseUint(row[0], 10, 64)
		if err != nil {
			return fmt.Errorf("update table %q: corrupt row ID %q: %w", t.Name, row[0], err)
		}
		changes = append(changes, change{info: rowIt.GetCurrentInfo(), data: data, id: id})
	}

	// Simulate replacements first so a later row that cannot be relocated does
	// not leave earlier matching rows partially updated.
	simulatedPages := make(map[int]*pager.Page)
	for i := range changes {
		c := &changes[i]
		page := simulatedPages[c.info.PageID]
		if page == nil {
			loaded, err := t.Pager.GetPage(c.info.PageID)
			if err != nil {
				return fmt.Errorf("update table %q page %d: %w", t.Name, c.info.PageID, err)
			}
			page = loaded
			simulatedPages[c.info.PageID] = page
		}
		if err := page.ReplaceRow(c.info.SlotID, c.data); err == nil {
			continue
		} else if !errors.Is(err, pager.ErrPageFull) {
			return fmt.Errorf("update table %q page %d slot %d: %w", t.Name, c.info.PageID, c.info.SlotID, err)
		}

		index := t.Indexes["id"]
		if index == nil {
			return fmt.Errorf("update table %q: missing primary key index", t.Name)
		}
		rid, err := index.Find(c.id)
		if err != nil {
			return fmt.Errorf("update table %q: find ID %d for relocation: %w", t.Name, c.id, err)
		}
		if int(rid.GetPageID()) != c.info.PageID || int(rid.GetSlotID()) != c.info.SlotID {
			return fmt.Errorf("update table %q: primary index location mismatch for ID %d", t.Name, c.id)
		}
		c.oldRID = rid
		if err := page.DeleteRow(c.info.SlotID); err != nil {
			return fmt.Errorf("update table %q page %d slot %d: %w", t.Name, c.info.PageID, c.info.SlotID, err)
		}
	}

	for _, c := range changes {
		page, err := t.Pager.GetPage(c.info.PageID)
		if err != nil {
			return fmt.Errorf("update table %q page %d: %w", t.Name, c.info.PageID, err)
		}
		if err := page.ReplaceRow(c.info.SlotID, c.data); err == nil {
			if err := t.Pager.Flush(page); err != nil {
				return fmt.Errorf("update table %q page %d: %w", t.Name, c.info.PageID, err)
			}
			continue
		} else if !errors.Is(err, pager.ErrPageFull) {
			return fmt.Errorf("update table %q page %d slot %d: %w", t.Name, c.info.PageID, c.info.SlotID, err)
		}

		index := t.Indexes["id"]
		if index == nil {
			return fmt.Errorf("update table %q: missing primary key index", t.Name)
		}
		if c.oldRID == nil {
			rid, err := index.Find(c.id)
			if err != nil {
				return fmt.Errorf("update table %q: find ID %d for relocation: %w", t.Name, c.id, err)
			}
			if int(rid.GetPageID()) != c.info.PageID || int(rid.GetSlotID()) != c.info.SlotID {
				return fmt.Errorf("update table %q: primary index location mismatch for ID %d", t.Name, c.id)
			}
		}

		destination, err := t.Pager.NewPage()
		if err != nil {
			return fmt.Errorf("update table %q: allocate relocation page: %w", t.Name, err)
		}
		if err := destination.AddRow(c.data); err != nil {
			return fmt.Errorf("update table %q: add relocated row: %w", t.Name, err)
		}
		if err := t.Pager.Flush(destination); err != nil {
			return fmt.Errorf("update table %q: flush relocated row: %w", t.Name, err)
		}
		if err := index.UpdateRID(c.id, uint32(destination.ID), 0); err != nil {
			rollbackErr := destination.DeleteRow(0)
			if rollbackErr == nil {
				rollbackErr = t.Pager.Flush(destination)
			}
			return errors.Join(fmt.Errorf("update table %q: update primary index for ID %d: %w", t.Name, c.id, err), rollbackErr)
		}
		if err := page.DeleteRow(c.info.SlotID); err != nil {
			return fmt.Errorf("update table %q: tombstone old row: %w", t.Name, err)
		}
		if err := t.Pager.Flush(page); err != nil {
			return fmt.Errorf("update table %q: flush old row tombstone: %w", t.Name, err)
		}
	}

	return nil
}

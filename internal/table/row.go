package table

import (
	"fmt"
	"github.com/mugiwara999/goDB/internal/pager"
	"strconv"
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

	lastID, err := t.Pager.LastID()

	if err != nil {
		return fmt.Errorf("insert into table %q: %w", t.Name, err)
	}

	lastID++
	t.Pager.SetLastID(lastID)

	var lastPage *pager.Page

	values = append([]string{fmt.Sprintf("%d", lastID-1)}, values...)

	if len(values) != len(t.cols) {
		return fmt.Errorf("insert into table %q: expected %d values, got %d", t.Name, len(t.cols), len(values))
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

		if !lastPage.CanFit(len(SerializeRow(values))) {
			lastPage, err = t.Pager.NewPage()
			if err != nil {
				return fmt.Errorf("insert into table %q: %w", t.Name, err)
			}
		}
	}

	rowData := SerializeRow(values)
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
		return err
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
			rid, err := t.Indexes["id"].Find(id)
			if err != nil {
				return nil, fmt.Errorf("select from table %q: %w", t.Name, err)
			}
			page, err := t.Pager.GetPage(int(rid.GetPageID()))
			if err != nil {
				return nil, fmt.Errorf("select from table %q: %w", t.Name, err)
			}
			rowData, err := page.GetRow(int(rid.GetSlotID()))
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
		if len(row) < len(t.cols) {
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

		for _, v := range toUpdate {
			if v.ColIdx < 0 || v.ColIdx >= len(row) {
				return fmt.Errorf("update table %q: update column index %d is out of range for row with %d values", t.Name, v.ColIdx, len(row))
			}

			if v.ColIdx == 0 {
				return fmt.Errorf("update table %q: cannot update primary key column 'id'", t.Name)
			}
			row[v.ColIdx] = v.Value
		}

		info := rowIt.GetCurrentInfo()
		newData := SerializeRow(row)

		page, err := t.Pager.GetPage(info.PageID)
		if err != nil {
			return fmt.Errorf("update table %q page %d slot %d: %w", t.Name, info.PageID, info.SlotID, err)
		}

		if len(newData) == len(rowData) {
			if err := page.Overwrite(info.SlotID, newData); err != nil {
				return fmt.Errorf("update table %q page %d slot %d: %w", t.Name, info.PageID, info.SlotID, err)
			}
			if err := t.Pager.Flush(page); err != nil {
				return fmt.Errorf("update table %q page %d slot %d: %w", t.Name, info.PageID, info.SlotID, err)
			}
			continue
		}

		if err := page.DeleteRow(info.SlotID); err != nil {
			return fmt.Errorf("update table %q page %d slot %d: %w", t.Name, info.PageID, info.SlotID, err)
		}
		if err := t.Pager.Flush(page); err != nil {
			return fmt.Errorf("update table %q page %d slot %d: %w", t.Name, info.PageID, info.SlotID, err)
		}

		if err := t.Insert(row); err != nil {
			return fmt.Errorf("update table %q page %d slot %d: %w", t.Name, info.PageID, info.SlotID, err)
		}
	}

	return nil
}

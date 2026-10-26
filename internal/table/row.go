package table

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/mugiwara999/goDB/internal/btree"
	"github.com/mugiwara999/goDB/internal/filelock"
	"github.com/mugiwara999/goDB/internal/pager"
	"github.com/mugiwara999/goDB/internal/wal"
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

func decodeTableRow(data []byte, expectedColumns int) ([]string, error) {
	if len(data) == 0 || data[len(data)-1] != 0 {
		return nil, fmt.Errorf("row payload is missing its final field terminator")
	}
	values := DeserializeRow(data)
	if len(values) != expectedColumns {
		return nil, fmt.Errorf("row has %d values, expected %d", len(values), expectedColumns)
	}
	return values, nil
}

func (t *Table) decodeRow(data []byte) ([]string, error) {
	values, err := decodeTableRow(data, len(t.cols))
	if err != nil {
		return nil, err
	}
	if len(t.colTypes) != len(values) {
		return nil, fmt.Errorf("table schema has %d types for %d columns", len(t.colTypes), len(values))
	}
	for i, value := range values {
		canonical, err := t.normalizeColumnValue(i, value)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", t.cols[i], err)
		}
		if canonical != value {
			return nil, fmt.Errorf("column %q has noncanonical %s value %q", t.cols[i], typeName(t.colTypes[i]), value)
		}
	}
	return values, nil
}

func (t *Table) normalizeColumnValue(colIdx int, value string) (string, error) {
	if colIdx < 0 || colIdx >= len(t.colTypes) {
		return "", fmt.Errorf("column index %d is out of range", colIdx)
	}
	if strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("values cannot contain NUL")
	}
	switch t.colTypes[colIdx] {
	case pager.ColumnTypeText:
		return value, nil
	case pager.ColumnTypeUint64:
		number, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return "", fmt.Errorf("invalid UINT64 value %q: %w", value, err)
		}
		return strconv.FormatUint(number, 10), nil
	default:
		return "", fmt.Errorf("unsupported stored type %d", t.colTypes[colIdx])
	}
}

func (t *Table) normalizeFilters(filters []ColEq, operation string) ([]ColEq, error) {
	normalized := make([]ColEq, len(filters))
	for i, filter := range filters {
		if filter.ColIdx < 0 || filter.ColIdx >= len(t.cols) {
			return nil, fmt.Errorf("%s table %q: filter column index %d is out of range", operation, t.Name, filter.ColIdx)
		}
		value, err := t.normalizeColumnValue(filter.ColIdx, filter.Value)
		if err != nil {
			return nil, fmt.Errorf("%s table %q: invalid filter for column %q: %w", operation, t.Name, t.cols[filter.ColIdx], err)
		}
		normalized[i] = ColEq{ColIdx: filter.ColIdx, Value: value}
	}
	return normalized, nil
}

func typeName(columnType ColumnType) string {
	switch columnType {
	case pager.ColumnTypeText:
		return "TEXT"
	case pager.ColumnTypeUint64:
		return "UINT64"
	default:
		return fmt.Sprintf("type(%d)", columnType)
	}
}

func (t *Table) Insert(values []string) error {
	return t.mutate(func() error { return t.insert(values) })
}

func (t *Table) insert(values []string) error {
	if len(values)+1 != len(t.cols) {
		return fmt.Errorf("insert into table %q: expected %d values, got %d", t.Name, len(t.cols)-1, len(values))
	}
	values = append([]string(nil), values...)
	for i, value := range values {
		canonical, err := t.normalizeColumnValue(i+1, value)
		if err != nil {
			return fmt.Errorf("insert into table %q column %q: %w", t.Name, t.cols[i+1], err)
		}
		values[i] = canonical
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
	for columnIndex := 1; columnIndex < len(t.cols); columnIndex++ {
		index := t.Indexes[t.cols[columnIndex]]
		if index == nil {
			continue
		}
		key, err := t.indexKey(t.colTypes[columnIndex], values[columnIndex])
		if err != nil {
			return fmt.Errorf("insert into table %q: invalid indexed value for column %q: %w", t.Name, t.cols[columnIndex], err)
		}
		if err := index.InsertDuplicate(key, uint32(lastPage.ID), uint16(slotID)); err != nil {
			return fmt.Errorf("insert into secondary index %q: %w", t.cols[columnIndex], err)
		}
	}

	return nil
}

func (t *Table) Select(columns []string, colEquals []ColEq) ([][]string, error) {
	if t == nil || t.Pager == nil {
		return nil, fmt.Errorf("select: table is not open")
	}
	t.mutationMu.Lock()
	defer t.mutationMu.Unlock()
	if t.closed {
		return nil, fmt.Errorf("select from table %q: %w", t.Name, ErrTableClosed)
	}
	if t.poisoned {
		return nil, fmt.Errorf("select from table %q: table must be reopened after a failed rollback", t.Name)
	}
	lock, err := filelock.Acquire(t.Path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock table %q for select: %w", t.Name, err)
	}
	defer lock.Close()
	if err := wal.Recover(t.Path + ".wal"); err != nil {
		return nil, fmt.Errorf("recover table %q before select: %w", t.Name, err)
	}
	if err := t.Pager.Refresh(); err != nil {
		return nil, fmt.Errorf("refresh table %q before select: %w", t.Name, err)
	}
	if err := t.refreshIndexesForRead(); err != nil {
		return nil, fmt.Errorf("refresh table %q indexes before select: %w", t.Name, err)
	}
	return t.selectRows(columns, colEquals)
}

func (t *Table) selectRows(columns []string, colEquals []ColEq) ([][]string, error) {
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
	colEquals, err := t.normalizeFilters(colEquals, "select from")
	if err != nil {
		return nil, err
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
			row, err := t.decodeRow(rowData)
			if err != nil {
				return nil, fmt.Errorf("select from table %q: corrupt row: %w", t.Name, err)
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
			return result, nil

		}
	}

	type indexCandidate struct {
		filter ColEq
		index  *btree.Index
		key    uint64
		count  uint64
	}
	candidates := make([]indexCandidate, 0, len(colEquals))
	for _, filter := range colEquals {
		if filter.ColIdx == 0 {
			continue
		}
		index := t.Indexes[t.cols[filter.ColIdx]]
		if index == nil {
			continue
		}
		key, err := t.indexKey(t.colTypes[filter.ColIdx], filter.Value)
		if err != nil {
			return nil, fmt.Errorf("select from table %q: invalid indexed value for column %q: %w", t.Name, t.cols[filter.ColIdx], err)
		}
		candidates = append(candidates, indexCandidate{filter: filter, index: index, key: key})
	}
	if len(candidates) > 0 {
		best := 0
		var rids []btree.RID
		if len(candidates) == 1 {
			rids, err = candidates[0].index.FindAll(candidates[0].key)
			if err != nil {
				if errors.Is(err, btree.ErrKeyNotFound) {
					return result, nil
				}
				return nil, fmt.Errorf("select from table %q index %q: %w", t.Name, t.cols[candidates[0].filter.ColIdx], err)
			}
		} else {
			for i := range candidates {
				candidates[i].count, err = candidates[i].index.CountAll(candidates[i].key)
				if err != nil {
					if errors.Is(err, btree.ErrKeyNotFound) {
						return result, nil
					}
					return nil, fmt.Errorf("select from table %q index %q: %w", t.Name, t.cols[candidates[i].filter.ColIdx], err)
				}
				if candidates[i].count < candidates[best].count {
					best = i
				}
			}
			rids, err = candidates[best].index.FindAll(candidates[best].key)
			if err != nil {
				return nil, fmt.Errorf("select from table %q index %q: %w", t.Name, t.cols[candidates[best].filter.ColIdx], err)
			}
		}
		filter := candidates[best].filter
		pages := make(map[uint32]*pager.Page)
		for _, rid := range rids {
			pageID := rid.GetPageID()
			page := pages[pageID]
			if page == nil {
				page, err = t.Pager.GetPage(int(pageID))
				if err != nil {
					return nil, fmt.Errorf("select from table %q index %q points to invalid page %d: %w", t.Name, t.cols[filter.ColIdx], pageID, err)
				}
				pages[pageID] = page
			}
			rowData, err := page.GetRow(int(rid.GetSlotID()))
			if err != nil {
				return nil, fmt.Errorf("select from table %q index %q points to invalid row (%d,%d): %w", t.Name, t.cols[filter.ColIdx], rid.GetPageID(), rid.GetSlotID(), err)
			}
			row, err := t.decodeRow(rowData)
			if err != nil {
				return nil, fmt.Errorf("select from table %q: corrupt indexed row: %w", t.Name, err)
			}
			if row[filter.ColIdx] != filter.Value {
				if t.colTypes[filter.ColIdx] != ColumnText {
					return nil, fmt.Errorf("select from table %q: stale index %q points to row with value %q", t.Name, t.cols[filter.ColIdx], row[filter.ColIdx])
				}
				rowKey, err := t.indexKey(t.colTypes[filter.ColIdx], row[filter.ColIdx])
				if err != nil || rowKey != candidates[best].key {
					return nil, fmt.Errorf("select from table %q: stale index %q points to row with value %q", t.Name, t.cols[filter.ColIdx], row[filter.ColIdx])
				}
			}
			match := true
			for _, other := range colEquals {
				if row[other.ColIdx] != other.Value {
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

	rowIt := t.Pager.RowIterator()
	for {
		rowData, err := rowIt.Next()
		if err != nil {
			return nil, fmt.Errorf("select from table %q: %w", t.Name, err)
		}
		if rowData == nil {
			break
		}

		row, err := t.decodeRow(rowData)
		if err != nil {
			return nil, fmt.Errorf("select from table %q: corrupt row: %w", t.Name, err)
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
	return t.mutate(func() error { return t.delete(filters) })
}

func (t *Table) delete(filters []ColEq) error {
	filters, err := t.normalizeFilters(filters, "delete from")
	if err != nil {
		return err
	}
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

		row, err := t.decodeRow(rowData)
		if err != nil {
			return fmt.Errorf("delete from table %q: corrupt row: %w", t.Name, err)
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
			for columnIndex := 1; columnIndex < len(t.cols); columnIndex++ {
				secondary := t.Indexes[t.cols[columnIndex]]
				if secondary == nil {
					continue
				}
				key, err := t.indexKey(t.colTypes[columnIndex], row[columnIndex])
				if err != nil {
					return fmt.Errorf("delete from table %q: invalid indexed value for column %q: %w", t.Name, t.cols[columnIndex], err)
				}
				if err := secondary.DeleteRID(key, uint32(matchInfo.PageID), uint16(matchInfo.SlotID)); err != nil {
					return fmt.Errorf("delete from secondary index %q: %w", t.cols[columnIndex], err)
				}
			}
		}
	}

	return nil
}

func (t *Table) Update(filters []ColEq, toUpdate []UpdateValue) error {
	return t.mutate(func() error { return t.update(filters, toUpdate) })
}

func (t *Table) update(filters []ColEq, toUpdate []UpdateValue) error {
	filters, err := t.normalizeFilters(filters, "update")
	if err != nil {
		return err
	}
	toUpdate = append([]UpdateValue(nil), toUpdate...)
	for i, update := range toUpdate {
		if update.ColIdx <= 0 || update.ColIdx >= len(t.cols) {
			return fmt.Errorf("update table %q: update column index %d is invalid", t.Name, update.ColIdx)
		}
		canonical, err := t.normalizeColumnValue(update.ColIdx, update.Value)
		if err != nil {
			return fmt.Errorf("update table %q column %q: %w", t.Name, t.cols[update.ColIdx], err)
		}
		toUpdate[i].Value = canonical
	}

	type change struct {
		info   pager.RowItInfo
		data   []byte
		id     uint64
		oldRow []string
		newRow []string
		oldRID *btree.RID
	}
	var changes []change
	// Collect matches first so relocated rows cannot extend the active scan.
	rowIt := t.Pager.RowIterator()

	for {
		rowData, err := rowIt.Next()
		if err != nil {
			return fmt.Errorf("update table %q: %w", t.Name, err)
		}
		if rowData == nil {
			break
		}

		row, err := t.decodeRow(rowData)
		if err != nil {
			return fmt.Errorf("update table %q: corrupt row: %w", t.Name, err)
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
		changes = append(changes, change{
			info: rowIt.GetCurrentInfo(), data: data, id: id,
			oldRow: row, newRow: updated,
		})
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
			if err := t.maintainSecondaryIndexes(c.oldRow, c.newRow, uint32(c.info.PageID), uint16(c.info.SlotID), uint32(c.info.PageID), uint16(c.info.SlotID)); err != nil {
				return fmt.Errorf("update table %q secondary indexes: %w", t.Name, err)
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
		if err := t.maintainSecondaryIndexes(c.oldRow, c.newRow, uint32(c.info.PageID), uint16(c.info.SlotID), uint32(destination.ID), 0); err != nil {
			return fmt.Errorf("update table %q secondary indexes: %w", t.Name, err)
		}
	}

	return nil
}

package table

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
)

// secondaryIndexKey maps supported table values into the uint64 key space
// used by the current B+ tree. TEXT values use a stable SHA-256 bucket key;
// indexed SELECT always compares the original value before returning a row,
// so hash collisions can add work but cannot change query results.
func secondaryIndexKey(columnType ColumnType, value string) (uint64, error) {
	switch columnType {
	case ColumnUint64:
		key, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid UINT64 index value %q: %w", value, err)
		}
		return key, nil
	case ColumnText:
		digest := sha256.Sum256([]byte(value))
		return binary.BigEndian.Uint64(digest[:8]), nil
	default:
		return 0, fmt.Errorf("unsupported secondary index column type %d", columnType)
	}
}

func (t *Table) indexKey(columnType ColumnType, value string) (uint64, error) {
	if columnType == ColumnText && t.textKey != nil {
		return t.textKey(value), nil
	}
	return secondaryIndexKey(columnType, value)
}

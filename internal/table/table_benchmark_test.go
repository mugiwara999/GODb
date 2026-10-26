package table_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mugiwara999/goDB/internal/table"
)

func benchmarkTable(b *testing.B, name string) *table.Table {
	b.Helper()
	dataDir := filepath.Join(b.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		b.Fatal(err)
	}
	b.Setenv("DATA_DIR", dataDir)
	tbl, err := table.Create(name, []string{"value"})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := tbl.Close(); err != nil {
			b.Errorf("close benchmark table: %v", err)
		}
	})
	return tbl
}

func BenchmarkTableInsert(b *testing.B) {
	tbl := benchmarkTable(b, "insert")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tbl.Insert([]string{strconv.Itoa(i)}); err != nil {
			b.Fatalf("Insert(%d): %v", i, err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(tbl.Pager.GetNumPages()), "pages")
}

func BenchmarkTableSelectByID(b *testing.B) {
	tbl := benchmarkTable(b, "select")
	const rows = 100
	for i := 0; i < rows; i++ {
		if err := tbl.Insert([]string{fmt.Sprintf("value-%d", i)}); err != nil {
			b.Fatalf("seed Insert(%d): %v", i, err)
		}
	}
	filter := []table.ColEq{{ColIdx: 0, Value: strconv.Itoa(rows / 2)}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := tbl.Select([]string{"value"}, filter)
		if err != nil {
			b.Fatalf("Select: %v", err)
		}
		if len(result) != 2 {
			b.Fatalf("Select returned %d rows, want header and one result", len(result))
		}
	}
}

func benchmarkUint64Table(b *testing.B, name string, rowCount, distinctValues int, indexed bool) *table.Table {
	b.Helper()
	dataDir := filepath.Join(b.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		b.Fatal(err)
	}
	b.Setenv("DATA_DIR", dataDir)
	tbl, err := table.CreateWithTypes(name, []table.Column{{Name: "key", Type: table.ColumnUint64}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := tbl.Close(); err != nil {
			b.Errorf("close benchmark table: %v", err)
		}
	})
	for i := 0; i < rowCount; i++ {
		if err := tbl.Insert([]string{strconv.Itoa(i % distinctValues)}); err != nil {
			b.Fatalf("seed Insert(%d): %v", i, err)
		}
	}
	if indexed {
		if err := tbl.CreateIndex("key"); err != nil {
			b.Fatalf("CreateIndex: %v", err)
		}
	}
	return tbl
}

func BenchmarkTableSelectSecondaryIndex(b *testing.B) {
	for _, tc := range []struct {
		name           string
		rows, distinct int
	}{
		{name: "low_selectivity", rows: 2048, distinct: 2048},
		{name: "high_selectivity_posting_chain", rows: 2048, distinct: 4},
	} {
		for _, indexed := range []bool{false, true} {
			variant := "scan"
			if indexed {
				variant = "secondary_index"
			}
			b.Run(tc.name+"/"+variant, func(b *testing.B) {
				tbl := benchmarkUint64Table(b, "select_bench", tc.rows, tc.distinct, indexed)
				filter := []table.ColEq{{ColIdx: 1, Value: "0"}}
				wantRows := tc.rows / tc.distinct
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					rows, err := tbl.Select([]string{"id"}, filter)
					if err != nil {
						b.Fatalf("Select: %v", err)
					}
					if len(rows) != wantRows+1 {
						b.Fatalf("Select returned %d rows, want header and %d matches", len(rows), wantRows)
					}
				}
			})
		}
	}
}

func BenchmarkTableSelectParallelReaders(b *testing.B) {
	for _, indexed := range []bool{false, true} {
		variant := "scan"
		if indexed {
			variant = "secondary_index"
		}
		b.Run(variant, func(b *testing.B) {
			tbl := benchmarkUint64Table(b, "parallel_select", 2048, 1024, indexed)
			filter := []table.ColEq{{ColIdx: 1, Value: "42"}}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					rows, err := tbl.Select([]string{"id"}, filter)
					if err != nil {
						b.Errorf("Select: %v", err)
						return
					}
					if len(rows) != 3 {
						b.Errorf("Select returned %d rows, want header and two matches", len(rows))
						return
					}
				}
			})
		})
	}
}

func BenchmarkTableSelectMultipleIndexes(b *testing.B) {
	dataDir := filepath.Join(b.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		b.Fatal(err)
	}
	b.Setenv("DATA_DIR", dataDir)
	tbl, err := table.CreateWithTypes("multi_index_bench", []table.Column{
		{Name: "account", Type: table.ColumnUint64},
		{Name: "code", Type: table.ColumnUint64},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = tbl.Close() })
	const rows = 2048
	for i := 0; i < rows; i++ {
		if err := tbl.Insert([]string{"1", strconv.Itoa(i)}); err != nil {
			b.Fatalf("seed Insert(%d): %v", i, err)
		}
	}
	for _, column := range []string{"account", "code"} {
		if err := tbl.CreateIndex(column); err != nil {
			b.Fatalf("CreateIndex(%s): %v", column, err)
		}
	}
	for _, tc := range []struct {
		name    string
		filters []table.ColEq
	}{
		{name: "broad_first", filters: []table.ColEq{{ColIdx: 1, Value: "1"}, {ColIdx: 2, Value: "1024"}}},
		{name: "selective_first", filters: []table.ColEq{{ColIdx: 2, Value: "1024"}, {ColIdx: 1, Value: "1"}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := tbl.Select([]string{"id"}, tc.filters)
				if err != nil {
					b.Fatalf("Select: %v", err)
				}
				if len(result) != 2 {
					b.Fatalf("Select returned %d rows, want header and one match", len(result))
				}
			}
		})
	}
}

func BenchmarkTableRangeUpdate(b *testing.B) {
	for _, indexed := range []bool{false, true} {
		variant := "scan"
		if indexed {
			variant = "secondary_index"
		}
		b.Run(variant, func(b *testing.B) {
			tbl := benchmarkRangeUpdateTable(b, indexed)
			selectedRange := table.Uint64Range{Lower: uint64Pointer(1024), LowerInclusive: true, Upper: uint64Pointer(1027), UpperInclusive: true}
			updates := []table.UpdateValue{{ColIdx: 2, Value: "changed"}}
			// The key is deliberately left unchanged, so repeated iterations use
			// the same four matching rows and isolate range-selection work.
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := tbl.UpdateRange(selectedRange, nil, updates); err != nil {
					b.Fatalf("UpdateRange: %v", err)
				}
			}
		})
	}
}

func uint64Pointer(value uint64) *uint64 { return &value }

func benchmarkRangeUpdateTable(b *testing.B, indexed bool) *table.Table {
	b.Helper()
	dataDir := filepath.Join(b.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		b.Fatal(err)
	}
	b.Setenv("DATA_DIR", dataDir)
	tbl, err := table.CreateWithTypes("range_update", []table.Column{
		{Name: "key", Type: table.ColumnUint64},
		{Name: "payload", Type: table.ColumnText},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := tbl.Close(); err != nil {
			b.Errorf("close benchmark table: %v", err)
		}
	})
	for i := 0; i < 2048; i++ {
		if err := tbl.Insert([]string{strconv.Itoa(i), "original"}); err != nil {
			b.Fatalf("seed Insert(%d): %v", i, err)
		}
	}
	if indexed {
		if err := tbl.CreateIndex("key"); err != nil {
			b.Fatalf("CreateIndex: %v", err)
		}
	}
	return tbl
}

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

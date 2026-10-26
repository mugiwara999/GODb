package table

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func createTextIndexTestTable(t *testing.T, name string) *Table {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	tbl, err := CreateWithTypes(name, []Column{{Name: "value", Type: ColumnText}})
	if err != nil {
		t.Fatalf("CreateWithTypes: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })
	return tbl
}

func TestTextSecondaryIndexExactEqualityAndLifecycle(t *testing.T) {
	tbl := createTextIndexTestTable(t, "text_index")
	long := strings.Repeat("λ", 900)
	values := []string{"", "repeat", "repeat", "café", "東京", long}
	for _, value := range values {
		if err := tbl.Insert([]string{value}); err != nil {
			t.Fatalf("Insert(%q): %v", value, err)
		}
	}
	if err := tbl.CreateIndex("value"); err != nil {
		t.Fatalf("CreateIndex(TEXT): %v", err)
	}
	for _, tc := range []struct {
		value string
		count int
	}{{"", 1}, {"repeat", 2}, {"café", 1}, {"東京", 1}, {long, 1}, {"missing", 0}} {
		rows, err := tbl.Select([]string{"value"}, []ColEq{{ColIdx: 1, Value: tc.value}})
		if err != nil {
			t.Fatalf("Select(%q): %v", tc.value, err)
		}
		if len(rows) != tc.count+1 {
			t.Fatalf("Select(%q) returned %d rows, want header and %d matches", tc.value, len(rows), tc.count)
		}
		for _, row := range rows[1:] {
			if row[0] != tc.value {
				t.Fatalf("Select(%q) returned non-equal value %q", tc.value, row[0])
			}
		}
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes: %v", err)
	}
	if err := tbl.Update([]ColEq{{ColIdx: 1, Value: "café"}}, []UpdateValue{{ColIdx: 1, Value: "Cafe ☕"}}); err != nil {
		t.Fatalf("Update indexed text: %v", err)
	}
	if err := tbl.Delete([]ColEq{{ColIdx: 1, Value: "東京"}}); err != nil {
		t.Fatalf("Delete indexed text: %v", err)
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after mutations: %v", err)
	}
	if err := tbl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open("text_index")
	if err != nil {
		t.Fatalf("Open v4 text index: %v", err)
	}
	defer reopened.Close()
	rows, err := reopened.Select([]string{"value"}, []ColEq{{ColIdx: 1, Value: "Cafe ☕"}})
	if err != nil || len(rows) != 2 || rows[1][0] != "Cafe ☕" {
		t.Fatalf("Select after reopen = %q, %v; want Cafe ☕", rows, err)
	}
	if err := reopened.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes after reopen: %v", err)
	}
	if err := reopened.DropIndex("value"); err != nil {
		t.Fatalf("DropIndex(TEXT): %v", err)
	}
	rows, err = reopened.Select([]string{"value"}, []ColEq{{ColIdx: 1, Value: long}})
	if err != nil || len(rows) != 2 || rows[1][0] != long {
		t.Fatalf("scan after dropping TEXT index = %q, %v", rows, err)
	}
}

func TestTextIndexFiltersHashBucketCollisions(t *testing.T) {
	tbl := createTextIndexTestTable(t, "text_collision")
	// Force a deterministic collision so the test exercises exact-value
	// filtering without needing to find a cryptographic hash collision.
	tbl.textKey = func(string) uint64 { return 7 }
	for _, value := range []string{"alpha", "beta"} {
		if err := tbl.Insert([]string{value}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tbl.CreateIndex("value"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"alpha", "beta"} {
		rows, err := tbl.Select([]string{"value"}, []ColEq{{ColIdx: 1, Value: value}})
		if err != nil {
			t.Fatalf("Select(%q) through colliding hash bucket: %v", value, err)
		}
		if len(rows) != 2 || rows[1][0] != value {
			t.Fatalf("Select(%q) through colliding hash bucket = %q; want only exact match", value, rows)
		}
	}
	if err := tbl.ValidateIndexes(); err != nil {
		t.Fatalf("ValidateIndexes for colliding bucket: %v", err)
	}
}

func TestTextIndexCreationUpgradesLegacyUntypedTable(t *testing.T) {
	tdata := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(tdata, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", tdata)
	tbl, err := Create("legacy_text_index", []string{"name"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tbl.Close() })
	if err := tbl.Insert([]string{"Ada"}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.CreateIndex("name"); err != nil {
		t.Fatalf("CreateIndex on legacy table: %v", err)
	}
	if err := tbl.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open("legacy_text_index")
	if err != nil {
		t.Fatalf("Open upgraded table: %v", err)
	}
	defer reopened.Close()
	rows, err := reopened.Select([]string{"name"}, []ColEq{{ColIdx: 1, Value: "Ada"}})
	if err != nil || len(rows) != 2 || rows[1][0] != "Ada" {
		t.Fatalf("Select after legacy upgrade = %q, %v", rows, err)
	}
}

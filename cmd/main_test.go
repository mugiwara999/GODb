package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mugiwara999/goDB/internal/table"
)

func TestCommandLoopExecutesAndPersistsSQLSubset(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)

	input := strings.Join([]string{
		"CREATE TABLE users (name, age)",
		"INSERT INTO users VALUES 'Ada', 36",
		"SELECT id, name FROM users WHERE id=0",
		"UPDATE users SET name='Augusta' WHERE id=0",
		"SELECT name FROM users WHERE id=0",
		"DELETE FROM users WHERE id=0",
		"SELECT * FROM users",
		"SELECT missing FROM users",
		"INSERT INTO users VALUES trailing,",
	}, "\n")
	var output bytes.Buffer
	run(strings.NewReader(input), &output)
	got := output.String()
	for _, expected := range []string{
		"DB is starting",
		"id,name\n0,Ada\n",
		"name\nAugusta\n",
		"id,name,age\n",
		"column \"missing\" does not exist",
		"invalid syntax",
	} {
		if !strings.Contains(got, expected) {
			t.Errorf("command output %q does not contain %q", got, expected)
		}
	}

	tbl, err := table.Open("users")
	if err != nil {
		t.Fatalf("Open after CLI commands: %v", err)
	}
	defer tbl.Close()
	rows, err := tbl.Select([]string{"*"}, nil)
	if err != nil {
		t.Fatalf("Select after CLI commands: %v", err)
	}
	if len(rows) != 1 || strings.Join(rows[0], ",") != "id,name,age" {
		t.Fatalf("persisted rows = %q, want header only after delete", rows)
	}
	if err := tbl.ValidatePrimaryIndex(); err != nil {
		t.Fatalf("ValidatePrimaryIndex after CLI commands: %v", err)
	}
}

func TestCommandLoopCreatesTypedColumnsAndValidatesValues(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	input := strings.Join([]string{
		"CREATE TABLE people (age UINT64, name TEXT)",
		"INSERT INTO people VALUES 00042, 'Ada'",
		"INSERT INTO people VALUES 42, 'Grace'",
		"CREATE INDEX ON people(age)",
		"SELECT age FROM people WHERE age=42",
		"INSERT INTO people VALUES invalid, 'Grace'",
		"UPDATE people SET age=43 WHERE name='Ada'",
		"DROP INDEX ON people(age)",
		"SELECT age FROM people WHERE age=00043",
	}, "\n")
	var output bytes.Buffer
	run(strings.NewReader(input), &output)
	got := output.String()
	for _, expected := range []string{
		"age\n42\n",
		"invalid UINT64 value \"invalid\"",
		"age\n43\n",
	} {
		if !strings.Contains(got, expected) {
			t.Errorf("command output %q does not contain %q", got, expected)
		}
	}

	tbl, err := table.Open("people")
	if err != nil {
		t.Fatalf("Open typed table: %v", err)
	}
	defer tbl.Close()
	if got, want := tbl.GetColumnTypes(), []table.ColumnType{table.ColumnUint64, table.ColumnUint64, table.ColumnText}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GetColumnTypes() = %v, want %v", got, want)
	}
	if tbl.Indexes["age"] != nil {
		t.Fatal("dropped age index was reopened")
	}
	rows, err := tbl.Select([]string{"age", "name"}, nil)
	if err != nil {
		t.Fatalf("Select persisted rows: %v", err)
	}
	if len(rows) != 3 || strings.Join(rows[1], ",") != "43,Ada" || strings.Join(rows[2], ",") != "42,Grace" {
		t.Fatalf("persisted rows = %q, want Ada and Grace", rows)
	}
}

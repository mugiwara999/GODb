package parser

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseSupportedStatements(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Query
	}{
		{"create", "CREATE TABLE users (name, age)", Query{Type: "create", Table: "users", Columns: []string{"name", "age"}}},
		{"typed create", "CREATE TABLE users (name TEXT, age UINT64)", Query{Type: "create", Table: "users", Columns: []string{"name", "age"}, ColumnTypes: []string{"text", "uint64"}}},
		{"insert", "INSERT INTO users VALUES 'Ada Lovelace', 36", Query{Type: "insert", Table: "users", Values: []string{"Ada Lovelace", "36"}}},
		{"select", "SELECT id, name FROM users WHERE id=7 AND name='Ada'", Query{Type: "select", Table: "users", Columns: []string{"id", "name"}, Filters: []Pair{{"id", "7"}, {"name", "Ada"}}}},
		{"delete all", "DELETE FROM users", Query{Type: "delete", Table: "users"}},
		{"update", "UPDATE users SET name='Ada', age=37 WHERE id=7", Query{Type: "update", Table: "users", Updates: []Pair{{"name", "Ada"}, {"age", "37"}}, Filters: []Pair{{"id", "7"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Parse(test.input)
			if err != nil {
				t.Fatalf("Parse(%q): %v", test.input, err)
			}
			if !reflect.DeepEqual(*got, test.want) {
				t.Fatalf("Parse(%q) = %#v, want %#v", test.input, *got, test.want)
			}
		})
	}
}

func TestParseRejectsMalformedSeparatorsAndTrailingTokens(t *testing.T) {
	inputs := []string{
		"CREATE TABLE users (name,,age)",
		"CREATE TABLE users (name,)",
		"CREATE TABLE users (name) ignored",
		"CREATE TABLE users (name TEXT, age)",
		"CREATE TABLE users (name DECIMAL)",
		"INSERT INTO users VALUES Ada 36",
		"INSERT INTO users VALUES Ada,",
		"SELECT id name FROM users",
		"SELECT id FROM users WHERE id=7 AND",
		"SELECT id FROM users WHERE AND id=7",
		"DELETE FROM users WHERE id=7 AND AND name=Ada",
		"UPDATE users SET name=Ada, WHERE id=7",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			if _, err := Parse(input); !errors.Is(err, ErrInvalidSyntax) {
				t.Fatalf("Parse(%q) error = %v, want ErrInvalidSyntax", input, err)
			}
		})
	}
}

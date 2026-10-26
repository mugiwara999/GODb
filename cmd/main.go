package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/mugiwara999/goDB/internal/parser"
	"github.com/mugiwara999/goDB/internal/table"
)

func main() {
	run(os.Stdin, os.Stdout)
}

func run(input io.Reader, output io.Writer) {
	scanner := bufio.NewScanner(input)
	fmt.Fprintln(output, "DB is starting")
	fmt.Fprint(output, ">")

	for scanner.Scan() {
		command := strings.TrimSpace(scanner.Text())
		if command == "" {
			fmt.Fprint(output, ">")
			continue
		}

		query, err := parser.Parse(command)
		if err != nil {
			fmt.Fprintln(output, err)
			fmt.Fprint(output, ">")
			continue
		}

		if query == nil {
			fmt.Fprint(output, ">")
			continue
		}

		switch query.Type {
		case "create":
			var tbl *table.Table
			if len(query.ColumnTypes) == 0 {
				tbl, err = table.Create(query.Table, query.Columns)
			} else {
				columns := make([]table.Column, len(query.Columns))
				for i, name := range query.Columns {
					columnType := table.ColumnText
					if query.ColumnTypes[i] == "uint64" {
						columnType = table.ColumnUint64
					}
					columns[i] = table.Column{Name: name, Type: columnType}
				}
				tbl, err = table.CreateWithTypes(query.Table, columns)
			}
			if err != nil {
				fmt.Fprintln(output, err)
				break
			}
			if err := tbl.Close(); err != nil {
				fmt.Fprintln(output, err)
			}

		case "insert", "select", "delete", "update":
			tbl, err := table.Open(query.Table)
			if err != nil {
				fmt.Fprintln(output, err)
				break
			}

			switch query.Type {
			case "insert":
				if err := tbl.Insert(query.Values); err != nil {
					fmt.Fprintln(output, err)
				}
			case "select":
				filters, err := resolveFilters(tbl, query.Filters)
				if err != nil {
					fmt.Fprintln(output, err)
					break
				}
				res, err := tbl.Select(query.Columns, filters)
				if err != nil {
					fmt.Fprintln(output, err)
					break
				}
				for _, row := range res {
					fmt.Fprintln(output, strings.Join(row, ","))
				}
			case "delete":
				filters, err := resolveFilters(tbl, query.Filters)
				if err != nil {
					fmt.Fprintln(output, err)
					break
				}
				if err := tbl.Delete(filters); err != nil {
					fmt.Fprintln(output, err)
				}
			case "update":
				filters, err := resolveFilters(tbl, query.Filters)
				if err != nil {
					fmt.Fprintln(output, err)
					break
				}
				updates, err := resolveUpdates(tbl, query.Updates)
				if err != nil {
					fmt.Fprintln(output, err)
					break
				}
				if err := tbl.Update(filters, updates); err != nil {
					fmt.Fprintln(output, err)
				}
			}

			if err := tbl.Close(); err != nil {
				fmt.Fprintln(output, err)
			}
		default:
			fmt.Fprintf(output, "unsupported query type %q\n", query.Type)
		}

		fmt.Fprint(output, ">")
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintln(output, table.ErrorTakingInput, err)
	}
}

func resolveFilters(tbl *table.Table, filters []parser.Pair) ([]table.ColEq, error) {
	resolved := make([]table.ColEq, 0, len(filters))
	columns := tbl.GetColumns()

	for _, f := range filters {
		colIdx := slices.Index(columns, f.ColName)
		if colIdx == -1 {
			return nil, fmt.Errorf("table %q: column %q does not exist", tbl.Name, f.ColName)
		}
		resolved = append(resolved, table.ColEq{
			ColIdx: colIdx,
			Value:  f.ColValue,
		})
	}

	return resolved, nil
}

func resolveUpdates(tbl *table.Table, updates []parser.Pair) ([]table.UpdateValue, error) {
	resolved := make([]table.UpdateValue, 0, len(updates))
	columns := tbl.GetColumns()

	for _, u := range updates {
		colIdx := slices.Index(columns, u.ColName)
		if colIdx == -1 {
			return nil, fmt.Errorf("table %q: column %q does not exist", tbl.Name, u.ColName)
		}
		resolved = append(resolved, table.UpdateValue{
			ColIdx: colIdx,
			Value:  u.ColValue,
		})
	}

	return resolved, nil
}

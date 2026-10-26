package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
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

		case "create_index", "drop_index", "insert", "select", "delete", "update":
			tbl, err := table.Open(query.Table)
			if err != nil {
				fmt.Fprintln(output, err)
				break
			}

			switch query.Type {
			case "create_index":
				if err := tbl.CreateIndex(query.Columns[0]); err != nil {
					fmt.Fprintln(output, err)
				}
			case "drop_index":
				if err := tbl.DropIndex(query.Columns[0]); err != nil {
					fmt.Fprintln(output, err)
				}
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
				var res [][]string
				if len(query.RangeFilters) > 0 {
					rangeBy, err := resolveRangeFilters(tbl, query.RangeFilters)
					if err != nil {
						fmt.Fprintln(output, err)
						break
					}
					res, err = tbl.SelectRange(query.Columns, rangeBy, filters)
				} else {
					res, err = tbl.Select(query.Columns, filters)
				}
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

func resolveRangeFilters(tbl *table.Table, filters []parser.RangePair) (table.Uint64Range, error) {
	if len(filters) == 0 {
		return table.Uint64Range{}, fmt.Errorf("range select requires at least one range predicate")
	}
	columns := tbl.GetColumns()
	types := tbl.GetColumnTypes()
	selected := table.Uint64Range{ColIdx: -1}
	for _, filter := range filters {
		columnIndex := slices.Index(columns, filter.ColName)
		if columnIndex < 0 {
			return table.Uint64Range{}, fmt.Errorf("table %q: column %q does not exist", tbl.Name, filter.ColName)
		}
		if types[columnIndex] != table.ColumnUint64 {
			return table.Uint64Range{}, fmt.Errorf("table %q: range column %q must be UINT64", tbl.Name, filter.ColName)
		}
		if selected.ColIdx >= 0 && selected.ColIdx != columnIndex {
			return table.Uint64Range{}, fmt.Errorf("table %q: one range column is supported per SELECT", tbl.Name)
		}
		selected.ColIdx = columnIndex
		value, err := strconv.ParseUint(filter.ColValue, 10, 64)
		if err != nil {
			return table.Uint64Range{}, fmt.Errorf("table %q: invalid UINT64 range value %q: %w", tbl.Name, filter.ColValue, err)
		}
		switch filter.Operator {
		case ">", ">=":
			inclusive := filter.Operator == ">="
			if selected.Lower == nil || value > *selected.Lower {
				selected.Lower = &value
				selected.LowerInclusive = inclusive
			} else if value == *selected.Lower {
				selected.LowerInclusive = selected.LowerInclusive && inclusive
			}
		case "<", "<=":
			inclusive := filter.Operator == "<="
			if selected.Upper == nil || value < *selected.Upper {
				selected.Upper = &value
				selected.UpperInclusive = inclusive
			} else if value == *selected.Upper {
				selected.UpperInclusive = selected.UpperInclusive && inclusive
			}
		default:
			return table.Uint64Range{}, fmt.Errorf("unsupported range operator %q", filter.Operator)
		}
	}
	return selected, nil
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

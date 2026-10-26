package parser

import (
	"errors"
	"fmt"

	"github.com/mugiwara999/goDB/internal/lexer"
)

type Pair struct {
	ColName  string
	ColValue string
}

type RangePair struct {
	ColName  string
	Operator string
	ColValue string
}

var ErrInvalidSyntax = errors.New("invalid syntax")

type Query struct {
	Type         string
	Table        string
	Columns      []string
	ColumnTypes  []string
	Values       []string
	Filters      []Pair
	RangeFilters []RangePair
	Updates      []Pair
}

func syntaxError(op, msg string) error {
	return fmt.Errorf("%s: %s: %w", op, msg, ErrInvalidSyntax)
}

func unexpectedToken(op string, token lexer.Token) error {
	if token.Type == lexer.TOKEN_EOF {
		return syntaxError(op, "unexpected end of input")
	}
	if token.Type == lexer.TOKEN_ILLEGAL {
		return syntaxError(op, fmt.Sprintf("illegal token %q", token.Value))
	}
	return syntaxError(op, fmt.Sprintf("unexpected token %q", token.Value))
}

func parseFilters(l *lexer.Lexer, token lexer.Token, op string, allowRanges bool) ([]Pair, []RangePair, error) {
	filters := make([]Pair, 0)
	var ranges []RangePair
	for {
		if token.Type != lexer.TOKEN_IDENT {
			return nil, nil, unexpectedToken(op, token)
		}

		colName := token.Value
		operator := l.NextToken()
		operatorValue := operator.Value
		if operator.Type != lexer.TOKEN_EQ {
			if !allowRanges || operator.Type != lexer.TOKEN_LT && operator.Type != lexer.TOKEN_LTE && operator.Type != lexer.TOKEN_GT && operator.Type != lexer.TOKEN_GTE {
				return nil, nil, syntaxError(op, fmt.Sprintf("expected '=' after column %q", colName))
			}
		}

		token = l.NextToken()
		if token.Type != lexer.TOKEN_IDENT && token.Type != lexer.TOKEN_STRING && token.Type != lexer.TOKEN_NUMBER {
			return nil, nil, syntaxError(op, fmt.Sprintf("expected value after column %q", colName))
		}

		if operator.Type == lexer.TOKEN_EQ {
			filters = append(filters, Pair{ColName: colName, ColValue: token.Value})
		} else {
			ranges = append(ranges, RangePair{ColName: colName, Operator: operatorValue, ColValue: token.Value})
		}
		token = l.NextToken()
		if token.Type == lexer.TOKEN_EOF {
			return filters, ranges, nil
		}
		if token.Type != lexer.TOKEN_AND {
			return nil, nil, unexpectedToken(op, token)
		}
		token = l.NextToken()
	}
}

func parseIdentList(op string, l *lexer.Lexer, stop lexer.TokenType) ([]string, lexer.Token, error) {
	values := make([]string, 0)
	token := l.NextToken()
	for {
		if token.Type == stop || token.Type == lexer.TOKEN_EOF {
			return values, token, nil
		}
		if token.Type != lexer.TOKEN_IDENT && token.Type != lexer.TOKEN_STRING && token.Type != lexer.TOKEN_NUMBER && token.Type != lexer.TOKEN_STAR {
			return nil, token, unexpectedToken(op, token)
		}
		values = append(values, token.Value)
		token = l.NextToken()
		if token.Type == stop || token.Type == lexer.TOKEN_EOF {
			return values, token, nil
		}
		if token.Type != lexer.TOKEN_COMMA {
			return nil, token, syntaxError(op, "expected ',' between values")
		}
		token = l.NextToken()
		if token.Type == stop || token.Type == lexer.TOKEN_EOF {
			return nil, token, syntaxError(op, "expected value after ','")
		}
	}
}

func parseIndexDDL(l *lexer.Lexer, query *Query, op string) (*Query, error) {
	token := l.NextToken()
	if token.Type != lexer.TOKEN_IDENT || token.Value != "on" {
		return nil, syntaxError(op, "expected ON after INDEX")
	}
	token = l.NextToken()
	if token.Type != lexer.TOKEN_IDENT {
		return nil, syntaxError(op, "expected table name after ON")
	}
	query.Table = token.Value
	if token = l.NextToken(); token.Type != lexer.TOKEN_LPAREN {
		return nil, syntaxError(op, "expected '(' after table name")
	}
	token = l.NextToken()
	if token.Type != lexer.TOKEN_IDENT {
		return nil, syntaxError(op, "expected indexed column name")
	}
	query.Columns = []string{token.Value}
	if token = l.NextToken(); token.Type != lexer.TOKEN_RPAREN {
		return nil, syntaxError(op, "expected ')' after indexed column")
	}
	if token = l.NextToken(); token.Type != lexer.TOKEN_EOF {
		return nil, unexpectedToken(op, token)
	}
	query.Type = op
	return query, nil
}

func Parse(input string) (*Query, error) {
	l := lexer.New(input)
	query := &Query{}
	token := l.NextToken()
	if token.Type == lexer.TOKEN_IDENT && token.Value == "drop" {
		token = l.NextToken()
		if token.Type != lexer.TOKEN_IDENT || token.Value != "index" {
			return nil, syntaxError("parse drop", "expected INDEX after DROP")
		}
		return parseIndexDDL(l, query, "drop_index")
	}

	switch token.Type {
	case lexer.TOKEN_CREATE:
		token = l.NextToken()
		if token.Type == lexer.TOKEN_IDENT && token.Value == "index" {
			return parseIndexDDL(l, query, "create_index")
		}
		if token.Type != lexer.TOKEN_TABLE {
			return nil, syntaxError("parse create", "expected TABLE or INDEX after CREATE")
		}
		query.Type = "create"

		token = l.NextToken()
		if token.Type != lexer.TOKEN_IDENT {
			return nil, syntaxError("parse create table", "expected table name after CREATE TABLE")
		}
		query.Table = token.Value

		token = l.NextToken()
		if token.Type != lexer.TOKEN_LPAREN {
			return nil, syntaxError("parse create table", "expected '(' after table name")
		}

		columns := make([]string, 0)
		columnTypes := make([]string, 0)
		typedMode := -1
		token = l.NextToken()
		for {
			if token.Type != lexer.TOKEN_IDENT {
				if token.Type == lexer.TOKEN_EOF {
					return nil, syntaxError("parse create table", "expected ')' after column list")
				}
				return nil, unexpectedToken("parse create table", token)
			}
			columns = append(columns, token.Value)
			token = l.NextToken()
			isTyped := token.Type == lexer.TOKEN_IDENT
			if isTyped {
				if token.Value != "text" && token.Value != "uint64" {
					return nil, syntaxError("parse create table", fmt.Sprintf("unsupported type %q", token.Value))
				}
				columnTypes = append(columnTypes, token.Value)
				token = l.NextToken()
			}
			if typedMode == -1 {
				if isTyped {
					typedMode = 1
				} else {
					typedMode = 0
				}
			} else if (typedMode == 1) != isTyped {
				return nil, syntaxError("parse create table", "all columns must either declare a type or omit types")
			}
			if token.Type == lexer.TOKEN_RPAREN {
				break
			}
			if token.Type != lexer.TOKEN_COMMA {
				return nil, syntaxError("parse create table", "expected ',' or ')' after column name")
			}
			token = l.NextToken()
			if token.Type == lexer.TOKEN_RPAREN {
				return nil, syntaxError("parse create table", "expected column name after ','")
			}
		}
		if len(columns) == 0 {
			return nil, syntaxError("parse create table", "at least one column name is required")
		}
		if token := l.NextToken(); token.Type != lexer.TOKEN_EOF {
			return nil, unexpectedToken("parse create table", token)
		}

		query.Columns = columns
		if typedMode == 1 {
			query.ColumnTypes = columnTypes
		}
		return query, nil

	case lexer.TOKEN_SELECT:
		query.Type = "select"

		columns, token, err := parseIdentList("parse select", l, lexer.TOKEN_FROM)
		if err != nil {
			return nil, err
		}
		if token.Type != lexer.TOKEN_FROM {
			return nil, syntaxError("parse select", "expected FROM after column list")
		}
		if len(columns) == 0 {
			return nil, syntaxError("parse select", "expected at least one column or '*'")
		}
		query.Columns = columns

		token = l.NextToken()
		if token.Type != lexer.TOKEN_IDENT {
			return nil, syntaxError("parse select", "expected table name after FROM")
		}
		query.Table = token.Value

		token = l.NextToken()
		if token.Type == lexer.TOKEN_EOF {
			return query, nil
		}
		if token.Type != lexer.TOKEN_WHERE {
			return nil, syntaxError("parse select", "expected WHERE after table name")
		}

		filters, ranges, err := parseFilters(l, l.NextToken(), "parse select where clause", true)
		if err != nil {
			return nil, err
		}
		query.Filters = filters
		query.RangeFilters = ranges
		return query, nil

	case lexer.TOKEN_DELETE:
		query.Type = "delete"

		token = l.NextToken()
		if token.Type != lexer.TOKEN_FROM {
			return nil, syntaxError("parse delete", "expected FROM after DELETE")
		}

		token = l.NextToken()
		if token.Type != lexer.TOKEN_IDENT {
			return nil, syntaxError("parse delete", "expected table name after FROM")
		}
		query.Table = token.Value

		token = l.NextToken()
		if token.Type == lexer.TOKEN_EOF {
			return query, nil
		}
		if token.Type != lexer.TOKEN_WHERE {
			return nil, syntaxError("parse delete", "expected WHERE after table name")
		}

		filters, ranges, err := parseFilters(l, l.NextToken(), "parse delete where clause", false)
		if err != nil {
			return nil, err
		}
		if len(ranges) > 0 {
			return nil, syntaxError("parse delete where clause", "range predicates are supported only for SELECT")
		}
		query.Filters = filters
		return query, nil

	case lexer.TOKEN_INSERT:
		query.Type = "insert"

		token = l.NextToken()
		if token.Type != lexer.TOKEN_INTO {
			return nil, syntaxError("parse insert", "expected INTO after INSERT")
		}

		token = l.NextToken()
		if token.Type != lexer.TOKEN_IDENT {
			return nil, syntaxError("parse insert", "expected table name after INSERT INTO")
		}
		query.Table = token.Value

		token = l.NextToken()
		if token.Type != lexer.TOKEN_VALUES {
			return nil, syntaxError("parse insert", "expected VALUES after table name")
		}

		values, token, err := parseIdentList("parse insert values", l, lexer.TOKEN_EOF)
		if err != nil {
			return nil, err
		}
		if len(values) == 0 {
			return nil, syntaxError("parse insert", "expected at least one value")
		}
		if token.Type != lexer.TOKEN_EOF {
			return nil, unexpectedToken("parse insert", token)
		}

		query.Values = values
		return query, nil

	case lexer.TOKEN_UPDATE:
		query.Type = "update"

		token = l.NextToken()
		if token.Type != lexer.TOKEN_IDENT {
			return nil, syntaxError("parse update", "expected table name after UPDATE")
		}
		query.Table = token.Value

		token = l.NextToken()
		if token.Type != lexer.TOKEN_SET {
			return nil, syntaxError("parse update", "expected SET after table name")
		}

		updates := make([]Pair, 0)
		token = l.NextToken()
		for {
			if token.Type != lexer.TOKEN_IDENT {
				if token.Type == lexer.TOKEN_WHERE || token.Type == lexer.TOKEN_EOF {
					break
				}
				return nil, unexpectedToken("parse update", token)
			}

			colName := token.Value
			token = l.NextToken()
			if token.Type != lexer.TOKEN_EQ {
				return nil, syntaxError("parse update", fmt.Sprintf("expected '=' after column %q", colName))
			}

			token = l.NextToken()
			if token.Type != lexer.TOKEN_IDENT && token.Type != lexer.TOKEN_STRING && token.Type != lexer.TOKEN_NUMBER {
				return nil, syntaxError("parse update", fmt.Sprintf("expected value after column %q", colName))
			}

			updates = append(updates, Pair{ColName: colName, ColValue: token.Value})
			token = l.NextToken()
			if token.Type == lexer.TOKEN_WHERE || token.Type == lexer.TOKEN_EOF {
				break
			}
			if token.Type != lexer.TOKEN_COMMA {
				return nil, syntaxError("parse update", "expected ',' between assignments")
			}
			token = l.NextToken()
			if token.Type == lexer.TOKEN_WHERE || token.Type == lexer.TOKEN_EOF {
				return nil, syntaxError("parse update", "expected assignment after ','")
			}
		}

		if len(updates) == 0 {
			return nil, syntaxError("parse update", "expected at least one assignment after SET")
		}
		query.Updates = updates

		if token.Type == lexer.TOKEN_EOF {
			return query, nil
		}
		if token.Type != lexer.TOKEN_WHERE {
			return nil, syntaxError("parse update", "expected WHERE after assignments")
		}

		filters, ranges, err := parseFilters(l, l.NextToken(), "parse update where clause", false)
		if err != nil {
			return nil, err
		}
		if len(ranges) > 0 {
			return nil, syntaxError("parse update where clause", "range predicates are supported only for SELECT")
		}
		query.Filters = filters
		return query, nil
	}

	return nil, syntaxError("parse query", fmt.Sprintf("unexpected token %q", token.Value))
}

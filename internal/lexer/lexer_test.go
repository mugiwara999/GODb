package lexer

import "testing"

func TestNextTokenKeywordsValuesAndEOF(t *testing.T) {
	input := "SELECT id, name FROM Users WHERE id=42 AND name='Ada Lovelace'"
	want := []Token{
		{TOKEN_SELECT, "select"}, {TOKEN_IDENT, "id"}, {TOKEN_COMMA, ","},
		{TOKEN_IDENT, "name"}, {TOKEN_FROM, "from"}, {TOKEN_IDENT, "users"},
		{TOKEN_WHERE, "where"}, {TOKEN_IDENT, "id"}, {TOKEN_EQ, "="},
		{TOKEN_NUMBER, "42"}, {TOKEN_AND, "and"}, {TOKEN_IDENT, "name"},
		{TOKEN_EQ, "="}, {TOKEN_STRING, "Ada Lovelace"}, {TOKEN_EOF, ""},
	}
	l := New(input)
	for i, expected := range want {
		if got := l.NextToken(); got != expected {
			t.Fatalf("token %d = %#v, want %#v", i, got, expected)
		}
	}
	if got := l.NextToken(); got.Type != TOKEN_EOF {
		t.Fatalf("token after EOF = %#v, want EOF", got)
	}
}

func TestNextTokenReportsIllegalInput(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "@", want: "@"},
		{input: "'unterminated", want: "unterminated string literal"},
	} {
		t.Run(test.input, func(t *testing.T) {
			got := New(test.input).NextToken()
			if got.Type != TOKEN_ILLEGAL || got.Value != test.want {
				t.Fatalf("token = %#v, want illegal %q", got, test.want)
			}
		})
	}
}

func TestNextTokenRangeOperators(t *testing.T) {
	input := "age<10 AND age<=20 AND age>1 AND age>=2"
	want := []Token{
		{TOKEN_IDENT, "age"}, {TOKEN_LT, "<"}, {TOKEN_NUMBER, "10"}, {TOKEN_AND, "and"},
		{TOKEN_IDENT, "age"}, {TOKEN_LTE, "<="}, {TOKEN_NUMBER, "20"}, {TOKEN_AND, "and"},
		{TOKEN_IDENT, "age"}, {TOKEN_GT, ">"}, {TOKEN_NUMBER, "1"}, {TOKEN_AND, "and"},
		{TOKEN_IDENT, "age"}, {TOKEN_GTE, ">="}, {TOKEN_NUMBER, "2"}, {TOKEN_EOF, ""},
	}
	l := New(input)
	for i, expected := range want {
		if got := l.NextToken(); got != expected {
			t.Fatalf("token %d = %#v, want %#v", i, got, expected)
		}
	}
}

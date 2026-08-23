package testcatalog

import "testing"

func TestNormalizeSQLPreservesQuotedWhitespace(t *testing.T) {
	input := "  CREATE  TABLE \"two  words\" (\n value TEXT DEFAULT 'line  one\nline two',\n note TEXT CHECK (note = 'it''s  exact')\n )  "
	want := "CREATE TABLE \"two  words\" ( value TEXT DEFAULT 'line  one\nline two', note TEXT CHECK (note = 'it''s  exact') )"
	if got := NormalizeSQL(input); got != want {
		t.Fatalf("normalized SQL = %q, want %q", got, want)
	}
	if NormalizeSQL(`CHECK (value = 'one space')`) == NormalizeSQL(`CHECK (value = 'one  space')`) {
		t.Fatal("normalization collapsed whitespace inside a quoted literal")
	}
}

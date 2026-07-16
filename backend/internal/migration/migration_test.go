package migration

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadSortsContiguousMigrations(t *testing.T) {
	source := fstest.MapFS{
		"000002_second.sql": {Data: []byte("CREATE TABLE second_table (id INT); ")},
		"000001_first.sql":  {Data: []byte("CREATE TABLE first_table (id INT); ")},
	}

	items, err := Load(source)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("loaded %d migrations, want 2", len(items))
	}
	if items[0].Version != 1 || items[0].Name != "first" {
		t.Fatalf("first migration = %#v, want version 1 and name first", items[0])
	}
	if items[1].Version != 2 || items[1].Name != "second" {
		t.Fatalf("second migration = %#v, want version 2 and name second", items[1])
	}
}

func TestLoadRejectsVersionGap(t *testing.T) {
	source := fstest.MapFS{
		"000001_first.sql": {Data: []byte("SELECT 1;")},
		"000003_third.sql": {Data: []byte("SELECT 3;")},
	}

	_, err := Load(source)
	if err == nil || !strings.Contains(err.Error(), "must be contiguous") {
		t.Fatalf("Load error = %v, want contiguous-version error", err)
	}
}

func TestSplitStatementsPreservesQuotedSemicolons(t *testing.T) {
	script := "-- migration note;\n" +
		"INSERT INTO example (value) VALUES ('a; b');\n" +
		"INSERT INTO example (value) VALUES (`column;name`);\n" +
		"/* comment; */ SELECT \"c; d\";"

	statements, err := SplitStatements(script)
	if err != nil {
		t.Fatalf("SplitStatements returned error: %v", err)
	}
	if len(statements) != 3 {
		t.Fatalf("SplitStatements returned %d statements, want 3: %#v", len(statements), statements)
	}
	if !strings.Contains(statements[0], "'a; b'") {
		t.Fatalf("first statement lost quoted semicolon: %q", statements[0])
	}
	if !strings.Contains(statements[1], "`column;name`") {
		t.Fatalf("second statement lost quoted identifier: %q", statements[1])
	}
	if !strings.Contains(statements[2], "\"c; d\"") {
		t.Fatalf("third statement lost quoted semicolon: %q", statements[2])
	}
}

func TestSplitStatementsRejectsUnterminatedQuote(t *testing.T) {
	_, err := SplitStatements("INSERT INTO example (value) VALUES ('not closed);")
	if err == nil {
		t.Fatal("SplitStatements returned nil error for an unterminated quote")
	}
}

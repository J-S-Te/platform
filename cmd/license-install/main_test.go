package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/J-S-Te/license-core"
)

func TestReadLicenseBoundaries(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{{"empty", "", false}, {"too-large", strings.Repeat("x", core.MaxTokenBytes+1), false}, {"bounded", " signed-test-token\n", true}} {
		path := filepath.Join(dir, tc.name)
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		raw, err := readLicense(path)
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.valid && raw != "signed-test-token" {
			t.Fatal("whitespace not normalized")
		}
	}
	if _, err := readLicense(dir); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := readLicense(""); err == nil {
		t.Fatal("missing path accepted")
	}
}

func TestCLIRejectsUncontrolledArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"import", "--timeout", "0s"}, {"activate", "--timeout", "2h"}, {"activate", "--host", "attacker.invalid"}, {"import", "unexpected"}, {"prepare"}, {"prepare", "--scenario", "unknown", "--customer", "customer"}, {"prepare", "--scenario", "fresh"}, {"prepare", "--scenario", "migrate", "--customer", "customer", "--timeout", "0s"}, {"migrate", "--file", "token"}, {"migrate", "--scenario", "fresh"}, {"import", "--customer", "customer"}} {
		if err := run(args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

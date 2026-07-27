package main

import (
	"strings"
	"testing"
)

func TestParseOptionsRequiresSecretOnStandardInput(t *testing.T) {
	_, err := parseOptions([]string{"--display-name", "平台管理员", "--account-name", "admin"})
	if err == nil || !strings.Contains(err.Error(), "--password-stdin") {
		t.Fatalf("parseOptions() error = %v, want password-stdin requirement", err)
	}
}

func TestParseOptionsRejectsBootstrapArgumentsInStatusMode(t *testing.T) {
	_, err := parseOptions([]string{"--status", "--account-name", "admin"})
	if err == nil || !strings.Contains(err.Error(), "--status") {
		t.Fatalf("parseOptions() error = %v, want status-mode validation", err)
	}
}

func TestReadPasswordRemovesOnlyTransportLineEnding(t *testing.T) {
	password, err := readPassword(strings.NewReader("StrongPassword-1!\r\n"), true)
	if err != nil {
		t.Fatalf("readPassword() error = %v", err)
	}
	if password != "StrongPassword-1!" {
		t.Fatalf("readPassword() = %q, want password without transport line ending", password)
	}
}

func TestReadPasswordRejectsOversizedInput(t *testing.T) {
	_, err := readPassword(strings.NewReader(strings.Repeat("a", maxBootstrapPasswordBytes+1)), true)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("readPassword() error = %v, want size validation", err)
	}
}

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeVerifier(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "release-verifier")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReleaseIntegrityCheckFromEnv(t *testing.T) {
	dir := t.TempDir()
	trust := filepath.Join(dir, "trust")
	if err := os.Mkdir(trust, 0o700); err != nil {
		t.Fatal(err)
	}
	verifier := writeFakeVerifier(t, dir, `printf '{"status":"INSTALLED_MANIFEST_VERIFIED"}\n'`)
	t.Setenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED", "true")
	t.Setenv("SUBSYSTEM_RELEASE_VERIFIER", verifier)
	t.Setenv("SUBSYSTEM_RELEASE_TRUST_DIR", trust)
	check, err := releaseIntegrityCheckFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if check == nil {
		t.Fatal("required release integrity check is nil")
	}
	if err := check(context.Background(), filepath.Join(dir, "target"), ""); err != nil {
		t.Fatalf("check: %v", err)
	}
}

func TestReleaseIntegrityCheckRejectsBadStatus(t *testing.T) {
	dir := t.TempDir()
	trust := filepath.Join(dir, "trust")
	if err := os.Mkdir(trust, 0o700); err != nil {
		t.Fatal(err)
	}
	verifier := writeFakeVerifier(t, dir, `printf '{"status":"FAILED"}\n'`)
	t.Setenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED", "true")
	t.Setenv("SUBSYSTEM_RELEASE_VERIFIER", verifier)
	t.Setenv("SUBSYSTEM_RELEASE_TRUST_DIR", trust)
	check, err := releaseIntegrityCheckFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := check(context.Background(), filepath.Join(dir, "target"), ""); err == nil {
		t.Fatal("non-verified status must fail closed")
	}
}

func TestReleaseIntegrityCheckDisabledByDefault(t *testing.T) {
	t.Setenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED", "false")
	check, err := releaseIntegrityCheckFromEnv()
	if err != nil || check != nil {
		t.Fatalf("disabled check must be nil, err=%v", err)
	}
}

func TestReleaseIntegrityCheckRejectsRelativeVerifier(t *testing.T) {
	t.Setenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED", "true")
	t.Setenv("SUBSYSTEM_RELEASE_VERIFIER", "release-verifier")
	t.Setenv("SUBSYSTEM_RELEASE_TRUST_DIR", "/etc/unified-identity-platform/release-trust")
	if _, err := releaseIntegrityCheckFromEnv(); err == nil {
		t.Fatal("relative verifier must be rejected")
	}
}

func TestReleaseIntegrityCheckPassesExpectedDigestAndRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	trust := filepath.Join(dir, "trust")
	if err := os.Mkdir(trust, 0o700); err != nil {
		t.Fatal(err)
	}
	expected := "sha256:" + strings.Repeat("a", 64)
	verifier := writeFakeVerifier(t, dir, `last=""; for arg in "$@"; do last="$arg"; done; if [ "$last" != "$EXPECTED" ]; then echo "last=$last expected=$EXPECTED" >&2; exit 7; fi; printf '{"status":"INSTALLED_MANIFEST_VERIFIED"}\n'`)
	t.Setenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED", "true")
	t.Setenv("SUBSYSTEM_RELEASE_VERIFIER", verifier)
	t.Setenv("SUBSYSTEM_RELEASE_TRUST_DIR", trust)
	t.Setenv("SUBSYSTEM_RELEASE_EXPECTED_DIGEST", expected)
	t.Setenv("EXPECTED", expected)
	check, err := releaseIntegrityCheckFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := check(context.Background(), filepath.Join(dir, "target"), ""); err != nil {
		t.Fatalf("expected digest was not passed correctly: %v", err)
	}
	requested := "sha256:" + strings.Repeat("b", 64)
	t.Setenv("EXPECTED", requested)
	if err := check(context.Background(), filepath.Join(dir, "target"), requested); err != nil {
		t.Fatalf("API requested digest was not preferred: %v", err)
	}
	if err := check(context.Background(), filepath.Join(dir, "target"), "sha256:ABC"); err == nil {
		t.Fatal("malformed API requested digest must fail closed")
	}
	t.Setenv("SUBSYSTEM_RELEASE_EXPECTED_DIGEST", "sha256:ABC")
	if _, err := releaseIntegrityCheckFromEnv(); err == nil {
		t.Fatal("malformed expected digest must be rejected at startup")
	}
}

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runtimeManifest(t *testing.T, digest string) string {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "runtime", "release-integrity")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"images": []any{map[string]string{"config_digest": digest}}})
	if err := os.WriteFile(filepath.Join(directory, "release-manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func fakeDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	script := `#!/bin/sh
if [ "$1" = "ps" ]; then printf '%s\n' "${FAKE_PS:-}"; exit 0; fi
if [ "$1" = "inspect" ]; then printf '%s\n' "${FAKE_INSPECT:-}"; exit 0; fi
exit 2
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func TestRuntimeProjectionAcceptsSignedImage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	root := runtimeManifest(t, digest)
	fakeDocker(t)
	t.Setenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED", "true")
	t.Setenv("SUBSYSTEM_PRODUCTION_DEPLOY_ROOT", root)
	t.Setenv("SUBSYSTEM_PLATFORM_COMPOSE_PROJECT", "basic-platform-production")
	t.Setenv("SUBSYSTEM_DOCKER_BINARY", "docker")
	t.Setenv("FAKE_PS", "container-1")
	t.Setenv("FAKE_INSPECT", digest)
	check, err := runtimeProjectionCheckFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := check(context.Background()); err != nil {
		t.Fatalf("signed image rejected: %v", err)
	}
}

func TestRuntimeProjectionRejectsUnlistedImage(t *testing.T) {
	root := runtimeManifest(t, "sha256:"+strings.Repeat("a", 64))
	fakeDocker(t)
	t.Setenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED", "true")
	t.Setenv("SUBSYSTEM_PRODUCTION_DEPLOY_ROOT", root)
	t.Setenv("SUBSYSTEM_PLATFORM_COMPOSE_PROJECT", "basic-platform-production")
	t.Setenv("SUBSYSTEM_DOCKER_BINARY", "docker")
	t.Setenv("FAKE_PS", "container-1")
	t.Setenv("FAKE_INSPECT", "sha256:"+strings.Repeat("b", 64))
	check, err := runtimeProjectionCheckFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := check(context.Background()); err == nil {
		t.Fatal("unlisted running image must fail projection")
	}
}

func TestRuntimeProjectionAllowsNoRunningContainers(t *testing.T) {
	root := runtimeManifest(t, "sha256:"+strings.Repeat("a", 64))
	fakeDocker(t)
	t.Setenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED", "true")
	t.Setenv("SUBSYSTEM_PRODUCTION_DEPLOY_ROOT", root)
	t.Setenv("SUBSYSTEM_PLATFORM_COMPOSE_PROJECT", "basic-platform-production")
	t.Setenv("SUBSYSTEM_DOCKER_BINARY", "docker")
	t.Setenv("FAKE_PS", "")
	check, err := runtimeProjectionCheckFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := check(context.Background()); err != nil {
		t.Fatalf("no running containers must pass: %v", err)
	}
}

func TestRuntimeProjectionRequiresManifest(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED", "true")
	t.Setenv("SUBSYSTEM_PRODUCTION_DEPLOY_ROOT", root)
	if _, err := runtimeProjectionCheckFromEnv(); err == nil {
		t.Fatal("missing signed manifest must fail closed")
	}
}

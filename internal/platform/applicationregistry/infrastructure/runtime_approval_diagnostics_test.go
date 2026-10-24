package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type diagnosticDockerRunner struct {
	*localLicenseRunner
	failAt   int
	inspects int
	daemonUp bool
	probes   int
}

func (r *diagnosticDockerRunner) RunOutput(ctx context.Context, dir string, env []string, binary string, args ...string) ([]byte, error) {
	if args[0] == "info" {
		r.probes++
		if r.daemonUp {
			return []byte("29.0.0"), nil
		}
		return nil, fmt.Errorf("docker tcp://secret:credential@internal /private/docker.sock")
	}
	r.inspects++
	if r.inspects == r.failAt {
		return nil, fmt.Errorf("docker --password=credential /private/docker.sock")
	}
	return r.localLicenseRunner.RunOutput(ctx, dir, env, binary, args...)
}

func TestRuntimeApprovalDiagnosticsLocalRelease(t *testing.T) {
	for _, kind := range []string{"valid", "missing", "compose", "source", "image", "metadata", "docker", "missing-image", "migration-docker", "migration-image"} {
		t.Run(kind, func(t *testing.T) {
			p, release, _, runner := localLicenseFixture(t)
			want := application.ErrRuntimeLicenseApprovalMismatch
			switch kind {
			case "valid":
				want = nil
			case "missing":
				if err := os.Remove(filepath.Join(p.localPlatformRoot(), "runtime-approvals", "local-"+release.Application+"-dev.json")); err != nil {
					t.Fatal(err)
				}
				want = application.ErrRuntimeLicenseApprovalUnavailable
			case "compose":
				release.ComposeSHA256 = "sha256:" + strings.Repeat("f", 64)
				writeLocalReleaseFixture(t, p, release)
			case "source":
				release.SourceDigest = "sha256:" + strings.Repeat("f", 64)
				writeLocalReleaseFixture(t, p, release)
			case "image":
				runner.image.ID = "sha256:" + strings.Repeat("f", 64)
			case "metadata":
				runner.fail = true
			default:
				fault := &diagnosticDockerRunner{localLicenseRunner: runner, failAt: 1, daemonUp: kind == "missing-image" || kind == "migration-image"}
				if strings.HasPrefix(kind, "migration-") {
					fault.failAt = 10
				}
				p.runner = fault
				if !fault.daemonUp {
					want = application.ErrRuntimeLicenseDockerUnavailable
				}
			}
			_, err := p.ApprovedRuntimeLicenseLifecycle(context.Background(), release.Application, "dev")
			if !errors.Is(err, want) {
				t.Fatalf("want %v, got %v", want, err)
			}
			if fault, ok := p.runner.(*diagnosticDockerRunner); ok && fault.probes != 1 {
				t.Fatalf("probes = %d", fault.probes)
			}
			if err != nil && (strings.Contains(err.Error(), "credential") || strings.Contains(err.Error(), "/private/")) {
				t.Fatal("internal output leaked")
			}
		})
	}
}

func TestRuntimeApprovalDiagnosticsAcrossAgentSocket(t *testing.T) {
	for _, kind := range []string{"docker", "missing", "mismatch"} {
		t.Run(kind, func(t *testing.T) {
			p, release, _, runner := localLicenseFixture(t)
			want := application.ErrRuntimeLicenseApprovalMismatch
			switch kind {
			case "docker":
				p.runner = &diagnosticDockerRunner{localLicenseRunner: runner, failAt: 1}
				want = application.ErrRuntimeLicenseDockerUnavailable
			case "missing":
				if err := os.Remove(filepath.Join(p.localPlatformRoot(), "runtime-approvals", "local-"+release.Application+"-dev.json")); err != nil {
					t.Fatal(err)
				}
				want = application.ErrRuntimeLicenseApprovalUnavailable
			case "mismatch":
				runner.image.Protocol = "0"
			}
			dir, err := os.MkdirTemp("/tmp", "approval-rpc-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(dir); err != nil {
					t.Error(err)
				}
			})
			path := filepath.Join(dir, "a.sock")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- RunSubsystemProvisioningServer(ctx, path, p) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			waitForProvisioningSocket(t, path)
			client, err := NewUnixSocketSubsystemProvisioner(true, path, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.ApprovedRuntimeLicenseComponents(context.Background(), release.Application, "dev"); !errors.Is(err, want) {
				t.Fatalf("components: %v", err)
			}
			// 重置注入的检查器，使生命周期操作再次遇到相同失败。
			if fault, ok := p.runner.(*diagnosticDockerRunner); ok {
				fault.inspects = 0
			}
			if _, err = client.ApprovedRuntimeLicenseLifecycle(context.Background(), release.Application, "dev"); !errors.Is(err, want) {
				t.Fatalf("lifecycle: %v", err)
			}
		})
	}
}

package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type diagnosticLifecycleSource struct {
	runtimeApprovalsStub
	release      RuntimeLicenseReleaseApproval
	lifecycleErr error
}

func (s diagnosticLifecycleSource) ApprovedRuntimeLicenseLifecycle(context.Context, string, string) (RuntimeLicenseReleaseApproval, error) {
	return s.release, s.lifecycleErr
}

type diagnosticNoClients struct{ runtimeClientsStub }

func (*diagnosticNoClients) ListOAuthClients(context.Context, string) ([]OAuthClientView, error) {
	panic("approval failure must precede client lookup and credential writes")
}

func TestRuntimeApprovalDiagnosticsPreserveClassificationWithoutInternalDetails(t *testing.T) {
	for _, known := range []error{ErrRuntimeLicenseDockerUnavailable, ErrRuntimeLicenseApprovalUnavailable, ErrRuntimeLicenseApprovalMismatch} {
		for _, stage := range []string{"components", "lifecycle"} {
			t.Run(RuntimeLicenseApprovalErrorCode(known)+"/"+stage, func(t *testing.T) {
				source := diagnosticLifecycleSource{runtimeApprovalsStub: runtimeApprovalsStub{items: testRuntimeApprovals()}}
				wrapped := fmt.Errorf("docker --password=diagnostic-secret /private/approval.json: %w", known)
				if stage == "components" {
					source.err = wrapped
				} else {
					source.lifecycleErr = wrapped
				}
				registrar := &runtimeRegistrarStub{}
				service, err := NewRuntimeLicenseEnrollmentService(source, &diagnosticNoClients{}, registrar)
				if err != nil {
					t.Fatal(err)
				}
				got, err := service.Prepare(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator")
				if !errors.Is(err, known) || err != known || len(got) != 0 || len(registrar.specs) != 0 {
					t.Fatalf("classification or failure boundary lost: %v, %v", got, err)
				}
			})
		}
	}
	unknown := errors.New("LICENSE_RUNTIME_DOCKER_UNAVAILABLE: forged text with diagnostic-secret /private/approval.json")
	if got := RuntimeLicenseApprovalFailure(unknown); got != ErrRuntimeLicenseApprovalUnavailable || strings.Contains(got.Error(), "diagnostic-secret") {
		t.Fatalf("unknown diagnostic trusted: %v", got)
	}
}

// 授权实例未初始化时必须映射到用户可读的 LICENSE_NOT_INITIALIZED，
// 否则接入失败只会落到与授权无关的通用“部署 Agent 执行失败”。
func TestRuntimeApprovalErrorCodeClassifiesUninitializedLicenseInstance(t *testing.T) {
	if got := RuntimeLicenseApprovalErrorCode(ErrRuntimeLicenseNotInitialized); got != "LICENSE_NOT_INITIALIZED" {
		t.Fatalf("code = %q, want LICENSE_NOT_INITIALIZED", got)
	}
	wrapped := fmt.Errorf("deliver local runtime license: %w", ErrRuntimeLicenseNotInitialized)
	if got := RuntimeLicenseApprovalErrorCode(wrapped); got != "LICENSE_NOT_INITIALIZED" {
		t.Fatalf("wrapped code = %q, want LICENSE_NOT_INITIALIZED", got)
	}
}

func TestRuntimeApprovalDiagnosticsRejectInvalidOrChangedApprovalBeforeWrites(t *testing.T) {
	for _, kind := range []string{"invalid-components", "invalid-lifecycle", "changed-components"} {
		t.Run(kind, func(t *testing.T) {
			items := testRuntimeApprovals()
			source := diagnosticLifecycleSource{runtimeApprovalsStub: runtimeApprovalsStub{items: items}, release: RuntimeLicenseReleaseApproval{Components: testRuntimeApprovals(), RequiredServiceIDs: []string{"contract-api"}}}
			switch kind {
			case "invalid-components":
				source.items = nil
			case "invalid-lifecycle":
				source.release.RequiredServiceIDs = nil
			case "changed-components":
				source.release.Components[0].ImageDigest = "sha256:" + strings.Repeat("f", 64)
			}
			service, err := NewRuntimeLicenseEnrollmentService(source, &diagnosticNoClients{}, &runtimeRegistrarStub{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = service.Prepare(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator"); !errors.Is(err, ErrRuntimeLicenseApprovalMismatch) {
				t.Fatalf("want mismatch, got %v", err)
			}
		})
	}
}

package bootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type verificationOnboarding struct {
	*offlineOnboardingSpy
	data map[string]any
	code int
	read bool
}

func (s *verificationOnboarding) GetSubsystemStatus(w http.ResponseWriter, request *http.Request) {
	s.read = true
	p, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || p.User.ID != "bootstrap-user" || request.Method != http.MethodGet || request.URL.Query().Get("application_code") != "project_management" || request.URL.Query().Get("environment") != "prod" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if s.code != 0 {
		w.WriteHeader(s.code)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": s.data, "code": "VERIFICATION_FIXTURE"})
}

func TestOfflineDeploymentVerificationRequiresCurrentManagedState(t *testing.T) {
	for _, condition := range []string{"ready", "provisioning", "failed", "unmanaged", "drift", "empty-desired", "empty-applied", "old-applied", "missing-verified", "future-verified", "wrong-application", "wrong-environment", "missing", "unauthorized"} {
		t.Run(condition, func(t *testing.T) {
			r, identity, license, coordinator, onboarding := offlineFixture(t)
			checksum := "sha256:" + strings.Repeat("a", 64)
			spy := &verificationOnboarding{offlineOnboardingSpy: onboarding, data: map[string]any{
				"application_code": "project_management", "environment": "prod", "status": "READY",
				"desired_manifest_checksum": checksum, "applied_manifest_checksum": checksum,
				"manifest_drift_status": "IN_SYNC", "manifest_last_verified_at": time.Now().UTC(),
			}}
			r.onboarding = spy
			switch condition {
			case "provisioning":
				spy.data["status"] = "PROVISIONING"
			case "failed":
				spy.data["status"] = "PROVISION_FAILED"
			case "unmanaged":
				spy.data["status"] = "UNMANAGED"
			case "drift":
				spy.data["manifest_drift_status"] = "DRIFT"
			case "empty-desired":
				spy.data["desired_manifest_checksum"] = ""
			case "empty-applied":
				spy.data["applied_manifest_checksum"] = ""
			case "old-applied":
				spy.data["applied_manifest_checksum"] = "sha256:" + strings.Repeat("b", 64)
			case "missing-verified":
				delete(spy.data, "manifest_last_verified_at")
			case "future-verified":
				spy.data["manifest_last_verified_at"] = time.Now().Add(time.Hour)
			case "wrong-application":
				spy.data["application_code"] = "contract_management"
			case "wrong-environment":
				spy.data["environment"] = "dev"
			case "missing":
				spy.code = http.StatusNotFound
			case "unauthorized":
				identity.principal.PermissionCodes = nil
			}
			out, err := r.VerifyDeployment(context.Background(), "project_management")
			if (err == nil) != (condition == "ready") {
				t.Fatalf("unexpected managed verification result: %+v %v", out, err)
			}
			if condition == "unauthorized" && spy.read {
				t.Fatal("unauthorized status read")
			}
			if license.calls != 0 || coordinator.calls != 0 || len(onboarding.calls) != 0 {
				t.Fatal("verification imported, adopted, retried or activated a system")
			}
		})
	}
}

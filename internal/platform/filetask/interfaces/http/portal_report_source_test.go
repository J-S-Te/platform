package filetaskhttp

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

func TestPortalReportSourceRequiresDedicatedMachineIdentity(t *testing.T) {
	valid := authctx.Principal{SessionID: "customer_portal-prod-portal-report-source", Tenant: authctx.ReferenceName{ID: "tenant"}, Account: authctx.ReferenceName{ID: "portal-app", Code: "customer_portal"}, PermissionCodes: []string{portalReportFileScope}}
	if !portalReportSourceCaller(valid) {
		t.Fatal("dedicated machine identity refused")
	}
	for _, change := range []func(*authctx.Principal){
		func(p *authctx.Principal) { p.Account.Code = "project_management" },
		func(p *authctx.Principal) { p.Account.ID = "" },
		func(p *authctx.Principal) { p.Tenant.ID = "" },
		func(p *authctx.Principal) { p.SessionID = "customer_portal-prod-file-gateway-writer" },
		func(p *authctx.Principal) { p.User.ID = "browser-user" },
		func(p *authctx.Principal) { p.PermissionCodes = []string{"platform:file:download"} },
		func(p *authctx.Principal) { p.PermissionCodes = []string{"*"} },
	} {
		invalid := valid
		change(&invalid)
		if portalReportSourceCaller(invalid) {
			t.Fatal("ordinary file or browser identity gained delegated source access")
		}
	}
}

func TestPortalReportSourceRejectsInvalidIntegrityClaims(t *testing.T) {
	digest := sha256.Sum256([]byte("test-pdf"))
	valid := portalReportSourceRequest{RequestID: 1, ExpectedSHA256: hex.EncodeToString(digest[:]), ExpectedSize: 8, ExpectedMIME: "application/pdf", ExpectedName: "report.pdf"}
	if _, ok := validPortalReportSource(valid); !ok {
		t.Fatal("valid source claims refused")
	}
	for _, change := range []func(*portalReportSourceRequest){
		func(r *portalReportSourceRequest) { r.RequestID = 0 },
		func(r *portalReportSourceRequest) { r.ExpectedSHA256 = "wrong" },
		func(r *portalReportSourceRequest) { r.ExpectedSize = 0 },
		func(r *portalReportSourceRequest) { r.ExpectedSize = 21 << 20 },
		func(r *portalReportSourceRequest) { r.ExpectedMIME = "text/html" },
		func(r *portalReportSourceRequest) { r.ExpectedName = "../report.pdf" },
		func(r *portalReportSourceRequest) { r.ExpectedName = "report\r\n.pdf" },
	} {
		invalid := valid
		change(&invalid)
		if _, ok := validPortalReportSource(invalid); ok {
			t.Fatal("invalid integrity/source claim accepted")
		}
	}
}

func TestPortalReportSourcePinsActualVersionAndContent(t *testing.T) {
	digest := sha256.Sum256([]byte("test-pdf"))
	input := portalReportSourceRequest{RequestID: 1, ExpectedSHA256: hex.EncodeToString(digest[:]), ExpectedSize: 8, ExpectedMIME: "application/pdf", ExpectedName: "report.pdf"}
	session := uploadV2Session{TenantID: "tenant", ApplicationID: "project-app", FileID: "file", VersionID: "version", ExpectedSize: 8, ExpectedSHA256: digest[:]}
	stored := domain.StoredFile{File: domain.File{ID: "file", TenantID: "tenant", ApplicationID: "project-app", Namespace: "project", Purpose: "report", Status: domain.FileStatusReady}, Version: domain.FileVersion{ID: "version", FileID: "file", SizeBytes: 8, SHA256: digest[:], MediaType: "application/pdf", OriginalName: "report.pdf", Status: domain.FileVersionStatusReady}}
	if !portalReportSourceMatches(stored, session, input, digest[:]) {
		t.Fatal("valid immutable source refused")
	}
	for _, change := range []func(*domain.StoredFile){
		func(s *domain.StoredFile) { s.File.TenantID = "other-tenant" },
		func(s *domain.StoredFile) { s.File.ApplicationID = "portal-app" },
		func(s *domain.StoredFile) { s.File.Purpose = "field-evidence" },
		func(s *domain.StoredFile) { s.File.Namespace = "contract" },
		func(s *domain.StoredFile) { s.File.Status = domain.FileStatusQuarantined },
		func(s *domain.StoredFile) { s.Version.Status = domain.FileVersionStatusFailed },
		func(s *domain.StoredFile) { s.Version.ID = "replacement" },
		func(s *domain.StoredFile) { s.Version.SizeBytes = 9 },
		func(s *domain.StoredFile) { s.Version.SHA256 = make([]byte, 32) },
		func(s *domain.StoredFile) { s.Version.OriginalName = "different.pdf" },
	} {
		invalid := stored
		change(&invalid)
		if portalReportSourceMatches(invalid, session, input, digest[:]) {
			t.Fatal("changed source/version released")
		}
	}
}

package filetaskhttp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type releaseFileService struct {
	*stubV1FileService
	input application.BindingInput
	calls int
	err   error
}

func (s *releaseFileService) ReleaseResourceBinding(_ context.Context, input application.BindingInput) error {
	s.input = input
	s.calls++
	return s.err
}

func TestReleaseFileBindingScopesAndAuditsMachineCoordinates(t *testing.T) {
	for _, tc := range []struct {
		name, app string
		want      int
	}{{"own application", "app-1", 200}, {"cross application", "app-2", 403}} {
		t.Run(tc.name, func(t *testing.T) {
			service := &releaseFileService{stubV1FileService: &stubV1FileService{}}
			script := &auditScript{}
			h, err := NewHandler(service, &stubV1JobService{}, slog.New(&captureLogger{}), newAuditTestDB(t, script))
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/files/file-1/bindings/release", strings.NewReader(`{"application_id":"`+tc.app+`","resource_type":"contract_template_source","resource_id":"generation-1","binding_type":"TEMPLATE_SOURCE"}`))
			r.SetPathValue("file_id", "file-1")
			p := v1Principal()
			p.User.ID = ""
			r = r.WithContext(authctx.WithPrincipal(r.Context(), p))
			w := httptest.NewRecorder()
			h.ReleaseFileBinding(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.want == 200 && (service.input.TenantID != "tenant-1" || service.input.ApplicationID != "app-1" || service.input.OperatorUserID != "app-1" || service.input.FileID != "file-1" || len(script.inserts()) != 1) {
				t.Fatalf("missing scoped coordinates/audit: %+v", service.input)
			}
			if tc.want == 403 && service.calls != 0 {
				t.Fatal("cross-app request reached persistence")
			}
		})
	}
}

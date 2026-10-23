package filetaskhttp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type resolveFileService struct {
	*stubV1FileService
	tenant, app, requestID string
	err                    error
}

func (s *resolveFileService) RetireUploadRequest(ctx context.Context, tenant, app, requestID string) (string, string, error) {
	s.tenant, s.app, s.requestID = tenant, app, requestID
	return "", "CANCELLED", s.err
}

func (s *resolveFileService) ResolveUploadRequest(_ context.Context, tenant, app, requestID string) (string, string, error) {
	s.tenant, s.app, s.requestID = tenant, app, requestID
	return "file-stable", "FAILED", s.err
}

func TestRetireUploadRequestHTTPPersistsMissingIdentityWithoutApplicationOverride(t *testing.T) {
	service := &resolveFileService{stubV1FileService: &stubV1FileService{}}
	script := &auditScript{}
	h, err := NewHandler(service, &stubV1JobService{}, slog.New(&captureLogger{}), newAuditTestDB(t, script))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/file-upload-requests/absent/retire?application_id=other", nil)
	req.SetPathValue("request_id", "absent")
	req = req.WithContext(authctx.WithPrincipal(req.Context(), v1Principal()))
	w := httptest.NewRecorder()
	h.RetireUploadRequest(w, req)
	var result struct {
		Data struct {
			FileID, Status string
			Retired        bool
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || !result.Data.Retired || result.Data.Status != "CANCELLED" || result.Data.FileID != "" || service.app != "app-1" || service.tenant != "tenant-1" {
		t.Fatalf("retire response=%d %s scope=%+v", w.Code, w.Body.String(), service)
	}
}

func TestResolveUploadRequestHTTPUsesVerifiedScopeAndStableResult(t *testing.T) {
	for _, missing := range []bool{false, true} {
		service := &resolveFileService{stubV1FileService: &stubV1FileService{}}
		if missing {
			service.err = application.ErrNotFound
		}
		script := &auditScript{}
		handler, err := NewHandler(service, &stubV1JobService{}, slog.New(&captureLogger{}), newAuditTestDB(t, script))
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "/api/v1/file-upload-requests/source-key?application_id=other-app", nil)
		req.SetPathValue("request_id", "source-key")
		req = req.WithContext(authctx.WithPrincipal(req.Context(), v1Principal()))
		w := httptest.NewRecorder()
		handler.ResolveUploadRequest(w, req)
		if service.tenant != "tenant-1" || service.app != "app-1" || service.requestID != "source-key" {
			t.Fatalf("untrusted scope used: %+v", service)
		}
		if missing {
			if w.Code != 404 {
				t.Fatalf("missing status=%d", w.Code)
			}
			continue
		}
		var body struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK || body.Data["request_id"] != "source-key" || body.Data["file_id"] != "file-stable" || body.Data["status"] != "FAILED" {
			t.Fatalf("response=%d %s", w.Code, w.Body.String())
		}
	}
}

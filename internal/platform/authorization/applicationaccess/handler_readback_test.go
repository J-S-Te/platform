package applicationaccess

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteErrorAccessReadbackContract(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			assertReadbackContract(t, method)
		})
	}
}

func assertReadbackContract(t *testing.T, method string) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/api/v1/iam/users/u1/applications/a1/access", nil)
	writeError(recorder, request, fmt.Errorf("%w: database details must not leak", ErrAccessReadback))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if strings.Contains(recorder.Body.String(), "database details") {
		t.Fatalf("internal details leaked: %s", recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["code"] != "APPLICATION_ACCESS_READBACK_FAILED" {
		t.Fatalf("code = %v", body["code"])
	}
	if body["message"] != "授权已保存，但最新结果读取失败，请刷新核对，勿重复提交。" {
		t.Fatalf("message = %v", body["message"])
	}
	details, ok := body["details"].(map[string]any)
	if !ok || details["write_committed"] != "true" || details["next_action"] != "refresh" {
		t.Fatalf("details = %#v", body["details"])
	}
}

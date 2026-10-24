package identityhttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
)

// 自审拒绝是主体不能执行该操作，不能被误报为可重试的服务端故障。
func TestPersonnelSelfApprovalReturnsForbidden(t *testing.T) {
	for _, err := range []error{
		application.ErrSelfApprovalForbidden,
		fmt.Errorf("approval rejected: %w", application.ErrSelfApprovalForbidden),
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/personnel-changes/change-1/transition", nil)
		response := httptest.NewRecorder()
		writePersonnelChangeError(response, request, err)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status=%d, want 403; body=%s", response.Code, response.Body.String())
		}
		if envelope := decodeErrorEnvelope(t, response); envelope.Code != "AUTH_FORBIDDEN" {
			t.Fatalf("code=%q, want AUTH_FORBIDDEN", envelope.Code)
		}
	}
}

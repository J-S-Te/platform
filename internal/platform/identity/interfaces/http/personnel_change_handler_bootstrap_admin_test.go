package identityhttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
)

func TestPersonnelChangeErrorMapsBootstrapAdministratorProtection(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "protected",
			err:        fmt.Errorf("validate personnel change: %w", bootstrapadmin.ErrProtected),
			wantStatus: http.StatusConflict,
			wantCode:   "IAM_BOOTSTRAP_ADMIN_PROTECTED",
		},
		{
			name:       "configuration anomaly",
			err:        fmt.Errorf("resolve bootstrap administrator: %w", bootstrapadmin.ErrConfiguration),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "IAM_BOOTSTRAP_ADMIN_CONFIGURATION_INVALID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/personnel-changes", nil)
			recorder := httptest.NewRecorder()
			writePersonnelChangeError(recorder, request, tc.err)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			envelope := decodeErrorEnvelope(t, recorder)
			if envelope.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", envelope.Code, tc.wantCode)
			}
		})
	}
}

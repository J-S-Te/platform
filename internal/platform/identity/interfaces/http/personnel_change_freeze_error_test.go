package identityhttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
)

func TestPersonnelChangeLegacyReleasedFreezeHasActionableError(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/personnel-changes/legacy/transition", nil)
	recorder := httptest.NewRecorder()
	writePersonnelChangeError(recorder, request, fmt.Errorf("execution guard: %w", application.ErrHandoverFreezeReleased))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d", recorder.Code)
	}
	envelope := decodeErrorEnvelope(t, recorder)
	if envelope.Code != "IAM_HANDOVER_FREEZE_RELEASED" || !strings.Contains(recorder.Body.String(), "重新核验") {
		t.Fatalf("missing controlled-recovery guidance: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "execution guard") {
		t.Fatal("internal error chain leaked")
	}
}

package identityhttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	authzapplication "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
)

func TestIdentityDelegationDenialsReturnForbidden(t *testing.T) {
	for _, personnel := range []bool{false, true} {
		t.Run(fmt.Sprintf("personnel=%t", personnel), func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/memberships", nil)
			err := fmt.Errorf("delegation rejected: %w", authzapplication.ErrForbidden)
			if personnel {
				writePersonnelChangeError(response, request, err)
			} else {
				(&ManagementHandler{}).writeError(response, request, err)
			}
			if response.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want 403; body=%s", response.Code, response.Body.String())
			}
		})
	}
}

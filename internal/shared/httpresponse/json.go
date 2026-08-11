// Package httpresponse writes the platform's standard JSON response envelopes.
package httpresponse

import (
	"encoding/json"
	"net/http"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

// Envelope is the standard successful API response shape.
type Envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Data      any    `json:"data"`
}

// ErrorEnvelope is the standard failed API response shape.
type ErrorEnvelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Details   any    `json:"details,omitempty"`
}

// WriteSuccess writes a successful response using the shared envelope contract.
func WriteSuccess(w http.ResponseWriter, r *http.Request, status int, message string, data any) {
	writeJSON(w, status, Envelope{
		Code:      "OK",
		Message:   message,
		RequestID: requestctx.RequestID(r.Context()),
		Data:      data,
	})
}

// WriteError writes a stable error response. The caller should log non-client-safe causes before
// invoking this function.
func WriteError(w http.ResponseWriter, r *http.Request, status int, apiError httperror.Error) {
	writeJSON(w, status, ErrorEnvelope{
		Code:      apiError.Code,
		Message:   apiError.Message,
		RequestID: requestctx.RequestID(r.Context()),
		Details:   apiError.Details,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

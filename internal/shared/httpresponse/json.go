// Package httpresponse 输出基础平台统一的 JSON 响应信封。
package httpresponse

import (
	"encoding/json"
	"net/http"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

// Envelope 是成功 API 响应的标准结构。
type Envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Data      any    `json:"data"`
}

// ErrorEnvelope 是失败 API 响应的标准结构。
type ErrorEnvelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Details   any    `json:"details,omitempty"`
}

// WriteSuccess 按统一响应信封契约输出成功响应。
func WriteSuccess(w http.ResponseWriter, r *http.Request, status int, message string, data any) {
	writeJSON(w, status, Envelope{
		Code:      "OK",
		Message:   message,
		RequestID: requestctx.RequestID(r.Context()),
		Data:      data,
	})
}

// WriteError 输出稳定的错误响应。调用方应在调用前记录不适合返回给客户端的底层原因。
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

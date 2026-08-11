// Package http 暴露仅供机器客户端调用的外部身份管理协议。浏览器会话不能构造 appctx.Principal，
// 防重放所需的幂等键、时间戳和 nonce 也必须来自受信任集成方。
package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const maxRequestBytes = 16 << 10

type externalIdentityService interface {
	Provision(context.Context, appctx.Principal, application.RequestProof, application.ProvisionInput) (application.ProvisionResult, error)
	AssignPortalRole(context.Context, appctx.Principal, application.RequestProof, application.RoleInput) (domain.RoleResult, error)
	RevokePortalRole(context.Context, appctx.Principal, application.RequestProof, application.RoleInput) (domain.RoleResult, error)
}

type Handler struct {
	service externalIdentityService
	logger  *slog.Logger
}

func NewHandler(service externalIdentityService, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("external identity handler dependencies must not be nil")
	}
	return &Handler{service: service, logger: logger}, nil
}

type provisionRequest struct {
	DisplayName string `json:"display_name"`
	Mobile      string `json:"mobile,omitempty"`
	Email       string `json:"email,omitempty"`
}

type roleRequest struct {
	PlatformUserID  string `json:"platform_user_id"`
	ApplicationCode string `json:"application_code"`
	RoleCode        string `json:"role_code"`
}

func (handler *Handler) Provision(writer http.ResponseWriter, request *http.Request) {
	principal, proof, ok := requestContext(writer, request)
	if !ok {
		return
	}
	var payload provisionRequest
	if !decode(writer, request, &payload) {
		return
	}
	result, err := handler.service.Provision(request.Context(), principal, proof, application.ProvisionInput{DisplayName: payload.DisplayName, Mobile: payload.Mobile, Email: payload.Email})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "外部客户身份已预置", result)
}

func (handler *Handler) AssignRole(writer http.ResponseWriter, request *http.Request) {
	handler.changeRole(writer, request, false)
}

func (handler *Handler) RevokeRole(writer http.ResponseWriter, request *http.Request) {
	handler.changeRole(writer, request, true)
}

func (handler *Handler) changeRole(writer http.ResponseWriter, request *http.Request, revoke bool) {
	principal, proof, ok := requestContext(writer, request)
	if !ok {
		return
	}
	var payload roleRequest
	if !decode(writer, request, &payload) {
		return
	}
	input := application.RoleInput{PlatformUserID: payload.PlatformUserID, ApplicationCode: payload.ApplicationCode, RoleCode: payload.RoleCode}
	var result domain.RoleResult
	var err error
	if revoke {
		result, err = handler.service.RevokePortalRole(request.Context(), principal, proof, input)
	} else {
		result, err = handler.service.AssignPortalRole(request.Context(), principal, proof, input)
	}
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "应用角色状态已收敛", result)
}

func requestContext(writer http.ResponseWriter, request *http.Request) (appctx.Principal, application.RequestProof, bool) {
	// OAuth 客户端身份由前置中间件验证；这里仅从上下文读取，绝不接受请求体声明 client 或 tenant。
	principal, ok := appctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return appctx.Principal{}, application.RequestProof{}, false
	}
	timestamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(request.Header.Get("X-Integration-Timestamp")))
	if err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return appctx.Principal{}, application.RequestProof{}, false
	}
	proof := application.RequestProof{IdempotencyKey: request.Header.Get("Idempotency-Key"), Timestamp: timestamp, Nonce: request.Header.Get("X-Integration-Nonce")}
	return principal, proof, true
}

func decode(writer http.ResponseWriter, request *http.Request, target any) bool {
	// 限制请求体并拒绝未知/多余 JSON，减少协议漂移和超大载荷占用内存的风险。
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	return true
}

func (handler *Handler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrValidation):
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrConflict), errors.Is(err, application.ErrReplay):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrUnavailable):
		httpresponse.WriteError(writer, request, http.StatusServiceUnavailable, httperror.Internal)
	default:
		handler.logger.Error("external identity request failed", "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

package identityhttp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	identityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

const (
	bootstrapTokenHeader     = "X-Bootstrap-Token"
	maxBootstrapRequestBytes = 8 * 1024
)

// bootstrapInitializer is the use-case dependency exposed to the HTTP bootstrap boundary.
type bootstrapInitializer interface {
	InitializeFirstSuperAdmin(context.Context, identityapplication.BootstrapInput) (identityapplication.BootstrapResult, error)
}

// BootstrapHandler exposes the tightly controlled, one-time first-super-administrator endpoint.
// It stores only a SHA-256 digest of the configured bootstrap token.
type BootstrapHandler struct {
	service       bootstrapInitializer
	logger        *slog.Logger
	enabled       bool
	tokenDigest   [sha256.Size]byte
	auditRecorder lifecycleAuditRecorder
	auditConfig   config.AuditConfig
}

type bootstrapRequest struct {
	DisplayName string `json:"display_name"`
	AccountName string `json:"account_name"`
	Password    string `json:"password"`
}

type bootstrapReferenceResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Code string `json:"code,omitempty"`
}

type bootstrapResponse struct {
	BootstrapID   string                     `json:"bootstrap_id"`
	Tenant        bootstrapReferenceResponse `json:"tenant"`
	User          bootstrapReferenceResponse `json:"user"`
	Account       bootstrapReferenceResponse `json:"account"`
	Role          bootstrapReferenceResponse `json:"role"`
	InitializedAt time.Time                  `json:"initialized_at"`
}

// NewBootstrapHandler constructs the operator-only bootstrap transport. An empty configuration
// token intentionally disables the endpoint, allowing applications to keep the route installed
// without exposing setup functionality after initialization.
func NewBootstrapHandler(
	service bootstrapInitializer,
	logger *slog.Logger,
	bootstrapToken string,
	auditRecorder lifecycleAuditRecorder,
	auditConfig config.AuditConfig,
) (*BootstrapHandler, error) {
	if service == nil || logger == nil || auditRecorder == nil {
		return nil, errors.New("identity bootstrap handler dependencies must not be nil")
	}
	if strings.TrimSpace(auditConfig.ApplicationCode) == "" || strings.TrimSpace(auditConfig.EnvironmentCode) == "" {
		return nil, errors.New("identity bootstrap audit configuration must not be blank")
	}

	configuredToken := strings.TrimSpace(bootstrapToken)
	handler := &BootstrapHandler{
		service:       service,
		logger:        logger,
		enabled:       configuredToken != "",
		auditRecorder: auditRecorder,
		auditConfig:   auditConfig,
	}
	if handler.enabled {
		handler.tokenDigest = sha256.Sum256([]byte(configuredToken))
	}
	return handler, nil
}

// InitializeFirstSuperAdmin handles the only unauthenticated IAM mutation. The configured token
// is compared in constant time and is never persisted, logged, or returned to the caller.
func (handler *BootstrapHandler) InitializeFirstSuperAdmin(writer http.ResponseWriter, request *http.Request) {
	if !handler.enabled {
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
		return
	}
	providedDigest := sha256.Sum256([]byte(request.Header.Get(bootstrapTokenHeader)))
	if subtle.ConstantTimeCompare(providedDigest[:], handler.tokenDigest[:]) != 1 {
		httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
		return
	}

	payload, err := decodeBootstrapRequest(writer, request)
	if err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	result, err := handler.service.InitializeFirstSuperAdmin(request.Context(), identityapplication.BootstrapInput{
		DisplayName: payload.DisplayName,
		AccountName: payload.AccountName,
		Password:    payload.Password,
	})
	if err != nil {
		handler.writeBootstrapError(writer, request, err)
		return
	}

	handler.recordBootstrapEvent(request, result)
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "初始化成功", toBootstrapResponse(result))
}

func (handler *BootstrapHandler) writeBootstrapError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, identityapplication.ErrValidation):
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, identityapplication.ErrBootstrapAlreadyInitialized), errors.Is(err, identityapplication.ErrConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, identityapplication.ErrBootstrapUnavailable):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	default:
		handler.logger.Error("first super administrator bootstrap failed", "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func (handler *BootstrapHandler) recordBootstrapEvent(request *http.Request, result identityapplication.BootstrapResult) {
	input := auditapplication.EventInput{
		EventID:         result.BootstrapID,
		ApplicationCode: handler.auditConfig.ApplicationCode,
		EnvironmentCode: handler.auditConfig.EnvironmentCode,
		ActorType:       "SYSTEM",
		ActorName:       "bootstrap",
		OccurredAt:      result.InitializedAt.UTC(),
		Action:          "platform:iam.bootstrap.first-super-admin",
		ResourceType:    "iam_bootstrap_state",
		ResourceID:      result.BootstrapID,
		ResourceName:    "first-super-administrator",
		Result:          "SUCCESS",
		RiskLevel:       "HIGH",
		Classification:  "INTERNAL",
		Summary:         "首个超级管理员已完成受控初始化",
		Metadata:        map[string]any{"method": request.Method, "path": request.URL.Path},
		SourceIP:        remoteIP(request).String(),
		UserAgent:       request.UserAgent(),
		EventCategory:   "SECURITY",
		EventType:       "platform:iam.bootstrap.first-super-admin",
	}
	if _, err := handler.auditRecorder.Ingest(request.Context(), result.TenantID, input); err != nil {
		handler.logger.Error("record first super administrator bootstrap audit event", "error", err, "tenant_id", result.TenantID)
	}
}

func decodeBootstrapRequest(writer http.ResponseWriter, request *http.Request) (bootstrapRequest, error) {
	defer request.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxBootstrapRequestBytes))
	decoder.DisallowUnknownFields()

	var payload bootstrapRequest
	if err := decoder.Decode(&payload); err != nil {
		return bootstrapRequest{}, err
	}
	if err := ensureSingleJSONValue(decoder); err != nil {
		return bootstrapRequest{}, err
	}
	if strings.TrimSpace(payload.DisplayName) == "" || strings.TrimSpace(payload.AccountName) == "" || payload.Password == "" {
		return bootstrapRequest{}, fmt.Errorf("bootstrap request does not meet contract")
	}
	return payload, nil
}

func toBootstrapResponse(result identityapplication.BootstrapResult) bootstrapResponse {
	return bootstrapResponse{
		BootstrapID:   result.BootstrapID,
		Tenant:        bootstrapReferenceResponse{ID: result.TenantID, Code: result.TenantCode},
		User:          bootstrapReferenceResponse{ID: result.UserID, Name: result.DisplayName},
		Account:       bootstrapReferenceResponse{ID: result.AccountID, Name: result.AccountName},
		Role:          bootstrapReferenceResponse{ID: result.RoleID, Name: result.RoleName, Code: result.RoleCode},
		InitializedAt: result.InitializedAt.UTC(),
	}
}

var _ http.HandlerFunc = (*BootstrapHandler)(nil).InitializeFirstSuperAdmin

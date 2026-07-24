package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/mfastepup/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// MFAStepUpGrantHeader carries the one-time grant issued after a successful MFA step-up.
const MFAStepUpGrantHeader = "X-MFA-Step-Up-Grant"

// MFAStepUpGrantConsumer is deliberately narrow so transport does not depend on persistence.
type MFAStepUpGrantConsumer interface {
	ConsumeGrant(ctx context.Context, tenantID, accountID, sessionID, grant string) error
}

// RequireMFAStepUp consumes exactly one session-bound grant before the protected handler runs.
// Authentication must execute earlier in the route chain. It never stores the header value in a
// context or writes it to logs, so downstream handlers cannot accidentally reuse or disclose it.
func RequireMFAStepUp(consumer MFAStepUpGrantConsumer) gin.HandlerFunc {
	return func(context *gin.Context) {
		if consumer == nil {
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusInternalServerError, httperror.Internal)
			return
		}
		principal, ok := authctx.PrincipalFromContext(context.Request.Context())
		if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.Account.ID) == "" || strings.TrimSpace(principal.SessionID) == "" {
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			return
		}
		values := context.Request.Header.Values(MFAStepUpGrantHeader)
		if len(values) != 1 || strings.TrimSpace(values[0]) == "" || len(values[0]) > 512 {
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusForbidden, mfaStepUpRequiredError())
			return
		}
		if err := consumer.ConsumeGrant(context.Request.Context(), principal.Tenant.ID, principal.Account.ID, principal.SessionID, strings.TrimSpace(values[0])); err != nil {
			context.Abort()
			writeMFAStepUpConsumeError(context.Writer, context.Request, err)
			return
		}
		context.Next()
	}
}

func mfaStepUpRequiredError() httperror.Error {
	return httperror.New("AUTH_MFA_STEP_UP_REQUIRED", "该高风险操作需要当前会话完成 MFA 二次验证", nil)
}

func writeMFAStepUpConsumeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrGrantExpired):
		httpresponse.WriteError(writer, request, http.StatusGone, httperror.New("AUTH_MFA_STEP_UP_EXPIRED", "MFA 二次验证授权已过期，请重新验证", nil))
	case errors.Is(err, domain.ErrInvalidInput), errors.Is(err, domain.ErrGrantNotFound), errors.Is(err, domain.ErrGrantBinding), errors.Is(err, domain.ErrGrantConsumed), errors.Is(err, domain.ErrGrantNotIssued):
		httpresponse.WriteError(writer, request, http.StatusForbidden, mfaStepUpRequiredError())
	default:
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

// Package http exposes the machine-only owner directory endpoint.
package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

type directoryService interface {
	List(context.Context, appctx.Principal, application.Query) (domain.Page, error)
}

type Handler struct {
	service directoryService
	logger  *slog.Logger
}

func NewHandler(service directoryService, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("owner directory handler dependencies must not be nil")
	}
	return &Handler{service: service, logger: logger}, nil
}

func (handler *Handler) List(writer http.ResponseWriter, request *http.Request) {
	principal, ok := appctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	page, pageErr := optionalPositiveInt(request.URL.Query().Get("page"))
	pageSize, pageSizeErr := optionalPositiveInt(request.URL.Query().Get("page_size"))
	if pageErr != nil || pageSizeErr != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	result, err := handler.service.List(request.Context(), principal, application.Query{
		Keyword: request.URL.Query().Get("keyword"), UserID: request.URL.Query().Get("user_id"),
		RoleCodes: request.URL.Query()["role_code"], Page: page, PageSize: pageSize,
	})
	if err != nil {
		if errors.Is(err, application.ErrValidation) {
			httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
			return
		}
		handler.logger.Error("owner directory request failed", "error", err)
		httpresponse.WriteError(writer, request, http.StatusServiceUnavailable, httperror.Internal)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "负责人目录查询成功", result)
}

func optionalPositiveInt(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, application.ErrValidation
	}
	return parsed, nil
}

// Package application validates machine-principal directory queries.
package application

import (
	"context"
	"errors"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
)

const (
	defaultPageSize = 20
	maximumPageSize = 50
)

var (
	ErrValidation  = errors.New("owner directory query is invalid")
	ErrUnavailable = errors.New("owner directory is unavailable")
)

// Query contains optional exact-user or display search filters.
type Query struct {
	Keyword   string
	UserID    string
	RoleCodes []string
	Page      int
	PageSize  int
}

// Repository reads only active, application-authorized internal users.
type Repository interface {
	List(context.Context, string, string, string, Query) (domain.Page, error)
}

type Service struct{ repository Repository }

func NewService(repository Repository) (*Service, error) {
	if repository == nil {
		return nil, errors.New("owner directory repository must not be nil")
	}
	return &Service{repository: repository}, nil
}

// List derives tenant and application scope exclusively from the authenticated machine token.
func (service *Service) List(ctx context.Context, principal appctx.Principal, query Query) (domain.Page, error) {
	if !principal.Valid() {
		return domain.Page{}, ErrValidation
	}
	query.Keyword = strings.TrimSpace(query.Keyword)
	query.UserID = strings.TrimSpace(query.UserID)
	seenRoles := make(map[string]struct{}, len(query.RoleCodes))
	roles := make([]string, 0, len(query.RoleCodes))
	for _, raw := range query.RoleCodes {
		role := strings.TrimSpace(raw)
		if role == "" || len(role) > 128 || len(roles) >= 20 {
			return domain.Page{}, ErrValidation
		}
		if _, exists := seenRoles[role]; !exists {
			seenRoles[role] = struct{}{}
			roles = append(roles, role)
		}
	}
	query.RoleCodes = roles
	if query.Keyword != "" && query.UserID != "" {
		return domain.Page{}, ErrValidation
	}
	if len([]rune(query.Keyword)) > 100 || len(query.UserID) > 128 || query.Page < 0 || query.PageSize < 0 || query.PageSize > maximumPageSize {
		return domain.Page{}, ErrValidation
	}
	if query.Page == 0 {
		query.Page = 1
	}
	if query.PageSize == 0 {
		query.PageSize = defaultPageSize
	}
	page, err := service.repository.List(ctx, principal.TenantID, principal.ApplicationID, principal.EnvironmentID, query)
	if err != nil {
		return domain.Page{}, errors.Join(ErrUnavailable, err)
	}
	return page, nil
}

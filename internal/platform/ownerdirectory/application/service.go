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
	// maximumRoleFilters 限制一次查询的角色与来源过滤数量，目录查询是机器接口，参数不应无限增长。
	maximumRoleFilters = 20
)

var (
	ErrValidation  = errors.New("owner directory query is invalid")
	ErrUnavailable = errors.New("owner directory is unavailable")
)

// roleGrantOrigins 是 authz_role_binding.grant_origin 允许被过滤的取值：
// TEMPLATE 由岗位授权模板生成，MANUAL 由管理员直接授予，SYSTEM 由入职/身份同步写入。
// 未知取值一律拒绝，避免拼写错误被当成"没有过滤"而放大候选人范围。
var roleGrantOrigins = map[string]struct{}{"TEMPLATE": {}, "MANUAL": {}, "SYSTEM": {}}

// Query contains optional exact-user or display search filters.
type Query struct {
	Keyword  string
	UserID   string
	// RoleCodes 只返回在这些应用角色上确有有效授权的用户。
	RoleCodes []string
	// RoleOrigins 进一步限定角色授权的来源（grant_origin）；必须与 RoleCodes 同时使用，
	// 否则"限定来源"会退化成对整个授权集合的模糊约束。
	RoleOrigins []string
	Page        int
	PageSize    int
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
		if role == "" || len(role) > 128 || len(roles) >= maximumRoleFilters {
			return domain.Page{}, ErrValidation
		}
		if _, exists := seenRoles[role]; !exists {
			seenRoles[role] = struct{}{}
			roles = append(roles, role)
		}
	}
	query.RoleCodes = roles
	seenOrigins := make(map[string]struct{}, len(query.RoleOrigins))
	origins := make([]string, 0, len(query.RoleOrigins))
	for _, raw := range query.RoleOrigins {
		origin := strings.ToUpper(strings.TrimSpace(raw))
		if _, allowed := roleGrantOrigins[origin]; !allowed {
			return domain.Page{}, ErrValidation
		}
		if _, exists := seenOrigins[origin]; !exists {
			seenOrigins[origin] = struct{}{}
			origins = append(origins, origin)
		}
	}
	if len(origins) > 0 && len(roles) == 0 {
		return domain.Page{}, ErrValidation
	}
	query.RoleOrigins = origins
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

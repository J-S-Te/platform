package identityhttp

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/managementscope"
	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"gorm.io/gorm"
)

// AuthorizationOverviewHandler 返回单个人员的紧凑只读 IAM 总览。
type AuthorizationOverviewHandler struct {
	logger          *slog.Logger
	scopeAuthorizer managementscope.Authorizer
	store           authorizationOverviewStore
}

// NewAuthorizationOverviewHandler 构造总览处理器；查询始终以会话主体的租户为边界，
// 目标用户必须落在调用者对该权限码的有效管理范围内。
func NewAuthorizationOverviewHandler(db *gorm.DB, scopeAuthorizer managementscope.Authorizer, logger *slog.Logger) (*AuthorizationOverviewHandler, error) {
	if db == nil || scopeAuthorizer == nil || logger == nil {
		return nil, errors.New("authorization overview handler dependencies must not be nil")
	}
	return &AuthorizationOverviewHandler{
		logger:          logger,
		scopeAuthorizer: scopeAuthorizer,
		store:           &gormAuthorizationOverviewStore{database: db},
	}, nil
}

// newAuthorizationOverviewHandlerWithStore 供处理器测试注入确定性只读实现。
func newAuthorizationOverviewHandlerWithStore(store authorizationOverviewStore, scopeAuthorizer managementscope.Authorizer, logger *slog.Logger) *AuthorizationOverviewHandler {
	return &AuthorizationOverviewHandler{logger: logger, scopeAuthorizer: scopeAuthorizer, store: store}
}

// Get 返回指定用户的账号、任职、角色绑定、在途变更、交接项与 Keycloak 同步状态
// 的只读聚合；目标用户不存在时返回 404。
func (handler *AuthorizationOverviewHandler) Get(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	userID := strings.TrimSpace(request.PathValue("user_id"))
	if userID == "" {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	scope, ok := handler.requireManagementScope(writer, request, principal, "platform:user:read")
	if !ok {
		return
	}
	if !scope.Unrestricted {
		// 受限范围按目标用户当前 ACTIVE 任职的组织归属判定。范围外与用户不存在同样返回
		// 403，不能用 404 泄露范围外用户是否存在。
		orgUnitIDs, err := handler.store.ActiveMembershipOrgUnitIDs(request.Context(), principal.Tenant.ID, userID)
		if err != nil {
			handler.internalError(writer, request, "load authorization overview membership scope", err)
			return
		}
		if !overviewScopeAllowsMemberships(scope, orgUnitIDs) {
			handler.forbidden(writer, request)
			return
		}
	}
	// 部署初始化超管与 ListUsers 保持同一隐藏口径：即使落在允许范围内也不输出其总览。
	bootstrapIdentity, bootstrapInitialized, err := handler.store.BootstrapAdministrator(request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.logger.Error("resolve bootstrap administrator for authorization overview", "error", err, "path", request.URL.Path)
		if errors.Is(err, bootstrapadmin.ErrConfiguration) {
			httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.BootstrapAdminConfiguration)
			return
		}
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
		return
	}
	if bootstrapInitialized && bootstrapIdentity.ProtectsUser(userID) {
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
		return
	}
	user, err := handler.store.User(request.Context(), principal.Tenant.ID, userID)
	if err != nil {
		if errors.Is(err, application.ErrNotFound) {
			httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
			return
		}
		handler.internalError(writer, request, "read authorization overview user", err)
		return
	}
	sections, err := handler.store.Sections(request.Context(), principal.Tenant.ID, userID)
	if err != nil {
		handler.internalError(writer, request, "read authorization overview sections", err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", map[string]any{
		"user":            user,
		"accounts":        emptyIfNil(sections.Accounts),
		"memberships":     emptyIfNil(sections.Memberships),
		"role_bindings":   emptyIfNil(sections.RoleBindings),
		"pending_changes": emptyIfNil(sections.PendingChanges),
		"handover":        emptyIfNil(sections.Handover),
		"keycloak_sync":   emptyIfNil(sections.KeycloakSync),
	})
}

// requireManagementScope 解析调用者在给定权限码上的有效管理范围。解析失败返回 500，
// 空范围表示没有任何绑定隐含该权限，按 403 处理。
func (handler *AuthorizationOverviewHandler) requireManagementScope(writer http.ResponseWriter, request *http.Request, principal authctx.Principal, permissionCode string) (managementscope.Scope, bool) {
	scope, err := handler.scopeAuthorizer.Resolve(request.Context(), managementscope.Subject{
		TenantID:  principal.Tenant.ID,
		UserID:    principal.User.ID,
		AccountID: principal.Account.ID,
	}, permissionCode)
	if err != nil {
		handler.internalError(writer, request, "resolve authorization overview management scope", err)
		return managementscope.Scope{}, false
	}
	if scope.Empty() {
		handler.forbidden(writer, request)
		return managementscope.Scope{}, false
	}
	return scope, true
}

func (handler *AuthorizationOverviewHandler) internalError(writer http.ResponseWriter, request *http.Request, message string, err error) {
	handler.logger.Error(message, "error", err, "path", request.URL.Path)
	httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
}

func (handler *AuthorizationOverviewHandler) forbidden(writer http.ResponseWriter, request *http.Request) {
	httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
}

// overviewScopeAllowsMemberships 判断目标用户是否至少有一条 ACTIVE 任职落在允许范围内。
// 没有任何任职时失败关闭。
func overviewScopeAllowsMemberships(scope managementscope.Scope, orgUnitIDs []string) bool {
	if scope.Unrestricted {
		return true
	}
	for _, orgUnitID := range orgUnitIDs {
		if scope.Allows(strings.TrimSpace(orgUnitID), "") {
			return true
		}
	}
	return false
}

func emptyIfNil(rows []map[string]any) []map[string]any {
	if rows == nil {
		return []map[string]any{}
	}
	return rows
}

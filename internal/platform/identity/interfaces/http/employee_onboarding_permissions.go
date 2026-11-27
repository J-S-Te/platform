package identityhttp

import (
	"net/http"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

type employeeOnboardingPermissionsResponse struct {
	CanCreateAccount bool     `json:"can_create_account"`
	AllOrganizations bool     `json:"all_organizations"`
	OrganizationIDs  []string `json:"organization_ids"`
}

// EmployeeOnboardingPermissions 返回当前会话可用于入职的账号创建权限和任职组织范围。
// 参数：writer 用于返回预检结果，request 提供服务端认证的当前主体。
// 返回值：无。范围不可解析时返回错误，预检结果仅用于表单提示。
func (handler *ManagementHandler) EmployeeOnboardingPermissions(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}
	scope, ok := handler.resolveManagementScope(writer, request, principal, "platform:membership:create")
	if !ok {
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "已读取入职授权", employeeOnboardingPermissionsResponse{
		CanCreateAccount: principalHasPermission(principal, "platform:account:create"),
		AllOrganizations: scope.Unrestricted,
		OrganizationIDs:  append([]string{}, scope.OrgUnitIDs...),
	})
}

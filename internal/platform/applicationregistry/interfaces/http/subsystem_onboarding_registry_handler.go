// subsystem_onboarding_registry_handler.go：子系统目录登记（registry）——应用目录/环境/登录目标登记与门户应用列表只读查询。
package http

import (
	stdhttp "net/http"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// RegisterSubsystemDirectory 处理 POST /api/v1/subsystem-directory。
//
// 这是 V2 登记入口：只登记应用目录、环境与登录目标数据。此处不创建任何平台
// OAuth Client、Keycloak Client、服务凭据、部署状态或初始角色授权。迁移期间
// 既有的一体化 /subsystem-onboarding 调用方继续受支持，但新的 Keycloak 集成
// 必须先走本端点，再使用专用 Keycloak 集成 API。
func (handler *SubsystemOnboardingHandler) RegisterSubsystemDirectory(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemDirectoryRegistrationRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	effectiveIssuerAlias := handler.effectiveIssuerAlias(payload.IssuerAlias)
	if _, err := handler.issuerForAlias(effectiveIssuerAlias); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	result, err := handler.service.RegisterSubsystemDirectory(request.Context(), application.SubsystemDirectoryRegistrationInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID,
		ApplicationCode: payload.ApplicationCode, ApplicationName: payload.ApplicationName,
		Description: payload.Description, Environment: payload.Environment,
		PublicBaseURL: payload.PublicBaseURL, UpstreamURL: payload.UpstreamURL,
		PathPrefix: payload.PathPrefix, IssuerAlias: optionalIssuerAlias(effectiveIssuerAlias),
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusCreated, "应用目录与登录目标已登记；请继续在 Keycloak 认证接入中同步 Client 并配置运行时", subsystemDirectoryRegistrationResponse{
		Application: applicationToResponse(result.Application),
		Environment: environmentToResponse(result.Environment),
		LoginTarget: loginTargetToResponse(result.LoginTarget),
		NextAction:  "继续执行 Keycloak Client 同步；完成后再应用子系统运行时配置。",
	})
}

// ListPortalApplications 处理 GET /api/v1/portal/applications。所有已认证用户可读
// 当前租户的活跃目录；该只读端点不要求管理权限。
func (handler *SubsystemOnboardingHandler) ListPortalApplications(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	// 本响应是用户级授权投影。若无显式缓存控制，账号切换后浏览器或反向代理可能
	// 继续返回上一个 bp_session 用户的模块列表，即使认证本身已正确解析新 Cookie。
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Add("Vary", "Cookie")
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	items, err := handler.service.ListPortalApplications(request.Context(), principal.Tenant.ID, principal.User.ID, request.URL.Query().Get("environment"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	responses := make([]portalApplicationResponse, 0, len(items))
	for _, item := range items {
		responses = append(responses, portalApplicationResponse{
			ApplicationID: item.ApplicationID, Code: item.Code, Name: item.Name, Description: item.Description,
			EnvironmentID: item.EnvironmentID, Environment: item.Environment, PathPrefix: item.PathPrefix,
			TargetCode: item.TargetCode, TargetURI: item.TargetURI, PublicURL: item.PublicURL,
			Allowed: item.Allowed, ProjectionStatus: item.Projection.Status,
			ProjectionReady: item.Projection.Ready, ProjectionNextAction: item.Projection.NextAction,
		})
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "门户应用目录查询成功", responses)
}

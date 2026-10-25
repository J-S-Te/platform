// subsystem_onboarding_diagnostics_handler.go：子系统诊断（diagnostics）——健康看板、部署状态、Keycloak 集成与同步状态只读端点。
package http

import (
	"context"
	"errors"
	stdhttp "net/http"
	"strings"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// SubsystemHealthEntry 描述单个子系统环境的集成健康度。
type SubsystemHealthEntry struct {
	ApplicationCode   string `json:"application_code"`
	ApplicationName   string `json:"application_name"`
	Environment       string `json:"environment"`
	DirectoryOK       bool   `json:"directory_ok"`
	CredentialsOK     bool   `json:"credentials_ok"`
	RuntimeOK         bool   `json:"runtime_ok"`
	KeycloakOK        bool   `json:"keycloak_ok"`
	DirectoryStatus   string `json:"directory_status"`
	CredentialsStatus string `json:"credentials_status"`
	RuntimeStatus     string `json:"runtime_status"`
	KeycloakStatus    string `json:"keycloak_status"`
	ProjectionStatus  string `json:"projection_status,omitempty"`
	Status            string `json:"status"`
	NextAction        string `json:"next_action,omitempty"`
}

// GetSubsystemStatus 处理 GET /api/v1/subsystem-status。仅暴露持久化的生命周期
// 元数据；部署命令、文件系统路径与凭据绝不经过该 API。
func (handler *SubsystemOnboardingHandler) GetSubsystemStatus(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	if handler.deploymentState == nil {
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	applicationCode := strings.TrimSpace(request.URL.Query().Get("application_code"))
	environment := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("environment")))
	if applicationCode == "" || environment == "" {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	state, err := handler.deploymentState.GetSubsystemDeploymentState(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if err != nil {
		// 手工登记的应用/环境先于（或有意绕过）受管部署记录存在，但仍是合法的
		// 控制面资源，因此状态读取不能把整个运行时卡片变成 404。先解析登记边界：
		// 未知应用保持真实 404，已知应用返回明确的安全状态。
		if !errors.Is(err, application.ErrNotFound) {
			handler.writeError(writer, request, err)
			return
		}
		applicationID, environmentID, resolveErr := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if resolveErr != nil {
			handler.writeError(writer, request, resolveErr)
			return
		}
		state = application.SubsystemDeploymentState{
			TenantID: principal.Tenant.ID, ApplicationID: applicationID, EnvironmentID: environmentID,
			ApplicationCode: applicationCode, Environment: environment,
			Status: application.SubsystemDeploymentStatusUnmanaged, Operation: "NONE",
		}
	}
	state = handler.recoverStaleSubsystemDeployment(request.Context(), state)
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统部署状态查询成功", subsystemDeploymentStateResponse{
		ApplicationCode:         state.ApplicationCode,
		Environment:             state.Environment,
		Status:                  state.Status,
		Operation:               state.Operation,
		Generation:              state.Generation,
		AttemptCount:            state.AttemptCount,
		DesiredManifestChecksum: state.DesiredManifestChecksum,
		AppliedManifestChecksum: state.AppliedManifestChecksum,
		ManifestDriftStatus:     state.ManifestDriftStatus,
		ManifestLastAppliedAt:   state.ManifestLastAppliedAt,
		ManifestLastVerifiedAt:  state.ManifestLastVerifiedAt,
		LastErrorCode:           state.LastErrorCode,
		LastError:               state.LastError,
		NextAction:              subsystemDeploymentStateNextAction(state),
		StartedAt:               state.StartedAt,
		CompletedAt:             state.CompletedAt,
		UpdatedAt:               state.UpdatedAt,
	})
}

// GetKeycloakIntegrationStatus 处理 GET /api/v1/keycloak-integration/status。
// 与部署状态分开：响应是所选提供方、其非敏感 Client 映射和服务端核验切换门禁的
// 持久视图。运行时凭据与任意环境元数据被刻意排除。
func (handler *SubsystemOnboardingHandler) GetKeycloakIntegrationStatus(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	applicationCode := strings.TrimSpace(request.URL.Query().Get("application_code"))
	environment := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("environment")))
	if applicationCode == "" || environment == "" {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	applicationID, environmentID, err := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	issuerAlias := handler.effectiveIssuerAlias("")
	if resolver, supported := handler.service.(subsystemEnvironmentIssuerResolver); supported {
		if persisted, resolveErr := resolver.ResolveEnvironmentIssuerAlias(request.Context(), principal.Tenant.ID, applicationCode, environment); resolveErr == nil {
			issuerAlias = handler.effectiveIssuerAlias(persisted)
		}
	}
	mapping := KeycloakClientMapping{Status: "NOT_CONFIGURED"}
	if inspector, supported := handler.keycloakMappings.(keycloakClientMappingInspector); supported {
		loaded, loadErr := inspector.GetKeycloakClientMapping(request.Context(), principal.Tenant.ID, applicationID, environmentID)
		if loadErr != nil {
			handler.logger.Warn("load Keycloak Client mapping status failed", "application_code", applicationCode, "environment", environment, "error", loadErr)
			mapping.Status = "UNKNOWN"
		} else {
			mapping = loaded
		}
	}
	if mapping.Realm == "" {
		mapping.Realm = handler.keycloakRealm
	}
	readiness := handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, applicationCode, environment)
	claimsState := "NOT_CONFIGURED"
	if mapping.Exists && strings.EqualFold(mapping.Status, "SYNCED") {
		claimsState = "MAPPERS_SYNCED"
	} else if mapping.Status == "UNKNOWN" {
		claimsState = "UNKNOWN"
	}
	cutover := KeycloakCutoverLifecycle{Status: "NOT_CONFIGURED"}
	var timeline []KeycloakCutoverTimelineEvent
	if handler.keycloakCutover != nil {
		loaded, lifecycleErr := handler.keycloakCutover.GetKeycloakCutoverLifecycle(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if lifecycleErr != nil {
			handler.logger.Warn("load Keycloak cutover lifecycle failed", "application_code", applicationCode, "environment", environment, "error", lifecycleErr)
			cutover.Status = "UNKNOWN"
		} else {
			cutover = loaded
			timeline, lifecycleErr = handler.keycloakCutover.ListKeycloakCutoverTimeline(request.Context(), principal.Tenant.ID, applicationCode, environment, 50)
			if lifecycleErr != nil {
				handler.logger.Warn("load Keycloak cutover timeline failed", "application_code", applicationCode, "environment", environment, "error", lifecycleErr)
			}
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 认证接入状态查询成功", map[string]any{
		"application_code": applicationCode,
		"environment":      environment,
		"provider":         issuerAlias,
		"realm":            mapping.Realm,
		"client_id":        mapping.ClientID,
		"client_state":     mapping.Status,
		"claims_state":     claimsState,
		"last_synced_at":   mapping.LastSyncedAt,
		"switch_ready":     readiness.SwitchReady,
		"switch_gates":     readiness.Gates,
		"next_action":      readiness.NextAction,
		"cutover":          cutover,
		"timeline":         timeline,
	})
}

// KeycloakSyncStatusResponse 是单个子系统环境的聚焦同步状态，包含供运维看板
// 使用的漂移检测结果。
type KeycloakSyncStatusResponse struct {
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment"`
	ClientID        string `json:"client_id,omitempty"`
	ClientState     string `json:"client_state"`
	Realm           string `json:"realm"`
	LastSyncedAt    any    `json:"last_synced_at,omitempty"`
	SwitchReady     bool   `json:"switch_ready"`
	DriftAvailable  bool   `json:"drift_available"`
	DriftError      string `json:"drift_error,omitempty"`
	Drift           *struct {
		HasDrift       bool     `json:"has_drift"`
		MissingRoles   []string `json:"missing_roles,omitempty"`
		StaleRoles     []string `json:"stale_roles,omitempty"`
		MissingMappers []string `json:"missing_mappers,omitempty"`
		DriftedMappers []string `json:"drifted_mappers,omitempty"`
		RedirectURIOK  bool     `json:"redirect_uri_ok"`
		BrokerConfigOK bool     `json:"broker_config_ok"`
	} `json:"drift,omitempty"`
}

func (handler *SubsystemOnboardingHandler) loadKeycloakRoleCodesForDrift(ctx context.Context, tenantID, applicationID string) ([]string, error) {
	if handler.keycloakCatalog == nil {
		return nil, errors.New("Keycloak authorization catalog is not configured")
	}
	return handler.keycloakCatalog.ListKeycloakRoleCodes(ctx, tenantID, applicationID)
}

// GetKeycloakSyncStatus 返回单个子系统环境的聚焦同步状态，含漂移检测。
func (handler *SubsystemOnboardingHandler) GetKeycloakSyncStatus(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	applicationCode := strings.TrimSpace(request.URL.Query().Get("application_code"))
	environment := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("environment")))
	if applicationCode == "" || environment == "" {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	applicationID, environmentID, err := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	response := KeycloakSyncStatusResponse{
		ApplicationCode: applicationCode,
		Environment:     environment,
		Realm:           handler.keycloakRealm,
	}
	// 读取 Client 映射。
	if inspector, supported := handler.keycloakMappings.(keycloakClientMappingInspector); supported {
		mapping, loadErr := inspector.GetKeycloakClientMapping(request.Context(), principal.Tenant.ID, applicationID, environmentID)
		if loadErr != nil {
			handler.logger.Warn("load Keycloak client mapping for sync status failed",
				"application_code", applicationCode, "environment", environment, "error", loadErr)
			response.DriftError = "CLIENT_MAPPING_UNAVAILABLE"
		} else {
			response.ClientID = mapping.ClientID
			response.ClientState = mapping.Status
			response.LastSyncedAt = mapping.LastSyncedAt
		}
	}
	// 读取切换门禁就绪状态。
	readiness := handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, applicationCode, environment)
	response.SwitchReady = readiness.SwitchReady
	// 控制面可用时执行漂移检测。
	if handler.keycloakControl != nil && response.ClientID != "" {
		roleCodes, catalogErr := handler.loadKeycloakRoleCodesForDrift(request.Context(), principal.Tenant.ID, applicationID)
		if catalogErr != nil {
			handler.logger.Warn("load role catalog for drift check failed",
				"application_code", applicationCode, "environment", environment, "error", catalogErr)
			response.DriftError = "ROLE_CATALOG_UNAVAILABLE"
		} else {
			resolver, supported := handler.service.(subsystemEnvironmentRedirectURIResolver)
			if !supported {
				response.DriftError = "GATEWAY_CONFIGURATION_UNAVAILABLE"
				writer.Header().Set("Cache-Control", "no-store, private")
				httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 同步状态查询成功", response)
				return
			}
			redirectURI, redirectErr := resolver.ResolveApplicationEnvironmentRedirectURI(request.Context(), principal.Tenant.ID, applicationID, environmentID)
			if redirectErr != nil {
				handler.logger.Warn("resolve redirect URI for drift check failed", "application_code", applicationCode, "environment", environment, "error", redirectErr)
				response.DriftError = "GATEWAY_CONFIGURATION_UNAVAILABLE"
				writer.Header().Set("Cache-Control", "no-store, private")
				httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 同步状态查询成功", response)
				return
			}
			drift, driftErr := handler.keycloakControl.DetectSubsystemKeycloakDrift(request.Context(), response.ClientID, redirectURI, roleCodes)
			if driftErr != nil {
				handler.logger.Warn("drift detection failed",
					"application_code", applicationCode, "environment", environment, "error", driftErr)
				response.DriftError = "DRIFT_DETECTION_FAILED"
			} else {
				response.DriftAvailable = true
				response.Drift = &struct {
					HasDrift       bool     `json:"has_drift"`
					MissingRoles   []string `json:"missing_roles,omitempty"`
					StaleRoles     []string `json:"stale_roles,omitempty"`
					MissingMappers []string `json:"missing_mappers,omitempty"`
					DriftedMappers []string `json:"drifted_mappers,omitempty"`
					RedirectURIOK  bool     `json:"redirect_uri_ok"`
					BrokerConfigOK bool     `json:"broker_config_ok"`
				}{
					HasDrift:       drift.HasDrift,
					MissingRoles:   drift.MissingRoles,
					StaleRoles:     drift.StaleRoles,
					MissingMappers: drift.MissingMappers,
					DriftedMappers: drift.DriftedMappers,
					RedirectURIOK:  drift.RedirectURIOK,
					BrokerConfigOK: drift.BrokerConfigOK,
				}
			}
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 同步状态查询成功", response)
}

func subsystemDeploymentStateNextAction(state application.SubsystemDeploymentState) string {
	if state.Status == application.SubsystemDeploymentStatusUnmanaged {
		return "该应用环境已登记，但尚未产生受控部署记录；请通过本地编排状态和健康检查确认服务运行。只有部署 Agent 已明确支持该应用时，才能执行受控运行时更新"
	}
	if state.Status != application.SubsystemDeploymentStatusFailed {
		return ""
	}
	if state.LastErrorCode == "INITIAL_ACCESS_ASSIGNMENT_FAILED" || state.LastErrorCode == "INITIAL_ACCESS_STATE_FAILED" {
		return "部署运行时已完成，但初始管理员授权尚未完成；确认目标用户有效且授权目录已同步后，点击“重试”"
	}
	if _, nextAction := runtimeLicenseApprovalDiagnostic(state.LastErrorCode); nextAction != "" {
		return nextAction
	}
	return "请检查部署 Agent、Docker 和目标 API 健康状态，修复后在当前环境点击“重试”；不要重复新增接入"
}

// GetSubsystemHealthDashboard 返回当前租户全部子系统的集成健康度，只读诊断端点。
func (handler *SubsystemOnboardingHandler) GetSubsystemHealthDashboard(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	items, err := handler.service.ListPortalApplications(request.Context(), principal.Tenant.ID, principal.User.ID, "")
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	// 真实运行时探活：从仓储解析每个激活环境的容器内健康 URL，并发探测（单目标 2 秒
	// 超时）。仓储不支持解析（如测试桩）时退回 UNVERIFIED，而不是假报 READY。
	healthResults := map[string]bool{}
	if resolver, ok := handler.deploymentState.(application.SubsystemHealthTargetResolver); ok && handler.deploymentState != nil {
		if targets, err := resolver.ResolveSubsystemHealthTargets(request.Context(), principal.Tenant.ID); err == nil {
			var waitGroup sync.WaitGroup
			var resultsMutex sync.Mutex
			// 安全（SEC-B5）：探针与反向代理共用同一出网策略——先做 URL 层校验，
			// 客户端 Transport 再在建连前对解析出的 IP 复验；被拒绝的目标直接跳过，
			// 看板保持非 READY，绝不触达云元数据或未授权的回环/私网地址。
			egress := handler.egress
			if egress == nil {
				egress = application.EgressPolicyFromEnv()
			}
			client := egress.NewEgressHTTPClient(2 * time.Second)
			for _, target := range targets {
				if err := egress.ValidateURL(target.HealthURL); err != nil {
					handler.logger.Warn("subsystem health probe blocked by egress policy",
						"application_code", target.ApplicationCode, "environment", target.Environment, "error", err)
					continue
				}
				waitGroup.Add(1)
				go func(target application.SubsystemHealthTarget) {
					defer waitGroup.Done()
					probeRequest, err := stdhttp.NewRequest(stdhttp.MethodGet, target.HealthURL, nil)
					if err != nil {
						return
					}
					response, err := client.Do(probeRequest)
					if err != nil {
						return
					}
					defer response.Body.Close()
					resultsMutex.Lock()
					healthResults[target.ApplicationCode+":"+target.Environment] = response.StatusCode >= 200 && response.StatusCode < 400
					resultsMutex.Unlock()
				}(target)
			}
			waitGroup.Wait()
		} else {
			handler.logger.Warn("subsystem health targets unavailable", "tenant_id", principal.Tenant.ID, "error", err)
		}
	}
	entries := make([]SubsystemHealthEntry, 0, len(items))
	for _, item := range items {
		entry := SubsystemHealthEntry{
			ApplicationCode:   item.Code,
			ApplicationName:   item.Name,
			Environment:       item.Environment,
			DirectoryOK:       true,
			DirectoryStatus:   "READY",
			CredentialsStatus: "UNKNOWN",
			KeycloakStatus:    "UNKNOWN",
			ProjectionStatus:  strings.ToUpper(strings.TrimSpace(item.Projection.Status)),
			Status:            "VERIFICATION_REQUIRED",
			NextAction:        "调用当前环境同步状态接口，分别验证凭据交付和 Keycloak 控制面状态。",
		}
		if !handler.keycloakEnabled {
			entry.KeycloakStatus = "NOT_APPLICABLE"
		}
		switch runtimeHealthy, probed := healthResults[item.Code+":"+item.Environment]; {
		case probed && runtimeHealthy:
			entry.RuntimeOK = true
			entry.RuntimeStatus = "READY"
		case probed:
			entry.RuntimeOK = false
			entry.RuntimeStatus = "UNAVAILABLE"
			entry.Status = "RUNTIME_UNAVAILABLE"
			entry.NextAction = "子系统容器健康检查失败；请查看目标容器日志，修复后点击重试。"
		default:
			entry.RuntimeOK = false
			entry.RuntimeStatus = "UNVERIFIED"
			entry.NextAction = "无法探测子系统运行时（未注册 upstream 或未部署）；请确认部署状态。"
		}
		entries = append(entries, entry)
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统健康状态查询成功", entries)
}

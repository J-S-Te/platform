package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Keycloak 接入的认证投影（旧流程 completeOnboardKeycloakProjection 的共享实现）。
//
// 旧完成后链在 Agent 部署成功之后依次执行：读取角色目录 → 在 Keycloak 中确保角色 →
// 保存 Client 映射 → 回填授权投影 → 写入就绪标记。这段逻辑此前只存在于 HTTP handler，
// 统一入口无法复用；这里抽成应用服务，新旧入口调用同一实现。
// KeycloakProjectionCode* 常量是投影链各步骤失败时写入操作记录的原因码，
// 供诊断界面与日志区分失败发生在哪一步；取值一经下发不得改动。
const (
	KeycloakProjectionCodeMappingUnavailable  = "KEYCLOAK_MAPPING_UNAVAILABLE"
	KeycloakProjectionCodeRoleCatalogSyncFail = "KEYCLOAK_ROLE_CATALOG_SYNC_FAILED"
	KeycloakProjectionCodeClientMappingFail   = "KEYCLOAK_CLIENT_MAPPING_FAILED"
	KeycloakProjectionCodeBackfillFail        = "KEYCLOAK_AUTHORIZATION_BACKFILL_FAILED"
	KeycloakProjectionCodeReadinessFail       = "KEYCLOAK_READINESS_UPDATE_FAILED"
)

// KeycloakRoleCatalogSource 读取应用的角色目录编码。
type KeycloakRoleCatalogSource interface {
	ListKeycloakRoleCodes(ctx context.Context, tenantID, applicationID string) ([]string, error)
}

// KeycloakClientRoleControl 在 Keycloak 侧确保 Client 角色存在。
type KeycloakClientRoleControl interface {
	EnsureClientRoles(ctx context.Context, clientID string, roleCodes []string) error
}

// KeycloakClientMappingStore 保存 Client 映射并回填授权投影。
type KeycloakClientMappingStore interface {
	SaveKeycloakClientMapping(ctx context.Context, tenantID, applicationID, environmentID, realm, clientID string) error
	BackfillKeycloakAuthorization(ctx context.Context, tenantID, applicationID, environmentID string) error
}

// KeycloakProjectionReadinessUpdater 写入"Client 与角色目录已同步"的就绪标记。
type KeycloakProjectionReadinessUpdater interface {
	MarkKeycloakClientAndRoleCatalogSynced(ctx context.Context, tenantID, applicationID, environmentID string) error
}

// KeycloakProjectionInput 是一次投影所需的输入（来自接入操作与部署结果）。
type KeycloakProjectionInput struct {
	TenantID      string
	ApplicationID string
	EnvironmentID string
	// ClientID 为空表示本次接入不走 Keycloak，投影直接跳过。
	ClientID string
	Realm    string
}

// KeycloakProjectionResult 描述投影结果：Projected=false 表示按输入跳过。
type KeycloakProjectionResult struct {
	Projected  bool
	ErrorCode  string
	Diagnostic string
}

// KeycloakAuthorizationProjectionService 复用既有 Keycloak 组件完成认证投影。
type KeycloakAuthorizationProjectionService struct {
	catalog   KeycloakRoleCatalogSource
	control   KeycloakClientRoleControl
	mappings  KeycloakClientMappingStore
	readiness KeycloakProjectionReadinessUpdater
}

// NewKeycloakAuthorizationProjectionService 构造投影服务。
// readiness 可为 nil（部署未注入就绪更新器时跳过最后一步并如实报告）。
func NewKeycloakAuthorizationProjectionService(catalog KeycloakRoleCatalogSource, control KeycloakClientRoleControl, mappings KeycloakClientMappingStore, readiness KeycloakProjectionReadinessUpdater) (*KeycloakAuthorizationProjectionService, error) {
	if catalog == nil || control == nil || mappings == nil {
		return nil, errors.New("keycloak authorization projection requires catalog, control and mapping store")
	}
	return &KeycloakAuthorizationProjectionService{catalog: catalog, control: control, mappings: mappings, readiness: readiness}, nil
}

// Project 执行投影。失败时返回稳定的错误码与诊断，由调用方决定如何收口生命周期。
func (service *KeycloakAuthorizationProjectionService) Project(ctx context.Context, input KeycloakProjectionInput) (KeycloakProjectionResult, error) {
	clientID := strings.TrimSpace(input.ClientID)
	if clientID == "" {
		return KeycloakProjectionResult{Projected: false}, nil
	}
	tenantID := strings.TrimSpace(input.TenantID)
	applicationID := strings.TrimSpace(input.ApplicationID)
	environmentID := strings.TrimSpace(input.EnvironmentID)
	if tenantID == "" || applicationID == "" || environmentID == "" {
		return KeycloakProjectionResult{}, fmt.Errorf("keycloak projection requires tenant, application and environment: %w", ErrValidation)
	}

	roleCodes, err := service.catalog.ListKeycloakRoleCodes(ctx, tenantID, applicationID)
	if err != nil {
		return KeycloakProjectionResult{Projected: true, ErrorCode: KeycloakProjectionCodeRoleCatalogSyncFail,
			Diagnostic: "Keycloak 角色目录读取失败: " + err.Error()}, nil
	}
	if err := service.control.EnsureClientRoles(ctx, clientID, roleCodes); err != nil {
		return KeycloakProjectionResult{Projected: true, ErrorCode: KeycloakProjectionCodeRoleCatalogSyncFail,
			Diagnostic: "Keycloak 角色目录同步失败: " + err.Error()}, nil
	}
	if err := service.mappings.SaveKeycloakClientMapping(ctx, tenantID, applicationID, environmentID, strings.TrimSpace(input.Realm), clientID); err != nil {
		return KeycloakProjectionResult{Projected: true, ErrorCode: KeycloakProjectionCodeClientMappingFail,
			Diagnostic: "Keycloak Client 映射保存失败: " + err.Error()}, nil
	}
	if err := service.mappings.BackfillKeycloakAuthorization(ctx, tenantID, applicationID, environmentID); err != nil {
		return KeycloakProjectionResult{Projected: true, ErrorCode: KeycloakProjectionCodeBackfillFail,
			Diagnostic: "Keycloak 授权投影回填失败: " + err.Error()}, nil
	}
	if service.readiness != nil {
		if err := service.readiness.MarkKeycloakClientAndRoleCatalogSynced(ctx, tenantID, applicationID, environmentID); err != nil {
			return KeycloakProjectionResult{Projected: true, ErrorCode: KeycloakProjectionCodeReadinessFail,
				Diagnostic: "Keycloak 就绪状态写入失败: " + err.Error()}, nil
		}
	}
	return KeycloakProjectionResult{Projected: true}, nil
}

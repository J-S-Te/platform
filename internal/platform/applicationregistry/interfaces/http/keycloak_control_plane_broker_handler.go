// keycloak_control_plane_broker_handler.go：IdP 生命周期与自愈（broker）——Ensure/Verify 入口、配置完整性自愈与确定性配置缺陷标记。
package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/url"
	"strings"
)

// keycloakBrokerConfigurationError 标记 Keycloak 返回的确定性配置缺陷。
// 调用方可据此判断是否适合执行窄范围的 broker 凭据恢复；
// 传输与授权失败绝不允许触发凭据轮换。
type keycloakBrokerConfigurationError struct {
	alias      string
	missingKey string
}

func (err *keycloakBrokerConfigurationError) Error() string {
	return fmt.Sprintf("Keycloak IdP %s config incomplete: missing %s", err.alias, err.missingKey)
}

// IsRecoverableKeycloakBrokerConfigurationError 判断 Keycloak 是否返回了完整、
// 局部的配置缺陷。刻意排除网络、认证与授权失败，避免把环境故障误判为可恢复。
func IsRecoverableKeycloakBrokerConfigurationError(err error) bool {
	var configurationError *keycloakBrokerConfigurationError
	return errors.As(err, &configurationError)
}

// EnsureBroker 注册上游基础平台身份提供方，并把授权 claims 导入 Keycloak 用户属性。
// 别名刻意显式区分：内部员工与外部客户使用不同 Broker 客户端，
// 一个应用的回调无法覆盖另一个应用的客户端凭据。
func (control *keycloakControlPlane) EnsureBroker(ctx context.Context, clientID, clientSecret string) error {
	return control.ensureBroker(ctx, "basic-platform", "基础平台", clientID, clientSecret)
}

// EnsureCustomerPortalBroker 配置与内部 Broker 隔离的客户门户 IdP，凭据互不复用。
func (control *keycloakControlPlane) EnsureCustomerPortalBroker(ctx context.Context, clientID, clientSecret string) error {
	return control.ensureBroker(ctx, "basic-platform-customer", "客户自助门户", clientID, clientSecret)
}

// VerifyBrokerExists 检查 basic-platform IdP 存在且配置完整；不修改 IdP，
// 也不需要客户端 Secret。适用于平台已持有有效凭据、仅需确认 Keycloak 侧健康的场景。
func (control *keycloakControlPlane) VerifyBrokerExists(ctx context.Context) error {
	return control.verifyBrokerExists(ctx, "basic-platform")
}

// VerifyCustomerPortalBrokerExists 检查客户门户 IdP 的健康状态。
func (control *keycloakControlPlane) VerifyCustomerPortalBrokerExists(ctx context.Context) error {
	return control.verifyBrokerExists(ctx, "basic-platform-customer")
}

func (control *keycloakControlPlane) verifyBrokerExists(ctx context.Context, brokerAlias string) error {
	token, err := control.token(ctx)
	if err != nil {
		return err
	}
	if err := control.ensureRealm(ctx, token); err != nil {
		return err
	}
	base := "/admin/realms/" + url.PathEscape(control.realm) + "/identity-provider/instances/" + url.PathEscape(brokerAlias)
	response, err := control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == stdhttp.StatusNotFound {
		// 平台对健康的 broker 凭据只保存哈希，因此重建被删除的 IdP 必须走窄范围的
		// 凭据恢复路径。该别名的 404 属于确定性的 Keycloak 配置漂移，与传输、认证或
		// 权限失败不同，因此可安全标记为可恢复。
		return &keycloakBrokerConfigurationError{alias: brokerAlias, missingKey: "identity-provider"}
	}
	if err := keycloakStatusError("read Keycloak IdP", response, stdhttp.StatusOK); err != nil {
		return err
	}
	var verified struct {
		Config map[string]string `json:"config"`
	}
	if err := json.NewDecoder(response.Body).Decode(&verified); err != nil {
		return fmt.Errorf("decode Keycloak IdP %s: %w", brokerAlias, err)
	}
	requiredKeys := []string{"clientId", "clientSecret", "tokenUrl", "userInfoUrl", "authorizationUrl"}
	for _, key := range requiredKeys {
		if strings.TrimSpace(verified.Config[key]) == "" {
			return &keycloakBrokerConfigurationError{alias: brokerAlias, missingKey: key}
		}
	}
	return nil
}

func (control *keycloakControlPlane) ensureBroker(ctx context.Context, brokerAlias, displayName, clientID, clientSecret string) error {
	token, err := control.token(ctx)
	if err != nil {
		return err
	}
	if err := control.ensureRealm(ctx, token); err != nil {
		return err
	}
	if err := control.ensurePrelinkedBrokerFlow(ctx, token); err != nil {
		return err
	}
	base := "/admin/realms/" + url.PathEscape(control.realm) + "/identity-provider/instances/" + url.PathEscape(brokerAlias)
	response, err := control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	exists := response.StatusCode == stdhttp.StatusOK
	if !exists && response.StatusCode != stdhttp.StatusNotFound {
		defer response.Body.Close()
		return keycloakStatusError("read Keycloak broker", response, stdhttp.StatusOK, stdhttp.StatusNotFound)
	}
	response.Body.Close()
	backchannel := control.platformBackchannel
	if backchannel == "" {
		backchannel = control.platformIssuer
	}
	// 面向浏览器的端点必须使用公开 Issuer；只有 token 与 userinfo 调用是服务端到
	// 服务端，可走 backchannel。把内部 Docker 主机名暴露为 logoutUrl 会让
	// Keycloak 在 Broker 登出期间把用户浏览器重定向到不可达主机。
	// 用户在进入子系统之前已由平台授权投影完成供给与关联；绝不暴露 Keycloak 的
	// 首次登录资料补全表单——邮箱与拆分的姓名在平台侧是可选字段，
	// 不能变成另一套注册契约。
	payload := control.brokerRepresentation(brokerAlias, displayName, clientID, clientSecret, backchannel)
	if exists {
		response, err = control.request(ctx, token, stdhttp.MethodPut, base, payload)
	} else {
		response, err = control.request(ctx, token, stdhttp.MethodPost, "/admin/realms/"+url.PathEscape(control.realm)+"/identity-provider/instances", payload)
	}
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("configure Keycloak broker", response, stdhttp.StatusNoContent, stdhttp.StatusCreated); err != nil {
		return err
	}
	response.Body.Close()
	response, err = control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	if err := keycloakStatusError("verify Keycloak broker", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return err
	}
	var verifiedBroker struct {
		FirstBrokerLoginFlowAlias string            `json:"firstBrokerLoginFlowAlias"`
		Config                    map[string]string `json:"config"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&verifiedBroker)
	response.Body.Close()
	if decodeErr != nil {
		return decodeErr
	}
	// 自愈：核验 IdP 配置完整性。PUT 期间的时序竞争或缓存未命中可能让 Keycloak
	// 返回空 config{}。重新提交一次载荷；仍失败则把错误上抛，由 Worker 在下次
	// 启动时重试，而不是静默留下一个损坏的 IdP。
	if err := control.ensureBrokerConfigComplete(ctx, token, base, brokerAlias, payload, verifiedBroker.Config); err != nil {
		return err
	}
	// Keycloak 26 可能持续把遗留的顶层 profile 模式报为 "on"；
	// 提供方专属的 deny-only 流程才是权威的"不注册"契约。
	if verifiedBroker.FirstBrokerLoginFlowAlias != keycloakPrelinkedBrokerFlowAlias {
		return fmt.Errorf("Keycloak Broker policy did not converge to prelinked-only mode")
	}
	if err := control.ensureBrokerReviewProfileDisabled(ctx, token, brokerAlias); err != nil {
		return err
	}
	if err := control.ensureBrokerUserProfile(ctx, token); err != nil {
		return err
	}
	type existingMapper struct {
		ID     string
		Config map[string]string
	}
	listMappers := func() (map[string]existingMapper, error) {
		response, err := control.request(ctx, token, stdhttp.MethodGet, base+"/mappers", nil)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if err := keycloakStatusError("read Keycloak broker mappers", response, stdhttp.StatusOK); err != nil {
			return nil, err
		}
		var mappers []struct {
			ID     string            `json:"id"`
			Name   string            `json:"name"`
			Config map[string]string `json:"config"`
		}
		if err := json.NewDecoder(response.Body).Decode(&mappers); err != nil {
			return nil, err
		}
		ids := make(map[string]existingMapper, len(mappers))
		for _, mapper := range mappers {
			ids[mapper.Name] = existingMapper{ID: mapper.ID, Config: mapper.Config}
		}
		return ids, nil
	}
	mapperIDs, err := listMappers()
	if err != nil {
		return err
	}
	for _, claim := range keycloakIdentityClaimMappings {
		mapper := map[string]any{"name": brokerAlias + "-" + claim.Name, "identityProviderAlias": brokerAlias, "identityProviderMapper": "oidc-user-attribute-idp-mapper", "config": map[string]string{"syncMode": "INHERIT", "claim": claim.Name, "user.attribute": claim.Name}}
		mapperName := brokerAlias + "-" + claim.Name
		existing := mapperIDs[mapperName]
		mapperID := existing.ID
		if mapperID != "" && existing.Config["syncMode"] == "INHERIT" && existing.Config["claim"] == claim.Name && existing.Config["user.attribute"] == claim.Name {
			continue
		}
		method, path := stdhttp.MethodPost, base+"/mappers"
		if mapperID != "" {
			// Keycloak 26 的缓存层可能拒绝就地更新 mapper，即使记录本身有效。
			// 只对发生漂移的 mapper 执行删除重建，避免放大影响面。
			response, err = control.request(ctx, token, stdhttp.MethodDelete, base+"/mappers/"+url.PathEscape(mapperID), nil)
			if err != nil {
				return err
			}
			if err := keycloakStatusError("delete Keycloak broker claim "+claim.Name, response, stdhttp.StatusNoContent, stdhttp.StatusNotFound); err != nil {
				response.Body.Close()
				return err
			}
			response.Body.Close()
		}
		response, err = control.request(ctx, token, method, path, mapper)
		if err != nil {
			return err
		}
		if response.StatusCode == stdhttp.StatusCreated {
			response.Body.Close()
			continue
		}
		createErr := keycloakStatusError("configure Keycloak broker claim "+claim.Name, response, stdhttp.StatusCreated)
		response.Body.Close()
		// Keycloak 26.2 可能在落库 IdP mapper 后、收尾 Admin 事件时返回 400。
		// 此时重读并更新该 mapper，使操作保持正确且可安全重试，
		// 而不是对外暴露一个虚假的 503。
		mapperIDs, err = listMappers()
		if err != nil {
			return err
		}
		existing = mapperIDs[mapperName]
		mapperID = existing.ID
		if mapperID == "" {
			return createErr
		}
		if existing.Config["syncMode"] != "INHERIT" || existing.Config["claim"] != claim.Name || existing.Config["user.attribute"] != claim.Name {
			return createErr
		}
	}
	return nil
}

// ensureBrokerConfigComplete 核验从 Keycloak 读回的 IdP 配置是否包含全部必需字段；
// 配置为空或缺关键键时重新提交一次期望载荷。该自愈针对 Keycloak 已知竞态：
// PUT 返回 204 但配置仍为空 {}。
func (control *keycloakControlPlane) ensureBrokerConfigComplete(ctx context.Context, token, base, brokerAlias string, expected map[string]any, actual map[string]string) error {
	requiredKeys := []string{"clientId", "clientSecret", "tokenUrl", "userInfoUrl", "authorizationUrl"}
	complete := true
	for _, key := range requiredKeys {
		if strings.TrimSpace(actual[key]) == "" {
			complete = false
			break
		}
	}
	if complete {
		return nil
	}
	// 重新提交一次期望配置。
	response, err := control.request(ctx, token, stdhttp.MethodPut, base, expected)
	if err != nil {
		return fmt.Errorf("self-heal Keycloak IdP %s config: %w", brokerAlias, err)
	}
	response.Body.Close()
	if response.StatusCode != stdhttp.StatusNoContent {
		return fmt.Errorf("self-heal Keycloak IdP %s config returned HTTP %d", brokerAlias, response.StatusCode)
	}
	// 核验修复已生效。
	response, err = control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return fmt.Errorf("verify self-healed Keycloak IdP %s config: %w", brokerAlias, err)
	}
	defer response.Body.Close()
	if err := keycloakStatusError("verify self-healed Keycloak IdP config", response, stdhttp.StatusOK); err != nil {
		return err
	}
	var recheck struct {
		Config map[string]string `json:"config"`
	}
	if err := json.NewDecoder(response.Body).Decode(&recheck); err != nil {
		return fmt.Errorf("decode self-healed Keycloak IdP %s config: %w", brokerAlias, err)
	}
	for _, key := range requiredKeys {
		if strings.TrimSpace(recheck.Config[key]) == "" {
			return &keycloakBrokerConfigurationError{alias: brokerAlias, missingKey: key}
		}
	}
	return nil
}

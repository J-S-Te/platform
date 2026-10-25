// keycloak_control_plane_broker_flow_handler.go：Broker 认证流程与用户资料（broker-flow）——deny-only 首登流程、Review Profile 关闭与导入属性声明。
package http

import (
	"context"
	"encoding/json"
	"fmt"
	stdhttp "net/http"
	"net/url"
	"strings"
)

func (control *keycloakControlPlane) brokerRepresentation(alias, displayName, clientID, clientSecret, backchannel string) map[string]any {
	return map[string]any{
		"alias": alias, "displayName": displayName, "providerId": "oidc",
		"enabled": true, "trustEmail": true, "storeToken": false,
		"updateProfileFirstLoginMode": "off",
		"firstBrokerLoginFlowAlias":   keycloakPrelinkedBrokerFlowAlias,
		"config": map[string]string{
			"clientId": clientID, "clientSecret": clientSecret, "clientAuthMethod": "client_secret_basic",
			"authorizationUrl": control.platformIssuer + "/authorize", "tokenUrl": backchannel + "/oauth2/token",
			"userInfoUrl": backchannel + "/oauth2/userinfo", "logoutUrl": control.platformIssuer + "/oauth2/logout",
			"defaultScope": "openid profile",
		},
	}
}

const (
	keycloakDefaultFirstBrokerLoginFlow = "first broker login"
	keycloakPrelinkedBrokerFlowAlias    = "basic-platform prelinked only"
	keycloakDenyAccessProviderID        = "deny-access-authenticator"
	keycloakReviewProfileProviderID     = "idp-review-profile"
	keycloakReviewProfileModeKey        = "update.profile.on.first.login"
)

// ensurePrelinkedBrokerFlow 创建提供方专属、失败关闭的首次登录流程。
// 对已关联的联合身份 Keycloak 会跳过该流程；未知主体只会命中唯一的 deny
// 执行项，而不会进入 Keycloak 的 JIT 用户/资料注册流程。
func (control *keycloakControlPlane) ensurePrelinkedBrokerFlow(ctx context.Context, token string) error {
	realmPath := "/admin/realms/" + url.PathEscape(control.realm)
	flowPath := realmPath + "/authentication/flows/" + url.PathEscape(keycloakPrelinkedBrokerFlowAlias)
	response, err := control.request(ctx, token, stdhttp.MethodGet, flowPath, nil)
	if err != nil {
		return err
	}
	status := response.StatusCode
	response.Body.Close()
	if status == stdhttp.StatusNotFound {
		response, err = control.request(ctx, token, stdhttp.MethodPost, realmPath+"/authentication/flows", map[string]any{
			"alias": keycloakPrelinkedBrokerFlowAlias, "description": "Basic Platform projected users only",
			"providerId": "basic-flow", "topLevel": true, "builtIn": false,
		})
		if err != nil {
			return err
		}
		status = response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusCreated && status != stdhttp.StatusConflict {
			return fmt.Errorf("create prelinked-only Keycloak Broker flow returned HTTP %d", status)
		}
	} else if status != stdhttp.StatusOK {
		return fmt.Errorf("read prelinked-only Keycloak Broker flow returned HTTP %d", status)
	}

	type flowExecution struct {
		ID          string `json:"id"`
		ProviderID  string `json:"providerId"`
		Requirement string `json:"requirement"`
	}
	executionsPath := flowPath + "/executions"
	readExecutions := func() ([]flowExecution, error) {
		response, err := control.request(ctx, token, stdhttp.MethodGet, executionsPath, nil)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if err := keycloakStatusError("read prelinked-only Keycloak Broker executions", response, stdhttp.StatusOK); err != nil {
			return nil, err
		}
		var executions []flowExecution
		if err := json.NewDecoder(response.Body).Decode(&executions); err != nil {
			return nil, err
		}
		return executions, nil
	}
	executions, err := readExecutions()
	if err != nil {
		return err
	}
	denyID := ""
	for _, execution := range executions {
		if execution.ProviderID == keycloakDenyAccessProviderID && denyID == "" {
			denyID = strings.TrimSpace(execution.ID)
			continue
		}
		if strings.TrimSpace(execution.ID) == "" {
			continue
		}
		response, err = control.request(ctx, token, stdhttp.MethodDelete, realmPath+"/authentication/executions/"+url.PathEscape(execution.ID), nil)
		if err != nil {
			return err
		}
		status = response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusNoContent {
			return fmt.Errorf("remove drifted prelinked-only Broker execution returned HTTP %d", status)
		}
	}
	if denyID == "" {
		response, err = control.request(ctx, token, stdhttp.MethodPost, flowPath+"/executions/execution", map[string]string{"provider": keycloakDenyAccessProviderID})
		if err != nil {
			return err
		}
		status = response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusCreated {
			return fmt.Errorf("add deny execution to prelinked-only Broker flow returned HTTP %d", status)
		}
		executions, err = readExecutions()
		if err != nil {
			return err
		}
		for _, execution := range executions {
			if execution.ProviderID == keycloakDenyAccessProviderID {
				denyID = strings.TrimSpace(execution.ID)
				break
			}
		}
	}
	if denyID == "" {
		return fmt.Errorf("prelinked-only Keycloak Broker flow has no deny execution")
	}
	response, err = control.request(ctx, token, stdhttp.MethodPut, executionsPath, map[string]string{"id": denyID, "requirement": "REQUIRED"})
	if err != nil {
		return err
	}
	status = response.StatusCode
	response.Body.Close()
	if status != stdhttp.StatusNoContent {
		return fmt.Errorf("require deny execution in prelinked-only Broker flow returned HTTP %d", status)
	}
	executions, err = readExecutions()
	if err != nil {
		return err
	}
	if len(executions) != 1 || executions[0].ProviderID != keycloakDenyAccessProviderID || executions[0].Requirement != "REQUIRED" {
		return fmt.Errorf("prelinked-only Keycloak Broker flow did not converge to one required deny execution")
	}
	return nil
}

// ensureBrokerReviewProfileDisabled 调和 Keycloak 26 的实际生效设置。
// IdentityProvider 表示上的 updateProfileFirstLoginMode 仅为兼容保留；
// 当前版本从首个 Broker 流程的 Review Profile authenticator 执行项读取该模式。
func (control *keycloakControlPlane) ensureBrokerReviewProfileDisabled(ctx context.Context, token, brokerAlias string) error {
	realmPath := "/admin/realms/" + url.PathEscape(control.realm)
	providerPath := realmPath + "/identity-provider/instances/" + url.PathEscape(brokerAlias)
	response, err := control.request(ctx, token, stdhttp.MethodGet, providerPath, nil)
	if err != nil {
		return err
	}
	if err := keycloakStatusError("read effective Keycloak Broker flow", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return err
	}
	var provider struct {
		FirstBrokerLoginFlowAlias string `json:"firstBrokerLoginFlowAlias"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&provider)
	response.Body.Close()
	if decodeErr != nil {
		return decodeErr
	}
	flowAlias := strings.TrimSpace(provider.FirstBrokerLoginFlowAlias)
	if flowAlias == "" {
		response, err = control.request(ctx, token, stdhttp.MethodGet, realmPath, nil)
		if err != nil {
			return err
		}
		if err := keycloakStatusError("read Keycloak Realm Broker flow", response, stdhttp.StatusOK); err != nil {
			response.Body.Close()
			return err
		}
		var realm struct {
			FirstBrokerLoginFlow string `json:"firstBrokerLoginFlow"`
		}
		decodeErr = json.NewDecoder(response.Body).Decode(&realm)
		response.Body.Close()
		if decodeErr != nil {
			return decodeErr
		}
		flowAlias = strings.TrimSpace(realm.FirstBrokerLoginFlow)
		if flowAlias == "" {
			flowAlias = keycloakDefaultFirstBrokerLoginFlow
		}
	}
	executionsPath := realmPath + "/authentication/flows/" + url.PathEscape(flowAlias) + "/executions"
	response, err = control.request(ctx, token, stdhttp.MethodGet, executionsPath, nil)
	if err != nil {
		return err
	}
	if err := keycloakStatusError("read Keycloak first Broker login executions", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return err
	}
	var executions []struct {
		ID                   string `json:"id"`
		ProviderID           string `json:"providerId"`
		AuthenticationConfig string `json:"authenticationConfig"`
	}
	decodeErr = json.NewDecoder(response.Body).Decode(&executions)
	response.Body.Close()
	if decodeErr != nil {
		return decodeErr
	}
	for _, execution := range executions {
		if execution.ProviderID != keycloakReviewProfileProviderID || strings.TrimSpace(execution.ID) == "" {
			continue
		}
		configID := strings.TrimSpace(execution.AuthenticationConfig)
		if configID == "" {
			payload := map[string]any{"alias": "basic-platform-review-profile", "config": map[string]string{keycloakReviewProfileModeKey: "off"}}
			response, err = control.request(ctx, token, stdhttp.MethodPost, realmPath+"/authentication/executions/"+url.PathEscape(execution.ID)+"/config", payload)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			return keycloakStatusError("configure Keycloak Broker Review Profile", response, stdhttp.StatusCreated)
		}
		configPath := realmPath + "/authentication/config/" + url.PathEscape(configID)
		response, err = control.request(ctx, token, stdhttp.MethodGet, configPath, nil)
		if err != nil {
			return err
		}
		if err := keycloakStatusError("read Keycloak Broker Review Profile config", response, stdhttp.StatusOK); err != nil {
			response.Body.Close()
			return err
		}
		var config struct {
			ID     string            `json:"id,omitempty"`
			Alias  string            `json:"alias"`
			Config map[string]string `json:"config"`
		}
		decodeErr = json.NewDecoder(response.Body).Decode(&config)
		response.Body.Close()
		if decodeErr != nil {
			return decodeErr
		}
		if config.Config[keycloakReviewProfileModeKey] == "off" {
			return nil
		}
		if config.Config == nil {
			config.Config = map[string]string{}
		}
		if strings.TrimSpace(config.Alias) == "" {
			config.Alias = "basic-platform-review-profile"
		}
		config.ID = configID
		config.Config[keycloakReviewProfileModeKey] = "off"
		response, err = control.request(ctx, token, stdhttp.MethodPut, configPath, config)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		return keycloakStatusError("update Keycloak Broker Review Profile config", response, stdhttp.StatusNoContent)
	}
	// 不含 Review Profile 的自定义流程本身已满足"零交互"契约，无需再配置 authenticator。
	return nil
}

// ensureBrokerUserProfile 声明从基础平台导入的属性。Keycloak 26 会按 Realm
// user-profile 模式校验 mapper 目标，Attribute Importer 无法在首次登录时
// 任意创建属性，因此必须先声明。
func (control *keycloakControlPlane) ensureBrokerUserProfile(ctx context.Context, token string) error {
	path := "/admin/realms/" + url.PathEscape(control.realm) + "/users/profile"
	response, err := control.request(ctx, token, stdhttp.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("read Keycloak user profile", response, stdhttp.StatusOK); err != nil {
		return err
	}
	var profile map[string]any
	if err := json.NewDecoder(response.Body).Decode(&profile); err != nil {
		return err
	}
	attributes, _ := profile["attributes"].([]any)
	// 资料校验归基础平台所有；Keycloak 不得把可选的平台字段变成首次登录注册表单。
	for index, raw := range attributes {
		attribute, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch attribute["name"] {
		case "email", "firstName", "lastName":
			delete(attribute, "required")
			attributes[index] = attribute
		}
	}
	for _, claim := range keycloakIdentityClaimMappings {
		found := false
		for index, raw := range attributes {
			attribute, ok := raw.(map[string]any)
			if !ok || attribute["name"] != claim.Name {
				continue
			}
			// 每次同步都修复被手工改动的资料元数据，防止 Keycloak 管理员把列表 claim
			// 静默改成标量，或把映射改成用户可编辑。
			attribute["displayName"] = claim.Name
			attribute["multivalued"] = claim.MultiValued
			attribute["permissions"] = map[string]any{"view": []string{"admin"}, "edit": []string{"admin"}}
			attributes[index] = attribute
			found = true
			break
		}
		if found {
			continue
		}
		attributes = append(attributes, map[string]any{
			"name":        claim.Name,
			"displayName": claim.Name,
			"multivalued": claim.MultiValued,
			"permissions": map[string]any{"view": []string{"admin"}, "edit": []string{"admin"}},
		})
	}
	profile["attributes"] = attributes
	response.Body.Close()
	response, err = control.request(ctx, token, stdhttp.MethodPut, path, profile)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("configure Keycloak user profile", response, stdhttp.StatusOK, stdhttp.StatusNoContent); err != nil {
		return err
	}
	response.Body.Close()
	response, err = control.request(ctx, token, stdhttp.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("verify Keycloak user profile", response, stdhttp.StatusOK); err != nil {
		return err
	}
	var verified map[string]any
	if err := json.NewDecoder(response.Body).Decode(&verified); err != nil {
		return err
	}
	verifiedAttributes, _ := verified["attributes"].([]any)
	for _, raw := range verifiedAttributes {
		attribute, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := attribute["name"].(string)
		if name != "email" && name != "firstName" && name != "lastName" {
			continue
		}
		if required, exists := attribute["required"]; exists && required != nil {
			if encoded, _ := json.Marshal(required); string(encoded) != "{}" && string(encoded) != "[]" && string(encoded) != "false" && string(encoded) != "null" {
				return fmt.Errorf("Keycloak user profile field %s remained required", name)
			}
		}
	}
	return nil
}

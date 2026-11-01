// keycloak_control_plane_client_handler.go：Realm Client 生命周期（client）——客户端与回调、角色目录、协议 mapper 的保障与调和。
package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/url"
	"sort"
	"strings"
)

var legacyKeycloakAuthorizationClaimNames = map[string]struct{}{
	"permissions":      {},
	"organization_ids": {},
	"role_config_hash": {},
	"authz_revision":   {},
}

// EnsureClient 创建/更新机密 RP 及既有子系统期望的 claims。来自上游平台 IdP 的
// 属性导入必须先单独配置完成，本方法才允许用于生产切换。
func (control *keycloakControlPlane) EnsureClient(ctx context.Context, clientID, name, redirectURI string) (keycloakClientResult, error) {
	return control.EnsureClientRedirectURIs(ctx, clientID, name, []string{redirectURI})
}

// EnsureClientRedirectURIs 调和受控客户端的精确回调及在线技术会话期限。
//   EnsureClientRedirectURIs 原子替换单个 Realm 客户端的全部平台管控回调集合。
//   传输层切换期间先配置两个回调等流量排空，再以单一目标回调重新调用。
//   除子系统接入所需的既有 Secret 读取外，本方法不读取也不改动任何无关客户端凭据。
// 参数：ctx为调用上下文与取消边界。
//   clientID为登记的OAuth客户端标识。
//   name为受控客户端展示名称。
//   redirectURIs为受控客户端的精确回调集合。
// 返回值：与该调用匹配的核验或持久化结果；失败保留原有错误语义。
func (control *keycloakControlPlane) EnsureClientRedirectURIs(ctx context.Context, clientID, name string, redirectURIs []string) (keycloakClientResult, error) {
	redirectURIs = canonicalRedirectURIs(redirectURIs)
	if len(redirectURIs) == 0 {
		return keycloakClientResult{}, errors.New("at least one Keycloak redirect URI is required")
	}
	token, err := control.token(ctx)
	if err != nil {
		return keycloakClientResult{}, err
	}
	if err := control.ensureRealm(ctx, token); err != nil {
		return keycloakClientResult{}, err
	}
	path := "/admin/realms/" + url.PathEscape(control.realm) + "/clients?clientId=" + url.QueryEscape(clientID)
	response, err := control.request(ctx, token, stdhttp.MethodGet, path, nil)
	if err != nil {
		return keycloakClientResult{}, err
	}
	if err := keycloakStatusError("read Keycloak Client", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return keycloakClientResult{}, err
	}
	var clients []struct {
		ID         string            `json:"id"`
		Attributes map[string]string `json:"attributes"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&clients)
	response.Body.Close()
	if decodeErr != nil {
		return keycloakClientResult{}, decodeErr
	}
	payload := map[string]any{"clientId": clientID, "name": name, "enabled": true, "protocol": "openid-connect", "publicClient": false, "standardFlowEnabled": true, "directAccessGrantsEnabled": false, "redirectUris": redirectURIs, "webOrigins": []string{"+"}}
	if control.centralSessionGovernance {
		var previous map[string]string
		if len(clients) > 0 {
			previous = clients[0].Attributes
		}
		payload["attributes"] = centralClientSessionAttributes(previous, true)
	}
	internalID := ""
	if len(clients) == 0 {
		response, err = control.request(ctx, token, stdhttp.MethodPost, "/admin/realms/"+url.PathEscape(control.realm)+"/clients", payload)
		if err != nil {
			return keycloakClientResult{}, err
		}
		if err := keycloakStatusError("create Keycloak Client", response, stdhttp.StatusCreated); err != nil {
			response.Body.Close()
			return keycloakClientResult{}, err
		}
		response.Body.Close()
		response, err = control.request(ctx, token, stdhttp.MethodGet, path, nil)
		if err != nil {
			return keycloakClientResult{}, err
		}
		if err := keycloakStatusError("read newly created Keycloak Client", response, stdhttp.StatusOK); err != nil {
			response.Body.Close()
			return keycloakClientResult{}, err
		}
		decodeErr = json.NewDecoder(response.Body).Decode(&clients)
		response.Body.Close()
		if decodeErr != nil || len(clients) == 0 {
			return keycloakClientResult{}, fmt.Errorf("read newly created Keycloak Client failed")
		}
	} else {
		internalID = clients[0].ID
	}
	if internalID == "" {
		internalID = clients[0].ID
	}
	response, err = control.request(ctx, token, stdhttp.MethodPut, "/admin/realms/"+url.PathEscape(control.realm)+"/clients/"+url.PathEscape(internalID), payload)
	if err != nil {
		return keycloakClientResult{}, err
	}
	if err := keycloakStatusError("update Keycloak Client", response, stdhttp.StatusNoContent); err != nil {
		response.Body.Close()
		return keycloakClientResult{}, err
	}
	response.Body.Close()
	if err := control.ensureClaimMappers(ctx, token, internalID, clientID); err != nil {
		return keycloakClientResult{}, err
	}
	response, err = control.request(ctx, token, stdhttp.MethodGet, "/admin/realms/"+url.PathEscape(control.realm)+"/clients/"+url.PathEscape(internalID)+"/client-secret", nil)
	if err != nil {
		return keycloakClientResult{}, err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("read Keycloak Client secret", response, stdhttp.StatusOK); err != nil {
		return keycloakClientResult{}, err
	}
	var secret struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(response.Body).Decode(&secret); err != nil {
		return keycloakClientResult{}, err
	}
	if secret.Value == "" {
		return keycloakClientResult{}, fmt.Errorf("Keycloak Client secret is empty")
	}
	return keycloakClientResult{ClientID: clientID, ClientSecret: secret.Value}, nil
}

func canonicalRedirectURIs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// EnsureClientRoles 把平台发布的角色目录镜像到目标 Keycloak 客户端。
// 角色始终按客户端命名空间隔离，合同管理里的 "admin" 角色绝不可能授予
// CRM 或项目系统的访问权限。
func (control *keycloakControlPlane) EnsureClientRoles(ctx context.Context, clientID string, roleCodes []string) error {
	token, err := control.token(ctx)
	if err != nil {
		return err
	}
	lookupPath := "/admin/realms/" + url.PathEscape(control.realm) + "/clients?clientId=" + url.QueryEscape(clientID)
	lookup, err := control.request(ctx, token, stdhttp.MethodGet, lookupPath, nil)
	if err != nil {
		return err
	}
	if err := keycloakStatusError("read Keycloak Client for roles", lookup, stdhttp.StatusOK); err != nil {
		lookup.Body.Close()
		return err
	}
	var clients []struct {
		ID string `json:"id"`
	}
	decodeErr := json.NewDecoder(lookup.Body).Decode(&clients)
	lookup.Body.Close()
	if decodeErr != nil {
		return decodeErr
	}
	if len(clients) == 0 || strings.TrimSpace(clients[0].ID) == "" {
		return fmt.Errorf("Keycloak Client %s was not found", clientID)
	}
	base := "/admin/realms/" + url.PathEscape(control.realm) + "/clients/" + url.PathEscape(clients[0].ID) + "/roles"
	response, err := control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("list Keycloak Client roles", response, stdhttp.StatusOK); err != nil {
		return err
	}
	var existing []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&existing); err != nil {
		return err
	}
	known := make(map[string]struct{}, len(existing))
	for _, role := range existing {
		known[strings.TrimSpace(role.Name)] = struct{}{}
	}
	for _, raw := range roleCodes {
		code := strings.TrimSpace(raw)
		if code == "" {
			continue
		}
		if _, exists := known[code]; exists {
			continue
		}
		created, requestErr := control.request(ctx, token, stdhttp.MethodPost, base, map[string]any{"name": code, "description": "Managed by Basic Platform"})
		if requestErr != nil {
			return requestErr
		}
		if err := keycloakStatusError("create Keycloak Client role "+code, created, stdhttp.StatusCreated, stdhttp.StatusConflict); err != nil {
			created.Body.Close()
			return err
		}
		created.Body.Close()
	}
	return nil
}

func (control *keycloakControlPlane) ensureClaimMappers(ctx context.Context, token, clientID, publicClientID string) error {
	base := "/admin/realms/" + url.PathEscape(control.realm) + "/clients/" + url.PathEscape(clientID) + "/protocol-mappers/models"
	response, err := control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	if err := keycloakStatusError("list Keycloak claim mappers", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return err
	}
	var existing []struct {
		ID             string            `json:"id"`
		Name           string            `json:"name"`
		ProtocolMapper string            `json:"protocolMapper"`
		Config         map[string]string `json:"config"`
	}
	err = json.NewDecoder(response.Body).Decode(&existing)
	response.Body.Close()
	if err != nil {
		return err
	}
	known := map[string]struct {
		ID             string
		ProtocolMapper string
		Config         map[string]string
	}{}
	for _, mapper := range existing {
		known[mapper.Name] = struct {
			ID             string
			ProtocolMapper string
			Config         map[string]string
		}{ID: mapper.ID, ProtocolMapper: mapper.ProtocolMapper, Config: mapper.Config}
	}
	managedMappers := make([]keycloakManagedProtocolMapper, 0, len(keycloakIdentityClaimMappings)+3)
	for _, claim := range keycloakIdentityClaimMappings {
		// identity_id 由下方的显式稳定 mapper 管理。遗留的通用投影 platform-identity_id
		// 曾把该属性映射到保留的 OIDC `sub` claim，覆盖了规范 subject，
		// 导致门户拒绝本应有效的授权码登录。
		if claim.Name == "identity_id" {
			continue
		}
		managedMappers = append(managedMappers, keycloakManagedProtocolMapper{
			Name:           "platform-" + claim.Name,
			ProtocolMapper: "oidc-usermodel-attribute-mapper",
			Config:         keycloakIdentityClaimMapperConfig(claim),
		})
	}
	managedMappers = append(managedMappers, keycloakAuthorizationProtocolMappers(publicClientID)...)
	managedNames := make(map[string]struct{}, len(managedMappers))
	for _, mapper := range managedMappers {
		managedNames[mapper.Name] = struct{}{}
	}

	// 移除过期的平台管控 mapper 与历史上精确的明细授权 Claim。按 claim.name 精确
	// 匹配而非按 mapper 名称模糊匹配，可以在不触碰第三方资料/角色 mapper 的
	// 前提下迁移手工命名的遗留 mapper。
	for name, current := range known {
		if _, keep := managedNames[name]; keep {
			continue
		}
		_, legacyDetailedAuthorization := legacyKeycloakAuthorizationClaimNames[strings.TrimSpace(current.Config["claim.name"])]
		if name != "platform-token-use" && !strings.HasPrefix(name, "platform-") && !legacyDetailedAuthorization {
			continue
		}
		response, err = control.request(ctx, token, stdhttp.MethodDelete, base+"/"+url.PathEscape(current.ID), nil)
		if err != nil {
			return err
		}
		if err := keycloakStatusError("delete stale Keycloak claim mapper "+name, response, stdhttp.StatusNoContent, stdhttp.StatusNotFound); err != nil {
			response.Body.Close()
			return err
		}
		response.Body.Close()
		delete(known, name)
	}

	for _, mapper := range managedMappers {
		current, exists := known[mapper.Name]
		if exists && current.ProtocolMapper == mapper.ProtocolMapper && keycloakMapperConfigMatches(current.Config, mapper.Config) {
			continue
		}
		if exists {
			response, err = control.request(ctx, token, stdhttp.MethodDelete, base+"/"+url.PathEscape(current.ID), nil)
			if err != nil {
				return err
			}
			if err := keycloakStatusError("delete drifted Keycloak mapper "+mapper.Name, response, stdhttp.StatusNoContent, stdhttp.StatusNotFound); err != nil {
				response.Body.Close()
				return err
			}
			response.Body.Close()
		}
		response, err = control.request(ctx, token, stdhttp.MethodPost, base, map[string]any{"name": mapper.Name, "protocol": "openid-connect", "protocolMapper": mapper.ProtocolMapper, "config": mapper.Config})
		if err != nil {
			return err
		}
		if err := keycloakStatusError("create Keycloak mapper "+mapper.Name, response, stdhttp.StatusCreated); err != nil {
			response.Body.Close()
			return err
		}
		response.Body.Close()
	}
	return nil
}

func keycloakAuthorizationProtocolMappers(publicClientID string) []keycloakManagedProtocolMapper {
	return []keycloakManagedProtocolMapper{
		{
			// 让稳定平台身份同时以其显式契约名 identity_id 与 OIDC sub 两个键可用：
			// 既有依赖方使用 identity_id，sub 保持为规范 OIDC subject。
			Name:           "platform-stable-identity-id",
			ProtocolMapper: "oidc-usermodel-attribute-mapper",
			Config: map[string]string{
				"user.attribute": "identity_id", "claim.name": "identity_id", "jsonType.label": "String", "multivalued": "false",
				"id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true", "introspection.token.claim": "true",
			},
		},
		{
			Name:           "platform-token-use-id",
			ProtocolMapper: "oidc-hardcoded-claim-mapper",
			Config: map[string]string{
				"claim.name": "token_use", "claim.value": "id_token", "claim.value.type": "String",
				"id.token.claim": "true", "access.token.claim": "false", "userinfo.token.claim": "false",
			},
		},
		{
			Name:           "platform-token-use-access",
			ProtocolMapper: "oidc-hardcoded-claim-mapper",
			Config: map[string]string{
				"claim.name": "token_use", "claim.value": "access_token", "claim.value.type": "String",
				"id.token.claim": "false", "access.token.claim": "true", "userinfo.token.claim": "false",
			},
		},
		{
			Name:           "platform-client-audience",
			ProtocolMapper: "oidc-audience-mapper",
			Config: map[string]string{
				"included.custom.audience": publicClientID,
				"id.token.claim":           "false",
				"access.token.claim":       "true",
			},
		},
	}
}

func keycloakIdentityClaimMapperConfig(claim keycloakIdentityClaimMapping) map[string]string {
	claimName := claim.ClaimName
	if claimName == "" {
		claimName = claim.Name
	}
	config := map[string]string{"user.attribute": claim.Name, "claim.name": claimName, "jsonType.label": "String", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true", "multivalued": "false"}
	if claim.MultiValued {
		config["multivalued"] = "true"
		config["jsonType.label"] = "JSON"
	}
	return config
}

func keycloakMapperConfigMatches(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

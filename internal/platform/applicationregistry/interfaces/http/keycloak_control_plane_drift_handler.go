// keycloak_control_plane_drift_handler.go：漂移诊断（drift）——只读比对平台期望状态与 Keycloak 实际状态并产出漂移报告。
package http

import (
	"context"
	"encoding/json"
	"fmt"
	stdhttp "net/http"
	"net/url"
	"sort"
	"strings"
)

// KeycloakDriftReport 描述单个子系统客户端在"平台期望状态"与"Keycloak 实际状态"
// 之间检测到的配置差异，各布尔字段即对应检查项的符合性。
type KeycloakDriftReport struct {
	ClientID       string   `json:"client_id"`
	HasDrift       bool     `json:"has_drift"`
	MissingRoles   []string `json:"missing_roles,omitempty"`
	StaleRoles     []string `json:"stale_roles,omitempty"`
	MissingMappers []string `json:"missing_mappers,omitempty"`
	DriftedMappers []string `json:"drifted_mappers,omitempty"`
	RedirectURIOK  bool     `json:"redirect_uri_ok"`
	BrokerConfigOK bool     `json:"broker_config_ok"`
}

func brokerAliasForClient(clientID string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(clientID)), "customer_portal-") {
		return "basic-platform-customer"
	}
	return "basic-platform"
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

// DetectSubsystemKeycloakDrift 将子系统客户端的期望 Keycloak 配置与实际状态比对，检查：
//   - 客户端存在性与回调 URI
//   - 角色目录完整性
//   - 协议 mapper 的存在与配置
//   - Broker IdP 配置完整性
//
// 本方法只读，不修改任何 Keycloak 状态；结果仅用于决定是否触发调和。
func (control *keycloakControlPlane) DetectSubsystemKeycloakDrift(ctx context.Context, clientID, expectedRedirectURI string, expectedRoleCodes []string) (KeycloakDriftReport, error) {
	report := KeycloakDriftReport{ClientID: clientID, RedirectURIOK: true, BrokerConfigOK: true}
	token, err := control.token(ctx)
	if err != nil {
		return report, err
	}
	// 检查客户端是否存在。
	lookupPath := "/admin/realms/" + url.PathEscape(control.realm) + "/clients?clientId=" + url.QueryEscape(clientID)
	response, err := control.request(ctx, token, stdhttp.MethodGet, lookupPath, nil)
	if err != nil {
		return report, err
	}
	if err := keycloakStatusError("read Keycloak Client for drift check", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return report, err
	}
	var clients []struct {
		ID           string   `json:"id"`
		RedirectURIs []string `json:"redirectUris"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&clients)
	response.Body.Close()
	if decodeErr != nil {
		return report, fmt.Errorf("decode Keycloak Client for drift check: %w", decodeErr)
	}
	if len(clients) == 0 {
		report.HasDrift = true
		return report, nil
	}
	internalID := clients[0].ID
	// 检查回调 URI。
	if expectedRedirectURI != "" {
		found := false
		for _, uri := range clients[0].RedirectURIs {
			if uri == expectedRedirectURI {
				found = true
				break
			}
		}
		if !found {
			report.RedirectURIOK = false
			report.HasDrift = true
		}
	}
	// 检查角色目录。
	rolePath := "/admin/realms/" + url.PathEscape(control.realm) + "/clients/" + url.PathEscape(internalID) + "/roles"
	response, err = control.request(ctx, token, stdhttp.MethodGet, rolePath, nil)
	if err != nil {
		return report, err
	}
	if err := keycloakStatusError("read Keycloak Client roles for drift check", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return report, err
	}
	var existingRoles []struct {
		Name string `json:"name"`
	}
	decodeRolesErr := json.NewDecoder(response.Body).Decode(&existingRoles)
	response.Body.Close()
	if decodeRolesErr != nil {
		return report, fmt.Errorf("decode Keycloak Client roles for drift check: %w", decodeRolesErr)
	}
	existingRoleSet := make(map[string]struct{}, len(existingRoles))
	for _, role := range existingRoles {
		existingRoleSet[role.Name] = struct{}{}
	}
	for _, code := range expectedRoleCodes {
		if _, exists := existingRoleSet[code]; !exists {
			report.MissingRoles = append(report.MissingRoles, code)
			report.HasDrift = true
		}
	}
	expectedRoleSet := make(map[string]struct{}, len(expectedRoleCodes))
	for _, code := range expectedRoleCodes {
		expectedRoleSet[code] = struct{}{}
	}
	for name := range existingRoleSet {
		if _, expected := expectedRoleSet[name]; !expected {
			report.StaleRoles = append(report.StaleRoles, name)
			report.HasDrift = true
		}
	}
	sort.Strings(report.MissingRoles)
	sort.Strings(report.StaleRoles)
	// 按名称与配置双维度比较平台管控的协议 mapper；
	// 名称相同但 claim 已变的 mapper 仍视为漂移。
	mapperPath := "/admin/realms/" + url.PathEscape(control.realm) + "/clients/" + url.PathEscape(internalID) + "/protocol-mappers/models"
	response, err = control.request(ctx, token, stdhttp.MethodGet, mapperPath, nil)
	if err != nil {
		return report, err
	}
	if err := keycloakStatusError("read Keycloak Client mappers for drift check", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return report, err
	}
	var existingMappers []struct {
		ID             string            `json:"id"`
		Name           string            `json:"name"`
		ProtocolMapper string            `json:"protocolMapper"`
		Config         map[string]string `json:"config"`
	}
	decodeMappersErr := json.NewDecoder(response.Body).Decode(&existingMappers)
	response.Body.Close()
	if decodeMappersErr != nil {
		return report, fmt.Errorf("decode Keycloak Client mappers for drift check: %w", decodeMappersErr)
	}
	actualMappers := make(map[string]struct {
		ProtocolMapper string
		Config         map[string]string
	}, len(existingMappers))
	for _, mapper := range existingMappers {
		actualMappers[mapper.Name] = struct {
			ProtocolMapper string
			Config         map[string]string
		}{mapper.ProtocolMapper, mapper.Config}
	}
	for _, expected := range keycloakAuthorizationProtocolMappers(clientID) {
		actual, ok := actualMappers[expected.Name]
		if !ok {
			report.MissingMappers = append(report.MissingMappers, expected.Name)
			report.HasDrift = true
			continue
		}
		if actual.ProtocolMapper != expected.ProtocolMapper || !mapsEqual(actual.Config, expected.Config) {
			report.DriftedMappers = append(report.DriftedMappers, expected.Name)
			report.HasDrift = true
		}
	}
	sort.Strings(report.MissingMappers)
	sort.Strings(report.DriftedMappers)
	// 检查 broker 配置。
	brokerAlias := brokerAliasForClient(clientID)
	response, err = control.request(ctx, token, stdhttp.MethodGet, "/admin/realms/"+url.PathEscape(control.realm)+"/identity-provider/instances/"+url.PathEscape(brokerAlias), nil)
	if err != nil {
		// 管理 API 不可达不等同于“配置正常”。向上返回错误，让健康仪表盘明确显示
		// DRIFT_DETECTION_FAILED，禁止用默认 BrokerConfigOK=true 制造假绿状态。
		return report, fmt.Errorf("read Keycloak Broker for drift check: %w", err)
	}
	if err := keycloakStatusError("read Keycloak Broker for drift check", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return report, err
	}
	var broker struct {
		Config map[string]string `json:"config"`
	}
	decodeBrokerErr := json.NewDecoder(response.Body).Decode(&broker)
	response.Body.Close()
	if decodeBrokerErr != nil {
		return report, fmt.Errorf("decode Keycloak Broker for drift check: %w", decodeBrokerErr)
	}
	requiredKeys := []string{"clientId", "clientSecret", "tokenUrl", "userInfoUrl", "authorizationUrl"}
	for _, key := range requiredKeys {
		if strings.TrimSpace(broker.Config[key]) == "" {
			report.BrokerConfigOK = false
			report.HasDrift = true
			break
		}
	}
	return report, nil
}

// keycloak_control_plane_realm_handler.go：Realm 供给与安全基线（realm）——确保 Realm 存在并把事件审计与失败锁定设置调和到平台基线。
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	stdhttp "net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	keycloakDefaultRealmEventsExpirationSeconds = int64(30 * 24 * 60 * 60)
	keycloakRealmMaxLoginFailures               = int64(5)
	keycloakRealmTemporaryLockSeconds           = int64(15 * 60)
	keycloakRealmSSOIdleTimeoutSeconds          = int64(30 * 60)
)

func (control *keycloakControlPlane) ensureRealm(ctx context.Context, token string) error {
	response, err := control.request(ctx, token, stdhttp.MethodGet, "/admin/realms/"+url.PathEscape(control.realm), nil)
	if err != nil {
		return err
	}
	if response.StatusCode == stdhttp.StatusOK {
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			return readErr
		}
		// 兼容的 Admin API 代理可能返回 200 却不携带 Realm 表示。保持接入兼容；
		// 待完整响应可用时由调和流程强制设置项。
		if len(bytes.TrimSpace(body)) == 0 {
			return nil
		}
		var realm map[string]any
		if err := json.Unmarshal(body, &realm); err != nil {
			return err
		}
		changed, updatePayload := realmEventSettingsPatch(realm)
		if !changed {
			return nil
		}
		response, err = control.request(ctx, token, stdhttp.MethodPut, "/admin/realms/"+url.PathEscape(control.realm), updatePayload)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if err := keycloakStatusError("update Keycloak Realm", response, stdhttp.StatusNoContent); err != nil {
			return err
		}
		return nil
	}
	if response.StatusCode != stdhttp.StatusNotFound {
		defer response.Body.Close()
		return keycloakStatusError("read Keycloak Realm", response, stdhttp.StatusNotFound)
	}
	response.Body.Close()
	response, err = control.request(ctx, token, stdhttp.MethodPost, "/admin/realms", map[string]any{
		"realm": control.realm, "enabled": true,
		"registrationAllowed": false, "verifyEmail": false,
		"bruteForceProtected": true, "permanentLockout": false,
		"failureFactor": keycloakRealmMaxLoginFailures, "waitIncrementSeconds": keycloakRealmTemporaryLockSeconds,
		"maxFailureWaitSeconds": keycloakRealmTemporaryLockSeconds,
		"ssoSessionIdleTimeout": keycloakRealmSSOIdleTimeoutSeconds,
		"eventsEnabled":         true, "adminEventsEnabled": true, "eventsExpiration": keycloakDefaultRealmEventsExpirationSeconds,
	})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("create Keycloak Realm", response, stdhttp.StatusCreated, stdhttp.StatusConflict); err != nil {
		return err
	}
	return nil
}

func realmEventSettingsPatch(realm map[string]any) (bool, map[string]any) {
	if realm == nil {
		return false, nil
	}
	payload := make(map[string]any, 12)
	changed := false
	payload["registrationAllowed"] = false
	payload["verifyEmail"] = false
	// Realm 关闭公开注册，并由 Keycloak 在认证入口统一执行失败锁定，
	// 避免各子系统分别统计失败次数而产生可绕过的安全边界。
	payload["bruteForceProtected"] = true
	payload["permanentLockout"] = false
	payload["failureFactor"] = keycloakRealmMaxLoginFailures
	payload["waitIncrementSeconds"] = keycloakRealmTemporaryLockSeconds
	payload["maxFailureWaitSeconds"] = keycloakRealmTemporaryLockSeconds
	payload["ssoSessionIdleTimeout"] = keycloakRealmSSOIdleTimeoutSeconds
	payload["eventsEnabled"] = true
	payload["adminEventsEnabled"] = true
	payload["eventsExpiration"] = keycloakDefaultRealmEventsExpirationSeconds
	if !asBool(realm["eventsEnabled"]) {
		changed = true
	}
	if !asBool(realm["adminEventsEnabled"]) {
		changed = true
	}
	if !asPositiveInt64(realm["eventsExpiration"]) {
		changed = true
	}
	if current, ok := realm["registrationAllowed"].(bool); !ok || current {
		changed = true
	}
	if current, ok := realm["verifyEmail"].(bool); !ok || current {
		changed = true
	}
	if !asBool(realm["bruteForceProtected"]) {
		changed = true
	}
	if current, ok := realm["permanentLockout"].(bool); !ok || current {
		changed = true
	}
	if !asInt64Equal(realm["failureFactor"], keycloakRealmMaxLoginFailures) {
		changed = true
	}
	if !asInt64Equal(realm["waitIncrementSeconds"], keycloakRealmTemporaryLockSeconds) {
		changed = true
	}
	if !asInt64Equal(realm["maxFailureWaitSeconds"], keycloakRealmTemporaryLockSeconds) {
		changed = true
	}
	if !asInt64Equal(realm["ssoSessionIdleTimeout"], keycloakRealmSSOIdleTimeoutSeconds) {
		changed = true
	}
	if changed {
		return true, payload
	}
	return false, nil
}

func asInt64Equal(value any, expected int64) bool {
	switch current := value.(type) {
	case int:
		return int64(current) == expected
	case int64:
		return current == expected
	case int32:
		return int64(current) == expected
	case int16:
		return int64(current) == expected
	case int8:
		return int64(current) == expected
	case float64:
		return current == float64(expected)
	case float32:
		return current == float32(expected)
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(current), 10, 64)
		return err == nil && parsed == expected
	}
	return false
}

func asPositiveInt64(value any) bool {
	switch current := value.(type) {
	case int:
		return current > 0
	case int64:
		return current > 0
	case int32:
		return int64(current) > 0
	case int16:
		return int64(current) > 0
	case int8:
		return int64(current) > 0
	case float64:
		return int64(current) > 0
	case float32:
		return int64(current) > 0
	case string:
		trimmed := strings.TrimSpace(current)
		if trimmed == "" {
			return false
		}
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		return err == nil && parsed > 0
	}
	return false
}

func asBool(value any) bool {
	if value == nil {
		return false
	}
	current, ok := value.(bool)
	if !ok {
		return false
	}
	return current
}

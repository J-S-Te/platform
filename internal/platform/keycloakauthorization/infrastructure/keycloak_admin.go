package infrastructure

// 本适配器刻意保持精简：Basic Platform 始终是事实源，Keycloak 只经其 Admin API
// 接收可重试安全的投影。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
)

const (
	platformGroupsRoot  = "basic-platform"
	platformBrokerAlias = "basic-platform"
)

// KeycloakAdmin 实现授权投影的 Keycloak Admin API 边界。所提供的管理员凭据
// 必须对配置的 realm 具有 realm-management 权限。
type KeycloakAdmin struct {
	adminURL, realm, username, password, clientID, clientSecret string
	httpClient                                                  *stdhttp.Client
}

// KeycloakAdminCredentials 显式提供 Keycloak Admin API 使用的凭据。依据控制面
// 凭据策略，优先使用完整的 service-account 对，而非遗留的 username/password grant。
// 这些值仅作为进程内输入，本适配器绝不回传。
type KeycloakAdminCredentials struct {
	ServiceAccountClientID     string
	ServiceAccountClientSecret string
	Username                   string
	Password                   string
}

// NewKeycloakAdmin 保留既有调用方使用的 username/password 构造方式。
// 新投影代码应使用 NewKeycloakAdminWithCredentials，使依赖显式化。
func NewKeycloakAdmin(adminURL, realm, username, password string, clients ...*stdhttp.Client) (*KeycloakAdmin, error) {
	return NewKeycloakAdminWithCredentials(adminURL, realm, KeycloakAdminCredentials{
		Username: username,
		Password: password,
	}, clients...)
}

// NewKeycloakAdminWithCredentials 用显式凭据构造 Admin API 适配器。它刻意不读
// 进程环境变量，使 Worker 与 API 的组装保持确定性，并支持在不引入隐藏依赖的
// 情况下轮换凭据。提供 client 便于测试以及需要自定义传输配置的调用方。
func NewKeycloakAdminWithCredentials(adminURL, realm string, credentials KeycloakAdminCredentials, clients ...*stdhttp.Client) (*KeycloakAdmin, error) {
	if len(clients) > 1 || strings.TrimSpace(adminURL) == "" || strings.Trim(strings.TrimSpace(realm), "/") == "" {
		return nil, errors.New("Keycloak admin configuration is invalid")
	}
	client := &stdhttp.Client{Timeout: 12 * time.Second}
	if len(clients) == 1 {
		if clients[0] == nil {
			return nil, errors.New("Keycloak admin HTTP client must not be nil")
		}
		client = clients[0]
	}
	admin := &KeycloakAdmin{
		adminURL:     strings.TrimRight(strings.TrimSpace(adminURL), "/"),
		realm:        strings.Trim(strings.TrimSpace(realm), "/"),
		username:     strings.TrimSpace(credentials.Username),
		password:     credentials.Password,
		clientID:     strings.TrimSpace(credentials.ServiceAccountClientID),
		clientSecret: strings.TrimSpace(credentials.ServiceAccountClientSecret),
		httpClient:   client,
	}
	if (admin.clientID == "") != (admin.clientSecret == "") {
		return nil, errors.New("Keycloak admin client credentials must be configured as a complete pair")
	}
	if admin.clientID == "" && (admin.username == "" || strings.TrimSpace(admin.password) == "") {
		return nil, errors.New("Keycloak admin configuration is invalid")
	}
	return admin, nil
}

// ListKeycloakAuditEvents 只读取标准的用户 LOGIN/LOGOUT 事件流和 realm 管理事件流。
// 原始 Keycloak JSON 不会越过 infrastructure 边界暴露；调用方收到的是刻意脱敏的
// 事件投影，可直接追加进平台审计日志。
func (admin *KeycloakAdmin) ListKeycloakAuditEvents(ctx context.Context, since time.Time) ([]projectionapplication.KeycloakAuditEvent, error) {
	token, err := admin.token(ctx)
	if err != nil {
		return nil, err
	}
	// Admin API 请求保持与已部署 Keycloak 实例支持的最小端点一致。
	// 分页与时间窗都在本地处理，避免版本相关的查询参数路由差异。
	userEvents, err := admin.listAllEventPages(ctx, token, "/events")
	if err != nil {
		// 事件采集是补充能力。部分已部署的 Keycloak 网关对 Admin API 客户端访问
		// event 资源返回 404，而 realm 与 token 端点可用。把这种变体视为空审计窗，
		// 使其不能中断投影或 Broker 流程。
		if strings.Contains(err.Error(), "returned HTTP 404") {
			return []projectionapplication.KeycloakAuditEvent{}, nil
		}
		return nil, fmt.Errorf("list Keycloak user audit events (admin_url=%q realm=%q): %w", admin.adminURL, admin.realm, err)
	}
	// 用户 LOGIN/LOGOUT 事件是 Broker 验证的必需证据。管理员事件是附加审计流：
	// 当 Keycloak 管理员没有 admin-event 权限时可能不可用（部分部署以 404 表现
	// 该状态）。绝不能因为可选的管理员流不可用就丢弃有效的登录证据。
	adminEvents, adminEventsErr := admin.listAllEventPages(ctx, token, "/admin-events")
	if adminEventsErr != nil {
		adminEvents = nil
	}
	items := make([]projectionapplication.KeycloakAuditEvent, 0, len(userEvents)+len(adminEvents))
	for _, event := range userEvents {
		kind, _ := event["type"].(string)
		if kind != "LOGIN" && kind != "LOGOUT" {
			continue
		}
		occurredAt := keycloakEventTime(event["time"])
		if !occurredAt.IsZero() && occurredAt.Before(since.UTC()) {
			continue
		}
		items = append(items, projectionapplication.KeycloakAuditEvent{EventID: stringValue(event["id"]), Category: "LOGIN", Type: kind, SubjectID: stringValue(event["userId"]), SessionID: stringValue(event["sessionId"]), ClientID: stringValue(event["clientId"]), SourceIP: stringValue(event["ipAddress"]), OccurredAt: occurredAt})
	}
	for _, event := range adminEvents {
		occurredAt := keycloakEventTime(event["time"])
		if !occurredAt.IsZero() && occurredAt.Before(since.UTC()) {
			continue
		}
		details, _ := event["authDetails"].(map[string]any)
		items = append(items, projectionapplication.KeycloakAuditEvent{EventID: stringValue(event["id"]), Category: "ADMIN", Type: "ADMIN", SubjectID: stringValue(details["userId"]), ClientID: stringValue(details["clientId"]), SourceIP: stringValue(details["ipAddress"]), ResourceType: stringValue(event["resourceType"]), ResourcePath: stringValue(event["resourcePath"]), OperationType: stringValue(event["operationType"]), OccurredAt: occurredAt})
	}
	return items, nil
}

// eventPageSize 是单页上限；Keycloak Admin API 的默认页大小可能截断窗口，
// 必须显式分页直到取到不足一页为止，否则停机/高峰后的事件会被静默丢弃。
const eventPageSize = 100

// listAllEventPages 按 first/max 循环取完整事件流（AUD-IP-2026 批次 3）。
func (admin *KeycloakAdmin) listAllEventPages(ctx context.Context, token, suffix string) ([]map[string]any, error) {
	items := make([]map[string]any, 0, eventPageSize)
	for first := 0; first < 10000; first += eventPageSize {
		separator := "?"
		if strings.Contains(suffix, "?") {
			separator = "&"
		}
		page, err := admin.listEventPage(ctx, token, fmt.Sprintf("%s%sfirst=%d&max=%d", suffix, separator, first, eventPageSize))
		if err != nil {
			return nil, err
		}
		items = append(items, page...)
		if len(page) < eventPageSize {
			return items, nil
		}
	}
	return items, nil
}

func (admin *KeycloakAdmin) listEventPage(ctx context.Context, token, suffix string) ([]map[string]any, error) {
	// request 已经为每个 Admin API 路径加了 /admin/realms/{realm} 前缀；这里若再传
	// 全限定路径，实际会请求 /admin/realms/{realm}/admin/realms/{realm}/events，
	// 恒返回 404。
	response, err := admin.request(ctx, token, stdhttp.MethodGet, suffix, nil)
	path := "/admin/realms/" + url.PathEscape(admin.realm) + suffix
	if err != nil {
		return nil, fmt.Errorf("request path %q: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != stdhttp.StatusOK {
		return nil, fmt.Errorf("list Keycloak audit events path %q returned HTTP %d", path, response.StatusCode)
	}
	var items []map[string]any
	if err := json.NewDecoder(response.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("decode Keycloak audit events: %w", err)
	}
	return items, nil
}

func stringValue(value any) string { item, _ := value.(string); return strings.TrimSpace(item) }
func keycloakEventTime(value any) time.Time {
	switch timestamp := value.(type) {
	case float64:
		return time.UnixMilli(int64(timestamp)).UTC()
	case int64:
		return time.UnixMilli(timestamp).UTC()
	case json.Number:
		millis, _ := timestamp.Int64()
		return time.UnixMilli(millis).UTC()
	default:
		return time.Time{}
	}
}

// EnsureUser 创建或更新与平台身份对齐的 Keycloak 用户：优先复用经由 Broker 登录
// 先行创建、且确属该上游 subject 的既有账号；冲突或归属其他租户时显式失败，
// 绝不合并账号。启用状态与展示属性以快照为准。
func (admin *KeycloakAdmin) EnsureUser(ctx context.Context, snapshot projectionapplication.Snapshot) error {
	// 用户以平台 identity_id 对齐；Keycloak 内部用户 ID 只作为外部资源句柄使用。
	token, err := admin.token(ctx)
	if err != nil {
		return err
	}
	// 预先把上游 OIDC subject 关联到投影出的 Keycloak 用户，使 Broker 登录能直接
	// 解析到既有账号，而不是触发 Keycloak 的 just-in-time 注册/资料补全流程。
	brokerAlias := platformBrokerAlias
	if strings.EqualFold(strings.TrimSpace(snapshot.ApplicationCode), "customer_portal") {
		brokerAlias = "basic-platform-customer"
	}
	// 此前的 Broker 登录可能先于平台投影创建 Keycloak 用户。只有当确切的该上游
	// subject 持有该关联、且账号尚未归属其他平台身份或租户时才允许复用，
	// 同时避免重复 Keycloak 账号与不安全的跨租户账号合并。
	ownerID, err := admin.findUserByBrokerIdentity(ctx, token, snapshot.IdentityID, brokerAlias)
	if err != nil {
		return err
	}
	if ownerID != "" {
		users, findErr := admin.findUsersByIdentity(ctx, token, snapshot.IdentityID)
		if findErr != nil {
			return findErr
		}
		if len(users) > 1 || (len(users) == 1 && users[0].ID != ownerID) {
			return fmt.Errorf("Keycloak Broker identity %q conflicts with the platform identity projection", snapshot.IdentityID)
		}
		user, readErr := admin.readUser(ctx, token, ownerID)
		if readErr != nil {
			return readErr
		}
		if !compatibleBrokerUser(user, snapshot) {
			return fmt.Errorf("Keycloak Broker identity %q is linked to a user owned by another platform identity or tenant", snapshot.IdentityID)
		}
		user.Enabled = snapshot.UserEnabled
		user.FirstName = strings.TrimSpace(snapshot.DisplayName)
		user.Email = strings.TrimSpace(snapshot.Email)
		user.Attributes = cloneAttributes(user.Attributes)
		setAuthorizationAttributes(user.Attributes, snapshot)
		return admin.updateUser(ctx, token, user)
	}
	user, err := admin.ensureUser(ctx, token, snapshot)
	if err != nil {
		return err
	}
	return admin.ensureBrokerIdentityWithAlias(ctx, token, user, snapshot.IdentityID, brokerAlias)
}

func compatibleBrokerUser(user keycloakUser, snapshot projectionapplication.Snapshot) bool {
	if identityID := strings.TrimSpace(strings.Join(user.Attributes["identity_id"], "")); identityID != "" && identityID != strings.TrimSpace(snapshot.IdentityID) {
		return false
	}
	if tenantID := strings.TrimSpace(strings.Join(user.Attributes["tenant_id"], "")); tenantID != "" && tenantID != strings.TrimSpace(snapshot.TenantID) {
		return false
	}
	return true
}

// ReadAccountStatus 读取某平台身份在 Keycloak 侧管理的用户状态。返回前会校验
// 租户属性，保证调用方无法借这条读取路径匹配其他租户的身份。
// available=false 表示不存在唯一映射的 Keycloak 用户，它不代表"已启用"。
func (admin *KeycloakAdmin) ReadAccountStatus(ctx context.Context, tenantID, identityID string) (status string, available bool, err error) {
	tenantID = strings.TrimSpace(tenantID)
	identityID = strings.TrimSpace(identityID)
	if tenantID == "" || identityID == "" {
		return "", false, errors.New("Keycloak account status identity is invalid")
	}
	token, err := admin.token(ctx)
	if err != nil {
		return "", false, err
	}
	users, err := admin.findUsersByIdentity(ctx, token, identityID)
	if err != nil {
		return "", false, err
	}
	if len(users) == 0 {
		// 较早投影的用户可能早于 identity_id 属性存在。其确定性用户名仍是安全、
		// 且经过租户校验的关联键。
		users, err = admin.findUsersByStableUsername(ctx, token, identityID)
		if err != nil {
			return "", false, err
		}
		if len(users) == 0 {
			return "", false, nil
		}
	}
	if len(users) > 1 {
		return "", false, fmt.Errorf("multiple Keycloak users have identity_id %q", identityID)
	}
	if !contains(users[0].Attributes["tenant_id"], tenantID) {
		return "", false, fmt.Errorf("Keycloak user identity %q does not belong to tenant %q", identityID, tenantID)
	}
	if users[0].Enabled {
		return "ACTIVE", true, nil
	}
	return "DISABLED", true, nil
}

// EnsureOrganizationGroups 把用户组织成员关系投影为平台托管命名空间内的组关系，
// 并移除快照之外的平台托管组；平台命名空间之外的组一律不触碰。
func (admin *KeycloakAdmin) EnsureOrganizationGroups(ctx context.Context, snapshot projectionapplication.Snapshot) error {
	// 仅管理 basic-platform/tenant-*/organization-* 命名空间，绝不触碰租户外的用户组。
	token, err := admin.token(ctx)
	if err != nil {
		return err
	}
	user, err := admin.ensureUser(ctx, token, snapshot)
	if err != nil {
		return err
	}
	for _, organizationID := range unique(snapshot.OrganizationIDs) {
		groupID, err := admin.ensureOrganizationGroup(ctx, token, snapshot.TenantID, organizationID)
		if err != nil {
			return err
		}
		response, err := admin.request(ctx, token, stdhttp.MethodPut, "/users/"+url.PathEscape(user.ID)+"/groups/"+url.PathEscape(groupID), nil)
		if err != nil {
			return err
		}
		status := response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusNoContent {
			return admin.statusError("add Keycloak user to organization group", status)
		}
	}
	groups, err := admin.userGroups(ctx, token, user.ID)
	if err != nil {
		return err
	}
	wanted := make(map[string]struct{}, len(unique(snapshot.OrganizationIDs)))
	for _, organizationID := range unique(snapshot.OrganizationIDs) {
		wanted[organizationID] = struct{}{}
	}
	for _, group := range groups {
		organizationID, managed := managedOrganizationID(group.Path, snapshot.TenantID)
		if !managed {
			continue
		}
		if _, keep := wanted[organizationID]; keep {
			continue
		}
		response, err := admin.request(ctx, token, stdhttp.MethodDelete, "/users/"+url.PathEscape(user.ID)+"/groups/"+url.PathEscape(group.ID), nil)
		if err != nil {
			return err
		}
		status := response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusNoContent {
			return admin.statusError("remove stale Keycloak organization group", status)
		}
	}
	return nil
}

// AssignClientRoles 把快照中的角色同步为目标 Client 的 Client Role：先确保角色
// 存在，再移除该 Client 上的陈旧平台托管映射，最后授予目标集合；不触碰其他 Client。
func (admin *KeycloakAdmin) AssignClientRoles(ctx context.Context, snapshot projectionapplication.Snapshot) error {
	// 角色变更限定在目标 Client，清理也只清理该 Client 的平台托管角色映射。
	token, err := admin.token(ctx)
	if err != nil {
		return err
	}
	user, err := admin.ensureUser(ctx, token, snapshot)
	if err != nil {
		return err
	}
	clientID, err := admin.clientInternalID(ctx, token, snapshot.KeycloakClientID)
	if err != nil {
		return err
	}
	roles, err := admin.ensureClientRoles(ctx, token, clientID, unique(snapshot.Roles))
	if err != nil {
		return err
	}
	current, err := admin.assignedClientRoles(ctx, token, user.ID, clientID)
	if err != nil {
		return err
	}
	wanted := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		if name, _ := role["name"].(string); name != "" {
			wanted[name] = struct{}{}
		}
	}
	stale := make([]map[string]any, 0)
	for _, role := range current {
		name, _ := role["name"].(string)
		if _, keep := wanted[name]; !keep {
			stale = append(stale, role)
		}
	}
	if len(stale) > 0 {
		response, err := admin.request(ctx, token, stdhttp.MethodDelete, "/users/"+url.PathEscape(user.ID)+"/role-mappings/clients/"+url.PathEscape(clientID), stale)
		if err != nil {
			return err
		}
		status := response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusNoContent {
			return admin.statusError("remove stale Keycloak Client roles", status)
		}
	}
	if len(roles) == 0 {
		return nil
	}
	response, err := admin.request(ctx, token, stdhttp.MethodPost, "/users/"+url.PathEscape(user.ID)+"/role-mappings/clients/"+url.PathEscape(clientID), roles)
	if err != nil {
		return err
	}
	status := response.StatusCode
	response.Body.Close()
	if status != stdhttp.StatusNoContent {
		return admin.statusError("assign Keycloak Client roles", status)
	}
	return nil
}

func (admin *KeycloakAdmin) assignedClientRoles(ctx context.Context, token, userID, clientID string) ([]map[string]any, error) {
	response, err := admin.request(ctx, token, stdhttp.MethodGet, "/users/"+url.PathEscape(userID)+"/role-mappings/clients/"+url.PathEscape(clientID), nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != stdhttp.StatusOK {
		return nil, admin.statusError("list Keycloak user Client roles", response.StatusCode)
	}
	var roles []map[string]any
	if err := json.NewDecoder(response.Body).Decode(&roles); err != nil {
		return nil, fmt.Errorf("decode Keycloak user Client roles: %w", err)
	}
	return roles, nil
}

// SetClientAuthorizationAttributes 把租户、身份、角色配置哈希与授权修订号等
// 稳定授权属性写入 Keycloak 用户属性；详细权限仍以基础平台为事实源。
func (admin *KeycloakAdmin) SetClientAuthorizationAttributes(ctx context.Context, snapshot projectionapplication.Snapshot) error {
	// Keycloak 用户属性只保留稳定身份和必要展示属性。角色通过
	// Client Role、组织通过 Group 投影；详细权限仍以基础平台为事实源。
	token, err := admin.token(ctx)
	if err != nil {
		return err
	}
	user, err := admin.ensureUser(ctx, token, snapshot)
	if err != nil {
		return err
	}
	attributes := cloneAttributes(user.Attributes)
	setAuthorizationAttributes(attributes, snapshot)
	user.Attributes = attributes
	return admin.updateUser(ctx, token, user)
}

type keycloakUser struct {
	ID         string              `json:"id,omitempty"`
	Username   string              `json:"username"`
	Enabled    bool                `json:"enabled"`
	FirstName  string              `json:"firstName,omitempty"`
	LastName   string              `json:"lastName,omitempty"`
	Email      string              `json:"email,omitempty"`
	Attributes map[string][]string `json:"attributes,omitempty"`
}

type keycloakFederatedIdentity struct {
	IdentityProvider string `json:"identityProvider"`
	UserID           string `json:"userId"`
	UserName         string `json:"userName"`
}

func (admin *KeycloakAdmin) ensureBrokerIdentity(ctx context.Context, token string, user keycloakUser, identityID string) error {
	return admin.ensureBrokerIdentityWithAlias(ctx, token, user, identityID, platformBrokerAlias)
}

func (admin *KeycloakAdmin) ensureBrokerIdentityWithAlias(ctx context.Context, token string, user keycloakUser, identityID, brokerAlias string) error {
	identityID = strings.TrimSpace(identityID)
	if user.ID == "" || identityID == "" {
		return errors.New("Keycloak Broker identity is invalid")
	}
	base := "/users/" + url.PathEscape(user.ID) + "/federated-identity"
	response, err := admin.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != stdhttp.StatusOK {
		status := response.StatusCode
		response.Body.Close()
		return admin.statusError("read Keycloak Broker identity", status)
	}
	var identities []keycloakFederatedIdentity
	decodeErr := json.NewDecoder(response.Body).Decode(&identities)
	response.Body.Close()
	if decodeErr != nil {
		return fmt.Errorf("decode Keycloak Broker identities: %w", decodeErr)
	}
	staleLink := false
	for _, identity := range identities {
		if identity.IdentityProvider != brokerAlias {
			continue
		}
		if identity.UserID == identityID {
			return nil
		}
		staleLink = true
		break
	}
	if staleLink {
		owner, err := admin.findUserByBrokerIdentity(ctx, token, identityID, brokerAlias)
		if err != nil {
			return err
		}
		if owner != "" && owner != user.ID {
			return fmt.Errorf("link Keycloak Broker identity: upstream identity %q is already linked to Keycloak user %q", identityID, owner)
		}
		if owner == user.ID {
			// 属主查询已经观察到期望结果，只是用户的关联列表尚未刷新。
			// 不要删除一条可能已在并发事务中收敛一致的关联。
			return nil
		}
		// 该 provider 关联属于本平台管理的 Keycloak 用户，但指向了过期的上游
		// subject。必须先证明期望的上游身份未被其他 Keycloak 用户持有，才允许修复。
		response, err = admin.request(ctx, token, stdhttp.MethodDelete, base+"/"+url.PathEscape(brokerAlias), nil)
		if err != nil {
			return err
		}
		status := response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusNoContent && status != stdhttp.StatusNotFound {
			return admin.statusError("remove stale Keycloak Broker identity", status)
		}
	}
	payload := keycloakFederatedIdentity{IdentityProvider: brokerAlias, UserID: identityID, UserName: stableUsername(identityID)}
	response, err = admin.request(ctx, token, stdhttp.MethodPost, base+"/"+url.PathEscape(brokerAlias), payload)
	if err != nil {
		return err
	}
	status := response.StatusCode
	response.Body.Close()
	if status == stdhttp.StatusConflict {
		// POST 不保证幂等。并发投影可能在我们 GET 与 POST 之间创建了完全相同的
		// 关联；必须重读权威状态，再判定 409 是否为真实的属主冲突。
		identities, readErr := admin.brokerIdentities(ctx, token, user.ID)
		if readErr != nil {
			return readErr
		}
		for _, identity := range identities {
			if identity.IdentityProvider == brokerAlias && identity.UserID == identityID {
				return nil
			}
		}
		owner, ownerErr := admin.findUserByBrokerIdentity(ctx, token, identityID, brokerAlias)
		if ownerErr != nil {
			return ownerErr
		}
		if owner != "" {
			return fmt.Errorf("link Keycloak Broker identity: upstream identity %q is already linked to Keycloak user %q", identityID, owner)
		}
		return fmt.Errorf("link Keycloak Broker identity: Keycloak returned conflict but no Broker link owns upstream identity %q", identityID)
	}
	if status != stdhttp.StatusNoContent {
		return admin.statusError("link Keycloak Broker identity", status)
	}
	identities, err = admin.brokerIdentities(ctx, token, user.ID)
	if err != nil {
		return err
	}
	for _, identity := range identities {
		if identity.IdentityProvider == brokerAlias && identity.UserID == identityID {
			return nil
		}
	}
	return fmt.Errorf("link Keycloak Broker identity: Keycloak did not persist upstream identity %q", identityID)
}

func (admin *KeycloakAdmin) brokerIdentities(ctx context.Context, token, userID string) ([]keycloakFederatedIdentity, error) {
	response, err := admin.request(ctx, token, stdhttp.MethodGet, "/users/"+url.PathEscape(userID)+"/federated-identity", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != stdhttp.StatusOK {
		return nil, admin.statusError("read Keycloak Broker identity", response.StatusCode)
	}
	var identities []keycloakFederatedIdentity
	if err := json.NewDecoder(response.Body).Decode(&identities); err != nil {
		return nil, fmt.Errorf("decode Keycloak Broker identities: %w", err)
	}
	return identities, nil
}

func (admin *KeycloakAdmin) findUserByBrokerIdentity(ctx context.Context, token, identityID, brokerAlias string) (string, error) {
	path := "/users?exact=true&idpAlias=" + url.QueryEscape(brokerAlias) + "&idpUserId=" + url.QueryEscape(identityID)
	response, err := admin.request(ctx, token, stdhttp.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != stdhttp.StatusOK {
		return "", admin.statusError("find Keycloak Broker identity owner", response.StatusCode)
	}
	var users []keycloakUser
	if err := json.NewDecoder(response.Body).Decode(&users); err != nil {
		return "", fmt.Errorf("decode Keycloak Broker identity owner: %w", err)
	}
	if len(users) > 1 {
		return "", fmt.Errorf("multiple Keycloak users own Broker identity %q", identityID)
	}
	if len(users) == 1 {
		return users[0].ID, nil
	}
	return "", nil
}

type keycloakGroup struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

func (admin *KeycloakAdmin) ensureUser(ctx context.Context, token string, snapshot projectionapplication.Snapshot) (keycloakUser, error) {
	users, err := admin.findUsersByIdentity(ctx, token, snapshot.IdentityID)
	if err != nil {
		return keycloakUser{}, err
	}
	if len(users) > 1 {
		return keycloakUser{}, fmt.Errorf("multiple Keycloak users have identity_id %q", snapshot.IdentityID)
	}
	if len(users) == 0 {
		user := keycloakUser{Username: stableUsername(snapshot.IdentityID), Enabled: snapshot.UserEnabled, FirstName: strings.TrimSpace(snapshot.DisplayName), Email: strings.TrimSpace(snapshot.Email), Attributes: map[string][]string{}}
		setAuthorizationAttributes(user.Attributes, snapshot)
		response, err := admin.request(ctx, token, stdhttp.MethodPost, "/users", user)
		if err != nil {
			return keycloakUser{}, err
		}
		status := response.StatusCode
		location := response.Header.Get("Location")
		response.Body.Close()
		if status != stdhttp.StatusCreated && status != stdhttp.StatusConflict {
			return keycloakUser{}, admin.statusError("create Keycloak user", status)
		}
		if status == stdhttp.StatusCreated && location != "" {
			user.ID = locationID(location)
			if user.ID != "" {
				return user, nil
			}
		}
		users, err = admin.findUsersByIdentity(ctx, token, snapshot.IdentityID)
		if err != nil || len(users) != 1 {
			if err != nil {
				return keycloakUser{}, err
			}
			return keycloakUser{}, fmt.Errorf("read created Keycloak user with identity_id %q", snapshot.IdentityID)
		}
		return users[0], nil
	}
	user := users[0]
	user.Enabled = snapshot.UserEnabled
	user.FirstName = strings.TrimSpace(snapshot.DisplayName)
	user.Email = strings.TrimSpace(snapshot.Email)
	user.Attributes = cloneAttributes(user.Attributes)
	setAuthorizationAttributes(user.Attributes, snapshot)
	if err := admin.updateUser(ctx, token, user); err != nil {
		return keycloakUser{}, err
	}
	return user, nil
}

func (admin *KeycloakAdmin) userGroups(ctx context.Context, token, userID string) ([]keycloakGroup, error) {
	response, err := admin.request(ctx, token, stdhttp.MethodGet, "/users/"+url.PathEscape(userID)+"/groups", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != stdhttp.StatusOK {
		return nil, admin.statusError("list Keycloak user groups", response.StatusCode)
	}
	var groups []keycloakGroup
	if err := json.NewDecoder(response.Body).Decode(&groups); err != nil {
		return nil, fmt.Errorf("decode Keycloak user groups: %w", err)
	}
	return groups, nil
}

func managedOrganizationID(path, tenantID string) (string, bool) {
	prefix := "/" + platformGroupsRoot + "/tenant-" + strings.TrimSpace(tenantID) + "/organization-"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	organizationID := strings.TrimPrefix(path, prefix)
	return organizationID, organizationID != "" && !strings.Contains(organizationID, "/")
}

func (admin *KeycloakAdmin) findUsersByIdentity(ctx context.Context, token, identityID string) ([]keycloakUser, error) {
	path := "/users?exact=true&q=" + url.QueryEscape("identity_id:"+identityID)
	response, err := admin.request(ctx, token, stdhttp.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != stdhttp.StatusOK {
		return nil, admin.statusError("find Keycloak user", response.StatusCode)
	}
	var users []keycloakUser
	if err := json.NewDecoder(response.Body).Decode(&users); err != nil {
		return nil, fmt.Errorf("decode Keycloak users: %w", err)
	}
	matched := users[:0]
	for _, user := range users {
		if contains(user.Attributes["identity_id"], identityID) {
			matched = append(matched, user)
		}
	}
	return matched, nil
}

func (admin *KeycloakAdmin) findUsersByStableUsername(ctx context.Context, token, identityID string) ([]keycloakUser, error) {
	username := stableUsername(identityID)
	path := "/users?exact=true&username=" + url.QueryEscape(username)
	response, err := admin.request(ctx, token, stdhttp.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != stdhttp.StatusOK {
		return nil, admin.statusError("find Keycloak user by stable username", response.StatusCode)
	}
	var users []keycloakUser
	if err := json.NewDecoder(response.Body).Decode(&users); err != nil {
		return nil, fmt.Errorf("decode Keycloak users by stable username: %w", err)
	}
	matched := users[:0]
	for _, user := range users {
		if user.Username == username {
			matched = append(matched, user)
		}
	}
	return matched, nil
}

func (admin *KeycloakAdmin) updateUser(ctx context.Context, token string, user keycloakUser) error {
	if user.ID == "" {
		return errors.New("Keycloak user ID is empty")
	}
	response, err := admin.request(ctx, token, stdhttp.MethodPut, "/users/"+url.PathEscape(user.ID), user)
	if err != nil {
		return err
	}
	status := response.StatusCode
	response.Body.Close()
	if status != stdhttp.StatusNoContent {
		return admin.statusError("update Keycloak user", status)
	}
	return nil
}

func (admin *KeycloakAdmin) readUser(ctx context.Context, token, userID string) (keycloakUser, error) {
	response, err := admin.request(ctx, token, stdhttp.MethodGet, "/users/"+url.PathEscape(strings.TrimSpace(userID)), nil)
	if err != nil {
		return keycloakUser{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != stdhttp.StatusOK {
		return keycloakUser{}, admin.statusError("read Keycloak Broker user", response.StatusCode)
	}
	var user keycloakUser
	if err := json.NewDecoder(response.Body).Decode(&user); err != nil {
		return keycloakUser{}, fmt.Errorf("decode Keycloak Broker user: %w", err)
	}
	if strings.TrimSpace(user.ID) == "" || user.ID != userID {
		return keycloakUser{}, errors.New("Keycloak Broker user identity is invalid")
	}
	return user, nil
}

func (admin *KeycloakAdmin) ensureOrganizationGroup(ctx context.Context, token, tenantID, organizationID string) (string, error) {
	rootID, err := admin.ensureGroup(ctx, token, "", platformGroupsRoot)
	if err != nil {
		return "", err
	}
	tenantID, err = admin.ensureGroup(ctx, token, rootID, "tenant-"+tenantID)
	if err != nil {
		return "", err
	}
	return admin.ensureGroup(ctx, token, tenantID, "organization-"+organizationID)
}

func (admin *KeycloakAdmin) ensureGroup(ctx context.Context, token, parentID, name string) (string, error) {
	path := "/groups?search=" + url.QueryEscape(name) + "&exact=true"
	if parentID != "" {
		path = "/groups/" + url.PathEscape(parentID) + "/children?search=" + url.QueryEscape(name) + "&exact=true"
	}
	response, err := admin.request(ctx, token, stdhttp.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	var groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&groups)
	status := response.StatusCode
	response.Body.Close()
	if status != stdhttp.StatusOK {
		return "", admin.statusError("find Keycloak group", status)
	}
	if decodeErr != nil {
		return "", fmt.Errorf("decode Keycloak groups: %w", decodeErr)
	}
	for _, group := range groups {
		if group.Name == name && group.ID != "" {
			return group.ID, nil
		}
	}
	createPath := "/groups"
	if parentID != "" {
		createPath = "/groups/" + url.PathEscape(parentID) + "/children"
	}
	response, err = admin.request(ctx, token, stdhttp.MethodPost, createPath, map[string]string{"name": name})
	if err != nil {
		return "", err
	}
	status = response.StatusCode
	location := response.Header.Get("Location")
	response.Body.Close()
	if status != stdhttp.StatusCreated && status != stdhttp.StatusConflict {
		return "", admin.statusError("create Keycloak group", status)
	}
	if status == stdhttp.StatusCreated && locationID(location) != "" {
		return locationID(location), nil
	}
	return admin.ensureGroup(ctx, token, parentID, name)
}

func (admin *KeycloakAdmin) clientInternalID(ctx context.Context, token, clientID string) (string, error) {
	response, err := admin.request(ctx, token, stdhttp.MethodGet, "/clients?clientId="+url.QueryEscape(clientID), nil)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != stdhttp.StatusOK {
		return "", admin.statusError("find Keycloak Client", response.StatusCode)
	}
	var clients []struct {
		ID       string `json:"id"`
		ClientID string `json:"clientId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&clients); err != nil {
		return "", fmt.Errorf("decode Keycloak Clients: %w", err)
	}
	for _, client := range clients {
		if client.ClientID == clientID && client.ID != "" {
			return client.ID, nil
		}
	}
	return "", fmt.Errorf("Keycloak Client %q was not found", clientID)
}

func (admin *KeycloakAdmin) ensureClientRoles(ctx context.Context, token, clientID string, names []string) ([]map[string]any, error) {
	base := "/clients/" + url.PathEscape(clientID) + "/roles"
	response, err := admin.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return nil, err
	}
	var existing []map[string]any
	decodeErr := json.NewDecoder(response.Body).Decode(&existing)
	status := response.StatusCode
	response.Body.Close()
	if status != stdhttp.StatusOK {
		return nil, admin.statusError("list Keycloak Client roles", status)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decode Keycloak Client roles: %w", decodeErr)
	}
	byName := make(map[string]map[string]any, len(existing))
	for _, role := range existing {
		if name, _ := role["name"].(string); name != "" {
			byName[name] = role
		}
	}
	for _, name := range names {
		if _, exists := byName[name]; exists {
			continue
		}
		response, err = admin.request(ctx, token, stdhttp.MethodPost, base, map[string]string{"name": name, "description": "Managed by Basic Platform"})
		if err != nil {
			return nil, err
		}
		status = response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusCreated && status != stdhttp.StatusConflict {
			return nil, admin.statusError("create Keycloak Client role", status)
		}
		response, err = admin.request(ctx, token, stdhttp.MethodGet, base+"/"+url.PathEscape(name), nil)
		if err != nil {
			return nil, err
		}
		var role map[string]any
		decodeErr = json.NewDecoder(response.Body).Decode(&role)
		status = response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusOK {
			return nil, admin.statusError("read created Keycloak Client role", status)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decode created Keycloak Client role %q: %w", name, decodeErr)
		}
		if role["id"] == "" {
			return nil, fmt.Errorf("created Keycloak Client role %q has no ID", name)
		}
		byName[name] = role
	}
	roles := make([]map[string]any, 0, len(names))
	for _, name := range names {
		roles = append(roles, byName[name])
	}
	return roles, nil
}

func (admin *KeycloakAdmin) token(ctx context.Context) (string, error) {
	// 管理凭据只在此边界换取短期令牌，令牌不会进入快照、outbox 或业务上下文。
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {admin.username}, "password": {admin.password}}
	if admin.clientID != "" && admin.clientSecret != "" {
		form = url.Values{"grant_type": {"client_credentials"}, "client_id": {admin.clientID}, "client_secret": {admin.clientSecret}}
	}
	request, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, admin.adminURL+"/realms/master/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build Keycloak token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := admin.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("request Keycloak admin token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != stdhttp.StatusOK {
		return "", admin.statusError("request Keycloak admin token", response.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode Keycloak admin token: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", errors.New("Keycloak admin token response has no access_token")
	}
	return payload.AccessToken, nil
}

func (admin *KeycloakAdmin) request(ctx context.Context, token, method, path string, body any) (*stdhttp.Response, error) {
	// 所有 Admin API 请求集中经过此处，统一限制为已认证的 Keycloak 管理边界。
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode Keycloak request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := stdhttp.NewRequestWithContext(ctx, method, admin.adminURL+"/admin/realms/"+url.PathEscape(admin.realm)+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build Keycloak Admin API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := admin.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request Keycloak Admin API: %w", err)
	}
	return response, nil
}

func (admin *KeycloakAdmin) statusError(operation string, status int) error {
	return fmt.Errorf("%s returned HTTP %d", operation, status)
}

func setAuthorizationAttributes(attributes map[string][]string, snapshot projectionapplication.Snapshot) {
	removeManagedAuthorizationAttributes(attributes)
	setStableKeycloakAttribute(attributes, "identity_id", snapshot.IdentityID)
	setStableKeycloakAttribute(attributes, "tenant_id", snapshot.TenantID)
	setStableKeycloakAttribute(attributes, "person_id", snapshot.PersonID)
	setStableKeycloakAttribute(attributes, "primary_org_id", snapshot.PrimaryOrganizationID)
}

func setStableKeycloakAttribute(attributes map[string][]string, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		delete(attributes, key)
		return
	}
	attributes[key] = []string{value}
}

func removeManagedAuthorizationAttributes(attributes map[string][]string) {
	for key := range attributes {
		if isManagedAuthorizationAttribute(key) {
			delete(attributes, key)
		}
	}
}

func isManagedAuthorizationAttribute(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "roles", "permissions", "organization_ids", "role_config_hash", "authz_revision":
		return true
	}
	if !strings.HasPrefix(key, "client_") {
		return false
	}
	for _, suffix := range []string{"_roles", "_permissions", "_organization_ids", "_role_config_hash", "_authz_revision"} {
		if strings.HasSuffix(key, suffix) && len(key) > len("client_")+len(suffix) {
			return true
		}
	}
	return false
}

func cloneAttributes(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source)+14)
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}
func unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			if _, exists := seen[value]; !exists {
				seen[value] = struct{}{}
				result = append(result, value)
			}
		}
	}
	return result
}
func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
func locationID(location string) string {
	return strings.TrimSpace(location[strings.LastIndex(strings.TrimRight(location, "/"), "/")+1:])
}
func stableUsername(identityID string) string { return "platform-" + attributeSegment(identityID) }
func attributeSegment(value string) string {
	var builder strings.Builder
	for _, character := range strings.TrimSpace(value) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			builder.WriteRune(unicode.ToLower(character))
		} else {
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}

var _ projectionapplication.KeycloakAdmin = (*KeycloakAdmin)(nil)

// LogoutIdentitySessions 为稳定的平台身份注销其投影 Keycloak 用户拥有的全部
// realm 会话。Broker 关联与受管的 identity_id 属性都会被纳入，使预关联用户与
// 遗留用户都被覆盖。用户不存在视为幂等成功。
func (admin *KeycloakAdmin) LogoutIdentitySessions(ctx context.Context, identityID string) error {
	identityID = strings.TrimSpace(identityID)
	if identityID == "" {
		return errors.New("Keycloak logout identity ID is required")
	}
	token, err := admin.token(ctx)
	if err != nil {
		return err
	}
	userIDs := map[string]struct{}{}
	brokerUserID, err := admin.findUserByBrokerIdentity(ctx, token, identityID, platformBrokerAlias)
	if err != nil {
		return fmt.Errorf("find Keycloak Broker user for logout: %w", err)
	}
	if brokerUserID != "" {
		userIDs[brokerUserID] = struct{}{}
	}
	users, err := admin.findUsersByIdentity(ctx, token, identityID)
	if err != nil {
		return fmt.Errorf("find projected Keycloak user for logout: %w", err)
	}
	for _, user := range users {
		if strings.TrimSpace(user.ID) != "" {
			userIDs[user.ID] = struct{}{}
		}
	}
	for userID := range userIDs {
		response, requestErr := admin.request(ctx, token, stdhttp.MethodPost, "/users/"+url.PathEscape(userID)+"/logout", nil)
		if requestErr != nil {
			return requestErr
		}
		status := response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusNoContent && status != stdhttp.StatusNotFound {
			return admin.statusError("logout Keycloak user sessions", status)
		}
	}
	return nil
}

// ResetPasswordAndLogout 更新联邦用户的 Keycloak 凭据并吊销该用户全部活动会话。
// 刻意只提供服务端调用路径，绝不暴露给浏览器端。
func (admin *KeycloakAdmin) ResetPasswordAndLogout(ctx context.Context, userID, password string) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(password) == "" {
		return errors.New("Keycloak reset user and password are required")
	}
	token, err := admin.token(ctx)
	if err != nil {
		return err
	}
	response, err := admin.request(ctx, token, stdhttp.MethodPut, "/users/"+url.PathEscape(userID)+"/reset-password", map[string]any{"type": "password", "value": password, "temporary": true})
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != stdhttp.StatusNoContent {
		return admin.statusError("reset Keycloak user password", response.StatusCode)
	}
	response, err = admin.request(ctx, token, stdhttp.MethodPost, "/users/"+url.PathEscape(userID)+"/logout", nil)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != stdhttp.StatusNoContent {
		return admin.statusError("logout Keycloak user sessions", response.StatusCode)
	}
	return nil
}

package http

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	application "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const (
	brokerClientIDKeycloak       = "keycloak-broker"
	brokerClientIDCustomerPortal = "keycloak-customer-portal-broker"
	brokerSelfHealTTL            = time.Minute
)

// brokerOAuthService 是 healer 消费的精简客户端查询/轮换边界，
// 由 OAuthClientManagementService 实现。
type brokerOAuthService interface {
	GetOAuthClientByClientID(context.Context, string, string) (application.OAuthClientView, error)
	RotateOAuthClientSecret(context.Context, application.OAuthClientSecretRotateInput) (application.OAuthClientSecretResult, error)
}

// brokerIdpSyncer 把 broker 客户端 Secret 同步到对应的 Keycloak Identity Provider，
// 由 keycloakControlPlane 实现。
type brokerIdpSyncer interface {
	EnsureBroker(context.Context, string, string) error
	EnsureCustomerPortalBroker(context.Context, string, string) error
}

// brokerDriftSelfHealer 修复 Keycloak 侧保存的 broker Secret 与平台 ACTIVE 凭据
// 不一致的漂移：令牌端点在 Keycloak 出示过期 Secret 被平台拒绝时观测到漂移，
// 本修复器随即轮换平台 Secret 并同步到 Keycloak IdP，使下一次 broker 交换成功。
// 修复按客户端限速，避免并发故障期间形成轮换风暴。
type brokerDriftSelfHealer struct {
	oauth    brokerOAuthService
	control  brokerIdpSyncer
	tenantID string
	logger   *slog.Logger

	mu       sync.Mutex
	lastHeal map[string]time.Time
	healTTL  time.Duration
	healErr  map[string]error
}

// NewBrokerDriftSelfHealer 构造修复器：oauth 提供客户端查询与 Secret 轮换，
// control 暴露 broker IdP 同步方法。
func NewBrokerDriftSelfHealer(oauth brokerOAuthService, control brokerIdpSyncer, tenantID string, logger *slog.Logger) *brokerDriftSelfHealer {
	if logger == nil {
		logger = slog.Default()
	}
	return &brokerDriftSelfHealer{
		oauth: oauth, control: control, tenantID: tenantID, logger: logger,
		lastHeal: make(map[string]time.Time), healTTL: brokerSelfHealTTL, healErr: make(map[string]error),
	}
}

// HealState 报告 broker 客户端最近一次自愈结果。客户端未知、从未修复或
// 最近一次修复失败时 ok 为 false 或 err 非 nil，供运维诊断而非自动重试。
func (h *brokerDriftSelfHealer) HealState(clientID string) (lastHeal time.Time, err error, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	at, healed := h.lastHeal[clientID]
	if !healed {
		return time.Time{}, nil, false
	}
	return at, h.healErr[clientID], true
}

// HealBrokerSecretDrift 轮换平台 broker 客户端 Secret 并把新值同步到对应的
// Keycloak Identity Provider。未知客户端 ID 是静默 no-op，不产生错误与审计噪音。
func (h *brokerDriftSelfHealer) HealBrokerSecretDrift(ctx context.Context, clientID string) error {
	alias, applicationCode := brokerTargetForClient(clientID)
	if alias == "" {
		return nil
	}
	if !h.acquire(clientID) {
		return nil
	}
	oauthClient, err := h.oauth.GetOAuthClientByClientID(ctx, h.tenantID, clientID)
	if err != nil {
		h.recordResult(clientID, err)
		return fmt.Errorf("lookup broker OAuth client %s: %w", clientID, err)
	}
	rotated, err := h.oauth.RotateOAuthClientSecret(ctx, application.OAuthClientSecretRotateInput{
		TenantID: h.tenantID, OAuthClientID: oauthClient.ID, OperatorID: "system-keycloak", OverlapSeconds: 0,
	})
	if err != nil {
		h.recordResult(clientID, err)
		return fmt.Errorf("rotate broker client %s secret: %w", clientID, err)
	}
	if applicationCode == "customer_portal" {
		err = h.control.EnsureCustomerPortalBroker(ctx, clientID, rotated.PlaintextSecret)
	} else {
		err = h.control.EnsureBroker(ctx, clientID, rotated.PlaintextSecret)
	}
	if err != nil {
		h.recordResult(clientID, err)
		return fmt.Errorf("sync broker %s IdP secret: %w", alias, err)
	}
	h.recordResult(clientID, nil)
	h.logger.Warn("broker secret drift self-healed", "client_id", clientID, "broker_alias", alias)
	return nil
}

// recordResult 在锁内持久化最近一次修复结果。窗口已由 acquire 预占，
// 此处记录即当前尝试的权威结论。
func (h *brokerDriftSelfHealer) recordResult(clientID string, healErr error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastHeal[clientID] = time.Now()
	h.healErr[clientID] = healErr
}

// acquire 判定 clientID 当前是否允许执行修复，遵循按客户端限速。
// 失败的尝试不重置窗口，短暂故障在每个周期内至多触发一次轮换。
func (h *brokerDriftSelfHealer) acquire(clientID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if previous, ok := h.lastHeal[clientID]; ok && time.Since(previous) < h.healTTL {
		return false
	}
	h.lastHeal[clientID] = time.Now()
	return true
}

// brokerTargetForClient 把平台 broker OAuth 客户端映射到其 Keycloak IdP 别名与
// 平台应用编码。别名为空表示该客户端不是受管 broker，修复器不得触碰。
func brokerTargetForClient(clientID string) (alias, applicationCode string) {
	switch clientID {
	case brokerClientIDKeycloak:
		return "basic-platform", "platform"
	case brokerClientIDCustomerPortal:
		return "basic-platform-customer", "customer_portal"
	default:
		return "", ""
	}
}

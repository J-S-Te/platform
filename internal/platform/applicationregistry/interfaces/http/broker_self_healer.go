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

// brokerOAuthService is the narrow client-lookup/rotation boundary consumed by the healer.
// OAuthClientManagementService implements it.
type brokerOAuthService interface {
	GetOAuthClientByClientID(context.Context, string, string) (application.OAuthClientView, error)
	RotateOAuthClientSecret(context.Context, application.OAuthClientSecretRotateInput) (application.OAuthClientSecretResult, error)
}

// brokerIdpSyncer syncs a broker client secret into the matching Keycloak Identity Provider.
// keycloakControlPlane implements it.
type brokerIdpSyncer interface {
	EnsureBroker(context.Context, string, string) error
	EnsureCustomerPortalBroker(context.Context, string, string) error
}

// brokerDriftSelfHealer repairs a platform OAuth broker client whose secret stored in
// Keycloak no longer matches the platform's active credential. The token endpoint
// observes the drift when Keycloak presents a stale secret and the platform rejects it;
// this healer then rotates the platform secret and syncs it into the Keycloak IdP so the
// next broker exchange succeeds. Repairs are rate-limited per client to avoid a failure
// storm during a concurrent outage.
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

// NewBrokerDriftSelfHealer constructs the healer. oauth supplies client lookup and secret
// rotation; control exposes the broker IdP sync methods.
func NewBrokerDriftSelfHealer(oauth brokerOAuthService, control brokerIdpSyncer, tenantID string, logger *slog.Logger) *brokerDriftSelfHealer {
	if logger == nil {
		logger = slog.Default()
	}
	return &brokerDriftSelfHealer{
		oauth: oauth, control: control, tenantID: tenantID, logger: logger,
		lastHeal: make(map[string]time.Time), healTTL: brokerSelfHealTTL, healErr: make(map[string]error),
	}
}

// HealState reports the most recent self-heal outcome for a broker client. ok is false when
// the client is unknown, has never healed, or the last repair failed.
func (h *brokerDriftSelfHealer) HealState(clientID string) (lastHeal time.Time, err error, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	at, healed := h.lastHeal[clientID]
	if !healed {
		return time.Time{}, nil, false
	}
	return at, h.healErr[clientID], true
}

// HealBrokerSecretDrift rotates the platform broker client secret and syncs the new
// secret into the matching Keycloak Identity Provider. Unknown client IDs are a no-op.
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

// recordResult persists the most recent repair outcome under the lock. acquire already
// reserved the window, so recording here is authoritative for the current attempt.
func (h *brokerDriftSelfHealer) recordResult(clientID string, healErr error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastHeal[clientID] = time.Now()
	h.healErr[clientID] = healErr
}

// acquire reports whether a repair may run now for clientID, honoring the per-client
// rate limit. Failed attempts do not reset the window, so a brief outage produces at most
// one rotation per client per interval.
func (h *brokerDriftSelfHealer) acquire(clientID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if previous, ok := h.lastHeal[clientID]; ok && time.Since(previous) < h.healTTL {
		return false
	}
	h.lastHeal[clientID] = time.Now()
	return true
}

// brokerTargetForClient maps a platform broker OAuth client to its Keycloak IdP alias and
// platform application code. Empty alias means the client is not a managed broker.
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

package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	keycloakapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
)

// keycloakEventAuditRunner polls standard Keycloak user/admin event endpoints.
// Its five-minute overlap is safe because the audit service deduplicates every
// record by the stable Keycloak event ID.
type keycloakEventAuditRunner struct {
	collector *keycloakapplication.KeycloakEventAuditCollector
	logger    *slog.Logger
	poll      time.Duration
	lastWarn  time.Time
}

func newKeycloakEventAuditRunner(collector *keycloakapplication.KeycloakEventAuditCollector, logger *slog.Logger, poll time.Duration) (*keycloakEventAuditRunner, error) {
	if collector == nil || logger == nil || poll <= 0 {
		return nil, errors.New("Keycloak event audit runner configuration is invalid")
	}
	return &keycloakEventAuditRunner{collector: collector, logger: logger, poll: poll}, nil
}

func (runner *keycloakEventAuditRunner) Run(ctx context.Context) {
	ticker := time.NewTicker(runner.poll)
	defer ticker.Stop()
	for {
		if _, err := runner.collector.Collect(ctx, time.Now().UTC().Add(-5*time.Minute)); err != nil && !errors.Is(err, context.Canceled) {
			// Audit collection is supplementary to durable authorization projection
			// and Broker verification. A transient/variant Keycloak event endpoint
			// must not create a hot error loop or make the worker look unhealthy.
			now := time.Now().UTC()
			if runner.lastWarn.IsZero() || now.Sub(runner.lastWarn) >= time.Minute {
				runner.lastWarn = now
				level := slog.LevelWarn
				if !strings.Contains(err.Error(), "HTTP 404") {
					level = slog.LevelError
				}
				runner.logger.Log(ctx, level, "collect Keycloak audit events deferred", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

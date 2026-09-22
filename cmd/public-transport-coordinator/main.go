// Command public-transport-coordinator advances the durable transport Saga.
// It emits only transport metadata; credentials, certificate content and
// Keycloak Client secrets are never printed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/platform/publictransport"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/database"
)

type safeResult struct {
	State, Mode, TransitionID, Phase, PlatformOrigin, SSOOrigin string
	DrainUntil                                                  *time.Time `json:",omitempty"`
	KeycloakClientCount                                         int
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("public transport coordination failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: public-transport-coordinator <init|prepare|commit|fail|status>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := database.OpenMySQL(cfg.MySQL)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close(db) }()
	coordinator, err := publictransport.New(db)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	switch args[0] {
	case "init":
		set := flag.NewFlagSet("init", flag.ContinueOnError)
		mode := set.String("mode", "", "HTTP or HTTPS")
		platformOrigin := set.String("platform-origin", "", "active platform origin")
		ssoOrigin := set.String("sso-origin", "", "active SSO origin")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		state, err := coordinator.Initialize(ctx, *mode, *platformOrigin, *ssoOrigin)
		if err != nil {
			return err
		}
		return writeState(state)
	case "prepare":
		set := flag.NewFlagSet("prepare", flag.ContinueOnError)
		mode := set.String("target-mode", "", "HTTP or HTTPS")
		platformOrigin := set.String("platform-origin", "", "target platform origin")
		ssoOrigin := set.String("sso-origin", "", "target SSO origin")
		fingerprint := set.String("certificate-fingerprint", "", "leaf certificate SHA-256 fingerprint")
		notAfterRaw := set.String("certificate-not-after", "", "certificate expiry RFC3339")
		drainRaw := set.String("drain-until", "", "secure-cookie drain deadline RFC3339")
		drainGraceRaw := set.String("drain-grace", "5m", "grace added after the configured session TTL")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		notAfter, err := optionalTime(*notAfterRaw)
		if err != nil {
			return fmt.Errorf("certificate-not-after: %w", err)
		}
		drainUntil, err := optionalTime(*drainRaw)
		if err != nil {
			return fmt.Errorf("drain-until: %w", err)
		}
		if strings.EqualFold(strings.TrimSpace(*mode), publictransport.ModeHTTP) && drainUntil == nil {
			grace, parseErr := time.ParseDuration(strings.TrimSpace(*drainGraceRaw))
			if parseErr != nil || grace < 0 {
				return errors.New("drain-grace must be a non-negative Go duration")
			}
			deadline := time.Now().UTC().Add(cfg.Auth.SessionTTL + grace)
			drainUntil = &deadline
		}
		transition, clients, err := coordinator.Begin(ctx, publictransport.BeginInput{TargetMode: *mode, TargetPlatformOrigin: *platformOrigin, TargetSSOOrigin: *ssoOrigin, CertificateFingerprint: *fingerprint, CertificateNotAfter: notAfter, DrainUntil: drainUntil})
		if errors.Is(err, publictransport.ErrAlreadyStable) {
			state, statusErr := coordinator.Status(ctx)
			if statusErr != nil {
				return statusErr
			}
			return writeState(state)
		}
		if err != nil {
			return err
		}
		if err := reconcileKeycloak(ctx, cfg, clients, "dual"); err != nil {
			rollbackErr := reconcileKeycloak(ctx, cfg, clients, "source")
			rollback := "Keycloak callbacks restored"
			if rollbackErr != nil {
				rollback = "Keycloak rollback failed: " + rollbackErr.Error()
			}
			_ = coordinator.Fail(ctx, transition.ID, "KEYCLOAK_DUAL_CALLBACK", err, rollback)
			return err
		}
		if err := coordinator.MarkExternalPrepared(ctx, transition.ID); err != nil {
			return err
		}
		return writeTransition(transition, publictransport.PhaseExternalPrepared, len(clients))
	case "commit":
		set := flag.NewFlagSet("commit", flag.ContinueOnError)
		id := set.String("transition-id", "", "active transition id")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		transition, clients, err := coordinator.Active(ctx)
		if err != nil {
			return err
		}
		if *id != "" && *id != transition.ID {
			return errors.New("requested transition is not active")
		}
		if err := coordinator.CommitControlPlane(ctx, transition.ID); err != nil {
			return err
		}
		if err := reconcileKeycloak(ctx, cfg, clients, "target"); err != nil {
			_ = coordinator.RecordRetryableFailure(ctx, transition.ID, "KEYCLOAK_TARGET_CALLBACK", err)
			return fmt.Errorf("control plane committed; Keycloak target-only reconciliation remains retryable: %w", err)
		}
		if err := coordinator.Finalize(ctx, transition.ID); err != nil {
			return err
		}
		state, err := coordinator.Status(ctx)
		if err != nil {
			return err
		}
		return writeState(state)
	case "fail":
		set := flag.NewFlagSet("fail", flag.ContinueOnError)
		id := set.String("transition-id", "", "active transition id")
		stage := set.String("stage", "OPERATOR_ABORT", "failure stage")
		message := set.String("message", "transition aborted by operator", "sanitized failure summary")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		transition, clients, err := coordinator.Active(ctx)
		if err != nil {
			return err
		}
		if *id != "" && *id != transition.ID {
			return errors.New("requested transition is not active")
		}
		rollbackErr := reconcileKeycloak(ctx, cfg, clients, "source")
		rollback := "Keycloak callbacks restored"
		if rollbackErr != nil {
			rollback = "Keycloak rollback failed: " + rollbackErr.Error()
		}
		if err := coordinator.Fail(ctx, transition.ID, *stage, errors.New(*message), rollback); err != nil {
			return err
		}
		state, err := coordinator.Status(ctx)
		if err != nil {
			return err
		}
		return writeState(state)
	case "status":
		state, err := coordinator.Status(ctx)
		if err != nil {
			return err
		}
		return writeState(state)
	default:
		return fmt.Errorf("unknown public transport coordinator command %q", args[0])
	}
}

func reconcileKeycloak(ctx context.Context, cfg config.Config, clients []publictransport.KeycloakClient, callbackSet string) error {
	if len(clients) == 0 {
		return nil
	}
	if !cfg.Keycloak.Enabled {
		return errors.New("managed Keycloak Clients exist but Keycloak management is disabled")
	}
	control := applicationregistryhttp.NewKeycloakControlPlaneWithCredentials(cfg.Keycloak.AdminURL, cfg.Keycloak.Realm, applicationregistryhttp.KeycloakControlPlaneCredentials{ServiceAccountClientID: cfg.Keycloak.AdminClientID, ServiceAccountClientSecret: cfg.Keycloak.AdminClientSecret, Username: cfg.Keycloak.AdminUsername, Password: cfg.Keycloak.AdminPassword}, cfg.Keycloak.BrokerClientID, cfg.Keycloak.BrokerClientSecret, cfg.Auth.PublicBaseURL, cfg.Keycloak.PlatformBackchannelURL)
	for _, client := range clients {
		var redirects []string
		switch callbackSet {
		case "dual":
			redirects = []string{client.SourceRedirectURI, client.TargetRedirectURI}
		case "source":
			redirects = []string{client.SourceRedirectURI}
		case "target":
			redirects = []string{client.TargetRedirectURI}
		default:
			return fmt.Errorf("unknown Keycloak callback set %q", callbackSet)
		}
		if _, err := control.EnsureClientRedirectURIs(ctx, client.ClientID, client.Name, redirects); err != nil {
			return fmt.Errorf("reconcile Keycloak Client %s: %w", client.ClientID, err)
		}
	}
	return nil
}

func optionalTime(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, err
	}
	parsed = parsed.UTC()
	return &parsed, nil
}
func writeState(state publictransport.State) error {
	result := safeResult{State: state.State, Mode: state.ActiveMode, PlatformOrigin: state.PlatformOrigin, SSOOrigin: state.SSOOrigin, DrainUntil: state.DrainUntil}
	if state.ActiveTransitionID != nil {
		result.TransitionID = *state.ActiveTransitionID
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
func writeTransition(transition publictransport.Transition, phase string, clients int) error {
	return json.NewEncoder(os.Stdout).Encode(safeResult{State: transition.State, Mode: transition.TargetMode, TransitionID: transition.ID, Phase: phase, PlatformOrigin: transition.TargetPlatformOrigin, SSOOrigin: transition.TargetSSOOrigin, DrainUntil: transition.DrainUntil, KeycloakClientCount: clients})
}

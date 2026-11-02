// Command public-transport-coordinator 推进持久化的传输 Saga。只输出传输元数据；
// 凭据、证书内容与 Keycloak Client secret 一律不打印。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
		return errors.New("usage: public-transport-coordinator <init|prepare|commit|fail|abort|status>")
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
		request, err := parseFailRequest(args[1:])
		if err != nil {
			return err
		}
		return failTransition(ctx, cfg, coordinator, request)
	case "abort":
		// abort 等价于 fail --stage OPERATOR_ABORT，但必须显式 --yes：
		// 放弃切换会恢复降级期被截断的会话有效期，运维要明确知道自己在放弃这次切换。
		request, err := parseAbortRequest(args[1:])
		if err != nil {
			return err
		}
		return failTransition(ctx, cfg, coordinator, request)
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

// failureRequest 是 fail/abort 共用的参数集合。两者最终都走同一条协调器失败路径，
// 唯一区别是 abort 固定使用 OPERATOR_ABORT 阶段并强制 --yes 确认。
type failureRequest struct {
	TransitionID, Stage, Message string
}

const (
	// operatorAbortStage 是运维主动放弃切换的阶段标记（与历史 fail 默认值一致）。
	operatorAbortStage = "OPERATOR_ABORT"
	// defaultAbortMessage 写入 transition.failure_message，只记录可公开的运维语义。
	defaultAbortMessage = "transition aborted by operator"
)

// parseFailRequest 保持既有 fail 语义：不带 --yes，默认阶段仍是 OPERATOR_ABORT。
func parseFailRequest(args []string) (failureRequest, error) {
	set := flag.NewFlagSet("fail", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	id := set.String("transition-id", "", "active transition id")
	stage := set.String("stage", operatorAbortStage, "failure stage")
	message := set.String("message", defaultAbortMessage, "sanitized failure summary")
	if err := set.Parse(args); err != nil {
		return failureRequest{}, err
	}
	if set.NArg() > 0 {
		return failureRequest{}, errors.New("fail accepts no positional arguments: " + strings.Join(set.Args(), " "))
	}
	return failureRequest{TransitionID: *id, Stage: *stage, Message: *message}, nil
}

// parseAbortRequest 要求显式 --yes：abort 会放弃整次切换并把降级期被截断的会话有效期
// 恢复原值，属于不可静默触发的操作。阶段固定为 OPERATOR_ABORT，不接受覆盖。
func parseAbortRequest(args []string) (failureRequest, error) {
	set := flag.NewFlagSet("abort", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	id := set.String("transition-id", "", "active transition id")
	message := set.String("message", defaultAbortMessage, "sanitized failure summary")
	yes := set.Bool("yes", false, "confirm dropping the active transition")
	if err := set.Parse(args); err != nil {
		return failureRequest{}, err
	}
	if set.NArg() > 0 {
		return failureRequest{}, errors.New("abort accepts no positional arguments: " + strings.Join(set.Args(), " "))
	}
	if !*yes {
		return failureRequest{}, errors.New("abort requires --yes: it drops the active transition, restores capped session lifetimes and removes target callbacks")
	}
	return failureRequest{TransitionID: *id, Stage: operatorAbortStage, Message: *message}, nil
}

// failTransition 撤销外部回调、把切换标记为失败，并由协调器恢复被截断的会话有效期。
// 只输出状态元数据，不打印凭据或证书内容。
func failTransition(ctx context.Context, cfg config.Config, coordinator *publictransport.Coordinator, request failureRequest) error {
	transition, clients, err := coordinator.Active(ctx)
	if err != nil {
		return err
	}
	if request.TransitionID != "" && request.TransitionID != transition.ID {
		return errors.New("requested transition is not active")
	}
	rollbackErr := reconcileKeycloak(ctx, cfg, clients, "source")
	rollback := "Keycloak callbacks restored"
	if rollbackErr != nil {
		rollback = "Keycloak rollback failed: " + rollbackErr.Error()
	}
	if err := coordinator.Fail(ctx, transition.ID, request.Stage, errors.New(request.Message), rollback); err != nil {
		return err
	}
	state, err := coordinator.Status(ctx)
	if err != nil {
		return err
	}
	return writeState(state)
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

// Package publictransport persists and reconciles the public HTTP/HTTPS cutover.
// It deliberately owns only system-managed origins and callback values; secrets
// and arbitrary third-party redirect URIs never enter this workflow.
package publictransport

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ModeHTTP  = "HTTP"
	ModeHTTPS = "HTTPS"

	StateHTTP           = "HTTP"
	StateEnablingHTTPS  = "ENABLING_HTTPS"
	StateHTTPS          = "HTTPS"
	StateDisablingHTTPS = "DISABLING_HTTPS"

	PhaseCallbacksPrepared = "CALLBACKS_PREPARED"
	PhaseExternalPrepared  = "EXTERNAL_PREPARED"
	PhaseControlCommitted  = "CONTROL_COMMITTED"
	PhaseCompleted         = "COMPLETED"
	PhaseFailed            = "FAILED"
)

type State struct {
	ID                   uint8      `gorm:"column:id"`
	State                string     `gorm:"column:state"`
	ActiveMode           string     `gorm:"column:active_mode"`
	DesiredMode          string     `gorm:"column:desired_mode"`
	PlatformOrigin       string     `gorm:"column:platform_origin"`
	SSOOrigin            string     `gorm:"column:sso_origin"`
	TargetPlatformOrigin *string    `gorm:"column:target_platform_origin"`
	TargetSSOOrigin      *string    `gorm:"column:target_sso_origin"`
	ActiveTransitionID   *string    `gorm:"column:active_transition_id"`
	DrainUntil           *time.Time `gorm:"column:drain_until"`
	Version              uint64     `gorm:"column:version"`
	UpdatedAt            time.Time  `gorm:"column:updated_at"`
}

type Transition struct {
	ID                     string     `gorm:"column:id"`
	FromMode               string     `gorm:"column:from_mode"`
	TargetMode             string     `gorm:"column:target_mode"`
	State                  string     `gorm:"column:state"`
	Phase                  string     `gorm:"column:phase"`
	SourcePlatformOrigin   string     `gorm:"column:source_platform_origin"`
	TargetPlatformOrigin   string     `gorm:"column:target_platform_origin"`
	SourceSSOOrigin        string     `gorm:"column:source_sso_origin"`
	TargetSSOOrigin        string     `gorm:"column:target_sso_origin"`
	CertificateFingerprint *string    `gorm:"column:certificate_fingerprint"`
	CertificateNotAfter    *time.Time `gorm:"column:certificate_not_after"`
	DrainUntil             *time.Time `gorm:"column:drain_until"`
	FailureStage           *string    `gorm:"column:failure_stage"`
	FailureMessage         *string    `gorm:"column:failure_message"`
	RollbackResult         *string    `gorm:"column:rollback_result"`
	StartedAt              time.Time  `gorm:"column:started_at"`
	CompletedAt            *time.Time `gorm:"column:completed_at"`
	FailedAt               *time.Time `gorm:"column:failed_at"`
	UpdatedAt              time.Time  `gorm:"column:updated_at"`
}

type KeycloakClient struct {
	TenantID, ApplicationID, EnvironmentID, ClientID, Name string
	SourceRedirectURI, TargetRedirectURI                   string
}

type BeginInput struct {
	TargetMode, TargetPlatformOrigin, TargetSSOOrigin string
	CertificateFingerprint                            string
	CertificateNotAfter                               *time.Time
	DrainUntil                                        *time.Time
}

type Coordinator struct {
	database *gorm.DB
	now      func() time.Time
	newID    func(time.Time) (string, error)
}

func New(database *gorm.DB) (*Coordinator, error) {
	if database == nil {
		return nil, errors.New("public transport database must not be nil")
	}
	return &Coordinator{database: database, now: func() time.Time { return time.Now().UTC() }, newID: ulid.New}, nil
}

func (c *Coordinator) Initialize(ctx context.Context, mode, platformOrigin, ssoOrigin string) (State, error) {
	mode, platformOrigin, ssoOrigin, err := validateStable(mode, platformOrigin, ssoOrigin)
	if err != nil {
		return State{}, err
	}
	now := c.now().UTC()
	row := State{ID: 1, State: mode, ActiveMode: mode, DesiredMode: mode, PlatformOrigin: platformOrigin, SSOOrigin: ssoOrigin, Version: 1, UpdatedAt: now}
	err = c.database.WithContext(ctx).Table("platform_public_transport_state").Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
	if err != nil {
		return State{}, fmt.Errorf("initialize public transport state: %w", err)
	}
	return c.Status(ctx)
}

func (c *Coordinator) Status(ctx context.Context) (State, error) {
	var state State
	if err := c.database.WithContext(ctx).Table("platform_public_transport_state").Where("id = 1").Take(&state).Error; err != nil {
		return State{}, fmt.Errorf("load public transport state: %w", err)
	}
	return state, nil
}

func (c *Coordinator) Active(ctx context.Context) (Transition, []KeycloakClient, error) {
	state, err := c.Status(ctx)
	if err != nil {
		return Transition{}, nil, err
	}
	if state.ActiveTransitionID == nil {
		return Transition{}, nil, errors.New("no active public transport transition")
	}
	var transition Transition
	if err := c.database.WithContext(ctx).Table("platform_public_transport_transition").Where("id = ?", *state.ActiveTransitionID).Take(&transition).Error; err != nil {
		return Transition{}, nil, fmt.Errorf("load active public transport transition: %w", err)
	}
	clients, err := loadKeycloakResources(c.database.WithContext(ctx), transition.ID)
	return transition, clients, err
}

// Begin is restart-safe. The first call creates the transition, adds target
// callbacks beside current callbacks and caps every login/refresh lifetime for
// downgrade. Later calls return the same transition without duplicating work.
func (c *Coordinator) Begin(ctx context.Context, input BeginInput) (Transition, []KeycloakClient, error) {
	input.TargetMode = strings.ToUpper(strings.TrimSpace(input.TargetMode))
	var transition Transition
	var clients []KeycloakClient
	err := c.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx)
		if err != nil {
			return err
		}
		if state.ActiveTransitionID != nil {
			if err := tx.Table("platform_public_transport_transition").Where("id = ?", *state.ActiveTransitionID).Take(&transition).Error; err != nil {
				return err
			}
			requestedMode, requestedPlatform, requestedSSO, validateErr := validateStable(input.TargetMode, input.TargetPlatformOrigin, input.TargetSSOOrigin)
			if validateErr != nil {
				return validateErr
			}
			if transition.TargetMode != requestedMode || transition.TargetPlatformOrigin != requestedPlatform || transition.TargetSSOOrigin != requestedSSO {
				return errors.New("requested transport target does not match the active transition")
			}
			clients, err = loadKeycloakResources(tx, transition.ID)
			return err
		}
		_, targetPlatform, targetSSO, err := validateStable(input.TargetMode, input.TargetPlatformOrigin, input.TargetSSOOrigin)
		if err != nil {
			return err
		}
		if state.ActiveMode == input.TargetMode && state.PlatformOrigin == targetPlatform && state.SSOOrigin == targetSSO {
			return ErrAlreadyStable
		}
		if state.ActiveMode == input.TargetMode {
			return errors.New("public origin changes within the same transport mode require a separate hostname migration")
		}
		if input.TargetMode == ModeHTTP && input.DrainUntil == nil {
			return errors.New("HTTPS downgrade requires an absolute drain deadline")
		}
		if input.DrainUntil != nil && !input.DrainUntil.After(c.now()) {
			return errors.New("drain deadline must be in the future")
		}
		id, err := c.newID(c.now())
		if err != nil {
			return err
		}
		transitionState := StateEnablingHTTPS
		if input.TargetMode == ModeHTTP {
			transitionState = StateDisablingHTTPS
		}
		now := c.now().UTC()
		transition = Transition{ID: id, FromMode: state.ActiveMode, TargetMode: input.TargetMode, State: transitionState, Phase: PhaseCallbacksPrepared, SourcePlatformOrigin: state.PlatformOrigin, TargetPlatformOrigin: targetPlatform, SourceSSOOrigin: state.SSOOrigin, TargetSSOOrigin: targetSSO, DrainUntil: input.DrainUntil, CertificateNotAfter: input.CertificateNotAfter, StartedAt: now, UpdatedAt: now}
		if fingerprint := strings.TrimSpace(input.CertificateFingerprint); fingerprint != "" {
			transition.CertificateFingerprint = &fingerprint
		}
		if err := tx.Table("platform_public_transport_transition").Create(&transition).Error; err != nil {
			return fmt.Errorf("create public transport transition: %w", err)
		}
		if err := prepareManagedResources(tx, transition, now); err != nil {
			return err
		}
		if input.TargetMode == ModeHTTP {
			if err := capSessionLifetimes(tx, *input.DrainUntil); err != nil {
				return err
			}
		}
		updates := map[string]any{"state": transitionState, "desired_mode": input.TargetMode, "target_platform_origin": targetPlatform, "target_sso_origin": targetSSO, "active_transition_id": id, "drain_until": input.DrainUntil, "version": gorm.Expr("version + 1"), "updated_at": now}
		if err := tx.Table("platform_public_transport_state").Where("id = 1").Updates(updates).Error; err != nil {
			return fmt.Errorf("activate public transport transition: %w", err)
		}
		clients, err = loadKeycloakResources(tx, id)
		return err
	})
	if err != nil {
		return Transition{}, nil, err
	}
	return transition, clients, nil
}

var ErrAlreadyStable = errors.New("public transport is already stable at the requested mode and origins")

func (c *Coordinator) MarkExternalPrepared(ctx context.Context, transitionID string) error {
	return c.advancePhase(ctx, transitionID, PhaseCallbacksPrepared, PhaseExternalPrepared)
}

// CommitControlPlane atomically switches environment/homepage origins and
// removes only the exact old callback values captured by Begin.
func (c *Coordinator) CommitControlPlane(ctx context.Context, transitionID string) error {
	return c.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, transition, err := lockActiveTransition(tx, transitionID)
		if err != nil {
			return err
		}
		if transition.Phase == PhaseControlCommitted || transition.Phase == PhaseCompleted {
			return nil
		}
		if transition.Phase != PhaseExternalPrepared {
			return fmt.Errorf("transition phase %s cannot commit control plane", transition.Phase)
		}
		if transition.TargetMode == ModeHTTP && transition.DrainUntil != nil && c.now().Before(*transition.DrainUntil) {
			return fmt.Errorf("secure-cookie drain remains active until %s", transition.DrainUntil.UTC().Format(time.RFC3339))
		}
		if err := applyCapturedTargets(tx, transition); err != nil {
			return err
		}
		now := c.now().UTC()
		if err := tx.Table("platform_public_transport_transition").Where("id = ?", transition.ID).Updates(map[string]any{"phase": PhaseControlCommitted, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Table("platform_public_transport_state").Where("id = ?", state.ID).Updates(map[string]any{"platform_origin": transition.TargetPlatformOrigin, "sso_origin": transition.TargetSSOOrigin, "version": gorm.Expr("version + 1"), "updated_at": now}).Error
	})
}

func (c *Coordinator) Finalize(ctx context.Context, transitionID string) error {
	return c.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, transition, err := lockActiveTransition(tx, transitionID)
		if err != nil {
			return err
		}
		if transition.Phase == PhaseCompleted {
			return nil
		}
		if transition.Phase != PhaseControlCommitted {
			return fmt.Errorf("transition phase %s cannot finalize", transition.Phase)
		}
		now := c.now().UTC()
		if err := tx.Table("platform_public_transport_transition").Where("id = ?", transition.ID).Updates(map[string]any{"state": transition.TargetMode, "phase": PhaseCompleted, "failure_stage": nil, "failure_message": nil, "failed_at": nil, "completed_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Table("platform_public_transport_state").Where("id = ?", state.ID).Updates(map[string]any{"state": transition.TargetMode, "active_mode": transition.TargetMode, "desired_mode": transition.TargetMode, "target_platform_origin": nil, "target_sso_origin": nil, "active_transition_id": nil, "drain_until": nil, "version": gorm.Expr("version + 1"), "updated_at": now}).Error
	})
}

func (c *Coordinator) Fail(ctx context.Context, transitionID, stage string, cause error, rollbackResult string) error {
	message := "unspecified transition failure"
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 1000 {
		message = message[:1000]
	}
	if len(rollbackResult) > 1000 {
		rollbackResult = rollbackResult[:1000]
	}
	now := c.now().UTC()
	return c.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, transition, err := lockActiveTransition(tx, transitionID)
		if err != nil {
			return err
		}
		if transition.Phase == PhaseCompleted {
			return errors.New("completed transition cannot be failed")
		}
		if err := rollbackPreparedResources(tx, transition); err != nil {
			return err
		}
		if err := tx.Table("platform_public_transport_transition").Where("id = ?", transition.ID).Updates(map[string]any{"phase": PhaseFailed, "failure_stage": strings.TrimSpace(stage), "failure_message": message, "rollback_result": rollbackResult, "failed_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Table("platform_public_transport_state").Where("id = ?", state.ID).Updates(map[string]any{"state": transition.FromMode, "active_mode": transition.FromMode, "desired_mode": transition.FromMode, "platform_origin": transition.SourcePlatformOrigin, "sso_origin": transition.SourceSSOOrigin, "target_platform_origin": nil, "target_sso_origin": nil, "active_transition_id": nil, "drain_until": nil, "version": gorm.Expr("version + 1"), "updated_at": now}).Error
	})
}

// RecordRetryableFailure keeps a post-commit external failure visible without
// undoing active database callback/origin state. Re-running commit reconciles
// Keycloak to the target set and then finalizes this same transition.
func (c *Coordinator) RecordRetryableFailure(ctx context.Context, transitionID, stage string, cause error) error {
	message := "unspecified retryable transition failure"
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 1000 {
		message = message[:1000]
	}
	now := c.now().UTC()
	result := c.database.WithContext(ctx).Table("platform_public_transport_transition").Where("id = ? AND phase = ?", transitionID, PhaseControlCommitted).Updates(map[string]any{"failure_stage": strings.TrimSpace(stage), "failure_message": message, "failed_at": now, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("retryable failure can only be recorded after control-plane commit")
	}
	return nil
}

func (c *Coordinator) advancePhase(ctx context.Context, id, from, to string) error {
	result := c.database.WithContext(ctx).Table("platform_public_transport_transition").Where("id = ? AND phase = ?", id, from).Updates(map[string]any{"phase": to, "updated_at": c.now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var row Transition
	if err := c.database.WithContext(ctx).Table("platform_public_transport_transition").Where("id = ?", id).Take(&row).Error; err != nil {
		return err
	}
	if row.Phase == to || row.Phase == PhaseControlCommitted || row.Phase == PhaseCompleted {
		return nil
	}
	return fmt.Errorf("transition phase %s cannot advance to %s", row.Phase, to)
}

func validateStable(mode, platformOrigin, ssoOrigin string) (string, string, string, error) {
	mode = strings.ToUpper(strings.TrimSpace(mode))
	if mode != ModeHTTP && mode != ModeHTTPS {
		return "", "", "", errors.New("transport mode must be HTTP or HTTPS")
	}
	p, err := canonicalOrigin(platformOrigin, strings.ToLower(mode))
	if err != nil {
		return "", "", "", fmt.Errorf("platform origin: %w", err)
	}
	s, err := canonicalOrigin(ssoOrigin, strings.ToLower(mode))
	if err != nil {
		return "", "", "", fmt.Errorf("SSO origin: %w", err)
	}
	return mode, p, s, nil
}

func canonicalOrigin(raw, expectedScheme string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("must be an absolute origin")
	}
	if parsed.Scheme != expectedScheme || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("must be a %s origin without credentials, path, query or fragment", expectedScheme)
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func replaceOrigin(value, source, target string) (string, bool) {
	if value == source {
		return target, true
	}
	if strings.HasPrefix(value, source+"/") {
		return target + strings.TrimPrefix(value, source), true
	}
	return value, false
}

func lockState(tx *gorm.DB) (State, error) {
	var state State
	err := tx.Table("platform_public_transport_state").Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = 1").Take(&state).Error
	if err != nil {
		return State{}, fmt.Errorf("lock public transport state: %w", err)
	}
	return state, nil
}

func lockActiveTransition(tx *gorm.DB, id string) (State, Transition, error) {
	state, err := lockState(tx)
	if err != nil {
		return State{}, Transition{}, err
	}
	if state.ActiveTransitionID == nil || *state.ActiveTransitionID != id {
		return State{}, Transition{}, errors.New("transition is not active")
	}
	var transition Transition
	if err := tx.Table("platform_public_transport_transition").Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&transition).Error; err != nil {
		return State{}, Transition{}, err
	}
	return state, transition, nil
}

type environmentRow struct{ ID, TenantID, ApplicationID, BaseURL string }
type callbackRow struct{ ClientID, Value string }

func prepareManagedResources(tx *gorm.DB, transition Transition, now time.Time) error {
	var environments []environmentRow
	if err := tx.Table("platform_application_environment").Select("id, tenant_id, application_id, base_url").Where("base_url IS NOT NULL AND base_url <> ''").Find(&environments).Error; err != nil {
		return err
	}
	for _, row := range environments {
		target, ok := replaceOrigin(row.BaseURL, transition.SourcePlatformOrigin, transition.TargetPlatformOrigin)
		if !ok {
			continue
		}
		if err := insertResource(tx, transition.ID, "APPLICATION_ENVIRONMENT", row.ID, row.TenantID, row.ApplicationID, row.ID, row.BaseURL, target, now); err != nil {
			return err
		}
	}
	var homepages []struct{ ID, TenantID, HomepageURL string }
	if err := tx.Table("platform_application").Select("id, tenant_id, homepage_url").Where("homepage_url IS NOT NULL AND homepage_url <> ''").Find(&homepages).Error; err != nil {
		return err
	}
	for _, row := range homepages {
		if target, ok := replaceOrigin(row.HomepageURL, transition.SourcePlatformOrigin, transition.TargetPlatformOrigin); ok {
			if err := insertResource(tx, transition.ID, "APPLICATION_HOMEPAGE", row.ID, row.TenantID, row.ID, "", row.HomepageURL, target, now); err != nil {
				return err
			}
		}
	}
	if err := prepareCallbackTable(tx, transition, "platform_oauth_redirect_uri", "redirect_uri", "OAUTH_REDIRECT", now); err != nil {
		return err
	}
	if err := prepareCallbackTable(tx, transition, "platform_oauth_post_logout_redirect_uri", "post_logout_redirect_uri", "OAUTH_POST_LOGOUT", now); err != nil {
		return err
	}
	var mappings []struct{ TenantID, ApplicationID, EnvironmentID, ClientID, Name, BaseURL, PathPrefix string }
	query := `SELECT mapping.tenant_id, mapping.application_id, mapping.environment_id, mapping.keycloak_client_id AS client_id,
        application.name, environment.base_url, COALESCE(environment.path_prefix, '') AS path_prefix
        FROM keycloak_application_client_mapping mapping
        JOIN platform_application application ON application.id = mapping.application_id AND application.tenant_id = mapping.tenant_id
        JOIN platform_application_environment environment ON environment.id = mapping.environment_id AND environment.application_id = mapping.application_id AND environment.tenant_id = mapping.tenant_id
        WHERE mapping.status = 'SYNCED'`
	if err := tx.Raw(query).Scan(&mappings).Error; err != nil {
		return err
	}
	for _, row := range mappings {
		source := strings.TrimRight(row.BaseURL, "/") + strings.TrimRight(row.PathPrefix, "/") + "/auth/callback"
		target, ok := replaceOrigin(source, transition.SourcePlatformOrigin, transition.TargetPlatformOrigin)
		if !ok {
			continue
		}
		resourceID := row.ClientID + "|" + row.Name
		if err := insertResource(tx, transition.ID, "KEYCLOAK_CLIENT", resourceID, row.TenantID, row.ApplicationID, row.EnvironmentID, source, target, now); err != nil {
			return err
		}
	}
	return nil
}

func prepareCallbackTable(tx *gorm.DB, transition Transition, table, column, resourceType string, now time.Time) error {
	var rows []callbackRow
	if err := tx.Table(table).Select("oauth_client_id AS client_id, " + column + " AS value").Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		target, ok := replaceOrigin(row.Value, transition.SourcePlatformOrigin, transition.TargetPlatformOrigin)
		if !ok || target == row.Value {
			continue
		}
		if err := insertResource(tx, transition.ID, resourceType, row.ClientID, "", "", "", row.Value, target, now); err != nil {
			return err
		}
		values := map[string]any{"oauth_client_id": row.ClientID, column: target, "created_at": now}
		if err := tx.Table(table).Clauses(clause.Insert{Modifier: "IGNORE"}).Create(values).Error; err != nil {
			return err
		}
	}
	return nil
}

func insertResource(tx *gorm.DB, transitionID, kind, id, tenantID, appID, envID, source, target string, now time.Time) error {
	return tx.Table("platform_public_transport_resource").Clauses(clause.Insert{Modifier: "IGNORE"}).Create(map[string]any{"transition_id": transitionID, "resource_type": kind, "resource_id": id, "tenant_id": nullable(tenantID), "application_id": nullable(appID), "environment_id": nullable(envID), "source_value": source, "target_value": target, "created_at": now}).Error
}
func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func capSessionLifetimes(tx *gorm.DB, deadline time.Time) error {
	deadline = deadline.UTC()
	for _, table := range []string{"iam_session", "oauth_token_family", "oauth_refresh_token"} {
		if err := tx.Table(table).Where("expires_at > ?", deadline).Update("expires_at", deadline).Error; err != nil {
			return fmt.Errorf("cap %s lifetime: %w", table, err)
		}
	}
	return nil
}

func applyCapturedTargets(tx *gorm.DB, transition Transition) error {
	type resource struct{ ResourceType, ResourceID, SourceValue, TargetValue string }
	var rows []resource
	if err := tx.Table("platform_public_transport_resource").Where("transition_id = ?", transition.ID).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		switch row.ResourceType {
		case "APPLICATION_ENVIRONMENT":
			if err := tx.Table("platform_application_environment").Where("id = ? AND base_url = ?", row.ResourceID, row.SourceValue).Updates(map[string]any{"base_url": row.TargetValue, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()}).Error; err != nil {
				return err
			}
		case "APPLICATION_HOMEPAGE":
			if err := tx.Table("platform_application").Where("id = ? AND homepage_url = ?", row.ResourceID, row.SourceValue).Updates(map[string]any{"homepage_url": row.TargetValue, "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()}).Error; err != nil {
				return err
			}
		case "OAUTH_REDIRECT":
			if err := tx.Table("platform_oauth_redirect_uri").Where("oauth_client_id = ? AND redirect_uri = ?", row.ResourceID, row.SourceValue).Delete(nil).Error; err != nil {
				return err
			}
		case "OAUTH_POST_LOGOUT":
			if err := tx.Table("platform_oauth_post_logout_redirect_uri").Where("oauth_client_id = ? AND post_logout_redirect_uri = ?", row.ResourceID, row.SourceValue).Delete(nil).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func rollbackPreparedResources(tx *gorm.DB, transition Transition) error {
	if transition.Phase == PhaseControlCommitted {
		return errors.New("control-plane commit requires forward recovery; automatic rollback would invalidate active runtime callbacks")
	}
	type resource struct{ ResourceType, ResourceID, TargetValue string }
	var rows []resource
	if err := tx.Table("platform_public_transport_resource").Where("transition_id = ?", transition.ID).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		switch row.ResourceType {
		case "OAUTH_REDIRECT":
			if err := tx.Table("platform_oauth_redirect_uri").Where("oauth_client_id = ? AND redirect_uri = ?", row.ResourceID, row.TargetValue).Delete(nil).Error; err != nil {
				return err
			}
		case "OAUTH_POST_LOGOUT":
			if err := tx.Table("platform_oauth_post_logout_redirect_uri").Where("oauth_client_id = ? AND post_logout_redirect_uri = ?", row.ResourceID, row.TargetValue).Delete(nil).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func loadKeycloakResources(tx *gorm.DB, transitionID string) ([]KeycloakClient, error) {
	type row struct{ TenantID, ApplicationID, EnvironmentID, ResourceID, SourceValue, TargetValue string }
	var rows []row
	err := tx.Table("platform_public_transport_resource").Select("tenant_id, application_id, environment_id, resource_id, source_value, target_value").Where("transition_id = ? AND resource_type = 'KEYCLOAK_CLIENT'", transitionID).Find(&rows).Error
	clients := make([]KeycloakClient, 0, len(rows))
	for _, item := range rows {
		parts := strings.SplitN(item.ResourceID, "|", 2)
		name := parts[0]
		if len(parts) == 2 {
			name = parts[1]
		}
		clients = append(clients, KeycloakClient{TenantID: item.TenantID, ApplicationID: item.ApplicationID, EnvironmentID: item.EnvironmentID, ClientID: parts[0], Name: name, SourceRedirectURI: item.SourceValue, TargetRedirectURI: item.TargetValue})
	}
	return clients, err
}

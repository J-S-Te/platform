package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"time"

	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	identity "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	license "github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	coordination "github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	core "github.com/J-S-Te/license-core"
	runtime "github.com/J-S-Te/license-core/runtime"
)

type offlinePrincipalResolver interface {
	OfflineBootstrapPrincipal(context.Context, time.Time) (authctx.Principal, error)
}
type offlineLicenseImporter interface {
	ImportDelivery(context.Context, string, string, domain.Actor) (license.DeliveryImportResult, error)
}
type offlineCoordinator interface {
	Status(context.Context, string) (coordination.Status, error)
	BeginActivation(context.Context, string, uint64, domain.Actor) error
}
type offlineInstallationSource interface {
	CollectMigrationInstallationEvidence(context.Context) (evidence.Report, error)
}
type offlineInstallationLifecycle interface {
	PrepareInstallation(context.Context, string, evidence.Report, domain.Actor) (coordination.InstallationBaseline, error)
	ReadInstallationBaseline(context.Context) (coordination.InstallationBaseline, error)
}
type offlineInstallationInitializer interface {
	Read(context.Context) (domain.State, error)
	Initialize(context.Context, license.InitializeInput, domain.Actor) (domain.State, error)
}
type offlineOnboarding interface {
	RegisterSubsystemDirectory(http.ResponseWriter, *http.Request)
	AdoptSubsystem(http.ResponseWriter, *http.Request)
	UpdateSubsystem(http.ResponseWriter, *http.Request)
	GetSubsystemStatus(http.ResponseWriter, *http.Request)
}

// OfflineDeliveryRunner is reachable only through the host-root maintenance CLI.
// It reuses controlled deployment handlers and durable coordinator state; it
// neither invents components nor acknowledges them on the customer's behalf.
type OfflineDeliveryRunner struct {
	identity            offlinePrincipalResolver
	licenses            offlineLicenseImporter
	coordinator         offlineCoordinator
	onboarding          offlineOnboarding
	environment, origin string
	interval            time.Duration
	source              offlineInstallationSource
	lifecycle           offlineInstallationLifecycle
	initializer         offlineInstallationInitializer
}

func (r *OfflineDeliveryRunner) ConfigureInstallation(source offlineInstallationSource, lifecycle offlineInstallationLifecycle, initializer offlineInstallationInitializer) error {
	if source == nil || lifecycle == nil || initializer == nil {
		return errors.New("installation preparation dependencies are required")
	}
	r.source, r.lifecycle, r.initializer = source, lifecycle, initializer
	return nil
}

// Prepare freezes observed installation facts before controlled enrollment.
// Existing identities and baselines are reused so retries cannot widen legacy eligibility.
func (r *OfflineDeliveryRunner) Prepare(ctx context.Context, scenario, customer, raw string) (coordination.InstallationBaseline, error) {
	var empty coordination.InstallationBaseline
	if scenario != "fresh" && scenario != "migrate" && scenario != "platform-only" && scenario != "expand" {
		return empty, errors.New("invalid installation scenario")
	}
	if strings.TrimSpace(customer) == "" || customer != strings.TrimSpace(customer) || r.source == nil || r.lifecycle == nil || r.initializer == nil {
		return empty, errors.New("customer and configured installation dependencies are required")
	}
	p, err := r.principal(ctx)
	if err != nil {
		return empty, err
	}
	actor := domain.Actor{TenantID: p.Tenant.ID, UserID: p.User.ID}
	state, err := r.initializer.Read(ctx)
	importedIdentity := false
	if errors.Is(err, domain.ErrNotInitialized) {
		if raw != "" {
			// This projection is only an early customer mismatch guard. ImportDelivery
			// still performs the trusted signature verification before any write.
			parts := strings.Split(raw, ".")
			if len(parts) != 3 {
				return empty, errors.New("invalid signed installation identity")
			}
			payload, e := base64.RawURLEncoding.DecodeString(parts[1])
			var signed struct {
				CustomerID string `json:"customer_id"`
			}
			if e != nil || json.Unmarshal(payload, &signed) != nil || signed.CustomerID != customer {
				return empty, errors.New("signed installation customer mismatch")
			}
			imported, e := r.Import(ctx, raw)
			if e != nil {
				return empty, e
			}
			state = imported.State
			importedIdentity = true
		} else {
			state, err = r.initializer.Initialize(ctx, license.InitializeInput{CustomerID: customer, Environment: r.environment}, actor)
			if err != nil {
				return empty, err
			}
		}
	} else if err != nil {
		return empty, err
	}
	if state.CustomerID != customer || state.Environment != r.environment || state.InstanceID == "" {
		return empty, errors.New("installation identity does not match customer/production environment")
	}
	if raw != "" && !importedIdentity {
		if _, err = r.Import(ctx, raw); err != nil {
			return empty, err
		}
	}
	baseline, err := r.lifecycle.ReadInstallationBaseline(ctx)
	var report evidence.Report
	if err == nil {
		if baseline.InstanceID != state.InstanceID || baseline.Environment != state.Environment {
			return empty, errors.New("installation baseline identity mismatch")
		}
		report.Project = baseline.Project
	} else if errors.Is(err, coordination.ErrNotReady) {
		report, err = r.source.CollectMigrationInstallationEvidence(ctx)
		if err != nil {
			return empty, fmt.Errorf("collect installation baseline: %w", err)
		}
	} else {
		return empty, err
	}
	return r.lifecycle.PrepareInstallation(ctx, scenario, report, actor)
}

func NewOfflineDeliveryRunner(i offlinePrincipalResolver, l offlineLicenseImporter, c offlineCoordinator, o offlineOnboarding, cfg config.Config) (*OfflineDeliveryRunner, error) {
	if i == nil || l == nil || c == nil || o == nil {
		return nil, errors.New("offline delivery dependencies are required")
	}
	if cfg.Environment != "production" || cfg.Audit.EnvironmentCode != "prod" {
		return nil, errors.New("offline delivery requires production/prod")
	}
	origin, err := url.Parse(cfg.HTTP.PublicBaseURL)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return nil, errors.New("offline delivery requires configured public origin")
	}
	return &OfflineDeliveryRunner{identity: i, licenses: l, coordinator: c, onboarding: o, environment: cfg.Environment, origin: origin.Scheme + "://" + origin.Host, interval: 2 * time.Second}, nil
}

var offlinePermissions = []string{"platform:license:read", "platform:license:manage", "platform:application:read", "platform:application:create", "platform:application:update", "platform:application-environment:create", "platform:application-environment:update", "platform:application-login-target:create", "platform:application-login-target:update", "platform:oauth-client:disable", "platform:role-binding:update"}

func (r *OfflineDeliveryRunner) principal(ctx context.Context) (authctx.Principal, error) {
	p, err := r.identity.OfflineBootstrapPrincipal(ctx, time.Now().UTC())
	if err != nil {
		return p, err
	}
	role := false
	for _, v := range p.Roles {
		if v.Code == identity.BootstrapSuperAdminRoleCode {
			role = true
		}
	}
	if !role || p.Tenant.Code != identity.BootstrapTenantCode || p.User.ID == "" || p.Account.ID == "" || p.Tenant.ID == "" {
		return authctx.Principal{}, errors.New("offline delivery requires active persisted bootstrap administrator")
	}
	granted := map[string]bool{}
	for _, code := range p.PermissionCodes {
		granted[code] = true
	}
	for _, code := range offlinePermissions {
		if !granted[code] {
			return authctx.Principal{}, fmt.Errorf("offline administrator lacks permission %s", code)
		}
	}
	return p, nil
}

func (r *OfflineDeliveryRunner) Import(ctx context.Context, raw string) (license.DeliveryImportResult, error) {
	p, err := r.principal(ctx)
	if err != nil {
		return license.DeliveryImportResult{}, err
	}
	return r.licenses.ImportDelivery(ctx, raw, r.environment, domain.Actor{TenantID: p.Tenant.ID, UserID: p.User.ID})
}

type OfflineActivationResult struct {
	Status       string   `json:"status"`
	Applications []string `json:"applications"`
}

// MigrateExisting deploys only the independently frozen legacy set. It never
// starts enforcement without a license or grants legacy eligibility to new apps.
func (r *OfflineDeliveryRunner) MigrateExisting(ctx context.Context) (OfflineActivationResult, error) {
	out := OfflineActivationResult{Status: "INCOMPLETE", Applications: []string{}}
	if r.lifecycle == nil || r.initializer == nil {
		return out, errors.New("installation dependencies are required")
	}
	if _, err := r.principal(ctx); err != nil {
		return out, err
	}
	state, err := r.initializer.Read(ctx)
	if err != nil {
		return out, err
	}
	b, err := r.lifecycle.ReadInstallationBaseline(ctx)
	if err != nil {
		return out, err
	}
	if b.InstanceID != state.InstanceID || b.Environment != state.Environment {
		return out, errors.New("installation baseline identity mismatch")
	}
	var report evidence.Report
	if json.Unmarshal([]byte(b.EvidenceJSON), &report) != nil || report.Project != b.Project || !report.Complete {
		return out, errors.New("invalid stored installation baseline")
	}
	apps := map[string]bool{}
	for _, f := range report.Facts {
		if _, ok := offlineTargets[f.Application]; !ok || f.Environment != "prod" {
			return out, errors.New("unsupported frozen legacy application")
		}
		apps[f.Application] = true
	}
	ordered := make([]string, 0, len(apps))
	for app := range apps {
		ordered = append(ordered, app)
	}
	sort.Strings(ordered)
	for _, app := range ordered {
		p, err := r.principal(ctx)
		if err != nil {
			return out, err
		}
		if err = r.ensureDeployment(ctx, p, app); err != nil {
			return out, err
		}
	}
	// Check the entire set together so replacement during another app's update
	// cannot make an earlier readiness/ACK confirmation stale.
	for {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		p, err := r.principal(ctx)
		if err != nil {
			return out, err
		}
		complete := true
		for _, app := range ordered {
			deployment, _, e := r.deployment(ctx, p, app)
			if e != nil {
				return out, e
			}
			if deployment == registry.SubsystemDeploymentStatusFailed {
				return out, fmt.Errorf("application %s migration deployment failed", app)
			}
			st, e := r.coordinator.Status(ctx, app)
			if e != nil && !errors.Is(e, coordination.ErrNotReady) {
				return out, e
			}
			if deployment != registry.SubsystemDeploymentStatusReady || e != nil || !(enforcementComplete(st) || (st.State == runtime.Pending && st.MigrationEligible && snapshotAcknowledged(st))) {
				complete = false
			}
		}
		if complete {
			out.Status, out.Applications = "MIGRATION_READY", ordered
			return out, nil
		}
		timer := time.NewTimer(r.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
	}
}

// Activate is safely resumable: registry provisioning and enforcement ACKs are
// persisted by their existing services, rather than an in-memory progress file.
func (r *OfflineDeliveryRunner) Activate(ctx context.Context, raw string) (OfflineActivationResult, error) {
	out := OfflineActivationResult{Status: "INCOMPLETE"}
	imported, err := r.Import(ctx, raw)
	if err != nil {
		return out, err
	}
	if imported.Status != "IMPORTED" || imported.State.Current == nil {
		out.Status = imported.Status
		return out, fmt.Errorf("offline activation is not available: %s", imported.Status)
	}
	applications := offlineDeploymentOrder(imported.State.Current.Applications)
	for _, app := range applications {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		p, err := r.principal(ctx)
		if err != nil {
			return out, err
		}
		if err := r.ensureDeployment(ctx, p, app.Code); err != nil {
			return out, err
		}
		for {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			p, err = r.principal(ctx)
			if err != nil {
				return out, err
			}
			state, _, err := r.deployment(ctx, p, app.Code)
			if err != nil {
				return out, err
			}
			if state == registry.SubsystemDeploymentStatusFailed {
				return out, fmt.Errorf("application %s deployment failed; controlled retry is required", app.Code)
			}
			if state == registry.SubsystemDeploymentStatusReady {
				st, e := r.coordinator.Status(ctx, app.Code)
				if e != nil && !errors.Is(e, coordination.ErrNotReady) {
					return out, e
				}
				if e == nil {
					if enforcementComplete(st) {
						out.Applications = append(out.Applications, app.Code)
						break
					}
					if st.State == runtime.Pending {
						e = r.coordinator.BeginActivation(ctx, app.Code, st.Revision, domain.Actor{TenantID: p.Tenant.ID, UserID: p.User.ID})
						if e != nil && !errors.Is(e, coordination.ErrNotReady) && !errors.Is(e, coordination.ErrConflict) {
							return out, e
						}
					}
				}
			}
			timer := time.NewTimer(r.interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return out, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if len(out.Applications) != len(applications) || len(applications) == 0 {
		return out, errors.New("offline enforcement confirmation incomplete")
	}
	// Recheck the entire signed set after the last application is deployed.
	// A concurrent component replacement may invalidate an earlier confirmation.
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if _, err := r.principal(ctx); err != nil {
			return out, err
		}
		current, err := r.Import(ctx, raw)
		if err != nil {
			return out, err
		}
		if current.Status != "IMPORTED" {
			return out, fmt.Errorf("license validity changed during activation: %s", current.Status)
		}
		complete := true
		for _, app := range applications {
			st, err := r.coordinator.Status(ctx, app.Code)
			if err != nil {
				return out, err
			}
			if !enforcementComplete(st) {
				complete = false
			}
		}
		if complete {
			break
		}
		timer := time.NewTimer(r.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
	}
	out.Status = "ENFORCED"
	return out, nil
}

// Portal's conditional invitation worker consumes CRM's controlled runtime
// binding. Resolve that purchased dependency before collecting Portal members;
// never add an unpurchased application or mutate the signed license payload.
func offlineDeploymentOrder(signed []core.Application) []core.Application {
	ordered := append([]core.Application(nil), signed...)
	crm, portal := -1, -1
	for i, app := range ordered {
		switch app.Code {
		case "customer_and_opportunity":
			crm = i
		case "customer_portal":
			portal = i
		}
	}
	if portal >= 0 && crm > portal {
		dependency := ordered[crm]
		copy(ordered[portal+1:crm+1], ordered[portal:crm])
		ordered[portal] = dependency
	}
	return ordered
}

func (r *OfflineDeliveryRunner) ensureDeployment(ctx context.Context, p authctx.Principal, app string) error {
	spec, ok := offlineTargets[app]
	if !ok {
		return errors.New("license includes unsupported deployment application")
	}
	payload := map[string]string{"application_code": app, "application_name": spec.name, "environment": "prod", "public_base_url": r.origin, "upstream_url": spec.upstream, "path_prefix": spec.prefix}
	deployment, code, err := r.deployment(ctx, p, app)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		if _, _, err := r.invoke(ctx, p, http.MethodPost, "/api/v1/subsystem-directory", payload, r.onboarding.RegisterSubsystemDirectory); err != nil {
			return err
		}
		deployment = registry.SubsystemDeploymentStatusUnmanaged
	}
	delete(payload, "application_name")
	switch deployment {
	case registry.SubsystemDeploymentStatusUnmanaged:
		if _, _, err := r.invoke(ctx, p, http.MethodPost, "/api/v1/subsystem-adoption", payload, r.onboarding.AdoptSubsystem); err != nil {
			return err
		}
	case registry.SubsystemDeploymentStatusFailed:
		if _, _, err := r.invoke(ctx, p, http.MethodPost, "/api/v1/subsystem-retry", payload, r.onboarding.UpdateSubsystem); err != nil {
			return err
		}
	case registry.SubsystemDeploymentStatusReady:
		st, e := r.coordinator.Status(ctx, app)
		if errors.Is(e, coordination.ErrNotReady) || (e == nil && len(st.Members) == 0) {
			if _, _, err := r.invoke(ctx, p, http.MethodPost, "/api/v1/subsystem-update", payload, r.onboarding.UpdateSubsystem); err != nil {
				return err
			}
		} else if e != nil {
			return e
		}
	case registry.SubsystemDeploymentStatusProvisioning, registry.SubsystemDeploymentStatusUpdating:
		// Another controlled attempt is active; wait, do not rotate credentials.
	default:
		return fmt.Errorf("application %s cannot resume deployment state %s", app, deployment)
	}
	return nil
}

func enforcementComplete(s coordination.Status) bool {
	return s.State == runtime.Enforced && snapshotAcknowledged(s)
}

func snapshotAcknowledged(s coordination.Status) bool {
	if !s.SnapshotCurrent || len(s.Members) == 0 {
		return false
	}
	for _, m := range s.Members {
		if m.ReadyAt == nil || m.AckAt == nil || m.AckRevision != s.Revision || m.IssuedRevision != s.Revision || m.RetiredAt != nil {
			return false
		}
	}
	return true
}

func (r *OfflineDeliveryRunner) deployment(ctx context.Context, p authctx.Principal, app string) (string, int, error) {
	data, code, err := r.invoke(ctx, p, http.MethodGet, "/api/v1/subsystem-status?application_code="+url.QueryEscape(app)+"&environment=prod", nil, r.onboarding.GetSubsystemStatus)
	if code == http.StatusNotFound {
		return "", code, nil
	}
	if err != nil {
		return "", code, err
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Status == "" {
		return "", code, errors.New("invalid controlled deployment status")
	}
	return out.Status, code, nil
}

func (r *OfflineDeliveryRunner) invoke(ctx context.Context, p authctx.Principal, method, path string, payload any, handler http.HandlerFunc) (json.RawMessage, int, error) {
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
	}
	req, err := http.NewRequestWithContext(authctx.WithPrincipal(ctx, p), method, "http://offline-maintenance.invalid"+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)
	var envelope struct {
		Data json.RawMessage `json:"data"`
		Code string          `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		return nil, w.Code, errors.New("invalid controlled deployment response")
	}
	if w.Code < 200 || w.Code >= 300 {
		return nil, w.Code, fmt.Errorf("controlled deployment request %s failed (%d, %s)", path, w.Code, envelope.Code)
	}
	return envelope.Data, w.Code, nil
}

// These routes match the reviewed production manifests, not caller input.
var offlineTargets = map[string]struct{ name, upstream, prefix string }{
	"contract_management":      {"合同管理系统", "http://contract-api:8081", "/contract_management"},
	"customer_and_opportunity": {"客户与商机管理系统", "http://customer-api:8090", "/customer-opportunity"},
	"project_management":       {"项目管理系统", "http://project-api:8082", "/project_management"},
	"settlement":               {"结算与开票管理系统", "http://settlement-api:8085", "/settlement"},
	"data_analysis":            {"数据看板与统计分析系统", "http://data-analysis-api:8080", "/data_analysis"},
	"customer_portal":          {"客户门户系统", "http://portal-api:8091", "/customer-portal"},
}

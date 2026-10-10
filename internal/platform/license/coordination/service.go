// Package coordination owns durable runtime enrollment and activation. Its
// installation inventory entry points are internal deployment calls, not DTOs
// accepted from a browser or a runtime machine token.
package coordination

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	core "github.com/J-S-Te/license-core"
	runtime "github.com/J-S-Te/license-core/runtime"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type LicenseReader interface {
	Read(context.Context) (domain.State, error)
}

// Signer signs with the platform's existing managed signing identity. It must
// not load a customer-supplied key or reuse the manufacturer's private key.
type Signer interface {
	Sign(runtime.PlatformSnapshot) (string, error)
	Verify(string, runtime.Binding) (runtime.PlatformSnapshot, error)
}
type Service struct {
	applicationEnvironment string
	db                     *gorm.DB
	reader                 LicenseReader
	vendor                 map[string]ed25519.PublicKey
	signer                 Signer
	now                    func() time.Time
	lifecycleApproval      LifecycleApprovalVerifier
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var containerPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Option func(*Service) error

// WithApplicationEnvironment binds the registered OAuth/profile environment
// independently of APP_ENV (e.g. prod versus production). This explicit boot
// configuration is never inferred from a machine request or a license token.
func WithApplicationEnvironment(environment string) Option {
	return func(s *Service) error {
		if !identifier.MatchString(environment) {
			return ErrInvalid
		}
		s.applicationEnvironment = environment
		return nil
	}
}
func NewService(db *gorm.DB, reader LicenseReader, vendor map[string]ed25519.PublicKey, signer Signer, now func() time.Time, options ...Option) (*Service, error) {
	if db == nil || reader == nil || signer == nil {
		return nil, ErrInvalid
	}
	keys := map[string]ed25519.PublicKey{}
	for kid, key := range vendor {
		if !identifier.MatchString(kid) || len(key) != ed25519.PublicKeySize {
			return nil, ErrInvalid
		}
		keys[kid] = append(ed25519.PublicKey(nil), key...)
	}
	if now == nil {
		now = time.Now
	}
	s := &Service{db: db, reader: reader, vendor: keys, signer: signer, now: now}
	for _, option := range options {
		if option == nil {
			return nil, ErrInvalid
		}
		if err := option(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}
func validApplication(app string) bool {
	switch app {
	case "contract_management", "customer_and_opportunity", "project_management", "settlement", "data_analysis", "customer_portal":
		return true
	}
	return false
}
func validSpec(spec ServiceSpec) bool {
	return validApplication(spec.Application) && identifier.MatchString(spec.Environment) && identifier.MatchString(spec.ServiceID) && identifier.MatchString(spec.OAuthClientID) && digestPattern.MatchString(spec.CoverageDigest) && digestPattern.MatchString(spec.ImageDigest)
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func fresh(t time.Time, now time.Time) bool {
	return !t.IsZero() && !t.After(now) && now.Sub(t) <= 60*time.Second
}

// lock first lets the existing licensing service observe time and activate an
// entire pending license. The row lock then checks the same revision and
// independently verifies raw artifacts, closing the import/clock TOCTOU gap.
func (s *Service) lock(ctx context.Context, fn func(*gorm.DB, *domain.Deployment, domain.State) error) error {
	st, err := s.reader.Read(ctx)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var d domain.Deployment
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&d, "id = ?", 1).Error; e != nil {
			return e
		}
		if d.Revision != st.Revision || d.CurrentDigest != st.CurrentDigest || d.PendingDigest != st.PendingDigest || d.HighestVersion != st.HighestVersion {
			return ErrConflict
		}
		if d.ClockBlocked || s.now().Unix() < d.HighestObservedAt-300 {
			return domain.ErrClock
		}
		if d.InstanceID != st.InstanceID || d.CustomerID != st.CustomerID || d.Environment != st.Environment {
			return domain.ErrCorrupt
		}
		for _, slot := range []struct {
			digest  string
			license *core.License
		}{{d.CurrentDigest, st.Current}, {d.PendingDigest, st.Pending}} {
			if slot.digest == "" {
				if slot.license != nil {
					return domain.ErrCorrupt
				}
				continue
			}
			var a domain.Artifact
			if e := tx.First(&a, "digest = ?", slot.digest).Error; e != nil {
				return domain.ErrCorrupt
			}
			l, e := core.Verify(a.RawJWS, s.vendor)
			if e != nil {
				return domain.ErrCorrupt
			}
			dg, e := core.Digest(a.RawJWS, s.vendor)
			if e != nil || dg != slot.digest || a.Version != l.Version || l.InstanceID != d.InstanceID || l.Environment != d.Environment || l.CustomerID != d.CustomerID || l.Version > d.HighestVersion || slot.license == nil || l.Version != slot.license.Version {
				return domain.ErrCorrupt
			}
		}
		if st.Pending != nil && st.Pending.Version != d.HighestVersion {
			return domain.ErrCorrupt
		}
		if st.Pending == nil && st.CurrentVersion != d.HighestVersion {
			return domain.ErrCorrupt
		}
		return fn(tx, &d, st)
	})
}
func (s *Service) bump(tx *gorm.DB, d *domain.Deployment, a *Application) error {
	d.Revision++
	d.UpdatedAt = s.now().UTC()
	a.Revision = d.Revision
	a.UpdatedAt = d.UpdatedAt
	if e := tx.Save(d).Error; e != nil {
		return e
	}
	return tx.Save(a).Error
}
func (s *Service) add(tx *gorm.DB, d *domain.Deployment, spec ServiceSpec, eligible bool) error {
	environment := s.applicationEnvironment
	if environment == "" {
		environment = d.Environment
	}
	if !validSpec(spec) || spec.Environment != environment {
		return ErrInvalid
	}
	var retiredClients int64
	if e := tx.Model(&RetiredClient{}).Where("oauth_client_id = ?", spec.OAuthClientID).Count(&retiredClients).Error; e != nil {
		return e
	}
	if retiredClients != 0 {
		return ErrConflict
	}
	var existing Member
	e := tx.First(&existing, "service_id = ?", spec.ServiceID).Error
	if e == nil {
		if existing.RetiredAt == nil && existing.Application == spec.Application && existing.Environment == spec.Environment && existing.OAuthClientID == spec.OAuthClientID && existing.CoverageDigest == spec.CoverageDigest && existing.ImageDigest == spec.ImageDigest {
			return nil
		}
		return ErrConflict
	}
	if !errors.Is(e, gorm.ErrRecordNotFound) {
		return e
	}
	var a Application
	e = tx.First(&a, "application = ?", spec.Application).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		a = Application{Application: spec.Application, State: runtime.Pending, MigrationEligible: eligible}
	} else if e != nil {
		return e
	} else {
		// Existing inventories cannot silently grow after activation or retain a
		// legacy bypass when a newly purchased service is added.
		if a.State != runtime.Pending {
			return ErrConflict
		}
		if !eligible {
			a.MigrationEligible = false
		}
		a.ContentDigest = ""
	}
	if e = s.bump(tx, d, &a); e != nil {
		return e
	}
	if e = tx.Create(&Member{ServiceID: spec.ServiceID, Application: spec.Application, Environment: spec.Environment, OAuthClientID: spec.OAuthClientID, CoverageDigest: spec.CoverageDigest, ImageDigest: spec.ImageDigest, CreatedAt: s.now().UTC()}).Error; e != nil {
		return e
	}
	return tx.Create(&domain.Event{Kind: "RUNTIME_SERVICE_REGISTERED", Revision: d.Revision, CreatedAt: s.now().UTC()}).Error
}

// FreezeInventory must only be called from the trusted deployment Agent with
// operator-approved release specs and its just-collected report. It is a
// once-only installation fact, never a repeatable "grandfather" switch.
func (s *Service) FreezeInventory(ctx context.Context, report evidence.Report, specs []ServiceSpec) error {
	now := s.now().UTC()
	if report.Scope != "INSTALLATION" || !report.BoundarySupported || !report.Complete || len(report.Problems) != 0 || !identifier.MatchString(report.Project) || !fresh(report.CollectedAt, now) || len(specs) > 64 {
		return ErrNotReady
	}
	facts := map[string]evidence.Fact{}
	for _, f := range report.Facts {
		if _, seen := facts[f.Service]; seen || !f.Running || len(f.Problems) != 0 || f.Protocol != "1" || !identifier.MatchString(f.Version) || !containerPattern.MatchString(f.ContainerID) || !digestPattern.MatchString(f.ImageDigest) || f.StartedAt.IsZero() || f.StartedAt.After(report.CollectedAt) {
			return ErrNotReady
		}
		facts[f.Service] = f
	}
	if len(facts) != len(specs) {
		return ErrNotReady
	}
	seen := map[string]bool{}
	for _, spec := range specs {
		f, ok := facts[spec.ServiceID]
		if !ok || seen[spec.ServiceID] || !validSpec(spec) || f.Application != spec.Application || f.Environment != spec.Environment || f.ImageDigest != spec.ImageDigest {
			return ErrNotReady
		}
		seen[spec.ServiceID] = true
	}
	if err := validateControlledInfrastructure(report, facts); err != nil {
		return err
	}
	b, e := json.Marshal(report)
	if e != nil {
		return e
	}
	return s.lock(ctx, func(tx *gorm.DB, d *domain.Deployment, _ domain.State) error {
		for _, f := range facts {
			if f.StartedAt.After(d.CreatedAt) {
				return ErrNotReady
			}
		}
		for _, f := range report.InfrastructureFacts {
			if f.StartedAt.After(d.CreatedAt) {
				return ErrNotReady
			}
		}
		var n int64
		if e := tx.Model(&Inventory{}).Count(&n).Error; e != nil {
			return e
		}
		if n != 0 {
			return ErrConflict
		}
		if e := tx.Model(&InstallationBaseline{}).Count(&n).Error; e != nil {
			return e
		}
		if n != 0 {
			return ErrConflict
		}
		// A freeze after any manual enrollment cannot rewrite those identities.
		if e := tx.Model(&Member{}).Count(&n).Error; e != nil {
			return e
		}
		if n != 0 {
			return ErrConflict
		}
		for _, spec := range specs {
			if e := s.add(tx, d, spec, true); e != nil {
				return e
			}
		}
		if e := tx.Create(&Inventory{ID: 1, Project: report.Project, EvidenceDigest: hash(string(b)), CollectedAt: report.CollectedAt, CreatedAt: now}).Error; e != nil {
			return e
		}
		return tx.Create(&domain.Event{Kind: "MIGRATION_INVENTORY_FROZEN", Revision: d.Revision, CreatedAt: now}).Error
	})
}

// Register is an internal controlled-deployment action. New services never
// inherit migration qualification, even after the original inventory freeze.
func (s *Service) Register(ctx context.Context, spec ServiceSpec) error {
	return s.lock(ctx, func(tx *gorm.DB, d *domain.Deployment, _ domain.State) error {
		eligible, err := s.baselineEligibility(tx, d, spec.Application)
		if err != nil {
			return err
		}
		return s.add(tx, d, spec, eligible)
	})
}
func (s *Service) member(tx *gorm.DB, ctx context.Context, d *domain.Deployment, id string) (Member, error) {
	p, ok := appctx.PrincipalFromContext(ctx)
	if !ok || !p.HasScope("license.runtime") || !identifier.MatchString(id) {
		return Member{}, ErrForbidden
	}
	var m Member
	if e := tx.First(&m, "service_id = ?", id).Error; e != nil {
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return m, ErrForbidden
		}
		return m, e
	}
	environment := s.applicationEnvironment
	if environment == "" {
		environment = d.Environment
	}
	if !matchesRuntimeMember(p, m, environment) {
		return m, ErrForbidden
	}
	return m, nil
}

func matchesRuntimeMember(p appctx.Principal, m Member, environment string) bool {
	// Enrollment and reviewed lifecycle approvals use the external client_id.
	// Principal.OAuthClientID is the database PK; Authenticate has already checked
	// both identities against the current active, tenant/app/environment-bound row.
	return m.RetiredAt == nil && p.ClientID == m.OAuthClientID && p.ApplicationCode == m.Application && p.EnvironmentCode == m.Environment && m.Environment == environment
}
func (s *Service) Ready(ctx context.Context, id string, in ReadyInput) error {
	return s.lock(ctx, func(tx *gorm.DB, d *domain.Deployment, _ domain.State) error {
		m, e := s.member(tx, ctx, d, id)
		if e != nil {
			return e
		}
		if in.Protocol != runtime.Protocol || in.CoverageDigest != m.CoverageDigest || in.ImageDigest != m.ImageDigest {
			return ErrNotReady
		}
		now := s.now().UTC()
		return tx.Model(&m).Update("ready_at", now).Error
	})
}
func contentDigest(a Application, d *domain.Deployment) string {
	b, _ := json.Marshal([]any{a.State, a.MigrationEligible, d.CurrentDigest, d.PendingDigest, d.HighestVersion})
	return hash(string(b))
}
func (s *Service) BeginActivation(ctx context.Context, app string, expected uint64, actor domain.Actor) error {
	if !validApplication(app) || !identifier.MatchString(actor.TenantID) || !identifier.MatchString(actor.UserID) {
		return ErrInvalid
	}
	return s.lock(ctx, func(tx *gorm.DB, d *domain.Deployment, st domain.State) error {
		var a Application
		if e := tx.First(&a, "application = ?", app).Error; e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				return ErrNotReady
			}
			return e
		}
		if a.Revision != expected {
			return ErrConflict
		}
		if a.State == runtime.Applying || a.State == runtime.Enforced {
			return nil
		}
		if a.State != runtime.Pending {
			return domain.ErrCorrupt
		}
		var members []Member
		if e := tx.Where("application = ? AND retired_at IS NULL", app).Find(&members).Error; e != nil {
			return e
		}
		if len(members) == 0 {
			return ErrNotReady
		}
		for _, m := range members {
			if m.ReadyAt == nil || !fresh(*m.ReadyAt, s.now().UTC()) {
				return ErrNotReady
			}
		}
		if st.Current == nil {
			return ErrNotReady
		}
		effectiveNow := s.now()
		if effectiveNow.Unix() < d.HighestObservedAt {
			effectiveNow = time.Unix(d.HighestObservedAt, 0)
		}
		if e := core.Evaluate(*st.Current, app, core.MUTATE_BUSINESS, effectiveNow, d.InstanceID, d.Environment); e != nil {
			return e
		}
		a.State = runtime.Applying
		a.MigrationEligible = false
		a.ContentDigest = contentDigest(a, d)
		if e := s.bump(tx, d, &a); e != nil {
			return e
		}
		if e := tx.Model(&Member{}).Where("application = ?", app).Updates(map[string]any{"ack_revision": 0, "ack_at": nil}).Error; e != nil {
			return e
		}
		return tx.Create(&domain.Event{Kind: "ENFORCEMENT_APPLYING", Revision: d.Revision, TenantID: actor.TenantID, UserID: actor.UserID, CreatedAt: s.now().UTC()}).Error
	})
}
func (s *Service) Snapshot(ctx context.Context, id string) (SnapshotOutput, error) {
	var out SnapshotOutput
	err := s.lock(ctx, func(tx *gorm.DB, d *domain.Deployment, _ domain.State) error {
		m, e := s.member(tx, ctx, d, id)
		if e != nil {
			return e
		}
		var a Application
		if e = tx.First(&a, "application = ?", m.Application).Error; e != nil {
			return e
		}
		dg := contentDigest(a, d)
		if a.ContentDigest != dg {
			a.ContentDigest = dg
			if e = s.bump(tx, d, &a); e != nil {
				return e
			}
			if e = tx.Model(&Member{}).Where("application = ?", a.Application).Updates(map[string]any{"ack_revision": 0, "ack_at": nil}).Error; e != nil {
				return e
			}
		}
		if m.IssuedRevision > a.Revision {
			return domain.ErrCorrupt
		}
		if m.IssuedRevision == a.Revision {
			if m.RawJWS == "" || m.IssuedDigest != hash(m.RawJWS) {
				return domain.ErrCorrupt
			}
			if e = s.verifyIssued(m, a, d); e != nil {
				return e
			}
			out = SnapshotOutput{m.RawJWS, m.IssuedDigest, m.IssuedRevision}
			return nil
		}
		payload := runtime.PlatformSnapshot{Protocol: runtime.Protocol, InstanceID: d.InstanceID, Environment: d.Environment, Application: m.Application, ServiceID: m.ServiceID, Revision: a.Revision, EnforcementState: a.State, MigrationEligible: a.MigrationEligible, HighestLicenseVersion: d.HighestVersion, SnapshotIssuedAt: s.now().Unix()}
		for _, slot := range []struct {
			digest string
			dst    *string
		}{{d.CurrentDigest, &payload.CurrentLicenseJWS}, {d.PendingDigest, &payload.PendingLicenseJWS}} {
			if slot.digest != "" {
				var art domain.Artifact
				if e = tx.First(&art, "digest = ?", slot.digest).Error; e != nil {
					return e
				}
				*slot.dst = art.RawJWS
			}
		}
		raw, e := s.signer.Sign(payload)
		if e != nil {
			return e
		}
		out = SnapshotOutput{raw, hash(raw), a.Revision}
		issued := m
		issued.RawJWS = raw
		issued.IssuedRevision = out.Revision
		issued.IssuedDigest = out.Digest
		if e = s.verifyIssued(issued, a, d); e != nil {
			return e
		}
		return tx.Model(&m).Updates(map[string]any{"issued_revision": out.Revision, "issued_digest": out.Digest, "raw_jws": raw}).Error
	})
	return out, err
}
func (s *Service) Ack(ctx context.Context, id string, in AckInput) error {
	return s.lock(ctx, func(tx *gorm.DB, d *domain.Deployment, _ domain.State) error {
		m, e := s.member(tx, ctx, d, id)
		if e != nil {
			return e
		}
		var a Application
		if e = tx.First(&a, "application = ?", m.Application).Error; e != nil {
			return e
		}
		if a.ContentDigest != contentDigest(a, d) || in.Revision != a.Revision || m.IssuedRevision != in.Revision || in.Digest != m.IssuedDigest || in.Digest == "" || hash(m.RawJWS) != in.Digest {
			return ErrConflict
		}
		if m.ReadyAt == nil || !fresh(*m.ReadyAt, s.now().UTC()) {
			return ErrNotReady
		}
		if e = s.verifyIssued(m, a, d); e != nil {
			return e
		}
		now := s.now().UTC()
		if e = tx.Model(&m).Updates(map[string]any{"ack_revision": in.Revision, "ack_at": now}).Error; e != nil {
			return e
		}
		if a.State != runtime.Applying {
			return nil
		}
		var members []Member
		if e = tx.Where("application = ? AND retired_at IS NULL", a.Application).Find(&members).Error; e != nil {
			return e
		}
		for _, member := range members {
			if member.AckRevision != a.Revision || member.ReadyAt == nil || !fresh(*member.ReadyAt, now) {
				return nil
			}
		}
		a.State = runtime.Enforced
		a.ContentDigest = contentDigest(a, d)
		if e = s.bump(tx, d, &a); e != nil {
			return e
		}
		return tx.Create(&domain.Event{Kind: "ENFORCEMENT_ENFORCED", Revision: d.Revision, CreatedAt: now}).Error
	})
}
func (s *Service) verifyIssued(m Member, a Application, d *domain.Deployment) error {
	payload, e := s.signer.Verify(m.RawJWS, runtime.Binding{InstanceID: d.InstanceID, Environment: d.Environment, Application: m.Application, ServiceID: m.ServiceID})
	if e != nil || payload.Revision != a.Revision || payload.EnforcementState != a.State || payload.MigrationEligible != a.MigrationEligible || payload.HighestLicenseVersion != d.HighestVersion {
		return domain.ErrCorrupt
	}
	for _, slot := range []struct{ raw, digest string }{{payload.CurrentLicenseJWS, d.CurrentDigest}, {payload.PendingLicenseJWS, d.PendingDigest}} {
		if slot.digest == "" {
			if slot.raw != "" {
				return domain.ErrCorrupt
			}
			continue
		}
		dg, e := core.Digest(slot.raw, s.vendor)
		if e != nil || dg != slot.digest {
			return domain.ErrCorrupt
		}
	}
	return nil
}
func (s *Service) Status(ctx context.Context, app string) (Status, error) {
	var out Status
	if !validApplication(app) {
		return out, ErrInvalid
	}
	err := s.lock(ctx, func(tx *gorm.DB, d *domain.Deployment, _ domain.State) error {
		if e := tx.First(&out.Application, "application = ?", app).Error; e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				return ErrNotReady
			}
			return e
		}
		out.DeploymentRevision = d.Revision
		// ACKs for an old license must not confirm a renewal before consumers
		// fetch and persist the new snapshot. Global revisions alone cannot
		// establish this because other applications also advance them.
		out.SnapshotCurrent = out.ContentDigest == contentDigest(out.Application, d)
		return tx.Where("application = ? AND retired_at IS NULL", app).Order("service_id ASC").Find(&out.Members).Error
	})
	return out, err
}

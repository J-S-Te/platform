package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	runtime "github.com/J-S-Te/license-core/runtime"
	"gorm.io/gorm"
)

// Approval is returned only by the trusted release inventory reader. Required
// members and explicit retirement permission must never be supplied by a runtime
// token or copied from a browser's proposed component list.
type LifecycleApproval struct {
	Specs              []ServiceSpec
	RequiredServiceIDs []string
	RetireServiceIDs   []string
}
type LifecycleInput struct {
	Application      string
	Environment      string
	ExpectedRevision uint64
	OperationID      string
	ReleaseDigest    string
}
type LifecycleApprovalVerifier interface {
	VerifyRuntimeLifecycle(context.Context, LifecycleInput) (LifecycleApproval, error)
}

func WithLifecycleApprovalVerifier(v LifecycleApprovalVerifier) Option {
	return func(s *Service) error {
		if v == nil {
			return ErrInvalid
		}
		s.lifecycleApproval = v
		return nil
	}
}

type LifecycleEvent struct {
	OperationID   string `gorm:"primaryKey;size:128"`
	Application   string
	RequestDigest string
	ReleaseDigest string
	OldSpecs      string
	NewSpecs      string
	Revision      uint64
	TenantID      string
	UserID        string
	CreatedAt     time.Time
}

func (LifecycleEvent) TableName() string { return "license_runtime_lifecycle_event" }

type RetiredClient struct {
	OAuthClientID string `gorm:"primaryKey;column:oauth_client_id;size:128"`
	ServiceID     string
	RetiredAt     time.Time
}

func (RetiredClient) TableName() string { return "license_runtime_retired_client" }

func canonicalApproval(in LifecycleInput, approval LifecycleApproval) (LifecycleApproval, error) {
	if !validApplication(in.Application) || !identifier.MatchString(in.Environment) || in.ExpectedRevision == 0 || !identifier.MatchString(in.OperationID) || !digestPattern.MatchString(in.ReleaseDigest) || len(approval.Specs) == 0 || len(approval.Specs) > 64 || len(approval.RequiredServiceIDs) == 0 {
		return approval, ErrInvalid
	}
	approval.Specs = append([]ServiceSpec(nil), approval.Specs...)
	approval.RequiredServiceIDs = append([]string(nil), approval.RequiredServiceIDs...)
	approval.RetireServiceIDs = append([]string(nil), approval.RetireServiceIDs...)
	specs, clients := map[string]bool{}, map[string]bool{}
	for _, spec := range approval.Specs {
		if !validSpec(spec) || spec.Application != in.Application || spec.Environment != in.Environment || specs[spec.ServiceID] || clients[spec.OAuthClientID] {
			return approval, ErrInvalid
		}
		specs[spec.ServiceID], clients[spec.OAuthClientID] = true, true
	}
	seen := map[string]bool{}
	for _, id := range approval.RequiredServiceIDs {
		if !identifier.MatchString(id) || !specs[id] || seen[id] {
			return approval, ErrInvalid
		}
		seen[id] = true
	}
	seen = map[string]bool{}
	for _, id := range approval.RetireServiceIDs {
		if !identifier.MatchString(id) || specs[id] || seen[id] {
			return approval, ErrInvalid
		}
		seen[id] = true
	}
	sort.Slice(approval.Specs, func(i, j int) bool { return approval.Specs[i].ServiceID < approval.Specs[j].ServiceID })
	sort.Strings(approval.RequiredServiceIDs)
	sort.Strings(approval.RetireServiceIDs)
	return approval, nil
}
func memberSpec(m Member) ServiceSpec {
	return ServiceSpec{m.Application, m.Environment, m.ServiceID, m.OAuthClientID, m.CoverageDigest, m.ImageDigest}
}
func validRetirementOwner(in LifecycleInput, m Member, requireTombstone bool) bool {
	return m.Application == in.Application && m.Environment == in.Environment && (!requireTombstone || m.RetiredAt != nil)
}

// ReconcileComponents is an internal controlled-release operation, not a user
// or machine endpoint. It atomically replaces the complete approved membership.
// An activated application never returns to migration compatibility during a
// rolling deployment. New clients cannot inherit the previous client's ACK.
func (s *Service) ReconcileComponents(ctx context.Context, in LifecycleInput, actor domain.Actor) error {
	if s.lifecycleApproval == nil || !identifier.MatchString(actor.TenantID) || !identifier.MatchString(actor.UserID) {
		return ErrForbidden
	}
	if principal, ok := appctx.PrincipalFromContext(ctx); ok && principal.HasScope("license.runtime") {
		return ErrForbidden
	}
	approved, err := s.lifecycleApproval.VerifyRuntimeLifecycle(ctx, in)
	if err != nil {
		return err
	}
	approved, err = canonicalApproval(in, approved)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(struct {
		Input    LifecycleInput
		Approval LifecycleApproval
		Actor    domain.Actor
	}{in, approved, actor})
	if err != nil {
		return err
	}
	requestDigest := hash(string(encoded))
	return s.lock(ctx, func(tx *gorm.DB, d *domain.Deployment, _ domain.State) error {
		var event LifecycleEvent
		err := tx.First(&event, "operation_id = ?", in.OperationID).Error
		if err == nil {
			if event.RequestDigest != requestDigest {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var app Application
		if err = tx.First(&app, "application = ?", in.Application).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotReady
			}
			return err
		}
		if app.Revision != in.ExpectedRevision {
			return ErrConflict
		}
		if app.State != runtime.Pending && app.State != runtime.Applying && app.State != runtime.Enforced {
			return domain.ErrCorrupt
		}
		var previous []Member
		if err = tx.Where("application = ? AND retired_at IS NULL", in.Application).Order("service_id ASC").Find(&previous).Error; err != nil {
			return err
		}
		current := map[string]Member{}
		desired := map[string]ServiceSpec{}
		retire := map[string]bool{}
		for _, member := range previous {
			current[member.ServiceID] = member
		}
		for _, spec := range approved.Specs {
			desired[spec.ServiceID] = spec
		}
		for _, id := range approved.RetireServiceIDs {
			retire[id] = true
			// Retirement permission is application-local, including idempotent
			// tombstones. A release must not name an unrelated or invented service
			// which the deployment Agent could otherwise stop outside this app.
			if member, present := current[id]; present {
				if !validRetirementOwner(in, member, false) {
					return ErrForbidden
				}
				continue
			}
			var tombstone Member
			lookup := tx.First(&tombstone, "service_id = ?", id).Error
			if errors.Is(lookup, gorm.ErrRecordNotFound) {
				return ErrForbidden
			}
			if lookup != nil {
				return lookup
			}
			if !validRetirementOwner(in, tombstone, true) {
				return ErrForbidden
			}
		}
		for _, member := range previous {
			spec, remains := desired[member.ServiceID]
			if !remains && !retire[member.ServiceID] {
				return ErrForbidden
			}
			if remains && memberSpec(member) != spec && member.OAuthClientID == spec.OAuthClientID {
				return ErrInvalid
			}
		}
		oldSpecs := make([]ServiceSpec, 0, len(previous))
		for _, member := range previous {
			oldSpecs = append(oldSpecs, memberSpec(member))
		}
		oldJSON, err := json.Marshal(oldSpecs)
		if err != nil {
			return err
		}
		newJSON, err := json.Marshal(approved.Specs)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		for _, spec := range approved.Specs {
			var count int64
			if err = tx.Model(&RetiredClient{}).Where("oauth_client_id = ?", spec.OAuthClientID).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return ErrConflict
			}
		}
		for _, member := range previous {
			replacement, remains := desired[member.ServiceID]
			if !remains || memberSpec(member) != replacement {
				if err = tx.Create(&RetiredClient{OAuthClientID: member.OAuthClientID, ServiceID: member.ServiceID, RetiredAt: now}).Error; err != nil {
					return err
				}
			}
			if _, remains := desired[member.ServiceID]; !remains {
				if err = tx.Model(&member).Updates(map[string]any{"retired_at": now, "ready_at": nil, "ack_at": nil, "ack_revision": 0, "issued_revision": 0, "issued_digest": "", "raw_jws": ""}).Error; err != nil {
					return err
				}
			}
		}
		environment := s.applicationEnvironment
		if environment == "" {
			environment = d.Environment
		}
		for _, spec := range approved.Specs {
			if spec.Environment != environment {
				return ErrInvalid
			}
			old, exists := current[spec.ServiceID]
			if exists {
				if memberSpec(old) == spec {
					continue
				}
				if err = tx.Model(&old).Updates(map[string]any{"oauth_client_id": spec.OAuthClientID, "coverage_digest": spec.CoverageDigest, "image_digest": spec.ImageDigest, "ready_at": nil}).Error; err != nil {
					return err
				}
			} else {
				// Only this explicit approved release can restore a logical service.
				// Its old OAuth identity remains permanently retired.
				var retired Member
				lookup := tx.First(&retired, "service_id = ?", spec.ServiceID).Error
				if lookup == nil {
					if retired.RetiredAt == nil || retired.Application != spec.Application || retired.Environment != spec.Environment || retired.OAuthClientID == spec.OAuthClientID {
						return ErrConflict
					}
					if err = tx.Model(&retired).Updates(map[string]any{"retired_at": nil, "oauth_client_id": spec.OAuthClientID, "coverage_digest": spec.CoverageDigest, "image_digest": spec.ImageDigest, "ready_at": nil, "ack_at": nil, "ack_revision": 0, "issued_revision": 0, "issued_digest": "", "raw_jws": ""}).Error; err != nil {
						return err
					}
					continue
				}
				if !errors.Is(lookup, gorm.ErrRecordNotFound) {
					return lookup
				}
				var count int64
				if err = tx.Model(&Member{}).Where("service_id = ? OR oauth_client_id = ?", spec.ServiceID, spec.OAuthClientID).Count(&count).Error; err != nil {
					return err
				}
				if count != 0 {
					return ErrConflict
				}
				if err = tx.Create(&Member{ServiceID: spec.ServiceID, Application: spec.Application, Environment: spec.Environment, OAuthClientID: spec.OAuthClientID, CoverageDigest: spec.CoverageDigest, ImageDigest: spec.ImageDigest, CreatedAt: now}).Error; err != nil {
					return err
				}
			}
		}
		// Enforcement is a monotonic execution policy, not the release readiness
		// indicator. Existing consumers persist ENFORCED and correctly reject an
		// APPLYING snapshot as a rollback. Keep the policy while invalidating every
		// current-release ACK below; deployment completion must still require fresh
		// READY/ACK from the entire approved membership, including unchanged images.
		// Only the original pending migration can retain its frozen qualification.
		// APPLYING/ENFORCED never regain compatibility during replacement.
		if app.State != runtime.Pending {
			app.MigrationEligible = false
		}
		app.ContentDigest = contentDigest(app, d)
		if err = s.bump(tx, d, &app); err != nil {
			return err
		}
		if err = tx.Model(&Member{}).Where("application = ? AND retired_at IS NULL", in.Application).Updates(map[string]any{"ack_revision": 0, "ack_at": nil, "issued_revision": 0, "issued_digest": "", "raw_jws": ""}).Error; err != nil {
			return err
		}
		event = LifecycleEvent{OperationID: in.OperationID, Application: in.Application, RequestDigest: requestDigest, ReleaseDigest: in.ReleaseDigest, OldSpecs: string(oldJSON), NewSpecs: string(newJSON), Revision: app.Revision, TenantID: actor.TenantID, UserID: actor.UserID, CreatedAt: now}
		if err = tx.Create(&event).Error; err != nil {
			return err
		}
		return tx.Create(&domain.Event{Kind: "RUNTIME_COMPONENTS_RECONCILED", Digest: strings.TrimPrefix(in.ReleaseDigest, "sha256:"), Revision: app.Revision, TenantID: actor.TenantID, UserID: actor.UserID, CreatedAt: now}).Error
	})
}

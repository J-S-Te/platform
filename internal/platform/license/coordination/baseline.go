package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func validInstallationScenario(scenario string) bool {
	return scenario == "fresh" || scenario == "migrate" || scenario == "platform-only" || scenario == "expand"
}

func validateBaselineReport(scenario string, report evidence.Report, environment string, now time.Time, requireFresh bool) error {
	if !validInstallationScenario(scenario) || report.Scope != "MIGRATION_INSTALLATION" || !report.BoundarySupported || !report.Complete || len(report.Problems) != 0 || !identifier.MatchString(report.Project) || report.CollectedAt.IsZero() || report.CollectedAt.After(now) || (requireFresh && !fresh(report.CollectedAt, now)) || len(report.Facts) > 64 {
		return ErrNotReady
	}
	empty := scenario == "fresh" || scenario == "platform-only"
	if empty != (len(report.Facts) == 0) {
		return ErrNotReady
	}
	containers := map[string]bool{}
	services := map[string]evidence.Fact{}
	for _, f := range report.Facts {
		// Legacy images need no license/version labels. Immutable identity and
		// the trusted complete project scan establish pre-upgrade presence.
		if !validApplication(f.Application) || !identifier.MatchString(f.Environment) || f.Environment != environment || !identifier.MatchString(f.Service) || !f.Running || len(f.Problems) != 0 || !containerPattern.MatchString(f.ContainerID) || containers[f.ContainerID] || !digestPattern.MatchString(f.ImageDigest) || f.StartedAt.IsZero() || f.StartedAt.After(report.CollectedAt) {
			return ErrNotReady
		}
		if old, ok := services[f.Service]; ok && (old.Application != f.Application || old.Environment != f.Environment || old.ImageDigest != f.ImageDigest) {
			return ErrNotReady
		}
		containers[f.ContainerID] = true
		services[f.Service] = f
	}
	if err := validateControlledInfrastructure(report, services); err != nil {
		return err
	}
	for _, f := range report.InfrastructureFacts {
		if containers[f.ContainerID] {
			return ErrNotReady
		}
		containers[f.ContainerID] = true
	}
	return nil
}

func baselineJSON(report evidence.Report) (string, error) {
	report.Facts = append([]evidence.Fact(nil), report.Facts...)
	report.InfrastructureFacts = append([]evidence.Fact(nil), report.InfrastructureFacts...)
	sort.Slice(report.Facts, func(i, j int) bool { return report.Facts[i].ContainerID < report.Facts[j].ContainerID })
	sort.Slice(report.InfrastructureFacts, func(i, j int) bool {
		return report.InfrastructureFacts[i].ContainerID < report.InfrastructureFacts[j].ContainerID
	})
	b, err := json.Marshal(report)
	if err != nil || len(b) > 256*1024 {
		return "", ErrNotReady
	}
	return string(b), nil
}

func (s *Service) baselineReport(b InstallationBaseline, d *domain.Deployment) (evidence.Report, error) {
	var report evidence.Report
	if b.ID != 1 || b.InstanceID != d.InstanceID || b.Environment != d.Environment || b.EvidenceDigest != hash(b.EvidenceJSON) || json.Unmarshal([]byte(b.EvidenceJSON), &report) != nil || report.Project != b.Project || !baselineCollectionTimeMatches(report.CollectedAt, b.CollectedAt) || validateBaselineReport(b.Scenario, report, s.environment(d), s.now().UTC(), false) != nil {
		return report, domain.ErrCorrupt
	}
	return report, nil
}

// MySQL DATETIME(3) rounds by default, while TIME_TRUNCATE_FRACTIONAL deployments
// truncate. Accept only those exact projections (or the pre-insert timestamp),
// never a broad tolerance that could conceal a changed audit timestamp.
func baselineCollectionTimeMatches(evidenceTime, storedTime time.Time) bool {
	return storedTime.Equal(evidenceTime) || storedTime.Equal(evidenceTime.Round(time.Millisecond)) || storedTime.Equal(evidenceTime.Truncate(time.Millisecond))
}

func (s *Service) environment(d *domain.Deployment) string {
	if s.applicationEnvironment != "" {
		return s.applicationEnvironment
	}
	return d.Environment
}

// PrepareInstallation is a trusted host deployment operation, called before
// replacing any runtime image. Once frozen, retries return the original facts;
// even an expand invocation cannot add newly installed applications to them.
func (s *Service) PrepareInstallation(ctx context.Context, scenario string, report evidence.Report, actor domain.Actor) (InstallationBaseline, error) {
	var out InstallationBaseline
	if !validInstallationScenario(scenario) || !identifier.MatchString(actor.TenantID) || !identifier.MatchString(actor.UserID) {
		return out, ErrInvalid
	}
	if p, ok := appctx.PrincipalFromContext(ctx); ok && p.HasScope("license.runtime") {
		return out, ErrForbidden
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var d domain.Deployment
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&d, "id = ?", 1).Error; err != nil {
			return err
		}
		err := tx.First(&out, "id = ?", 1).Error
		if err == nil {
			if _, err = s.baselineReport(out, &d); err != nil {
				return err
			}
			if report.Project != out.Project || (scenario != out.Scenario && scenario != "expand") {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err = validateBaselineReport(scenario, report, s.environment(&d), s.now().UTC(), true); err != nil {
			return err
		}
		for _, model := range []any{&Member{}, &Inventory{}, &Application{}} {
			var count int64
			if err = tx.Model(model).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return ErrConflict
			}
		}
		raw, err := baselineJSON(report)
		if err != nil {
			return err
		}
		out = InstallationBaseline{ID: 1, Scenario: scenario, Project: report.Project, InstanceID: d.InstanceID, Environment: d.Environment, EvidenceDigest: hash(raw), EvidenceJSON: raw, CollectedAt: report.CollectedAt.Round(time.Millisecond), CreatedAt: s.now().UTC(), TenantID: actor.TenantID, UserID: actor.UserID}
		if err = tx.Create(&out).Error; err != nil {
			return err
		}
		return tx.Create(&domain.Event{Kind: "INSTALLATION_BASELINE_FROZEN", Revision: d.Revision, Digest: out.EvidenceDigest, TenantID: actor.TenantID, UserID: actor.UserID, CreatedAt: out.CreatedAt}).Error
	})
	if err != nil {
		return InstallationBaseline{}, err
	}
	return out, nil
}

func (s *Service) ReadInstallationBaseline(ctx context.Context) (InstallationBaseline, error) {
	var out InstallationBaseline
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var d domain.Deployment
		if err := tx.First(&d, "id = ?", 1).Error; err != nil {
			return err
		}
		if err := tx.First(&out, "id = ?", 1).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotReady
			}
			return err
		}
		_, err := s.baselineReport(out, &d)
		return err
	})
	return out, err
}

func (s *Service) baselineEligibility(tx *gorm.DB, d *domain.Deployment, app string) (bool, error) {
	var b InstallationBaseline
	if err := tx.First(&b, "id = ?", 1).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	r, err := s.baselineReport(b, d)
	if err != nil {
		return false, err
	}
	for _, fact := range r.Facts {
		if fact.Application == app {
			return true, nil
		}
	}
	return false, nil
}

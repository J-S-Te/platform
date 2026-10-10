package coordination

import (
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

// Infrastructure evidence is audited but never enrolled for migration or ACK.
// A BI installation cannot freeze while silently dropping its upstream engine.
func validateControlledInfrastructure(report evidence.Report, business map[string]evidence.Fact) error {
	gateway, hasBI := business["data-analysis-api"]
	if len(report.InfrastructureFacts) > 64 || (hasBI && gateway.Application == "data_analysis" && len(report.InfrastructureFacts) == 0) {
		return ErrNotReady
	}
	seen := map[string]bool{}
	for _, f := range report.InfrastructureFacts {
		if !evidence.ReviewedControlledBIInfrastructure(f.Application, f.Service) || !hasBI || gateway.Application != f.Application || gateway.Environment != f.Environment || !f.Running || len(f.Problems) != 0 || f.Protocol != "" || f.Version != "" || !containerPattern.MatchString(f.ContainerID) || !digestPattern.MatchString(f.ImageDigest) || f.StartedAt.IsZero() || f.StartedAt.After(report.CollectedAt) || seen[f.ContainerID] {
			return ErrNotReady
		}
		seen[f.ContainerID] = true
	}
	return nil
}

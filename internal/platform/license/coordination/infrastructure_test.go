package coordination

import (
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

func TestControlledInfrastructureCannotBecomeMigrationMember(t *testing.T) {
	now := time.Now().UTC()
	engine := evidence.Fact{Application: "data_analysis", Environment: "prod", Service: "data-analysis-metabase", ContainerID: strings.Repeat("a", 64), ImageDigest: "sha256:" + strings.Repeat("b", 64), Running: true, StartedAt: now.Add(-time.Hour)}
	business := map[string]evidence.Fact{"data-analysis-api": {Application: "data_analysis", Environment: "prod"}}
	report := evidence.Report{CollectedAt: now, InfrastructureFacts: []evidence.Fact{engine}}
	if err := validateControlledInfrastructure(report, business); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"missing", "protocol", "version", "problem", "not-running", "foreign", "business", "duplicate", "no-gateway"} {
		t.Run(bad, func(t *testing.T) {
			r := report
			r.InfrastructureFacts = append([]evidence.Fact(nil), report.InfrastructureFacts...)
			b := business
			switch bad {
			case "missing":
				r.InfrastructureFacts = nil
			case "protocol":
				r.InfrastructureFacts[0].Protocol = "1"
			case "version":
				r.InfrastructureFacts[0].Version = "fake-v1"
			case "problem":
				r.InfrastructureFacts[0].Problems = []string{"published_infrastructure_port"}
			case "not-running":
				r.InfrastructureFacts[0].Running = false
			case "foreign":
				r.InfrastructureFacts[0].Environment = "dev"
			case "business":
				r.InfrastructureFacts[0].Service = "data-analysis-alert-worker"
			case "duplicate":
				r.InfrastructureFacts = append(r.InfrastructureFacts, engine)
			case "no-gateway":
				b = map[string]evidence.Fact{}
			}
			if validateControlledInfrastructure(r, b) == nil {
				t.Fatal("invalid infrastructure freeze accepted")
			}
		})
	}
}

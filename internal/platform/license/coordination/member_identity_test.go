package coordination

import (
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
)

func TestRuntimeMemberUsesOnlyAuthenticatedExternalClientIdentity(t *testing.T) {
	m := Member{OAuthClientID: "runtime-external", Application: "contract_management", Environment: "prod"}
	p := appctx.Principal{OAuthClientID: "database-pk", ClientID: "runtime-external", ApplicationCode: m.Application, EnvironmentCode: m.Environment}
	for _, kind := range []string{"valid", "wrong-external", "pk-only", "wrong-app", "wrong-env", "wrong-install-env", "retired"} {
		t.Run(kind, func(t *testing.T) {
			principal, member, environment := p, m, "prod"
			switch kind {
			case "wrong-external":
				principal.ClientID = "other"
			case "pk-only":
				principal.ClientID, principal.OAuthClientID = "other", m.OAuthClientID
			case "wrong-app":
				principal.ApplicationCode = "customer_portal"
			case "wrong-env":
				principal.EnvironmentCode = "test"
			case "wrong-install-env":
				environment = "test"
			case "retired":
				now := time.Now()
				member.RetiredAt = &now
			}
			if matchesRuntimeMember(principal, member, environment) != (kind == "valid") {
				t.Fatal("runtime identity boundary mismatch", kind)
			}
		})
	}
}

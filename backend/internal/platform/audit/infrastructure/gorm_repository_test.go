package infrastructure

import (
	"sync"
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	"gorm.io/gorm/schema"
)

// TestEventQueryRowMapsAuditEventColumns prevents audit_event fields from silently disappearing
// when a joined query is scanned. GORM only parses exported destination fields, so Event must stay
// exported and embedded in eventQueryRow.
func TestEventQueryRowMapsAuditEventColumns(t *testing.T) {
	parsed, err := schema.Parse(&eventQueryRow{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse audit query row schema: %v", err)
	}

	for _, column := range []string{
		"event_id",
		"occurred_at",
		"actor_name_snapshot",
		"action",
		"result",
		"risk_level",
		"application_code",
		"application_name",
		"environment_code",
	} {
		if _, exists := parsed.FieldsByDBName[column]; !exists {
			t.Errorf("audit query row does not map database column %q", column)
		}
	}
}

// TestActionCategoryPredicate locks the public operation-category contract to parameterized SQL.
// The category itself must never be interpolated into a clause, and unknown values must match no rows.
func TestActionCategoryPredicate(t *testing.T) {
	tests := []struct {
		name      string
		category  string
		wantArgs  int
		wantEmpty bool
	}{
		{name: "login", category: "LOGIN", wantArgs: 2},
		{name: "create normalized", category: " create ", wantArgs: 8},
		{name: "update", category: "UPDATE", wantArgs: 7},
		{name: "export", category: "EXPORT", wantArgs: 2},
		{name: "status change", category: "STATUS_CHANGE", wantArgs: 2},
		{name: "unknown", category: "DELETE", wantArgs: 0, wantEmpty: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clause, args := actionCategoryPredicate(test.category)
			if len(args) != test.wantArgs {
				t.Fatalf("argument count = %d, want %d; clause=%q", len(args), test.wantArgs, clause)
			}
			if test.wantEmpty {
				if clause != "1 = 0" {
					t.Fatalf("unknown category clause = %q, want %q", clause, "1 = 0")
				}
				return
			}
			if clause == "" || clause == "1 = 0" {
				t.Fatalf("known category produced non-matching clause %q", clause)
			}
		})
	}
}

// TestExportQueryPreservesActionCategory prevents asynchronous exports from silently dropping the
// operation category selected on the audit page.
func TestExportQueryPreservesActionCategory(t *testing.T) {
	page := application.PageRequest{ActionCategory: "CREATE"}
	persisted := exportQuery(page)
	if persisted.ActionCategory != page.ActionCategory {
		t.Fatalf("persisted action category = %q, want %q", persisted.ActionCategory, page.ActionCategory)
	}
	restored := pageRequest(persisted)
	if restored.ActionCategory != page.ActionCategory {
		t.Fatalf("restored action category = %q, want %q", restored.ActionCategory, page.ActionCategory)
	}
}

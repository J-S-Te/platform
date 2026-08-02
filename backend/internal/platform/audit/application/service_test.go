package application

import (
	"errors"
	"testing"
	"time"
)

func TestNormalizePageActionCategory(t *testing.T) {
	query := normalizePage(PageRequest{ActionCategory: " create "})
	if query.ActionCategory != "CREATE" {
		t.Fatalf("normalized action category = %q, want %q", query.ActionCategory, "CREATE")
	}
}

func TestValidActionCategory(t *testing.T) {
	for _, category := range []string{"", "LOGIN", "CREATE", "UPDATE", "DELETE", "EXPORT", "STATUS_CHANGE", "AUTHORIZATION_CHANGE", "SECRET_ROTATION", "PASSWORD_RESET", "CATALOG_SYNC", "AUDIT_ACCESS", "IMPORT"} {
		if !validActionCategory(category) {
			t.Errorf("expected action category %q to be valid", category)
		}
	}
	for _, category := range []string{"新增", "UNKNOWN", "create"} {
		if validActionCategory(category) {
			t.Errorf("expected action category %q to be invalid before normalization", category)
		}
	}
}

func TestValidatePageRejectsInvalidFilters(t *testing.T) {
	from := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(-time.Hour)
	for name, query := range map[string]PageRequest{
		"result":     {Result: "ERROR"},
		"risk":       {RiskLevel: "SEVERE"},
		"time range": {OccurredFrom: &from, OccurredTo: &to},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validatePage(query); !errors.Is(err, ErrValidation) {
				t.Fatalf("validatePage error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestNormalizePageNormalizesAllEnumFilters(t *testing.T) {
	query := normalizePage(PageRequest{Keyword: " user ", ApplicationCode: " platform ", EnvironmentCode: " prod ", Action: " update ", Result: " denied ", RiskLevel: " critical "})
	if query.Keyword != "user" || query.ApplicationCode != "platform" || query.EnvironmentCode != "prod" || query.Action != "update" || query.Result != "DENIED" || query.RiskLevel != "CRITICAL" {
		t.Fatalf("normalizePage = %#v", query)
	}
}

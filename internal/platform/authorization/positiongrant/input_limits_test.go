package positiongrant

import (
	"strings"
	"testing"
)

func TestTemplateInputRejectsOversizedDescriptionAndScope(t *testing.T) {
	input := TemplateInput{Name: "模板", Description: strings.Repeat("说", 1001)}
	if err := normalizeTemplateInput(&input); err == nil {
		t.Fatal("overlong description was accepted")
	}
	input = TemplateInput{Name: "模板", Roles: []TemplateRoleInput{{
		ApplicationID: "app", RoleID: "role", ScopeType: "ENVIRONMENT", ScopeID: strings.Repeat("A", 129),
	}}}
	if err := normalizeTemplateInput(&input); err == nil {
		t.Fatal("overlong scope id was accepted")
	}
}

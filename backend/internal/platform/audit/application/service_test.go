package application

import "testing"

func TestNormalizePageActionCategory(t *testing.T) {
	query := normalizePage(PageRequest{ActionCategory: " create "})
	if query.ActionCategory != "CREATE" {
		t.Fatalf("normalized action category = %q, want %q", query.ActionCategory, "CREATE")
	}
}

func TestValidActionCategory(t *testing.T) {
	for _, category := range []string{"", "LOGIN", "CREATE", "UPDATE", "EXPORT", "STATUS_CHANGE"} {
		if !validActionCategory(category) {
			t.Errorf("expected action category %q to be valid", category)
		}
	}
	for _, category := range []string{"新增", "DELETE", "create"} {
		if validActionCategory(category) {
			t.Errorf("expected action category %q to be invalid before normalization", category)
		}
	}
}

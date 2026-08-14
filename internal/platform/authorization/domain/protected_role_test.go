package domain

import "testing"

func TestIsProtectedRoleCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code string
		want bool
	}{
		{code: "platform-super-admin", want: true},
		{code: " PLATFORM-SUPER-ADMIN ", want: true},
		{code: "platform-emergency-admin", want: true},
		{code: "platform-break-glass-ops", want: true},
		{code: "platform-security-admin", want: false},
		{code: "platform-admin", want: false},
		{code: "portal_customer", want: false},
		{code: "", want: false},
	}
	for _, test := range tests {
		if got := IsProtectedRoleCode(test.code); got != test.want {
			t.Errorf("IsProtectedRoleCode(%q) = %v, want %v", test.code, got, test.want)
		}
	}
}

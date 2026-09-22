package publictransport

import "testing"

func TestCanonicalOriginRejectsPathAndSchemeMismatch(t *testing.T) {
	if got, err := canonicalOrigin("https://platform.example.com/", "https"); err != nil || got != "https://platform.example.com" {
		t.Fatalf("canonical origin = %q, %v", got, err)
	}
	for _, value := range []string{"http://platform.example.com", "https://platform.example.com/path", "https://user@platform.example.com", "platform.example.com"} {
		if _, err := canonicalOrigin(value, "https"); err == nil {
			t.Fatalf("origin %q must be rejected", value)
		}
	}
}

func TestReplaceOriginDoesNotRewriteAttackerPrefix(t *testing.T) {
	source := "http://platform.example.com"
	target := "https://platform.example.com"
	if got, ok := replaceOrigin(source+"/project/auth/callback", source, target); !ok || got != target+"/project/auth/callback" {
		t.Fatalf("managed callback = %q, %v", got, ok)
	}
	for _, value := range []string{"http://platform.example.com.attacker.test/callback", "https://other.example.com/callback"} {
		if got, ok := replaceOrigin(value, source, target); ok || got != value {
			t.Fatalf("unmanaged callback %q was rewritten to %q", value, got)
		}
	}
}

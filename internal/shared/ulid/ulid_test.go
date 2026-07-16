package ulid

import (
	"strings"
	"testing"
	"time"
)

func TestNewReturnsCrockfordBase32ULID(t *testing.T) {
	identifier, err := New(time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("new ULID: %v", err)
	}
	if len(identifier) != 26 {
		t.Fatalf("ULID length = %d, want 26", len(identifier))
	}
	for _, character := range identifier {
		if !strings.ContainsRune(alphabet, character) {
			t.Fatalf("ULID contains non-Crockford character %q", character)
		}
	}
}

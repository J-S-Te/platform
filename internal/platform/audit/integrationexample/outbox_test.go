package integrationexample

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventSerializesOptionalUserLoginIP(t *testing.T) {
	payload, err := json.Marshal(Event{EventID: "event-1", UserLoginIP: "203.0.113.10"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(payload), `"user_login_ip":"203.0.113.10"`) {
		t.Fatalf("payload = %s, want user_login_ip", payload)
	}
}

func TestEventOmitsUserLoginIPWhenUnset(t *testing.T) {
	payload, err := json.Marshal(Event{EventID: "event-1"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(payload), "user_login_ip") {
		t.Fatalf("payload = %s, should omit user_login_ip", payload)
	}
}

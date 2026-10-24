package bootstrap

import (
	"testing"

	identityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

// outbox 已生成稳定事件 ID 时，异步投递（包括重试）必须原样复用，审计接收端才能幂等去重。
func TestPersonnelChangeAuditEventIDReusesPersistedID(t *testing.T) {
	event := identityapplication.PersonnelChangeAuditEvent{EventID: "01JAUDIT000000000000000001"}
	eventID, err := personnelChangeAuditEventID(ulid.Generator{}, event)
	if err != nil {
		t.Fatal(err)
	}
	if eventID != event.EventID {
		t.Fatalf("event id = %s, want the stable outbox id %s", eventID, event.EventID)
	}
}

// 旧调用方没有事件 ID 时才生成新 ID，避免空 ID 让审计接收端无法落库。
func TestPersonnelChangeAuditEventIDGeneratesWhenMissing(t *testing.T) {
	eventID, err := personnelChangeAuditEventID(ulid.Generator{}, identityapplication.PersonnelChangeAuditEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if len(eventID) != 26 {
		t.Fatalf("generated event id = %q, want a 26-character ULID", eventID)
	}
}

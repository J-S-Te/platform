package identityhttp

import (
	"testing"
	"time"
)

func TestParseDateEndIncludesTheCompleteCalendarDay(t *testing.T) {
	value := "2026-08-31"
	parsed, err := parseDateEnd(&value)
	if err != nil {
		t.Fatalf("parse date end: %v", err)
	}
	want := time.Date(2026, time.August, 31, 23, 59, 59, 999999999, time.UTC)
	if !parsed.Equal(want) {
		t.Fatalf("parsed = %s, want %s", parsed.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

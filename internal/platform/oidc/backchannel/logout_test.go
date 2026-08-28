package backchannel

import (
	"context"
	"errors"
	"testing"
	"time"
)

func validClaims(now time.Time) Claims {
	return Claims{Issuer: "https://issuer", Audience: []string{"client"}, Subject: "user", JTI: "jti", IssuedAt: now.Add(-time.Second), Expires: now.Add(30 * time.Second), Events: map[string]any{EventURI: map[string]any{}}}
}

func TestValidateClaimsRequiresLogoutEventAndShortTTL(t *testing.T) {
	now := time.Now().UTC()
	if err := ValidateClaims(validClaims(now), "https://issuer", "client", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	bad := validClaims(now)
	delete(bad.Events, EventURI)
	if !errors.Is(ValidateClaims(bad, "https://issuer", "client", now, time.Minute), ErrInvalidToken) {
		t.Fatal("missing event accepted")
	}
	bad = validClaims(now)
	bad.Expires = now.Add(10 * time.Minute)
	if !errors.Is(ValidateClaims(bad, "https://issuer", "client", now, time.Minute), ErrInvalidToken) {
		t.Fatal("long token accepted")
	}
	bad = validClaims(now)
	bad.Subject = ""
	bad.Session = ""
	if !errors.Is(ValidateClaims(bad, "https://issuer", "client", now, time.Minute), ErrInvalidToken) {
		t.Fatal("missing subject and sid accepted")
	}
}

func TestValidateLogoutURI(t *testing.T) {
	if err := ValidateLogoutURI("https://rp.example/logout", false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLogoutURI("http://rp.example/logout", false); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("insecure URI accepted")
	}
}

type queue struct{ sent, failed bool }

func (q *queue) Claim(context.Context, time.Time, int) ([]Message, error) {
	return []Message{{ID: "1"}}, nil
}
func (q *queue) Complete(context.Context, string, time.Time) error     { q.sent = true; return nil }
func (q *queue) Fail(context.Context, string, string, time.Time) error { q.failed = true; return nil }

type sender struct{ err error }

func (s sender) Send(context.Context, Message) error { return s.err }

func TestDispatcherKeepsFailedMessageForRetry(t *testing.T) {
	q := &queue{}
	d := Dispatcher{Queue: q, Sender: sender{err: errors.New("down")}, BatchSize: 1}
	if _, err := d.RunOnce(context.Background(), time.Now()); err == nil || !q.failed || q.sent {
		t.Fatalf("unexpected result err=%v failed=%v sent=%v", err, q.failed, q.sent)
	}
}

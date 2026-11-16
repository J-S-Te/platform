package firstrun

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type fakeEventStore struct {
	events []Event
	err    error
}

func (store *fakeEventStore) Insert(_ context.Context, event Event) error {
	store.events = append(store.events, event)
	return store.err
}

func authenticatedRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/first-run/events", strings.NewReader(body))
	principal := authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"}}
	return request.WithContext(authctx.WithPrincipal(context.Background(), principal))
}

func TestRecordWritesAuthenticatedScopeOnly(t *testing.T) {
	store := &fakeEventStore{}
	handler, err := NewHandler(store, true, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	request := authenticatedRequest(`{"event":"tour_finished","guide_key":"portal","step":4,"outcome":"done","client_version":"2","occurred_at":"2026-11-15T04:00:00Z"}`)
	recorder := httptest.NewRecorder()
	handler.Record(recorder, request)
	if recorder.Code != http.StatusAccepted || len(store.events) != 1 {
		t.Fatalf("status=%d events=%d", recorder.Code, len(store.events))
	}
	event := store.events[0]
	if event.TenantID != "tenant-1" || event.UserID != "user-1" || event.Event != "tour_finished" || event.GuideKey != "portal" || event.Step != 4 || event.Outcome != "done" {
		t.Fatalf("event=%+v", event)
	}
}

func TestRecordRejectsUnknownEventAndMissingAuth(t *testing.T) {
	store := &fakeEventStore{}
	handler, _ := NewHandler(store, true, slog.Default())
	for _, body := range []string{
		`{"event":"unknown"}`,
		`{"event":"tour_started","guide_key":"portal","extra":"x"}`,
		`{"event":"tour_started","guide_key":"portal","step":101}`,
	} {
		recorder := httptest.NewRecorder()
		handler.Record(recorder, authenticatedRequest(body))
		if recorder.Code != http.StatusBadRequest || len(store.events) != 0 {
			t.Fatalf("body=%s status=%d events=%d", body, recorder.Code, len(store.events))
		}
	}
	recorder := httptest.NewRecorder()
	handler.Record(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/first-run/events", strings.NewReader(`{"event":"tour_started"}`)))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", recorder.Code)
	}
}

func TestRecordDisabledAndStoreFailure(t *testing.T) {
	store := &fakeEventStore{}
	disabled, _ := NewHandler(store, false, slog.Default())
	recorder := httptest.NewRecorder()
	disabled.Record(recorder, authenticatedRequest(`{"event":"tour_started","guide_key":"portal"}`))
	if recorder.Code != http.StatusNoContent || len(store.events) != 0 {
		t.Fatalf("disabled status=%d events=%d", recorder.Code, len(store.events))
	}

	store.err = errors.New("db down")
	enabled, _ := NewHandler(store, true, slog.Default())
	recorder = httptest.NewRecorder()
	enabled.Record(recorder, authenticatedRequest(`{"event":"tour_started","guide_key":"portal"}`))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("failure status=%d", recorder.Code)
	}
}

func TestConfigReturnsFeatureSwitch(t *testing.T) {
	handler, _ := NewHandler(&fakeEventStore{}, true, slog.Default())
	recorder := httptest.NewRecorder()
	handler.Config(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/first-run/config", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"enabled":true`) || !strings.Contains(recorder.Body.String(), `"version":2`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	_ = time.Now()
}

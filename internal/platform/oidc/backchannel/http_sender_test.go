package backchannel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type fakeSigner struct {
	ttl time.Duration
}

func (signer *fakeSigner) IssueLogoutToken(_ string, _ string, _ string, _ string, _ string, _ time.Time, ttl time.Duration) (string, error) {
	signer.ttl = ttl
	return "logout-token", nil
}

func TestHTTPFormSenderPostsLogoutToken(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("unexpected request")
		}
		body, _ := io.ReadAll(r.Body)
		values, _ := url.ParseQuery(string(body))
		if values.Get("logout_token") != "logout-token" {
			t.Errorf("missing logout token")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	signer := &fakeSigner{}
	if err := (HTTPFormSender{Signer: signer, Issuer: "https://issuer", TTL: 45 * time.Second, AllowLocalHTTP: true}).Send(context.Background(), Message{URI: server.URL, Audience: "client", JTI: "jti"}); err != nil {
		t.Fatal(err)
	}
	if signer.ttl != 45*time.Second {
		t.Fatalf("signer TTL = %s, want 45s", signer.ttl)
	}
	if !called {
		t.Fatal("sender was not called")
	}
}

func TestHTTPFormSenderRejectsHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	if err := (HTTPFormSender{Signer: &fakeSigner{}, AllowLocalHTTP: true}).Send(context.Background(), Message{URI: server.URL, Audience: "client", JTI: "jti"}); err == nil {
		t.Fatal("HTTP failure accepted")
	}
}

func TestHTTPFormSenderDoesNotFollowRedirects(t *testing.T) {
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalled = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	defer redirect.Close()

	client := redirect.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	err := (HTTPFormSender{Client: client, Signer: &fakeSigner{}, AllowLocalHTTP: true}).Send(context.Background(), Message{URI: redirect.URL, Audience: "client", JTI: "jti"})
	if err == nil || targetCalled {
		t.Fatalf("redirect result err=%v targetCalled=%t, want blocked redirect", err, targetCalled)
	}
}

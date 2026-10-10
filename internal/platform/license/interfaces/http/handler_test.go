package licensehttp

import (
	"context"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	core "github.com/J-S-Te/license-core"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Handler tests use the real application service, with an isolated storage
// contract fixture. Real SQL transactions are exercised by the MySQL test.
type handlerRepository struct {
	d      *domain.Deployment
	events []domain.Event
	calls  int
}

func (r *handlerRepository) Lock(_ context.Context, fn func(application.Transaction) error) error {
	r.calls++
	return fn(r)
}
func (r *handlerRepository) Deployment() *domain.Deployment            { return r.d }
func (r *handlerRepository) SaveDeployment(d *domain.Deployment) error { r.d = d; return nil }
func (r *handlerRepository) Artifact(string) (domain.Artifact, error) {
	return domain.Artifact{}, errors.New("no artifact")
}
func (r *handlerRepository) AddArtifact(domain.Artifact) error {
	return errors.New("unexpected artifact write")
}
func (r *handlerRepository) AddEvent(e domain.Event) error {
	r.events = append(r.events, e)
	return nil
}
func (r *handlerRepository) ConsumeRecovery(domain.ClockRecovery) error {
	return errors.New("unexpected recovery write")
}
func (r *handlerRepository) Events(_ context.Context, page, size int) (domain.EventPage, error) {
	r.calls++
	return domain.EventPage{Items: r.events, Total: int64(len(r.events)), Page: page, PageSize: size}, nil
}
func handlerFixture(t *testing.T) (*Handler, *handlerRepository) {
	t.Helper()
	repo := &handlerRepository{}
	s, e := application.NewService(repo, nil, time.Now)
	if e != nil {
		t.Fatal(e)
	}
	h, e := NewHandler(s, "production")
	if e != nil {
		t.Fatal(e)
	}
	return h, repo
}
func requestWith(body string, permission string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/license", strings.NewReader(body))
	if permission != "" {
		r = r.WithContext(authctx.WithPrincipal(r.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant"}, User: authctx.ReferenceName{ID: "admin"}, PermissionCodes: []string{permission}}))
	}
	return r
}
func invoke(fn http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	fn(w, r)
	return w
}
func TestLicenseHTTPAuthenticationAndPermissions(t *testing.T) {
	h, repo := handlerFixture(t)
	for name, fn := range map[string]http.HandlerFunc{"status": h.Status, "request": h.Request, "events": h.Events, "initialize": h.Initialize, "preview": h.Preview, "commit": h.Commit, "recovery": h.RestoreClock} {
		t.Run(name, func(t *testing.T) {
			r := requestWith("{}", "")
			r.Header.Set("X-User-ID", "admin")
			r.Header.Set("X-Tenant-ID", "tenant")
			r.Header.Set("X-Permissions", "platform:license:manage")
			if w := invoke(fn, r); w.Code != 401 {
				t.Fatalf("unauthenticated %d %s", w.Code, w.Body.String())
			}
			r = requestWith("{}", "subsystem:admin")
			if w := invoke(fn, r); w.Code != 403 {
				t.Fatalf("subsystem privilege %d %s", w.Code, w.Body.String())
			}
			r = requestWith("{}", "platform:license:manage")
			p, _ := authctx.PrincipalFromContext(r.Context())
			p.User.ID = ""
			r = r.WithContext(authctx.WithPrincipal(r.Context(), p))
			if w := invoke(fn, r); w.Code != 401 {
				t.Fatalf("incomplete principal %d", w.Code)
			}
		})
	}
	if repo.calls != 0 {
		t.Fatal("unauthorized request reached storage")
	}
	for _, fn := range []http.HandlerFunc{h.Initialize, h.Preview, h.Commit, h.RestoreClock} {
		if w := invoke(fn, requestWith("{}", "platform:license:read")); w.Code != 403 {
			t.Fatalf("read-only permission wrote: %d", w.Code)
		}
	}
}
func TestLicenseHTTPStrictBodiesAndEnvironment(t *testing.T) {
	h, repo := handlerFixture(t)
	for name, body := range map[string]string{"unknown": "{\"customer_id\":\"customer\",\"environment\":\"production\",\"unknown\":true}", "trailing": "{\"customer_id\":\"customer\",\"environment\":\"production\"} {}", "malformed": "{", "environment": "{\"customer_id\":\"customer\",\"environment\":\"staging\"}", "oversized": "{\"customer_id\":\"" + strings.Repeat("a", core.MaxTokenBytes) + "\",\"environment\":\"production\"}"} {
		t.Run(name, func(t *testing.T) {
			w := invoke(h.Initialize, requestWith(body, "platform:license:manage"))
			if w.Code < 400 || w.Code >= 500 {
				t.Fatalf("accepted invalid body %d %s", w.Code, w.Body.String())
			}
		})
	}
	if repo.calls != 0 {
		t.Fatal("invalid body reached storage")
	}
}
func TestLicenseHTTPEmptyTrustReadAndPagination(t *testing.T) {
	h, repo := handlerFixture(t)
	if w := invoke(h.Request, requestWith("", "platform:license:read")); w.Code != 409 || !strings.Contains(w.Body.String(), "LICENSE_NOT_INITIALIZED") {
		t.Fatalf("uninitialized request %d %s", w.Code, w.Body.String())
	}
	if w := invoke(h.Status, requestWith("", "platform:license:read")); w.Code != 200 || !strings.Contains(w.Body.String(), "NO_LICENSE") || !strings.Contains(w.Body.String(), "NOT_CONNECTED") {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	body := "{\"customer_id\":\"customer\",\"environment\":\"production\"}"
	if w := invoke(h.Initialize, requestWith(body, "platform:license:manage")); w.Code != 200 {
		t.Fatalf("initialize %d %s", w.Code, w.Body.String())
	}
	if repo.d == nil || repo.d.CustomerID != "customer" || repo.d.CustomerID == "tenant" {
		t.Fatal("tenant used as customer binding")
	}
	if w := invoke(h.Preview, requestWith("{\"raw_jws\":\"anything\"}", "platform:license:manage")); w.Code != 503 || !strings.Contains(w.Body.String(), "LICENSE_TRUST_NOT_CONFIGURED") {
		t.Fatalf("trust not configured %d %s", w.Code, w.Body.String())
	}
	if w := invoke(h.Request, requestWith("", "platform:license:read")); w.Code != 200 || !strings.Contains(w.Body.String(), repo.d.InstanceID) {
		t.Fatalf("request %d %s", w.Code, w.Body.String())
	}
	for _, query := range []string{"?page=0", "?page=-1", "?page=garbage", "?page_size=101", "?page_size=0"} {
		r := requestWith("", "platform:license:read")
		r.URL.RawQuery = strings.TrimPrefix(query, "?")
		if w := invoke(h.Events, r); w.Code != 400 {
			t.Fatalf("pagination %q %d %s", query, w.Code, w.Body.String())
		}
	}
	if w := invoke(h.Events, requestWith("", "platform:license:read")); w.Code != 200 {
		t.Fatalf("events %d %s", w.Code, w.Body.String())
	}
}

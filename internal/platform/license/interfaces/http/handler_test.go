package licensehttp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	core "github.com/J-S-Te/license-core"
	runtime "github.com/J-S-Te/license-core/runtime"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
)

// 处理器测试使用真实应用服务和隔离存储
// 契约夹具；真实 SQL 事务由 MySQL 测试覆盖。
type handlerRepository struct {
	d         *domain.Deployment
	artifacts map[string]domain.Artifact
	events    []domain.Event
	calls     int
}

func (r *handlerRepository) Lock(_ context.Context, fn func(application.Transaction) error) error {
	r.calls++
	return fn(r)
}
func (r *handlerRepository) Deployment() *domain.Deployment            { return r.d }
func (r *handlerRepository) SaveDeployment(d *domain.Deployment) error { r.d = d; return nil }
func (r *handlerRepository) Artifact(digest string) (domain.Artifact, error) {
	if a, ok := r.artifacts[digest]; ok {
		return a, nil
	}
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
	for name, fn := range map[string]http.HandlerFunc{"status": h.Status, "artifact": h.Artifact, "request": h.Request, "events": h.Events, "initialize": h.Initialize, "preview": h.Preview, "commit": h.Commit, "recovery": h.RestoreClock} {
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
	if w := invoke(h.Status, requestWith("", "platform:license:read")); w.Code != 200 || !strings.Contains(w.Body.String(), "NO_LICENSE") || !strings.Contains(w.Body.String(), `"runtime_enforcement":"UNAVAILABLE"`) || !strings.Contains(w.Body.String(), `"runtime_enforcement_source":"per_application_coordination"`) || !strings.Contains(w.Body.String(), `"enforcement":null`) {
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

// 确定性失败（版本不递增、实例绑定不匹配）必须与瞬时的状态冲突区分开，各自给出错误码与文案。
func TestLicenseHTTPDistinguishesDeterministicFailuresFromConflicts(t *testing.T) {
	cases := []struct {
		sentinel error
		code     string
		message  string
	}{
		{domain.ErrVersionNotIncreasing, "LICENSE_VERSION_NOT_INCREASING", "更高版本"},
		{domain.ErrBindingMismatch, "LICENSE_BINDING_MISMATCH", "已绑定"},
		{domain.ErrConflict, "LICENSE_VERSION_CONFLICT", "重新预览"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		writeError(w, requestWith("", "platform:license:manage"), c.sentinel)
		if w.Code != 409 || !strings.Contains(w.Body.String(), c.code) || !strings.Contains(w.Body.String(), c.message) {
			t.Fatalf("%v -> %d %s", c.sentinel, w.Code, w.Body.String())
		}
	}
}

// 前端 userError.js 的 safeUserMessage 会丢弃含技术细节的文案，尤其是大写带下划线的 token。
// 文案一旦被丢弃，409 会降级成“请刷新页面”，等于本次修复失效，因此这里做回归保护。
func TestLicenseHTTPMessagesSurviveTheFrontendFilter(t *testing.T) {
	technical := regexp.MustCompile("[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+")
	for _, sentinel := range []error{domain.ErrVersionNotIncreasing, domain.ErrBindingMismatch, domain.ErrConflict} {
		w := httptest.NewRecorder()
		writeError(w, requestWith("", "platform:license:manage"), sentinel)
		var envelope struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &envelope); e != nil {
			t.Fatal(e)
		}
		if !hasHan(envelope.Message) || len([]rune(envelope.Message)) > 400 || technical.MatchString(envelope.Message) {
			t.Fatalf("frontend would discard the message for %s: %q", envelope.Code, envelope.Message)
		}
	}
}

// 未初始化时导出必须失败关闭，不能返回空产物让调用方误判。
func TestLicenseHTTPArtifactRequiresInitializedDeployment(t *testing.T) {
	h, _ := handlerFixture(t)
	if w := invoke(h.Artifact, requestWith("", "platform:license:read")); w.Code != 409 || !strings.Contains(w.Body.String(), "LICENSE_NOT_INITIALIZED") {
		t.Fatalf("uninitialized artifact export %d %s", w.Code, w.Body.String())
	}
}

func hasHan(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// fakeRuntimeStatusReader 按应用伪造协调状态；未列出的应用返回 ErrNotReady，
// 与真实协调器“未登记”的语义一致。
type fakeRuntimeStatusReader struct {
	statuses map[string]coordination.Status
	errs     map[string]error
	called   []string
}

func (f *fakeRuntimeStatusReader) Status(_ context.Context, application string) (coordination.Status, error) {
	f.called = append(f.called, application)
	if e, ok := f.errs[application]; ok {
		return coordination.Status{}, e
	}
	if s, ok := f.statuses[application]; ok {
		return s, nil
	}
	return coordination.Status{}, coordination.ErrNotReady
}

// withRuntimeStatus 给夹具注入协调器读取桩，便于断言运行执行聚合。
func withRuntimeStatus(h *Handler, reader runtimeStatusReader) *Handler {
	h.ConfigureRuntimeStatus(reader)
	return h
}

type statusEnforcementPayload struct {
	Available          bool   `json:"available"`
	Registration       string `json:"registration"`
	Status             string `json:"status"`
	Revision           uint64 `json:"revision"`
	DeploymentRevision uint64 `json:"deployment_revision"`
	MemberCount        int    `json:"member_count"`
	ReadyCount         int    `json:"ready_count"`
	AckCount           int    `json:"ack_count"`
	LatestReadyAt      string `json:"latest_ready_at"`
	LatestAckAt        string `json:"latest_ack_at"`
	SnapshotCurrent    bool   `json:"snapshot_current"`
}

type statusSystemPayload struct {
	Code        string                    `json:"code"`
	Status      string                    `json:"status"`
	ReasonCode  string                    `json:"reason_code"`
	NotBefore   int64                     `json:"not_before"`
	ExpiresAt   int64                     `json:"expires_at"`
	Kind        string                    `json:"kind"`
	Enforcement *statusEnforcementPayload `json:"enforcement"`
}

type statusPayload struct {
	Initialized              bool                  `json:"initialized"`
	ServerTime               string                `json:"server_time"`
	ConfiguredEnvironment    string                `json:"configured_environment"`
	RuntimeEnforcement       string                `json:"runtime_enforcement"`
	RuntimeEnforcementSource string                `json:"runtime_enforcement_source"`
	Systems                  []statusSystemPayload `json:"systems"`
}

// readLicenseStatus 以读取权限调用状态接口并解析信封；enforcement 为 null 时解析为 nil。
func readLicenseStatus(t *testing.T, h *Handler) statusPayload {
	t.Helper()
	w := invoke(h.Status, requestWith("", "platform:license:read"))
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var envelope struct {
		Data statusPayload `json:"data"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &envelope); e != nil {
		t.Fatal(e)
	}
	if len(envelope.Data.Systems) != 6 {
		t.Fatalf("expected six systems, got %d", len(envelope.Data.Systems))
	}
	return envelope.Data
}

func systemPayload(t *testing.T, data statusPayload, code string) statusSystemPayload {
	t.Helper()
	for _, s := range data.Systems {
		if s.Code == code {
			return s
		}
	}
	t.Fatalf("system %s missing", code)
	return statusSystemPayload{}
}

// signedStatusFixture 用真实密钥签名许可证并预置存储状态，
// 保证 reason_code 断言基于真实的授权事实而不是伪造状态。
func signedStatusFixture(t *testing.T, now time.Time, clockBlocked bool, apps ...core.Application) (*Handler, *handlerRepository) {
	t.Helper()
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	trusted := map[string]ed25519.PublicKey{"vendor": pub}
	raw, e := core.Sign(core.License{ProtocolVersion: core.ProtocolVersion, Issuer: core.Issuer, ID: "license", Version: 1, CustomerID: "customer", ProductID: core.Product, Environment: "production", InstanceID: "instance-1", IssuedAt: now.Unix() - 7200, NotBefore: now.Unix() - 3600, Applications: apps}, "vendor", priv)
	if e != nil {
		t.Fatal(e)
	}
	digest, e := core.Digest(raw, trusted)
	if e != nil {
		t.Fatal(e)
	}
	repo := &handlerRepository{
		d:         &domain.Deployment{ID: 1, InstanceID: "instance-1", CustomerID: "customer", Environment: "production", Revision: 1, CurrentDigest: digest, HighestVersion: 1, HighestObservedAt: now.Unix() - 60, ClockBlocked: clockBlocked},
		artifacts: map[string]domain.Artifact{digest: {Digest: digest, RawJWS: raw, Version: 1, CreatedAt: now}},
	}
	s, e := application.NewService(repo, trusted, func() time.Time { return now })
	if e != nil {
		t.Fatal(e)
	}
	h, e := NewHandler(s, "production")
	if e != nil {
		t.Fatal(e)
	}
	return h, repo
}

// reason_code 必须与 status 同步：VALID 为空，校验失败报 VALIDATION_ERROR 但原因码是 STATE_INVALID。
func TestLicenseHTTPStatusReasonCodes(t *testing.T) {
	// 处理器按真实时钟判定生效/过期，因此许可证时间相对当前时刻构造。
	now := time.Now().UTC().Truncate(time.Second)
	validApps := []core.Application{{Code: "contract_management", NotBefore: now.Unix() - 3600, ExpiresAt: now.Unix() + 3600, Kind: "FULL"}}
	cases := []struct {
		name   string
		build  func(t *testing.T) (*Handler, *handlerRepository)
		system string
		status string
		reason string
	}{
		{name: "VALID", build: func(t *testing.T) (*Handler, *handlerRepository) {
			return signedStatusFixture(t, now, false, validApps...)
		}, system: "contract_management", status: "VALID", reason: ""},
		{name: "NOT_PURCHASED", build: func(t *testing.T) (*Handler, *handlerRepository) {
			return signedStatusFixture(t, now, false, validApps...)
		}, system: "customer_portal", status: "NOT_PURCHASED", reason: "NOT_PURCHASED"},
		{name: "EXPIRED", build: func(t *testing.T) (*Handler, *handlerRepository) {
			return signedStatusFixture(t, now, false, core.Application{Code: "contract_management", NotBefore: now.Unix() - 7200, ExpiresAt: now.Unix() - 60, Kind: "FULL"})
		}, system: "contract_management", status: "EXPIRED", reason: "EXPIRED"},
		{name: "NOT_EFFECTIVE", build: func(t *testing.T) (*Handler, *handlerRepository) {
			return signedStatusFixture(t, now, false, core.Application{Code: "contract_management", NotBefore: now.Unix() + 1800, ExpiresAt: now.Unix() + 7200, Kind: "FULL"})
		}, system: "contract_management", status: "NOT_EFFECTIVE", reason: "NOT_EFFECTIVE"},
		{name: "CLOCK_ABNORMAL", build: func(t *testing.T) (*Handler, *handlerRepository) {
			return signedStatusFixture(t, now, true, validApps...)
		}, system: "contract_management", status: "CLOCK_ABNORMAL", reason: "CLOCK_ABNORMAL"},
		{name: "VALIDATION_ERROR", build: func(t *testing.T) (*Handler, *handlerRepository) {
			h, repo := signedStatusFixture(t, now, false, validApps...)
			// 部署实例主键损坏即持久状态校验失败，读取必须失败关闭。
			repo.d.ID = 2
			return h, repo
		}, system: "contract_management", status: "VALIDATION_ERROR", reason: "STATE_INVALID"},
		{name: "NO_LICENSE", build: func(t *testing.T) (*Handler, *handlerRepository) { return handlerFixture(t) }, system: "contract_management", status: "NO_LICENSE", reason: "NO_LICENSE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _ := c.build(t)
			item := systemPayload(t, readLicenseStatus(t, h), c.system)
			if item.Status != c.status || item.ReasonCode != c.reason {
				t.Fatalf("%s: status %q reason %q, want %q/%q", c.system, item.Status, item.ReasonCode, c.status, c.reason)
			}
		})
	}
}

// 运行执行聚合必须一次查询带出，避免前端逐应用回查协调器。
func TestLicenseHTTPStatusRuntimeEnforcement(t *testing.T) {
	t.Run("ENFORCED", func(t *testing.T) {
		h, _ := handlerFixture(t)
		now := time.Now().UTC().Truncate(time.Second)
		readyA, readyB := now.Add(-30*time.Minute), now.Add(-10*time.Minute)
		ackA, ackOld := now.Add(-20*time.Minute), now.Add(-25*time.Minute)
		retiredAt, retiredReady := now.Add(-time.Hour), now.Add(-2*time.Hour)
		reader := &fakeRuntimeStatusReader{statuses: map[string]coordination.Status{"contract_management": {
			Application:        coordination.Application{Application: "contract_management", State: runtime.Enforced, Revision: 7},
			DeploymentRevision: 9,
			SnapshotCurrent:    true,
			Members: []coordination.Member{
				{ServiceID: "svc-a", ReadyAt: &readyA, AckAt: &ackA, AckRevision: 7},
				{ServiceID: "svc-b", ReadyAt: &readyB, AckAt: &ackOld, AckRevision: 6},
				{ServiceID: "svc-retired", ReadyAt: &retiredReady, AckAt: &retiredReady, AckRevision: 7, RetiredAt: &retiredAt},
			},
		}}}
		data := readLicenseStatus(t, withRuntimeStatus(h, reader))
		if data.RuntimeEnforcement != "AVAILABLE" || data.RuntimeEnforcementSource != "per_application_coordination" {
			t.Fatalf("runtime enforcement %q/%q", data.RuntimeEnforcement, data.RuntimeEnforcementSource)
		}
		enforcement := systemPayload(t, data, "contract_management").Enforcement
		if enforcement == nil || !enforcement.Available || enforcement.Registration != "REGISTERED" || enforcement.Status != "ENFORCED" {
			t.Fatalf("enforcement %+v", enforcement)
		}
		if enforcement.Revision != 7 || enforcement.DeploymentRevision != 9 || !enforcement.SnapshotCurrent {
			t.Fatalf("revision projection %+v", enforcement)
		}
		if enforcement.MemberCount != 2 || enforcement.ReadyCount != 2 || enforcement.AckCount != 1 {
			t.Fatalf("counts %+v", enforcement)
		}
		if enforcement.LatestReadyAt != readyB.Format(time.RFC3339) || enforcement.LatestAckAt != ackA.Format(time.RFC3339) {
			t.Fatalf("timestamps %q %q", enforcement.LatestReadyAt, enforcement.LatestAckAt)
		}
		// 未登记应用仍独立返回 UNREGISTERED，不影响已登记应用的聚合。
		other := systemPayload(t, data, "settlement").Enforcement
		if other == nil || !other.Available || other.Registration != "UNREGISTERED" || other.Status != "UNREGISTERED" || other.MemberCount != 0 || other.LatestReadyAt != "" || other.LatestAckAt != "" {
			t.Fatalf("unregistered enforcement %+v", other)
		}
		if len(reader.called) != 6 {
			t.Fatalf("expected one read per application, got %v", reader.called)
		}
	})
	t.Run("PENDING_ENFORCEMENT", func(t *testing.T) {
		h, _ := handlerFixture(t)
		reader := &fakeRuntimeStatusReader{statuses: map[string]coordination.Status{"project_management": {
			Application: coordination.Application{Application: "project_management", State: runtime.Pending, Revision: 3},
		}}}
		enforcement := systemPayload(t, readLicenseStatus(t, withRuntimeStatus(h, reader)), "project_management").Enforcement
		if enforcement == nil || enforcement.Status != "PENDING_ENFORCEMENT" || enforcement.Registration != "REGISTERED" || enforcement.Revision != 3 || enforcement.DeploymentRevision != 0 || enforcement.SnapshotCurrent {
			t.Fatalf("pending enforcement %+v", enforcement)
		}
	})
	t.Run("未登记", func(t *testing.T) {
		h, _ := handlerFixture(t)
		enforcement := systemPayload(t, readLicenseStatus(t, withRuntimeStatus(h, &fakeRuntimeStatusReader{})), "data_analysis").Enforcement
		if enforcement == nil || !enforcement.Available || enforcement.Registration != "UNREGISTERED" || enforcement.Status != "UNREGISTERED" || enforcement.MemberCount != 0 || enforcement.ReadyCount != 0 || enforcement.AckCount != 0 || enforcement.LatestReadyAt != "" || enforcement.LatestAckAt != "" {
			t.Fatalf("not ready enforcement %+v", enforcement)
		}
	})
	t.Run("读取失败", func(t *testing.T) {
		h, _ := handlerFixture(t)
		reader := &fakeRuntimeStatusReader{
			errs: map[string]error{"contract_management": errors.New("storage unavailable")},
			statuses: map[string]coordination.Status{"settlement": {
				Application: coordination.Application{Application: "settlement", State: runtime.Enforced, Revision: 2},
			}},
		}
		data := readLicenseStatus(t, withRuntimeStatus(h, reader))
		failed := systemPayload(t, data, "contract_management").Enforcement
		if failed == nil || failed.Available || failed.Registration != "UNKNOWN" || failed.Status != "UNKNOWN" {
			t.Fatalf("failed enforcement %+v", failed)
		}
		healthy := systemPayload(t, data, "settlement").Enforcement
		if healthy == nil || !healthy.Available || healthy.Registration != "REGISTERED" || healthy.Status != "ENFORCED" {
			t.Fatalf("healthy enforcement %+v", healthy)
		}
		// 部分失败但仍有成功读取，顶层仍可用。
		if data.RuntimeEnforcement != "AVAILABLE" {
			t.Fatalf("runtime enforcement %q", data.RuntimeEnforcement)
		}
	})
	// 已注入协调器但所有应用都读不到时，顶层必须反映实际可用性而不是“已注入”。
	t.Run("全部读取失败", func(t *testing.T) {
		h, _ := handlerFixture(t)
		errs := map[string]error{}
		for _, code := range []string{"contract_management", "customer_and_opportunity", "project_management", "settlement", "data_analysis", "customer_portal"} {
			errs[code] = errors.New("coordination storage unavailable")
		}
		data := readLicenseStatus(t, withRuntimeStatus(h, &fakeRuntimeStatusReader{errs: errs}))
		if data.RuntimeEnforcement != "UNAVAILABLE" {
			t.Fatalf("runtime enforcement %q", data.RuntimeEnforcement)
		}
		if data.RuntimeEnforcementSource != "per_application_coordination" {
			t.Fatalf("runtime enforcement source %q", data.RuntimeEnforcementSource)
		}
		for _, s := range data.Systems {
			if s.Enforcement == nil || s.Enforcement.Available || s.Enforcement.Registration != "UNKNOWN" || s.Enforcement.Status != "UNKNOWN" {
				t.Fatalf("system %s enforcement %+v", s.Code, s.Enforcement)
			}
		}
	})
	// 五个失败一个成功：顶层可用，成功行正常，失败行仍是 UNKNOWN。
	t.Run("五失败一成功", func(t *testing.T) {
		h, _ := handlerFixture(t)
		okCode := "data_analysis"
		errs := map[string]error{}
		for _, code := range []string{"contract_management", "customer_and_opportunity", "project_management", "settlement", "customer_portal"} {
			errs[code] = errors.New("coordination timeout")
		}
		reader := &fakeRuntimeStatusReader{
			errs: errs,
			statuses: map[string]coordination.Status{okCode: {
				Application: coordination.Application{Application: okCode, State: runtime.Enforced, Revision: 5},
			}},
		}
		data := readLicenseStatus(t, withRuntimeStatus(h, reader))
		if data.RuntimeEnforcement != "AVAILABLE" {
			t.Fatalf("runtime enforcement %q", data.RuntimeEnforcement)
		}
		healthy := systemPayload(t, data, okCode).Enforcement
		if healthy == nil || !healthy.Available || healthy.Registration != "REGISTERED" || healthy.Status != "ENFORCED" || healthy.Revision != 5 {
			t.Fatalf("healthy enforcement %+v", healthy)
		}
		for _, s := range data.Systems {
			if s.Code == okCode {
				continue
			}
			if s.Enforcement == nil || s.Enforcement.Available || s.Enforcement.Registration != "UNKNOWN" || s.Enforcement.Status != "UNKNOWN" {
				t.Fatalf("system %s enforcement %+v", s.Code, s.Enforcement)
			}
		}
	})
	t.Run("空成员", func(t *testing.T) {
		h, _ := handlerFixture(t)
		reader := &fakeRuntimeStatusReader{statuses: map[string]coordination.Status{"customer_and_opportunity": {
			Application:        coordination.Application{Application: "customer_and_opportunity", State: runtime.Enforced, Revision: 4},
			DeploymentRevision: 4,
			SnapshotCurrent:    true,
		}}}
		enforcement := systemPayload(t, readLicenseStatus(t, withRuntimeStatus(h, reader)), "customer_and_opportunity").Enforcement
		if enforcement == nil || enforcement.MemberCount != 0 || enforcement.ReadyCount != 0 || enforcement.AckCount != 0 || enforcement.LatestReadyAt != "" || enforcement.LatestAckAt != "" || !enforcement.SnapshotCurrent || enforcement.DeploymentRevision != 4 {
			t.Fatalf("empty members enforcement %+v", enforcement)
		}
	})
	t.Run("未接入", func(t *testing.T) {
		h, _ := handlerFixture(t)
		data := readLicenseStatus(t, withRuntimeStatus(h, nil))
		if data.RuntimeEnforcement != "UNAVAILABLE" {
			t.Fatalf("runtime enforcement %q", data.RuntimeEnforcement)
		}
		for _, s := range data.Systems {
			if s.Enforcement != nil {
				t.Fatalf("system %s expected null enforcement, got %+v", s.Code, s.Enforcement)
			}
		}
	})
}

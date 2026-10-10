package application

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	core "github.com/J-S-Te/license-core"
	"strings"
	"sync"
	"testing"
	"time"
)

// This transactional test store exercises the application contract without
// contacting production databases. Production persistence is exclusively GORM.
type memoryRepo struct {
	mu         sync.Mutex
	d          *domain.Deployment
	artifacts  map[string]domain.Artifact
	events     []domain.Event
	recoveries map[string]domain.ClockRecovery
}

var testActor = domain.Actor{TenantID: "tenant", UserID: "admin"}

type memoryTx struct{ r *memoryRepo }

func newMemory() *memoryRepo {
	return &memoryRepo{artifacts: map[string]domain.Artifact{}, recoveries: map[string]domain.ClockRecovery{}}
}
func (r *memoryRepo) Lock(_ context.Context, fn func(Transaction) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	backup := newMemory()
	if r.d != nil {
		d := *r.d
		backup.d = &d
	}
	for k, v := range r.artifacts {
		backup.artifacts[k] = v
	}
	for k, v := range r.recoveries {
		backup.recoveries[k] = v
	}
	backup.events = append([]domain.Event(nil), r.events...)
	err := fn(&memoryTx{r})
	if err != nil {
		r.d = backup.d
		r.artifacts = backup.artifacts
		r.events = backup.events
		r.recoveries = backup.recoveries
	}
	return err
}
func (r *memoryRepo) Events(_ context.Context, p, n int) (domain.EventPage, error) {
	return domain.EventPage{Items: r.events, Total: int64(len(r.events)), Page: p, PageSize: n}, nil
}
func (tx *memoryTx) Deployment() *domain.Deployment            { return tx.r.d }
func (tx *memoryTx) SaveDeployment(d *domain.Deployment) error { tx.r.d = d; return nil }
func (tx *memoryTx) Artifact(d string) (domain.Artifact, error) {
	a, ok := tx.r.artifacts[d]
	if !ok {
		return a, errors.New("missing")
	}
	return a, nil
}
func (tx *memoryTx) AddArtifact(a domain.Artifact) error {
	if _, ok := tx.r.artifacts[a.Digest]; ok {
		return domain.ErrConflict
	}
	tx.r.artifacts[a.Digest] = a
	return nil
}
func (tx *memoryTx) AddEvent(e domain.Event) error { tx.r.events = append(tx.r.events, e); return nil }
func (tx *memoryTx) ConsumeRecovery(r domain.ClockRecovery) error {
	if _, ok := tx.r.recoveries[r.ID]; ok {
		return domain.ErrConflict
	}
	tx.r.recoveries[r.ID] = r
	return nil
}

type fixture struct {
	repo     *memoryRepo
	s        *Service
	private  ed25519.PrivateKey
	now      time.Time
	instance string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{repo: newMemory(), private: priv, now: time.Unix(1900000000, 0)}
	f.s, e = NewService(f.repo, map[string]ed25519.PublicKey{"vendor": pub}, func() time.Time { return f.now })
	if e != nil {
		t.Fatal(e)
	}
	st, e := f.s.Initialize(context.Background(), InitializeInput{"customer", "production"}, domain.Actor{TenantID: "tenant", UserID: "admin"})
	if e != nil {
		t.Fatal(e)
	}
	f.instance = st.InstanceID
	return f
}
func (f *fixture) sign(t *testing.T, version uint64, nbf int64, apps ...core.Application) string {
	t.Helper()
	if len(apps) == 0 {
		apps = []core.Application{{Code: "contract_management", NotBefore: nbf, ExpiresAt: nbf + 86400, Kind: "FULL"}}
	}
	raw, e := core.Sign(core.License{ProtocolVersion: 1, Issuer: core.Issuer, ID: "license", Version: version, CustomerID: "customer", ProductID: core.Product, Environment: "production", InstanceID: f.instance, IssuedAt: nbf, NotBefore: nbf, Applications: apps}, "vendor", f.private)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func (f *fixture) input(t *testing.T, raw string) CommitInput {
	t.Helper()
	p, e := f.s.Preview(context.Background(), raw)
	if e != nil {
		t.Fatal(e)
	}
	return CommitInput{RawJWS: raw, Digest: p.Digest, ExpectedRevision: p.Revision, ExpectedCurrentVersion: p.CurrentVersion, ExpectedPendingDigest: p.PendingDigest, ConfirmChanges: true, ConfirmReplacePending: true}
}
func TestInitializationAndRequest(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	st, e := f.s.Initialize(ctx, InitializeInput{"customer", "production"}, testActor)
	if e != nil || st.InstanceID != f.instance || st.HighestVersion != 0 || len(f.repo.events) != 1 {
		t.Fatalf("idempotent initialization: %+v %v", st, e)
	}
	_, e = f.s.Initialize(ctx, InitializeInput{"other", "production"}, testActor)
	if !errors.Is(e, domain.ErrConflict) {
		t.Fatal(e)
	}
	f.repo.d.ClockBlocked = true
	req, e := f.s.Request(ctx)
	if e != nil || req.InstanceID != f.instance || req.CustomerID == "tenant" {
		t.Fatalf("request blocked/binding: %+v %v", req, e)
	}
}
func TestPendingWholeReplacementAndVersionConcurrency(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	n := f.now.Unix()
	first := f.input(t, f.sign(t, 1, n))
	if _, e := f.s.Commit(ctx, first, testActor); e != nil {
		t.Fatal(e)
	}
	second := f.input(t, f.sign(t, 2, n+3600, core.Application{Code: "settlement", NotBefore: n + 3600, ExpiresAt: n + 7200, Kind: "TRIAL"}))
	st, e := f.s.Commit(ctx, second, testActor)
	if e != nil || st.CurrentVersion != 1 || st.Pending == nil || st.HighestVersion != 2 {
		t.Fatalf("pending: %+v %v", st, e)
	}
	st, e = f.s.Read(ctx)
	if e != nil || len(st.Current.Applications) != 1 || st.Current.Applications[0].Code != "contract_management" {
		t.Fatalf("early replacement: %+v %v", st, e)
	}
	if _, e = f.s.Preview(ctx, f.sign(t, 2, n)); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("pending version downgraded: %v", e)
	}
	third := f.input(t, f.sign(t, 3, n+3600))
	third.ConfirmReplacePending = false
	if _, e = f.s.Commit(ctx, third, testActor); !errors.Is(e, domain.ErrConfirmation) {
		t.Fatal(e)
	}
	third.ConfirmReplacePending = true
	third.ExpectedRevision--
	if _, e = f.s.Commit(ctx, third, testActor); !errors.Is(e, domain.ErrConflict) {
		t.Fatal(e)
	}
	f.now = time.Unix(n+3600, 0)
	st, e = f.s.Read(ctx)
	if e != nil || st.CurrentVersion != 2 || st.Pending != nil || st.Current.Applications[0].Code != "settlement" {
		t.Fatalf("activation: %+v %v", st, e)
	}
}
func TestPreviewNoWritesAndExplicitChanges(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	raw := f.sign(t, 1, f.now.Unix())
	before := *f.repo.d
	in := f.input(t, raw)
	if *f.repo.d != before || len(f.repo.events) != 1 {
		t.Fatal("preview mutated state")
	}
	if _, e := f.s.Preview(ctx, "bad"); e == nil {
		t.Fatal("invalid preview accepted")
	}
	if *f.repo.d != before || len(f.repo.events) != 1 {
		t.Fatal("failed preview wrote")
	}
	in.ConfirmChanges = false
	if _, e := f.s.Commit(ctx, in, testActor); !errors.Is(e, domain.ErrConfirmation) {
		t.Fatal(e)
	}
	in.ConfirmChanges = true
	in.Digest = "wrong"
	if _, e := f.s.Commit(ctx, in, testActor); !errors.Is(e, domain.ErrConflict) {
		t.Fatal(e)
	}
}
func TestPersistedRawJWSIsFact(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	in := f.input(t, f.sign(t, 1, f.now.Unix()))
	if _, e := f.s.Commit(ctx, in, testActor); e != nil {
		t.Fatal(e)
	}
	a := f.repo.artifacts[in.Digest]
	a.RawJWS = "corrupted"
	f.repo.artifacts[in.Digest] = a
	if _, e := f.s.Read(ctx); !errors.Is(e, domain.ErrCorrupt) {
		t.Fatalf("corrupt restore silently accepted: %v", e)
	}
}
func TestClockLatchAndAtomicRecovery(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	start := f.now.Unix()
	in := f.input(t, f.sign(t, 1, start))
	if _, e := f.s.Commit(ctx, in, testActor); e != nil {
		t.Fatal(e)
	}
	f.now = time.Unix(start+3600, 0)
	if _, e := f.s.Read(ctx); e != nil {
		t.Fatal(e)
	}
	f.now = time.Unix(start, 0)
	if _, e := f.s.Read(ctx); !errors.Is(e, domain.ErrClock) {
		t.Fatal(e)
	}
	if !f.repo.d.ClockBlocked {
		t.Fatal("clock latch rolled back")
	}
	if _, e := f.s.Request(ctx); e != nil {
		t.Fatal(e)
	}
	r := core.Recovery{ProtocolVersion: 1, Issuer: core.Issuer, ID: "recover-1", ProductID: core.Product, InstanceID: f.instance, Environment: "production", LicenseID: "license", LicenseVersion: 1, IssuedAt: start, ExpiresAt: start + 600, AnchorAt: start}
	raw, e := core.SignRecovery(r, "vendor", f.private)
	if e != nil {
		t.Fatal(e)
	}
	st, e := f.s.RestoreClock(ctx, raw, testActor)
	if e != nil || st.ClockBlocked || st.HighestObservedAt != start {
		t.Fatalf("recovery: %+v %v", st, e)
	}
	if _, e = f.s.RestoreClock(ctx, raw, testActor); !errors.Is(e, domain.ErrConflict) {
		t.Fatal("replayed recovery", e)
	}
	if len(f.repo.recoveries) != 1 {
		t.Fatal("non atomic consume")
	}
	// A compromised wall clock cannot authorize an anchor that is still ahead.
	r.ID = "recover-2"
	r.AnchorAt = start + 400
	r.ExpiresAt = start + 900
	raw, e = core.SignRecovery(r, "vendor", f.private)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.RestoreClock(ctx, raw, testActor); !errors.Is(e, domain.ErrClock) {
		t.Fatal(e)
	}
	if len(f.repo.recoveries) != 1 {
		t.Fatal("rejected recovery consumed")
	}
}
func TestEmptyTrustAndConcurrentCommit(t *testing.T) {
	f := setup(t)
	empty, e := NewService(f.repo, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = empty.Preview(context.Background(), "bad"); !errors.Is(e, domain.ErrTrustNotConfigured) {
		t.Fatal(e)
	}
	in := f.input(t, f.sign(t, 1, f.now.Unix()))
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, e := f.s.Commit(context.Background(), in, testActor); results <- e }()
	}
	a, b := <-results, <-results
	if !((a == nil && errors.Is(b, domain.ErrConflict)) || (b == nil && errors.Is(a, domain.ErrConflict))) {
		t.Fatalf("concurrent commit: %v %v", a, b)
	}
}
func TestWriteActorRequiredAndValidated(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	in := f.input(t, f.sign(t, 1, f.now.Unix()))
	before := *f.repo.d
	for _, actor := range []domain.Actor{{}, {TenantID: "tenant"}, {UserID: "user"}, {TenantID: "tenant", UserID: " "}, {TenantID: "tenant", UserID: "user name"}, {TenantID: strings.Repeat("x", 129), UserID: "user"}} {
		if _, e := f.s.Initialize(ctx, InitializeInput{"customer", "production"}, actor); !errors.Is(e, domain.ErrInvalid) {
			t.Fatalf("initialize accepted %+v: %v", actor, e)
		}
		if _, e := f.s.Commit(ctx, in, actor); !errors.Is(e, domain.ErrInvalid) {
			t.Fatalf("commit accepted %+v: %v", actor, e)
		}
		if _, e := f.s.RestoreClock(ctx, "invalid", actor); !errors.Is(e, domain.ErrInvalid) {
			t.Fatalf("recovery accepted %+v: %v", actor, e)
		}
	}
	if *f.repo.d != before || len(f.repo.events) != 1 || len(f.repo.artifacts) != 0 {
		t.Fatal("rejected actor changed persistent state")
	}
}

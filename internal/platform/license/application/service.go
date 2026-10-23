package application

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	core "github.com/J-S-Te/license-core"
	"strings"
	"time"
	"unicode"
)

// Service 是授权安装状态机的应用服务；trusted 是编译期固定的厂商信任密钥。
type Service struct {
	repo    Repository
	trusted map[string]ed25519.PublicKey
	now     func() time.Time
}

// InitializeInput 携带初始化部署所需的客户与环境标识。
type InitializeInput struct {
	CustomerID  string `json:"customer_id"`
	Environment string `json:"environment"`
}

// CommitInput 携带双阶段提交的全部期望值：摘要、revision、当前版本与挂起摘要。
// ConfirmChanges/ConfirmReplacePending 是 Preview 要求的显式确认开关。
type CommitInput struct {
	RawJWS                 string `json:"raw_jws"`
	Digest                 string `json:"digest"`
	ExpectedRevision       uint64 `json:"expected_revision"`
	ExpectedCurrentVersion uint64 `json:"expected_current_version"`
	ExpectedPendingDigest  string `json:"expected_pending_digest"`
	ConfirmChanges         bool   `json:"confirm_changes"`
	ConfirmReplacePending  bool   `json:"confirm_replace_pending"`
}

// NewService 构造授权服务：仓储必填，时钟缺省使用 time.Now，
// 非法长度的信任密钥直接拒绝，保证启动即校验信任配置。
func NewService(repo Repository, trusted map[string]ed25519.PublicKey, now func() time.Time) (*Service, error) {
	if repo == nil {
		return nil, domain.ErrInvalid
	}
	if now == nil {
		now = time.Now
	}
	keys := make(map[string]ed25519.PublicKey, len(trusted))
	for id, key := range trusted {
		if len(key) != ed25519.PublicKeySize {
			return nil, domain.ErrInvalid
		}
		keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	return &Service{repo: repo, trusted: keys, now: now}, nil
}
// Initialize 创建部署记录：首次调用生成随机 InstanceID；重复调用校验客户与环境
// 绑定一致后返回现状，绑定不一致返回 ErrBindingMismatch。
func (s *Service) Initialize(ctx context.Context, in InitializeInput, actor domain.Actor) (domain.State, error) {
	var out domain.State
	if !validActor(actor) {
		return out, domain.ErrInvalid
	}
	err := core.ValidateRequest(core.Request{ProtocolVersion: core.ProtocolVersion, ProductID: core.Product, CustomerID: in.CustomerID, Environment: in.Environment, InstanceID: "validation"})
	if err != nil {
		return out, err
	}
	err = s.repo.Lock(ctx, func(tx Transaction) error {
		d := tx.Deployment()
		if d != nil {
			if d.CustomerID != in.CustomerID || d.Environment != in.Environment {
				return domain.ErrBindingMismatch
			}
			var e error
			out, e = s.state(tx)
			return e
		}
		b := make([]byte, 16)
		if _, e := rand.Read(b); e != nil {
			return e
		}
		now := s.now().UTC()
		d = &domain.Deployment{ID: 1, InstanceID: hex.EncodeToString(b), CustomerID: in.CustomerID, Environment: in.Environment, HighestObservedAt: now.Unix(), CreatedAt: now, UpdatedAt: now}
		if e := tx.SaveDeployment(d); e != nil {
			return e
		}
		if e := tx.AddEvent(event("INITIALIZED", d, "", 0, actor, now)); e != nil {
			return e
		}
		var e error
		out, e = s.state(tx)
		return e
	})
	return out, err
}
func event(kind string, d *domain.Deployment, digest string, version uint64, actor domain.Actor, now time.Time) domain.Event {
	return domain.Event{Kind: kind, Digest: digest, Version: version, Revision: d.Revision, TenantID: actor.TenantID, UserID: actor.UserID, CreatedAt: now}
}
func validActor(actor domain.Actor) bool {
	for _, id := range []string{actor.TenantID, actor.UserID} {
		if len(id) == 0 || len(id) > 128 || strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return false
		}
	}
	return true
}
func (s *Service) state(tx Transaction) (domain.State, error) {
	d := tx.Deployment()
	if d == nil {
		return domain.State{}, domain.ErrNotInitialized
	}
	if err := core.ValidateRequest(core.Request{ProtocolVersion: core.ProtocolVersion, ProductID: core.Product, CustomerID: d.CustomerID, Environment: d.Environment, InstanceID: d.InstanceID}); err != nil || d.ID != 1 || d.HighestObservedAt <= 0 {
		return domain.State{}, domain.ErrCorrupt
	}
	out := domain.State{InstanceID: d.InstanceID, CustomerID: d.CustomerID, Environment: d.Environment, Revision: d.Revision, HighestVersion: d.HighestVersion, CurrentDigest: d.CurrentDigest, PendingDigest: d.PendingDigest, HighestObservedAt: d.HighestObservedAt, ClockBlocked: d.ClockBlocked}
	for _, slot := range []struct {
		digest string
		target **core.License
	}{{d.CurrentDigest, &out.Current}, {d.PendingDigest, &out.Pending}} {
		if slot.digest == "" {
			continue
		}
		if len(s.trusted) == 0 {
			return out, domain.ErrTrustNotConfigured
		}
		a, e := tx.Artifact(slot.digest)
		if e != nil {
			return out, fmt.Errorf("%w: artifact unavailable", domain.ErrCorrupt)
		}
		l, e := core.Verify(a.RawJWS, s.trusted)
		if e != nil {
			return out, fmt.Errorf("%w: signature", domain.ErrCorrupt)
		}
		digest, e := core.Digest(a.RawJWS, s.trusted)
		if e != nil || digest != slot.digest || !bound(l, d) || l.Version > d.HighestVersion {
			return out, domain.ErrCorrupt
		}
		*slot.target = &l
	}
	if out.Current != nil {
		out.CurrentVersion = out.Current.Version
	}
	if out.Pending != nil && (out.Pending.Version <= out.CurrentVersion || out.Pending.Version != out.HighestVersion) {
		return out, domain.ErrCorrupt
	}
	if out.Pending == nil && out.CurrentVersion != out.HighestVersion {
		return out, domain.ErrCorrupt
	}
	return out, nil
}
func bound(l core.License, d *domain.Deployment) bool {
	return l.CustomerID == d.CustomerID && l.Environment == d.Environment && l.InstanceID == d.InstanceID
}

// observe 即使检测到时钟回拨也会提交观测值：持续记录最高观测时间是检测回拨的依据本身，
// 丢单反而会让后续比较失真。只有签名的恢复凭证才能清除这个持久化的失败关闭闩锁。
func (s *Service) observe(tx Transaction, now time.Time) (bool, error) {
	d := tx.Deployment()
	blocked := d.ClockBlocked || now.Unix() < d.HighestObservedAt-300
	if blocked {
		d.ClockBlocked = true
	}
	if now.Unix() > d.HighestObservedAt {
		d.HighestObservedAt = now.Unix()
	}
	d.UpdatedAt = now
	return blocked, tx.SaveDeployment(d)
}
func (s *Service) activate(tx Transaction, st domain.State, now time.Time) (domain.State, error) {
	if st.Pending == nil || now.Unix() < st.Pending.NotBefore {
		return st, nil
	}
	d := tx.Deployment()
	d.CurrentDigest = d.PendingDigest
	d.PendingDigest = ""
	d.Revision++
	d.UpdatedAt = now
	if e := tx.SaveDeployment(d); e != nil {
		return st, e
	}
	if e := tx.AddEvent(event("ACTIVATED", d, d.CurrentDigest, st.Pending.Version, domain.Actor{}, now)); e != nil {
		return st, e
	}
	return s.state(tx)
}
// Read 校验并返回当前状态，同时提交时钟观测、按需激活挂起包；
// 检测到时钟回拨时返回 ErrClock，状态保持失败关闭。
func (s *Service) Read(ctx context.Context) (domain.State, error) {
	var out domain.State
	var blocked bool
	err := s.repo.Lock(ctx, func(tx Transaction) error {
		var e error
		out, e = s.state(tx)
		if e != nil {
			return e
		}
		now := s.now().UTC()
		blocked, e = s.observe(tx, now)
		if e != nil {
			return e
		}
		if blocked {
			out.ClockBlocked = true
			return nil
		}
		out, e = s.activate(tx, out, now)
		if e == nil {
			out.HighestObservedAt = tx.Deployment().HighestObservedAt
		}
		return e
	})
	if err == nil && blocked {
		err = domain.ErrClock
	}
	return out, err
}

// Request 刻意绕过业务时钟闸门，保证时钟被锁死后签发恢复请求的通道仍然可用。
func (s *Service) Request(ctx context.Context) (core.Request, error) {
	var out core.Request
	err := s.repo.Lock(ctx, func(tx Transaction) error {
		d := tx.Deployment()
		if d == nil {
			return domain.ErrNotInitialized
		}
		out = core.Request{ProtocolVersion: core.ProtocolVersion, ProductID: core.Product, CustomerID: d.CustomerID, Environment: d.Environment, InstanceID: d.InstanceID}
		return core.ValidateRequest(out)
	})
	return out, err
}
func (s *Service) preview(tx Transaction, raw string) (domain.Preview, error) {
	st, e := s.state(tx)
	if e != nil {
		return domain.Preview{}, e
	}
	if len(s.trusted) == 0 {
		return domain.Preview{}, domain.ErrTrustNotConfigured
	}
	l, e := core.Verify(raw, s.trusted)
	if e != nil {
		return domain.Preview{}, e
	}
	if !bound(l, tx.Deployment()) {
		return domain.Preview{}, core.ErrDenied
	}
	if l.Version <= st.HighestVersion {
		return domain.Preview{}, domain.ErrVersionNotIncreasing
	}
	digest, e := core.Digest(raw, s.trusted)
	if e != nil {
		return domain.Preview{}, e
	}
	out := domain.Preview{State: st, Digest: digest, License: l, Changes: []core.ApplicationChange{}, RequiresPendingReplacement: st.Pending != nil}
	if st.Current != nil {
		out.Changes, e = core.Diff(l, *st.Current)
		if e != nil {
			return out, e
		}
	} else {
		for _, a := range l.Applications {
			fresh := a
			out.Changes = append(out.Changes, core.ApplicationChange{Code: a.Code, Kind: core.ADDED, New: &fresh})
		}
	}
	out.RequiresChangeConfirmation = len(out.Changes) > 0
	return out, nil
}
// Preview 验签并解析拟导入许可证与当前状态的差异，不落库；版本必须严格递增。
func (s *Service) Preview(ctx context.Context, raw string) (domain.Preview, error) {
	var out domain.Preview
	err := s.repo.Lock(ctx, func(tx Transaction) error { var e error; out, e = s.preview(tx, raw); return e })
	return out, err
}
// Commit 以双阶段确认完成导入：期望值任一不匹配即冲突，Preview 要求的确认开关
// 缺失即拒绝；时钟闩锁生效时拒绝写入。
func (s *Service) Commit(ctx context.Context, in CommitInput, actor domain.Actor) (domain.State, error) {
	var out domain.State
	if !validActor(actor) {
		return out, domain.ErrInvalid
	}
	var blocked bool
	err := s.repo.Lock(ctx, func(tx Transaction) error {
		p, e := s.preview(tx, in.RawJWS)
		if e != nil {
			return e
		}
		if p.Digest != in.Digest || p.Revision != in.ExpectedRevision || p.CurrentVersion != in.ExpectedCurrentVersion || p.PendingDigest != in.ExpectedPendingDigest {
			return domain.ErrConflict
		}
		if (p.RequiresChangeConfirmation && !in.ConfirmChanges) || (p.RequiresPendingReplacement && !in.ConfirmReplacePending) {
			return domain.ErrConfirmation
		}
		now := s.now().UTC()
		blocked, e = s.observe(tx, now)
		if e != nil {
			return e
		}
		if blocked {
			return nil
		}
		d := tx.Deployment()
		if e = tx.AddArtifact(domain.Artifact{Digest: p.Digest, RawJWS: in.RawJWS, Version: p.License.Version, CreatedAt: now}); e != nil {
			return e
		}
		d.HighestVersion = p.License.Version
		d.Revision++
		kind := "IMPORTED_PENDING"
		if now.Unix() >= p.License.NotBefore {
			d.CurrentDigest = p.Digest
			d.PendingDigest = ""
			kind = "IMPORTED_CURRENT"
		} else {
			d.PendingDigest = p.Digest
		}
		if e = tx.SaveDeployment(d); e != nil {
			return e
		}
		if e = tx.AddEvent(event(kind, d, p.Digest, p.License.Version, actor, now)); e != nil {
			return e
		}
		out, e = s.state(tx)
		return e
	})
	if err == nil && blocked {
		err = domain.ErrClock
	}
	return out, err
}
// Events 分页返回授权事件审计流；分页参数越界直接拒绝。
func (s *Service) Events(ctx context.Context, page, size int) (domain.EventPage, error) {
	if page < 1 || size < 1 || size > 100 || page > 1000000 {
		return domain.EventPage{}, domain.ErrInvalid
	}
	return s.repo.Events(ctx, page, size)
}
// RestoreClock 校验一次性签名恢复凭证后解除时钟闩锁，并把最高观测时间推进到锚点。
// 凭证只能消费一次，过期或回拨窗口不匹配即拒绝。
func (s *Service) RestoreClock(ctx context.Context, raw string, actor domain.Actor) (domain.State, error) {
	if !validActor(actor) {
		return domain.State{}, domain.ErrInvalid
	}
	if len(s.trusted) == 0 {
		return domain.State{}, domain.ErrTrustNotConfigured
	}
	r, e := core.VerifyRecovery(raw, s.trusted)
	if e != nil {
		return domain.State{}, e
	}
	var out domain.State
	e = s.repo.Lock(ctx, func(tx Transaction) error {
		st, err := s.state(tx)
		if err != nil {
			return err
		}
		if st.Current == nil {
			return domain.ErrNotInitialized
		}
		// 回拨期间主机墙钟不可信。恢复授权以单独签名的锚点时间作为可信时间基准。
		if err = core.ValidateRecovery(r, *st.Current, time.Unix(r.AnchorAt, 0)); err != nil {
			return err
		}
		now := s.now().UTC()
		if now.Unix() < r.AnchorAt-300 || now.Unix() >= r.ExpiresAt {
			return domain.ErrClock
		}
		if err = tx.ConsumeRecovery(domain.ClockRecovery{ID: r.ID, LicenseDigest: st.CurrentDigest, AnchorAt: r.AnchorAt, RawJWS: raw, CreatedAt: now}); err != nil {
			return err
		}
		d := tx.Deployment()
		d.HighestObservedAt = r.AnchorAt
		if now.Unix() > d.HighestObservedAt {
			d.HighestObservedAt = now.Unix()
		}
		d.ClockBlocked = false
		d.Revision++
		d.UpdatedAt = now
		if err = tx.SaveDeployment(d); err != nil {
			return err
		}
		if err = tx.AddEvent(event("CLOCK_RECOVERED", d, st.CurrentDigest, st.CurrentVersion, actor, now)); err != nil {
			return err
		}
		out, err = s.state(tx)
		return err
	})
	return out, e
}

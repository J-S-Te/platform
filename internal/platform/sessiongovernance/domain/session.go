// Package domain 定义平台统一会话的不可变认证起点和真实交互窗口。
package domain

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrInactive         = errors.New("central session is inactive")
	ErrBindingConflict  = errors.New("central session binding conflicts")
	ErrValidation       = errors.New("central session input is invalid")
	ErrUpstreamInactive = errors.New("upstream login lifetime is inactive")
)

// Policy 是平台管理的会话期限；各RP不得另行延长。
type Policy struct {
	IdleTimeout time.Duration
	AbsoluteTTL time.Duration
	Version     uint64
}

// PlatformAnchor 是已认证平台Cookie会话的可信原始期限，不来自浏览器报文。
type PlatformAnchor struct {
	ID, TenantID, IdentityID           string
	AuthenticatedAt, LastInteractiveAt time.Time
	ExpiresAt                          time.Time
	RevokedAt                          *time.Time
}

// Session 是所有已绑定RP共同消费的一个认证周期。
type Session struct {
	ID, TenantID, IdentityID           string
	AuthenticatedAt, LastInteractiveAt time.Time
	ExpiresAt                          time.Time
	RevokedAt                          *time.Time
	RevokeReason                       string
	Version                            uint64
}

// DefaultPolicy 返回本轮统一管理的三十分钟空闲和八小时绝对期限。
// 参数：无。
// 返回值：平台默认策略；策略版本从1开始。
func DefaultPolicy() Policy {
	return Policy{IdleTimeout: 30 * time.Minute, AbsoluteTTL: 8 * time.Hour, Version: 1}
}

// Validate 核验策略不会允许RP越过本轮统一期限上限。
// 参数：无。
// 返回值：合法时nil；非正版本、错误期限或过度延长时返回校验错误。
func (p Policy) Validate() error {
	if p.Version == 0 || p.IdleTimeout < time.Minute || p.IdleTimeout > 30*time.Minute ||
		p.AbsoluteTTL <= p.IdleTimeout || p.AbsoluteTTL > 8*time.Hour {
		return ErrValidation
	}
	return nil
}

// NewSession 以已验证的原始认证时间建立中央会话，不能用当前时间补造起点。
// 参数：id/tenant/identity为可信标识，authenticatedAt为已验证时间，now为服务端时钟，
//
//	policy为平台策略，parent为同一浏览器已认证平台锚点或nil。
//
// 返回值：保守继承原期限的会话；缺时间、过期或身份错位返回明确错误。
func NewSession(id, tenant, identity string, authenticatedAt, now time.Time,
	policy Policy, parent *PlatformAnchor) (Session, error) {
	if policy.Validate() != nil || strings.TrimSpace(id) == "" ||
		strings.TrimSpace(tenant) == "" || strings.TrimSpace(identity) == "" ||
		authenticatedAt.IsZero() || authenticatedAt.After(now.Add(5*time.Second)) {
		return Session{}, ErrValidation
	}
	s := Session{ID: id, TenantID: tenant, IdentityID: identity, Version: 1,
		AuthenticatedAt: authenticatedAt.UTC(), LastInteractiveAt: authenticatedAt.UTC(),
		ExpiresAt: authenticatedAt.Add(policy.AbsoluteTTL).UTC()}
	if parent != nil {
		if err := s.inherit(parent, now, policy); err != nil {
			return Session{}, err
		}
	}
	if !s.ValidAt(now, policy) {
		return Session{}, ErrInactive
	}
	return s, nil
}

// inherit 继承平台原认证周期，不把自动回调计作新的真实交互。
// 参数：parent为同一浏览器锚点，now为服务端时钟，policy为中央策略。
// 返回值：身份或原期限非法时错误；否则原地保守收窄当前会话。
func (s *Session) inherit(parent *PlatformAnchor, now time.Time, policy Policy) error {
	if parent.ID == "" || parent.TenantID != s.TenantID || parent.IdentityID != s.IdentityID {
		return ErrBindingConflict
	}
	if parent.RevokedAt != nil || parent.AuthenticatedAt.IsZero() ||
		!parent.ExpiresAt.After(now) || !parent.LastInteractiveAt.Add(policy.IdleTimeout).After(now) {
		return ErrInactive
	}
	if parent.AuthenticatedAt.Before(s.AuthenticatedAt) {
		s.AuthenticatedAt = parent.AuthenticatedAt.UTC()
	}
	if parent.ExpiresAt.Before(s.ExpiresAt) {
		s.ExpiresAt = parent.ExpiresAt.UTC()
	}
	if parent.LastInteractiveAt.After(s.LastInteractiveAt) {
		s.LastInteractiveAt = parent.LastInteractiveAt.UTC()
	}
	return nil
}

// ValidAt 只读判断统一会话是否仍在空闲与固定绝对期限内。
// 参数：now为服务端时钟，policy为当前平台策略。
// 返回值：仅未撤销、合法并同时满足两个严格时间边界时为真。
func (s Session) ValidAt(now time.Time, policy Policy) bool {
	return policy.Validate() == nil && s.ID != "" && s.TenantID != "" && s.IdentityID != "" &&
		s.RevokedAt == nil && !s.AuthenticatedAt.IsZero() && !s.LastInteractiveAt.IsZero() &&
		s.ExpiresAt.After(now) && s.LastInteractiveAt.Add(policy.IdleTimeout).After(now) &&
		!s.ExpiresAt.After(s.AuthenticatedAt.Add(policy.AbsoluteTTL))
}

// Interact 记录一个已认证RP代理的真实交互，不允许复活已失效会话。
// 参数：now为服务端接收交互时间，policy为当前平台策略。
// 返回值：更新交互窗口后的副本；到期或撤销时返回未认证错误。
func (s Session) Interact(now time.Time, policy Policy) (Session, error) {
	if !s.ValidAt(now, policy) {
		return Session{}, ErrInactive
	}
	if now.After(s.LastInteractiveAt) {
		s.LastInteractiveAt = now.UTC()
		s.Version++
	}
	return s, nil
}

// IdleRemaining 返回同时受原绝对截止约束的空闲剩余时间。
// 参数：now为服务端时钟，policy为当前平台策略。
// 返回值：非负剩余时间；已失效时为零。
func (s Session) IdleRemaining(now time.Time, policy Policy) time.Duration {
	if !s.ValidAt(now, policy) {
		return 0
	}
	deadline := s.LastInteractiveAt.Add(policy.IdleTimeout)
	if s.ExpiresAt.Before(deadline) {
		deadline = s.ExpiresAt
	}
	return deadline.Sub(now)
}

// CheckAnchorBinding 拒绝自动合并两个不同的已存在认证周期。
// 参数：platformID为平台已有中央锚点，externalID为上游sid已有中央锚点。
// 返回值：同一锚点或一侧未关联时nil；两个不同已存在锚点时冲突。
func CheckAnchorBinding(platformID, externalID string) error {
	if platformID != "" && externalID != "" && platformID != externalID {
		return ErrBindingConflict
	}
	return nil
}

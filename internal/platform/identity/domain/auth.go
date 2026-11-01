// Package domain 承载身份领域概念，不依赖任何传输层或数据库实现。
package domain

import (
	"net"
	"time"
)

// LoginAccount 汇总密码登录用例所需的身份与凭据状态，登录判定全部以此为准。
type LoginAccount struct {
	TenantID           string
	TenantName         string
	TenantCode         string
	TenantStatus       string
	UserID             string
	UserName           string
	UserStatus         string
	AccountID          string
	AccountName        string
	AccountStatus      string
	LockedUntil        *time.Time
	PasswordHash       []byte
	HashAlgorithm      string
	AlgorithmParams    []byte
	CredentialStatus   string
	CredentialExpiry   *time.Time
	MustChangePassword bool
}

// Session 是持久化的浏览器会话状态，用于构造 API 响应与会话 cookie。
type Session struct {
	ID               string
	TenantID         string
	AccountID        string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	IPAddress        []byte
	UserAgent        string
	CentralSessionID string
}

// SessionAnchor 是组合根经中央协议解析出的原始认证周期，不接受浏览器提供。
type SessionAnchor struct {
	ID        string
	ExpiresAt time.Time
}

// ReferenceName 是租户、用户、账号或角色的紧凑 API 表示。
type ReferenceName struct {
	ID   string
	Name string
	Code string
}

// Principal 是会话验证返回的已认证服务端身份，其中携带服务端授权快照。
type Principal struct {
	SessionID           string    `json:"-"`
	SessionIdleDeadline time.Time `json:"-"`
	LoginIP             net.IP
	Tenant              ReferenceName
	User                ReferenceName
	Account             ReferenceName
	Roles               []ReferenceName
	PermissionCodes     []string
}

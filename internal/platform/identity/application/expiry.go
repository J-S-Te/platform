package application

import (
	"context"
	"time"
)

const (
	// ExpiryKindUser 标识用户身份到期候选。
	ExpiryKindUser = "USER"
	// ExpiryKindAccount 标识登录账号到期候选。
	ExpiryKindAccount = "ACCOUNT"
)

// ExpiryCandidate 是一个配置有效期已届满、其登出/审计/通知生命周期尚未完成的身份。
type ExpiryCandidate struct {
	Kind       string
	TenantID   string
	ResourceID string
	UserID     string
	ValidUntil time.Time
	AccountIDs []string
}

// ExpiryRepository 列出到期候选并标记处理完成；MarkExpirationProcessed 必须以候选快照为
// 并发条件，防止看护任务重复登出或漏处理。
type ExpiryRepository interface {
	ListDueExpirations(context.Context, time.Time, int) ([]ExpiryCandidate, error)
	MarkExpirationProcessed(context.Context, ExpiryCandidate, time.Time) error
}

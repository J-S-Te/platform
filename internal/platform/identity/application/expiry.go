package application

import (
	"context"
	"time"
)

const (
	ExpiryKindUser    = "USER"
	ExpiryKindAccount = "ACCOUNT"
)

// ExpiryCandidate is an identity whose configured validity has elapsed and whose
// logout/audit/notification lifecycle has not completed yet.
type ExpiryCandidate struct {
	Kind       string
	TenantID   string
	ResourceID string
	UserID     string
	ValidUntil time.Time
	AccountIDs []string
}

type ExpiryRepository interface {
	ListDueExpirations(context.Context, time.Time, int) ([]ExpiryCandidate, error)
	MarkExpirationProcessed(context.Context, ExpiryCandidate, time.Time) error
}

// Package domain defines tenant-scoped business dictionary aggregates.
package domain

import "time"

// Status controls whether a dictionary or dictionary item is selectable at runtime.
type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusDisabled Status = "DISABLED"
)

// Dictionary groups related business enumeration values under a stable code.
type Dictionary struct {
	ID          string
	TenantID    string
	Code        string
	Name        string
	Description string
	Status      Status
	ItemCount   int64
	Version     uint64
	UpdatedAt   time.Time
}

// Item is a selectable dictionary value.
type Item struct {
	ID           string
	TenantID     string
	DictionaryID string
	Code         string
	Label        string
	Value        string
	SortOrder    uint
	Status       Status
	Version      uint64
	UpdatedAt    time.Time
}

// Package domain defines configuration aggregates independent of the HTTP and database layers.
package domain

import "time"

// Reference identifies a configuration-related resource in API responses.
type Reference struct {
	ID   string
	Code string
	Name string
}

// Namespace is an application configuration namespace scoped to a tenant and environment.
type Namespace struct {
	ID          string
	Application Reference
	Code        string
	Name        string
	Description string
	Version     uint64
}

// Item is a draft configuration value. Sensitive values are never returned in plaintext.
type Item struct {
	ID        string
	Namespace Reference
	Key       string
	ValueType string
	Value     any
	Secret    bool
	Version   uint64
	UpdatedAt time.Time
}

// VersionedItem selects one specific draft version for a release snapshot.
type VersionedItem struct {
	ItemID  string
	Version uint64
}

// Release is an immutable configuration snapshot publication record.
type Release struct {
	ID          string
	Namespace   Reference
	VersionNo   uint64
	Status      string
	Comment     string
	CreatedAt   time.Time
	PublishedAt *time.Time
}

// PublishedConfig is the safe runtime representation of a published namespace.
type PublishedConfig struct {
	ApplicationCode string
	NamespaceCode   string
	ReleaseVersion  uint64
	Values          map[string]any
}

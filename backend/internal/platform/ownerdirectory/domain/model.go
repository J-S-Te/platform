// Package domain defines the minimal application-facing owner directory contract.
package domain

// Organization is an active organization membership available for owner scoping.
type Organization struct {
	ID        string `json:"organization_id"`
	Name      string `json:"organization_name"`
	IsPrimary bool   `json:"is_primary"`
}

// User is an active internal user authorized for the calling application.
// Deliberately exclude account names, email addresses and mobile numbers.
type User struct {
	ID            string         `json:"user_id"`
	DisplayName   string         `json:"display_name"`
	Organizations []Organization `json:"organizations"`
}

// Page is a deterministic page of owner-directory users.
type Page struct {
	Items    []User `json:"items"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
	Total    int64  `json:"total"`
}

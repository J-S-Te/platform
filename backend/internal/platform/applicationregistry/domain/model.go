// Package domain defines application integration credentials without exposing secret material.
package domain

import "time"

// OAuthClient is the active portion of an application client registration needed to issue
// and validate Client Credentials access tokens. Credential hashes are never returned from
// this model because callers must not persist or log them.
type OAuthClient struct {
	ID                    string
	TenantID              string
	ApplicationID         string
	ApplicationCode       string
	EnvironmentID         string
	EnvironmentCode       string
	ClientID              string
	TokenAuthMethod       string
	AccessTokenTTLSeconds uint
	GrantTypes            map[string]struct{}
	Scopes                map[string]struct{}
}

// ClientCredential represents a currently usable secret hash. It deliberately stores only
// the one-way hash read from platform_oauth_client_credential.
type ClientCredential struct {
	SecretHash []byte
	ValidUntil *time.Time
}

package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/domain"
	"golang.org/x/crypto/bcrypt"
)

const maxAuthorizationCodeTTL = 10 * time.Minute

// Service implements authorization-code, PKCE, refresh-token rotation, revocation, and UserInfo
// state transitions. It is deliberately independent of HTTP, discovery, JWKS, and JWT encoding.
type Service struct {
	repository           Repository
	issuer               TokenIssuer
	ids                  IDGenerator
	secrets              SecretGenerator
	clock                Clock
	authorizationCodeTTL time.Duration
}

// NewService validates dependencies and creates an OIDC/OAuth application service.
func NewService(repository Repository, issuer TokenIssuer, ids IDGenerator, secrets SecretGenerator, clock Clock, authorizationCodeTTL time.Duration) (*Service, error) {
	if repository == nil || issuer == nil || ids == nil || secrets == nil || clock == nil {
		return nil, errors.New("OIDC/OAuth service dependencies must not be nil")
	}
	if authorizationCodeTTL <= 0 || authorizationCodeTTL > maxAuthorizationCodeTTL {
		return nil, fmt.Errorf("OIDC/OAuth authorization code TTL must be within (0, %s]", maxAuthorizationCodeTTL)
	}
	return &Service{
		repository: repository, issuer: issuer, ids: ids, secrets: secrets, clock: clock,
		authorizationCodeTTL: authorizationCodeTTL,
	}, nil
}

// IsRegisteredRedirectURI reports whether redirectURI exactly matches an active client's registered URI.
// It intentionally fails closed for missing or disabled clients and never performs prefix matching.
func (service *Service) IsRegisteredRedirectURI(ctx context.Context, clientID, redirectURI string) (bool, error) {
	clientID, redirectURI = strings.TrimSpace(clientID), strings.TrimSpace(redirectURI)
	if clientID == "" || redirectURI == "" {
		return false, nil
	}
	client, err := service.repository.FindClient(ctx, clientID, service.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("load OIDC/OAuth client redirect registrations: %w", err)
	}
	_, registered := client.RedirectURIs[redirectURI]
	return registered, nil
}

// Authorize validates an exact client redirect and the current local browser session, then creates
// a one-time opaque authorization code. The code value returned here is never persisted directly.
func (service *Service) Authorize(ctx context.Context, input AuthorizationInput) (AuthorizationResult, error) {
	input = normalizeAuthorizationInput(input)
	if input.ClientID == "" || input.RedirectURI == "" || input.SessionID == "" || !validProtocolText(input.State, 2048) || !validProtocolText(input.Nonce, 255) {
		return AuthorizationResult{}, ErrInvalidRequest
	}

	now := service.now()
	client, err := service.repository.FindClient(ctx, input.ClientID, now)
	if err != nil {
		return AuthorizationResult{}, mapClientError(err)
	}
	if !has(client.GrantTypes, "authorization_code") {
		return AuthorizationResult{}, ErrUnauthorizedClient
	}
	if _, exists := client.RedirectURIs[input.RedirectURI]; !exists {
		return AuthorizationResult{}, ErrInvalidRequest
	}
	scopes, err := registeredScopes(client, input.Scopes)
	if err != nil {
		return AuthorizationResult{}, err
	}
	challenge, method, err := validatePKCE(client.RequirePKCE, input.CodeChallenge, input.CodeChallengeMethod)
	if err != nil {
		return AuthorizationResult{}, err
	}

	subject, err := service.repository.ResolveSessionSubject(ctx, input.SessionID, now)
	if err != nil {
		return AuthorizationResult{}, mapGrantError(err)
	}
	if subject.TenantID != client.TenantID || !subject.ExpiresAt.After(now) {
		return AuthorizationResult{}, ErrInvalidGrant
	}

	codeID, err := service.ids.New(now)
	if err != nil {
		return AuthorizationResult{}, fmt.Errorf("generate authorization code id: %w", err)
	}
	rawCode, err := service.secrets.NewSecret()
	if err != nil {
		return AuthorizationResult{}, fmt.Errorf("generate authorization code: %w", err)
	}
	if !validOpaqueSecret(rawCode) {
		return AuthorizationResult{}, errors.New("OIDC/OAuth secret generator returned an invalid authorization code")
	}
	expiresAt := now.Add(service.authorizationCodeTTL)
	code := domain.AuthorizationCode{
		ID: codeID, TenantID: subject.TenantID, OAuthClientID: client.ID, SessionID: subject.SessionID,
		AccountID: subject.AccountID, UserID: subject.UserID, CodeHash: digest(rawCode), RedirectURI: input.RedirectURI,
		Scopes: scopes, Nonce: input.Nonce, CodeChallenge: challenge, CodeChallengeMethod: method,
		CreatedAt: now, ExpiresAt: expiresAt, Status: domain.AuthorizationCodeStatusActive,
	}
	if err := service.repository.CreateAuthorizationCode(ctx, code); err != nil {
		return AuthorizationResult{}, fmt.Errorf("persist authorization code: %w", err)
	}
	return AuthorizationResult{
		AuthorizationCode: rawCode, RedirectURI: input.RedirectURI, Scope: strings.Join(scopes, " "), State: input.State, ExpiresAt: expiresAt,
	}, nil
}

// ExchangeAuthorizationCode validates client authentication and PKCE, signs response tokens, and
// atomically consumes the code. A concurrent second exchange sees invalid_grant and never obtains
// a durable refresh token.
func (service *Service) ExchangeAuthorizationCode(ctx context.Context, input AuthorizationCodeExchangeInput) (TokenResult, error) {
	input = normalizeCodeExchangeInput(input)
	if input.ClientID == "" || input.Code == "" || input.RedirectURI == "" {
		return TokenResult{}, ErrInvalidRequest
	}
	now := service.now()
	client, err := service.authenticatedClient(ctx, input.ClientAuthentication, now)
	if err != nil {
		return TokenResult{}, err
	}
	if !has(client.GrantTypes, "authorization_code") {
		return TokenResult{}, ErrUnauthorizedClient
	}
	codeHash := digest(input.Code)
	code, err := service.repository.FindAuthorizationCode(ctx, codeHash, now)
	if err != nil {
		return TokenResult{}, mapGrantError(err)
	}
	if !validAuthorizationCode(code, client, input.RedirectURI, now) || !verifyPKCE(code.CodeChallenge, code.CodeChallengeMethod, input.CodeVerifier) {
		return TokenResult{}, ErrInvalidGrant
	}

	preview := grantFromAuthorizationCode(code, client)
	refresh, rawRefresh, err := service.newRefreshToken(client, preview, "", now)
	if err != nil {
		return TokenResult{}, err
	}
	result, err := service.issueTokens(ctx, client, preview, rawRefresh, now)
	if err != nil {
		return TokenResult{}, err
	}
	committedGrant, err := service.repository.ConsumeAuthorizationCode(ctx, ConsumeAuthorizationCodeCommand{
		CodeHash: codeHash, ClientID: client.ID, RedirectURI: input.RedirectURI, Refresh: refresh,
	}, now)
	if err != nil {
		return TokenResult{}, mapGrantError(err)
	}
	if !sameGrant(preview, committedGrant) {
		return TokenResult{}, errors.New("authorization code consumption returned a mismatched grant")
	}
	return result, nil
}

// Refresh rotates an active refresh token. If a previously consumed token is presented, the
// repository revokes its complete family before ErrRefreshTokenReplay is returned.
func (service *Service) Refresh(ctx context.Context, input RefreshTokenInput) (TokenResult, error) {
	input = normalizeRefreshInput(input)
	if input.ClientID == "" || input.RefreshToken == "" {
		return TokenResult{}, ErrInvalidRequest
	}
	now := service.now()
	client, err := service.authenticatedClient(ctx, input.ClientAuthentication, now)
	if err != nil {
		return TokenResult{}, err
	}
	if !has(client.GrantTypes, "refresh_token") {
		return TokenResult{}, ErrUnauthorizedClient
	}
	tokenHash := digest(input.RefreshToken)
	current, err := service.repository.FindRefreshToken(ctx, tokenHash, now)
	if err != nil {
		return TokenResult{}, mapGrantError(err)
	}
	if current.OAuthClientID != client.ID || current.TenantID != client.TenantID {
		return TokenResult{}, ErrInvalidGrant
	}
	preview := grantFromRefreshToken(current, client)
	if !validRefreshToken(current, now) {
		// A consumed node is a replay signal. Delegate to the transactional repository so it locks
		// the family and revokes all descendants before returning the protocol-safe error.
		if current.Status == domain.RefreshTokenStatusConsumed || current.UsedAt != nil {
			replayRefresh, _, generateErr := service.newRefreshToken(client, preview, current.ID, now)
			if generateErr != nil {
				return TokenResult{}, generateErr
			}
			if replayRefresh == nil {
				return TokenResult{}, ErrInvalidGrant
			}
			_, replayErr := service.repository.RotateRefreshToken(ctx, RotateRefreshTokenCommand{
				TokenHash: tokenHash, ClientID: client.ID, Refresh: *replayRefresh,
			}, now)
			if errors.Is(replayErr, ErrRefreshTokenReplay) {
				return TokenResult{}, ErrRefreshTokenReplay
			}
			return TokenResult{}, mapGrantError(replayErr)
		}
		return TokenResult{}, ErrInvalidGrant
	}
	refresh, rawRefresh, err := service.newRefreshToken(client, preview, current.ID, now)
	if err != nil {
		return TokenResult{}, err
	}
	result, err := service.issueTokens(ctx, client, preview, rawRefresh, now)
	if err != nil {
		return TokenResult{}, err
	}
	committedGrant, err := service.repository.RotateRefreshToken(ctx, RotateRefreshTokenCommand{
		TokenHash: tokenHash, ClientID: client.ID, Refresh: *refresh,
	}, now)
	if err != nil {
		if errors.Is(err, ErrRefreshTokenReplay) {
			return TokenResult{}, ErrRefreshTokenReplay
		}
		return TokenResult{}, mapGrantError(err)
	}
	if !sameGrant(preview, committedGrant) {
		return TokenResult{}, errors.New("refresh token rotation returned a mismatched grant")
	}
	return result, nil
}

// Revoke persists a token revocation. Refresh-token revocation invalidates the complete family;
// unknown tokens still return success in accordance with RFC 7009 and are stored as access-token
// digests only when the caller explicitly identifies them as access tokens.
func (service *Service) Revoke(ctx context.Context, input RevokeTokenInput) error {
	input = normalizeRevokeInput(input)
	if input.ClientID == "" || input.Token == "" || !oneOf(input.TokenType, domain.TokenTypeAccess, domain.TokenTypeRefresh) || !validProtocolText(input.Reason, 128) {
		return ErrInvalidRequest
	}
	now := service.now()
	client, err := service.authenticatedClient(ctx, input.ClientAuthentication, now)
	if err != nil {
		return err
	}
	var expiresAt *time.Time
	if input.ExpiresAt != nil {
		expires := input.ExpiresAt.UTC()
		expiresAt = &expires
	}
	revocationID, err := service.ids.New(now)
	if err != nil {
		return fmt.Errorf("generate OIDC/OAuth revocation id: %w", err)
	}
	if err := service.repository.RevokeToken(ctx, RevokeTokenCommand{
		RevocationID: revocationID, TenantID: client.TenantID, OAuthClientID: client.ID, TokenHash: digest(input.Token), TokenType: input.TokenType,
		ExpiresAt: expiresAt, Reason: input.Reason,
	}, now); err != nil {
		return fmt.Errorf("revoke OIDC/OAuth token: %w", err)
	}
	return nil
}

// IsAccessTokenRevoked hashes an opaque bearer value locally and reads the tenant-scoped durable
// revocation record. Callers should invoke it after signature and claim validation.
func (service *Service) IsAccessTokenRevoked(ctx context.Context, tenantID, rawAccessToken string) (bool, error) {
	tenantID, rawAccessToken = strings.TrimSpace(tenantID), strings.TrimSpace(rawAccessToken)
	if tenantID == "" || rawAccessToken == "" {
		return false, ErrInvalidRequest
	}
	revoked, err := service.repository.IsTokenRevoked(ctx, tenantID, digest(rawAccessToken), service.now())
	if err != nil {
		return false, fmt.Errorf("look up OIDC/OAuth token revocation: %w", err)
	}
	return revoked, nil
}

// ResolveUserInfo re-resolves a verified user access token against active local session, account,
// user, client and tenant state. It returns only claims allowed by the access token's scopes.
func (service *Service) ResolveUserInfo(ctx context.Context, input UserInfoInput) (UserInfo, error) {
	input = normalizeUserInfoInput(input)
	if input.TenantID == "" || input.OAuthClientID == "" || input.SessionID == "" || input.UserID == "" || !hasSlice(input.Scopes, "openid") {
		return UserInfo{}, ErrInvalidToken
	}
	subject, err := service.repository.ResolveUserInfo(ctx, UserInfoQuery{
		TenantID: input.TenantID, OAuthClientID: input.OAuthClientID, SessionID: input.SessionID, UserID: input.UserID,
	}, service.now())
	if err != nil {
		return UserInfo{}, ErrInvalidToken
	}
	if subject.TenantID != input.TenantID || subject.OAuthClientID != input.OAuthClientID || subject.SessionID != input.SessionID || subject.UserID != input.UserID {
		return UserInfo{}, ErrInvalidToken
	}
	result := UserInfo{Subject: subject.UserID}
	if hasSlice(input.Scopes, "profile") {
		result.Name, result.PreferredUsername = subject.DisplayName, subject.PreferredUsername
	}
	if hasSlice(input.Scopes, "email") {
		result.Email = subject.Email
	}
	return result, nil
}

func (service *Service) authenticatedClient(ctx context.Context, input ClientAuthentication, now time.Time) (domain.Client, error) {
	clientID := strings.TrimSpace(input.ClientID)
	if clientID == "" {
		return domain.Client{}, ErrInvalidClient
	}
	client, err := service.repository.FindClient(ctx, clientID, now)
	if err != nil {
		return domain.Client{}, mapClientError(err)
	}
	switch client.TokenAuthMethod {
	case "none":
		if strings.TrimSpace(input.ClientSecret) != "" || client.ClientType != "public" {
			return domain.Client{}, ErrInvalidClient
		}
	case "client_secret_basic":
		if input.ClientSecret == "" || !verifyClientSecret(client.Credentials, input.ClientSecret, now) {
			return domain.Client{}, ErrInvalidClient
		}
	case "private_key_jwt":
		if input.ClientSecret != "" || authenticatePrivateKeyJWT(ctx, service.repository, client, input, now) != nil {
			return domain.Client{}, ErrInvalidClient
		}
	default:
		return domain.Client{}, ErrInvalidClient
	}
	return client, nil
}

func (service *Service) newRefreshToken(client domain.Client, grant domain.TokenGrant, parentID string, now time.Time) (*NewRefreshToken, string, error) {
	if !has(client.GrantTypes, "refresh_token") || client.RefreshTokenTTLSeconds == 0 {
		return nil, "", nil
	}
	familyID := ""
	if parentID == "" {
		var err error
		familyID, err = service.ids.New(now)
		if err != nil {
			return nil, "", fmt.Errorf("generate refresh token family id: %w", err)
		}
	}
	refreshID, err := service.ids.New(now)
	if err != nil {
		return nil, "", fmt.Errorf("generate refresh token id: %w", err)
	}
	rawRefresh, err := service.secrets.NewSecret()
	if err != nil {
		return nil, "", fmt.Errorf("generate refresh token: %w", err)
	}
	if !validOpaqueSecret(rawRefresh) {
		return nil, "", errors.New("OIDC/OAuth secret generator returned an invalid refresh token")
	}
	expiresAt := now.Add(time.Duration(client.RefreshTokenTTLSeconds) * time.Second)
	return &NewRefreshToken{
		ID: refreshID, TokenFamilyID: familyID, ParentRefreshTokenID: parentID, TokenHash: digest(rawRefresh),
		TenantID: grant.TenantID, OAuthClientID: grant.OAuthClientID, SessionID: grant.SessionID, AccountID: grant.AccountID,
		UserID: grant.UserID, Scopes: append([]string(nil), grant.Scopes...), AuthorizedAt: grant.AuthorizedAt, IssuedAt: now, ExpiresAt: expiresAt,
	}, rawRefresh, nil
}

func (service *Service) issueTokens(ctx context.Context, client domain.Client, grant domain.TokenGrant, rawRefresh string, now time.Time) (TokenResult, error) {
	if client.AccessTokenTTLSeconds == 0 {
		return TokenResult{}, errors.New("OIDC/OAuth client access token TTL must be greater than zero")
	}
	accessTokenID, err := service.ids.New(now)
	if err != nil {
		return TokenResult{}, fmt.Errorf("generate access token id: %w", err)
	}
	ttl := time.Duration(client.AccessTokenTTLSeconds) * time.Second
	issued, err := service.issuer.IssueOIDCTokens(ctx, TokenIssue{
		AccessTokenID: accessTokenID, TenantID: grant.TenantID, OAuthClientID: grant.OAuthClientID, ClientID: client.ClientID,
		SessionID: grant.SessionID, AccountID: grant.AccountID, UserID: grant.UserID, Scopes: append([]string(nil), grant.Scopes...),
		Nonce: grant.Nonce, AuthorizedAt: grant.AuthorizedAt, IssuedAt: now, AccessTokenExpiresAt: now.Add(ttl),
		IssueIDToken: hasSlice(grant.Scopes, "openid"),
	})
	if err != nil {
		return TokenResult{}, fmt.Errorf("issue OIDC/OAuth tokens: %w", err)
	}
	if strings.TrimSpace(issued.AccessToken) == "" || (hasSlice(grant.Scopes, "openid") && strings.TrimSpace(issued.IDToken) == "") {
		return TokenResult{}, errors.New("OIDC/OAuth token issuer returned incomplete tokens")
	}
	return TokenResult{
		AccessToken: issued.AccessToken, TokenType: "Bearer", ExpiresIn: int64(ttl / time.Second), Scope: strings.Join(grant.Scopes, " "),
		IDToken: issued.IDToken, RefreshToken: rawRefresh,
	}, nil
}

func (service *Service) now() time.Time { return service.clock.Now().UTC() }

func digest(value string) [sha256.Size]byte { return sha256.Sum256([]byte(value)) }

func grantFromAuthorizationCode(code domain.AuthorizationCode, client domain.Client) domain.TokenGrant {
	return domain.TokenGrant{
		TenantID: code.TenantID, OAuthClientID: code.OAuthClientID, ClientID: client.ClientID, SessionID: code.SessionID,
		AccountID: code.AccountID, UserID: code.UserID, Scopes: append([]string(nil), code.Scopes...), Nonce: code.Nonce, AuthorizedAt: code.CreatedAt,
	}
}

func grantFromRefreshToken(token domain.RefreshToken, client domain.Client) domain.TokenGrant {
	return domain.TokenGrant{
		TenantID: token.TenantID, OAuthClientID: token.OAuthClientID, ClientID: client.ClientID, SessionID: token.SessionID,
		AccountID: token.AccountID, UserID: token.UserID, Scopes: append([]string(nil), token.Scopes...), AuthorizedAt: token.AuthorizedAt,
	}
}

func sameGrant(left, right domain.TokenGrant) bool {
	if left.TenantID != right.TenantID || left.OAuthClientID != right.OAuthClientID || left.ClientID != right.ClientID ||
		left.SessionID != right.SessionID || left.AccountID != right.AccountID || left.UserID != right.UserID || left.Nonce != right.Nonce ||
		!left.AuthorizedAt.Equal(right.AuthorizedAt) || len(left.Scopes) != len(right.Scopes) {
		return false
	}
	for index := range left.Scopes {
		if left.Scopes[index] != right.Scopes[index] {
			return false
		}
	}
	return true
}

func verifyClientSecret(credentials []domain.ClientCredential, secret string, now time.Time) bool {
	for _, credential := range credentials {
		if len(credential.SecretHash) == 0 || (credential.ValidUntil != nil && !credential.ValidUntil.After(now)) {
			continue
		}
		if bcrypt.CompareHashAndPassword(credential.SecretHash, []byte(secret)) == nil {
			return true
		}
	}
	return false
}

func registeredScopes(client domain.Client, requested []string) ([]string, error) {
	scopes := normalizeScopes(requested)
	if len(scopes) == 0 {
		for scope := range client.Scopes {
			scopes = append(scopes, scope)
		}
		sort.Strings(scopes)
	}
	for _, scope := range scopes {
		if _, allowed := client.Scopes[scope]; !allowed {
			return nil, ErrInvalidScope
		}
	}
	return scopes, nil
}

func validAuthorizationCode(code domain.AuthorizationCode, client domain.Client, redirectURI string, now time.Time) bool {
	return code.TenantID == client.TenantID && code.OAuthClientID == client.ID && code.RedirectURI == redirectURI &&
		code.Status == domain.AuthorizationCodeStatusActive && code.ConsumedAt == nil && code.ExpiresAt.After(now) &&
		code.SessionID != "" && code.AccountID != "" && code.UserID != ""
}

func validRefreshToken(token domain.RefreshToken, now time.Time) bool {
	return token.Status == domain.RefreshTokenStatusActive && token.UsedAt == nil && token.RevokedAt == nil && token.ExpiresAt.After(now) &&
		token.SessionID != "" && token.AccountID != "" && token.UserID != ""
}

func mapClientError(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrInvalidClient
	}
	return err
}

func mapGrantError(err error) error {
	if errors.Is(err, ErrRefreshTokenReplay) {
		return ErrRefreshTokenReplay
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrAuthorizationCodeUnavailable) {
		return ErrInvalidGrant
	}
	return err
}

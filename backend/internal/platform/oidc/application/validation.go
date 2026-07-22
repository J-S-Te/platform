package application

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"regexp"
	"sort"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/domain"
)

var (
	scopePattern         = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,128}$`)
	pkceVerifierPattern  = regexp.MustCompile(`^[A-Za-z0-9\-._~]{43,128}$`)
	pkceChallengePattern = regexp.MustCompile(`^[A-Za-z0-9\-._~]{43,128}$`)
)

func normalizeAuthorizationInput(input AuthorizationInput) AuthorizationInput {
	input.ClientID, input.RedirectURI, input.State, input.Nonce, input.CodeChallenge, input.CodeChallengeMethod, input.SessionID =
		strings.TrimSpace(input.ClientID), strings.TrimSpace(input.RedirectURI), strings.TrimSpace(input.State), strings.TrimSpace(input.Nonce),
		strings.TrimSpace(input.CodeChallenge), strings.TrimSpace(input.CodeChallengeMethod), strings.TrimSpace(input.SessionID)
	input.Scopes = normalizeScopes(input.Scopes)
	return input
}

func normalizeCodeExchangeInput(input AuthorizationCodeExchangeInput) AuthorizationCodeExchangeInput {
	input.ClientID, input.Code, input.RedirectURI, input.CodeVerifier = strings.TrimSpace(input.ClientID), strings.TrimSpace(input.Code), strings.TrimSpace(input.RedirectURI), strings.TrimSpace(input.CodeVerifier)
	return input
}

func normalizeRefreshInput(input RefreshTokenInput) RefreshTokenInput {
	input.ClientID, input.RefreshToken = strings.TrimSpace(input.ClientID), strings.TrimSpace(input.RefreshToken)
	return input
}

func normalizeRevokeInput(input RevokeTokenInput) RevokeTokenInput {
	input.ClientID, input.Token, input.TokenType, input.Reason = strings.TrimSpace(input.ClientID), strings.TrimSpace(input.Token), strings.TrimSpace(input.TokenType), strings.TrimSpace(input.Reason)
	return input
}

func normalizeUserInfoInput(input UserInfoInput) UserInfoInput {
	input.TenantID, input.OAuthClientID, input.SessionID, input.UserID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.SessionID), strings.TrimSpace(input.UserID)
	input.Scopes = normalizeScopes(input.Scopes)
	return input
}

func normalizeScopes(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func validatePKCE(required bool, challenge, method string) (string, string, error) {
	if challenge == "" && method == "" {
		if required {
			return "", "", ErrInvalidRequest
		}
		return "", "", nil
	}
	if challenge == "" || !pkceChallengePattern.MatchString(challenge) || !oneOf(method, "S256", "plain") {
		return "", "", ErrInvalidRequest
	}
	return challenge, method, nil
}

func verifyPKCE(challenge, method, verifier string) bool {
	if challenge == "" && method == "" {
		return verifier == ""
	}
	if !pkceVerifierPattern.MatchString(verifier) {
		return false
	}
	var candidate string
	switch method {
	case "S256":
		hash := sha256.Sum256([]byte(verifier))
		candidate = base64.RawURLEncoding.EncodeToString(hash[:])
	case "plain":
		candidate = verifier
	default:
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(challenge)) == 1
}

func validProtocolText(value string, maximum int) bool {
	if len(value) > maximum {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validOpaqueSecret(value string) bool {
	return len(value) >= 43 && len(value) <= 512 && validProtocolText(value, 512)
}

func has(values map[string]struct{}, wanted string) bool {
	_, exists := values[wanted]
	return exists
}

func hasSlice(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func tokenTypeIsKnown(value string) bool {
	return value == domain.TokenTypeAccess || value == domain.TokenTypeRefresh
}

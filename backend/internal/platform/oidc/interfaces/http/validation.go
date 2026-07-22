package oidchttp

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
)

var (
	scopePattern         = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,128}$`)
	pkceChallengePattern = regexp.MustCompile(`^[A-Za-z0-9\-._~]{43,128}$`)
)

func validateScopeParameter(raw string) error {
	if len(raw) > 4096 {
		return errors.New("scope is too long")
	}
	seen := map[string]struct{}{}
	for _, scope := range strings.Fields(raw) {
		if !scopePattern.MatchString(scope) {
			return errors.New("scope is invalid")
		}
		if _, exists := seen[scope]; exists {
			return errors.New("scope is duplicated")
		}
		seen[scope] = struct{}{}
	}
	if strings.TrimSpace(raw) != raw && raw != "" {
		return errors.New("scope contains surrounding whitespace")
	}
	return nil
}

func validateTextParameter(value string, maximum int) error {
	if len(value) > maximum {
		return errors.New("parameter is too long")
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return errors.New("parameter contains a control character")
		}
	}
	return nil
}

func validProtocolParameter(value string, maximum int) bool {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsSpace(character) || character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validPKCEChallenge(value string) bool {
	return value == "" || pkceChallengePattern.MatchString(value)
}

func validatePrompt(raw string) error {
	if raw == "" {
		return nil
	}
	seen := map[string]struct{}{}
	for _, value := range strings.Fields(raw) {
		switch value {
		case "none", "login", "consent", "select_account":
		default:
			return errors.New("prompt value is unsupported")
		}
		if _, exists := seen[value]; exists {
			return errors.New("prompt value is duplicated")
		}
		seen[value] = struct{}{}
	}
	if _, hasNone := seen["none"]; hasNone && len(seen) != 1 {
		return errors.New("prompt=none must be used alone")
	}
	return nil
}

package filetaskhttp

import (
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

func TestBindingOperatorUsesAuthenticatedApplicationForMachineClient(t *testing.T) {
	principal := authctx.Principal{Account: authctx.ReferenceName{ID: "application-1"}}
	if got := bindingOperator(principal); got != "application-1" {
		t.Fatalf("binding operator = %q, want authenticated application", got)
	}
}

func TestBindingOperatorPrefersAuthenticatedUser(t *testing.T) {
	principal := authctx.Principal{
		User:    authctx.ReferenceName{ID: "user-1"},
		Account: authctx.ReferenceName{ID: "application-1"},
	}
	if got := bindingOperator(principal); got != "user-1" {
		t.Fatalf("binding operator = %q, want authenticated user", got)
	}
}

package identityhttp

import (
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

func TestPrincipalHasRoleUsesVerifiedRoleCode(t *testing.T) {
	principal := authctx.Principal{Roles: []authctx.ReferenceName{
		{ID: "role-1", Name: "平台超级管理员", Code: "platform-super-admin"},
	}}
	if !principalHasRole(principal, "platform-super-admin") {
		t.Fatal("expected verified super administrator role to be recognized")
	}
	if principalHasRole(authctx.Principal{}, "platform-super-admin") {
		t.Fatal("principal without roles must not receive direct scheduling authority")
	}
}

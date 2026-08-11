package personneldirectory

import (
	"context"
	"testing"

	oidchttp "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/http"
	"gorm.io/gorm"
)

func TestNewRejectsMissingDatabase(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) error = nil")
	}
}

func TestListActivePersonnelRejectsMissingTenant(t *testing.T) {
	resolver, err := New(&gorm.DB{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := resolver.ListActivePersonnel(context.Background(), " ", "client-1"); err == nil {
		t.Fatal("ListActivePersonnel() error = nil")
	}
}

func TestAttachRolesDeduplicatesAndSortsEffectiveRoleCodes(t *testing.T) {
	entries := []oidchttp.PersonnelDirectoryEntry{
		{UserID: "user-1", DisplayName: "章六"},
		{UserID: "user-2", DisplayName: "蔡总"},
	}
	attachRoles(entries, []roleRow{
		{UserID: "user-1", RoleCode: "tech_director"},
		{UserID: "user-1", RoleCode: "sales_director"},
		{UserID: "user-1", RoleCode: "tech_director"},
		{UserID: "missing-user", RoleCode: "admin"},
		{UserID: "user-2", RoleCode: ""},
	})

	if len(entries[0].Roles) != 2 || entries[0].Roles[0] != "sales_director" || entries[0].Roles[1] != "tech_director" {
		t.Fatalf("user-1 roles = %#v", entries[0].Roles)
	}
	if len(entries[1].Roles) != 0 {
		t.Fatalf("user-2 roles = %#v", entries[1].Roles)
	}
}

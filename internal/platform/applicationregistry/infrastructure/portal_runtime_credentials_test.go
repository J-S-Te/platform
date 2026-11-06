package infrastructure

import (
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// 门户运行配置里的机器凭据必须逐键等于本次受控交付；沿用历史值必须失败关闭。
func TestVerifyPortalDeliveredCredentialsRejectsStaleOrMissingValues(t *testing.T) {
	input := application.SubsystemProvisioningInput{ServiceCredentials: []application.SubsystemServiceCredential{
		{Purpose: application.ServiceCredentialAuditIngest, OAuthClient: application.OAuthClientView{ClientID: "customer_portal-dev-audit-publisher"}, PlaintextSecret: "fresh-audit"},
		{Purpose: application.ServiceCredentialPortalInviteVerify, OAuthClient: application.OAuthClientView{ClientID: "customer_portal-dev-portal-invite-verify"}, PlaintextSecret: "fresh-invite"},
	}}
	values := map[string]string{
		"PLATFORM_AUDIT_CLIENT_ID":        "customer_portal-dev-audit-publisher",
		"PLATFORM_AUDIT_CLIENT_SECRET":    "fresh-audit",
		"PORTAL_CRM_INVITE_CLIENT_ID":     "customer_portal-dev-portal-invite-verify",
		"PORTAL_CRM_INVITE_CLIENT_SECRET": "fresh-invite",
	}
	if err := verifyPortalDeliveredCredentials(input, values); err != nil {
		t.Fatalf("本次交付应通过校验: %v", err)
	}

	stale := map[string]string{}
	for key, value := range values {
		stale[key] = value
	}
	stale["PLATFORM_AUDIT_CLIENT_SECRET"] = "stale-audit"
	if err := verifyPortalDeliveredCredentials(input, stale); err == nil {
		t.Fatal("沿用历史 Secret 必须失败关闭")
	}

	mismatchedID := map[string]string{}
	for key, value := range values {
		mismatchedID[key] = value
	}
	mismatchedID["PORTAL_CRM_INVITE_CLIENT_ID"] = "other-client"
	if err := verifyPortalDeliveredCredentials(input, mismatchedID); err == nil {
		t.Fatal("client_id 不一致必须失败关闭")
	}

	// 本次未交付该用途时保持既有行为（由上游用途完整性校验负责），不在此处误杀。
	if err := verifyPortalDeliveredCredentials(application.SubsystemProvisioningInput{}, values); err != nil {
		t.Fatalf("未交付用途应跳过校验: %v", err)
	}

	noSecret := application.SubsystemProvisioningInput{ServiceCredentials: []application.SubsystemServiceCredential{
		{Purpose: application.ServiceCredentialAuditIngest, OAuthClient: application.OAuthClientView{ClientID: "customer_portal-dev-audit-publisher"}},
		{Purpose: application.ServiceCredentialPortalInviteVerify, OAuthClient: application.OAuthClientView{ClientID: "customer_portal-dev-portal-invite-verify"}, PlaintextSecret: "fresh-invite"},
	}}
	if err := verifyPortalDeliveredCredentials(noSecret, values); err != nil {
		t.Fatalf("未携带明文的用途应跳过校验: %v", err)
	}
}

package main

import (
	"strings"
	"testing"
)

func TestSettlementEnvironmentContainsRequiredRuntimeKeys(t *testing.T) {
	content := settlementEnvironment("application-1", "client-1", "secret-1")
	for _, expected := range []string{
		"SETTLEMENT_FILE_GATEWAY_APPLICATION_ID=application-1",
		"FILE_GATEWAY_URL=http://file-gateway:8086",
		"FILE_GATEWAY_TOKEN_URL=http://platform-api:8080/oauth2/token",
		"FILE_GATEWAY_CLIENT_ID=client-1",
		"FILE_GATEWAY_CLIENT_SECRET=secret-1",
		"FILE_GATEWAY_SCOPE=platform:file:upload platform:file:bind platform:file:download",
	} {
		if !strings.Contains(content, expected+"\n") {
			t.Fatalf("generated environment is missing %q", expected)
		}
	}
}

func TestIAMImportEnvironmentDoesNotContainSettlementKeys(t *testing.T) {
	content := iamImportEnvironment("client-1", "secret-1")
	if strings.Contains(content, "SETTLEMENT_") || strings.Contains(content, "FILE_GATEWAY_URL=") {
		t.Fatalf("IAM import environment contains unrelated settlement keys: %q", content)
	}
}

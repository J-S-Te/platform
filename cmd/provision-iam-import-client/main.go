package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	registryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	registryinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/database"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	db, err := database.OpenMySQL(cfg.MySQL)
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close(db)

	repository, err := registryinfrastructure.NewOAuthClientManagementRepository(db)
	if err != nil {
		log.Fatal(err)
	}
	service, err := registryapplication.NewOAuthClientManagementService(repository, ulid.Generator{}, registryapplication.SystemClock{}, registryapplication.RedirectURIValidationPolicy{})
	if err != nil {
		log.Fatal(err)
	}

	tenantID := required("IAM_IMPORT_PROVISION_TENANT_ID")
	applicationID := required("IAM_IMPORT_PROVISION_APPLICATION_ID")
	environmentID := required("IAM_IMPORT_PROVISION_ENVIRONMENT_ID")
	clientID := required("IAM_IMPORT_PROVISION_CLIENT_ID")
	outputPath := required("IAM_IMPORT_PROVISION_OUTPUT_PATH")
	result, err := service.CreateOAuthClient(context.Background(), registryapplication.OAuthClientCreateInput{
		TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: "system-file-gateway",
		ClientID: clientID, ClientName: "平台人员导入文件网关", ClientType: "service", TokenAuthMethod: "client_secret_basic",
		AccessTokenTTLSeconds: 900, RefreshTokenTTLSeconds: 0, RequirePKCE: false,
		GrantTypes: []string{"client_credentials"}, Scopes: []string{"platform:file:upload", "platform:file:bind"},
	})
	if err != nil {
		log.Fatal(err)
	}

	content := fmt.Sprintf("IAM_IMPORT_FILE_GATEWAY_BASE_URL=http://file-gateway:8086\nIAM_IMPORT_FILE_GATEWAY_TOKEN_URL=http://platform-api:8080/oauth2/token\nIAM_IMPORT_FILE_GATEWAY_CLIENT_ID=%s\nIAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET=%s\n", clientID, result.PlaintextSecret)
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	if _, err = file.WriteString(content); err != nil {
		_ = file.Close()
		log.Fatal(err)
	}
	if err = file.Close(); err != nil {
		log.Fatal(err)
	}
	log.Printf("IAM import client provisioned; credentials written to %s", outputPath)
}

func required(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}

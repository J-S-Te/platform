package main

import (
	"context"
	"errors"
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

	kind := optional("FILE_GATEWAY_PROVISION_KIND", "iam-import")
	tenantID := requiredFor(kind, "TENANT_ID")
	applicationID := requiredFor(kind, "APPLICATION_ID")
	environmentID := requiredFor(kind, "ENVIRONMENT_ID")
	clientID := requiredFor(kind, "CLIENT_ID")
	outputPath := requiredFor(kind, "OUTPUT_PATH")
	clientName, scopes := "平台人员导入文件网关", []string{"platform:file:upload", "platform:file:bind"}
	if kind == "settlement" {
		clientName = "结算系统文件网关"
		scopes = append(scopes, "platform:file:download")
	} else if kind != "iam-import" {
		log.Fatalf("unsupported FILE_GATEWAY_PROVISION_KIND %q", kind)
	}

	ctx := context.Background()
	plaintextSecret := ""
	existing, lookupErr := service.GetOAuthClientByClientID(ctx, tenantID, clientID)
	if lookupErr == nil {
		if existing.ApplicationID != applicationID || existing.EnvironmentID != environmentID || existing.Status != "ACTIVE" {
			log.Fatal("existing OAuth client does not match the requested active application environment")
		}
		secret, secretErr := service.CreateOAuthClientSecret(ctx, registryapplication.OAuthClientSecretCreateInput{
			TenantID: tenantID, OAuthClientID: existing.ID, OperatorID: "system-file-gateway",
		})
		if secretErr != nil {
			log.Fatal(secretErr)
		}
		plaintextSecret = secret.PlaintextSecret
	} else if !errors.Is(lookupErr, registryapplication.ErrManagementNotFound) {
		log.Fatal(lookupErr)
	} else {
		result, createErr := service.CreateOAuthClient(ctx, registryapplication.OAuthClientCreateInput{
			TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: "system-file-gateway",
			ClientID: clientID, ClientName: clientName, ClientType: "service", TokenAuthMethod: "client_secret_basic",
			AccessTokenTTLSeconds: 900, RefreshTokenTTLSeconds: 0, RequirePKCE: false,
			GrantTypes: []string{"client_credentials"}, Scopes: scopes,
		})
		if createErr != nil {
			log.Fatal(createErr)
		}
		plaintextSecret = result.PlaintextSecret
	}

	content := iamImportEnvironment(clientID, plaintextSecret)
	if kind == "settlement" {
		content = settlementEnvironment(applicationID, clientID, plaintextSecret)
	}
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
	log.Printf("%s file gateway client provisioned; credentials written to %s", kind, outputPath)
}

func requiredFor(kind, suffix string) string {
	prefix := "IAM_IMPORT_PROVISION_"
	if kind != "iam-import" {
		prefix = "FILE_GATEWAY_PROVISION_"
	}
	name := prefix + suffix
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}

func optional(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func iamImportEnvironment(clientID, secret string) string {
	return fmt.Sprintf("IAM_IMPORT_FILE_GATEWAY_BASE_URL=http://file-gateway:8086\nIAM_IMPORT_FILE_GATEWAY_TOKEN_URL=http://platform-api:8080/oauth2/token\nIAM_IMPORT_FILE_GATEWAY_CLIENT_ID=%s\nIAM_IMPORT_FILE_GATEWAY_CLIENT_SECRET=%s\n", clientID, secret)
}

func settlementEnvironment(applicationID, clientID, secret string) string {
	return fmt.Sprintf("SETTLEMENT_FILE_GATEWAY_APPLICATION_ID=%s\nFILE_GATEWAY_URL=http://file-gateway:8086\nFILE_GATEWAY_TOKEN_URL=http://platform-api:8080/oauth2/token\nFILE_GATEWAY_CLIENT_ID=%s\nFILE_GATEWAY_CLIENT_SECRET=%s\nFILE_GATEWAY_SCOPE=platform:file:upload platform:file:bind platform:file:download\n", applicationID, clientID, secret)
}

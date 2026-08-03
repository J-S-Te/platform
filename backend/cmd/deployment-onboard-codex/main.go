// Command deployment-onboard-codex 先在数据库事务内完成受控本地 Docker 接入，再把一次性 OAuth
// Secret 写入调用方指定文件；文件写入失败不会回滚已完成的数据库接入。Secret 不输出到标准输出或日志，
// 调用方还需保证目标文件不存在或预先收紧权限，因为覆盖已有文件时 0600 参数不会改变原文件权限。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	application "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	infrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/infrastructure"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/database"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
)

type output struct {
	ApplicationID string `json:"application_id"`
	EnvironmentID string `json:"environment_id"`
	OAuthClientID string `json:"oauth_client_record_id"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	RedirectURI   string `json:"redirect_uri"`
	PublicURL     string `json:"public_url"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "subsystem onboarding failed:", err)
		os.Exit(1)
	}
	fmt.Println("subsystem onboarding completed")
}

func run() error {
	tenantID := strings.TrimSpace(os.Getenv("ONBOARD_TENANT_ID"))
	operatorID := strings.TrimSpace(os.Getenv("ONBOARD_OPERATOR_ID"))
	outputPath := strings.TrimSpace(os.Getenv("ONBOARD_OUTPUT_FILE"))
	if tenantID == "" || operatorID == "" || outputPath == "" {
		return errors.New("ONBOARD_TENANT_ID, ONBOARD_OPERATOR_ID and ONBOARD_OUTPUT_FILE are required")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	db, err := database.OpenMySQL(cfg.MySQL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = database.Close(db) }()

	repository, err := infrastructure.NewSubsystemOnboardingGORMRepository(db)
	if err != nil {
		return err
	}
	service, err := application.NewSubsystemOnboardingService(
		repository, ulid.Generator{}, application.SystemClock{},
		application.RedirectURIValidationPolicy{
			AllowInsecureHTTP: cfg.Auth.OAuthClientAllowInsecureHTTPRedirectURIs,
		},
	)
	if err != nil {
		return err
	}

	description := "合同台账、审批、签署、归档与统计分析"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := service.OnboardSubsystem(ctx, application.SubsystemOnboardingInput{
		TenantID: tenantID, OperatorID: operatorID,
		ApplicationCode: "contract_management", ApplicationName: "合同管理系统", Description: &description,
		Environment: "dev", PublicBaseURL: "http://localhost:8081", UpstreamURL: "http://contract-api:8081",
		PathPrefix: "/contract_management", ClientType: "confidential",
	})
	if err != nil {
		return err
	}

	payload, err := json.MarshalIndent(output{
		ApplicationID: result.Application.ID, EnvironmentID: result.Environment.ID,
		OAuthClientID: result.OAuthClient.ID, ClientID: result.OAuthClient.ClientID,
		ClientSecret: result.PlaintextSecret, RedirectURI: result.RedirectURI, PublicURL: result.PublicURL,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode onboarding output: %w", err)
	}
	// 文件包含不可再次查询的明文 Secret；权限在创建时一次性设定，调用方负责选择受保护路径。
	if err := os.WriteFile(outputPath, append(payload, '\n'), 0o600); err != nil {
		return fmt.Errorf("write onboarding output: %w", err)
	}
	return nil
}

// Command reset-admin-password-codex 用生产仓储事务执行一次性本地管理员凭据恢复，
// 使密码版本更新、旧会话撤销和审计语义与正常重置流程保持一致。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	identityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	identityinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/infrastructure"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/database"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/security"
)

const maximumPasswordInputBytes = 4096

type accountRow struct {
	ID          string  `gorm:"column:id"`
	TenantID    string  `gorm:"column:tenant_id"`
	UserID      *string `gorm:"column:user_id"`
	Username    *string `gorm:"column:username"`
	Status      string  `gorm:"column:status"`
	AccountType string  `gorm:"column:account_type"`
	AuthSource  string  `gorm:"column:auth_source"`
	Version     uint64  `gorm:"column:version"`
}

func main() {
	if err := run(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "administrator password reset failed:", err)
		os.Exit(1)
	}
	fmt.Println("administrator password reset and credential verification succeeded")
}

func run(input io.Reader) error {
	password, err := readPassword(input)
	if err != nil {
		return err
	}
	defer func() { password = "" }()
	if err := validateStrongPassword(password); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := database.OpenMySQL(cfg.MySQL)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close(db) }()

	repository, err := identityinfrastructure.NewGORMRepository(db)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var accounts []accountRow
	// 最多读取两条即可证明账号名不唯一；恢复工具拒绝猜测租户或选择任意账号。
	result := db.WithContext(ctx).
		Table("iam_account").
		Where("username = ?", "admin").
		Limit(2).
		Find(&accounts)
	if result.Error != nil {
		return fmt.Errorf("find administrator account: %w", result.Error)
	}
	if len(accounts) != 1 {
		return fmt.Errorf("expected exactly one admin account, found %d", len(accounts))
	}
	account := accounts[0]
	if account.UserID == nil || strings.TrimSpace(*account.UserID) == "" {
		return errors.New("admin account is not linked to a user")
	}
	if account.AccountType != "HUMAN" || account.AuthSource != "LOCAL" {
		return errors.New("admin account is not a local human account")
	}
	if account.Version == 0 {
		return errors.New("admin account version is invalid")
	}

	digest, metadata, err := (security.Argon2idPasswordHasher{}).Hash(password)
	if err != nil {
		return fmt.Errorf("hash administrator password: %w", err)
	}
	_, err = repository.ResetPassword(ctx, identityapplication.PasswordWrite{
		TenantID:        account.TenantID,
		AccountID:       account.ID,
		OperatorID:      strings.TrimSpace(*account.UserID),
		ExpectedVersion: account.Version,
		PasswordDigest:  digest,
		AlgorithmParams: metadata,
		OccurredAt:      time.Now().UTC(),
		RevokeReason:    "ADMIN_PASSWORD_RECOVERY",
	})
	if err != nil {
		return fmt.Errorf("reset administrator password: %w", err)
	}

	// 写入后立即从仓储重读并验证，避免工具在事务未真正更新目标凭据时误报恢复成功。
	credential, err := repository.FindLocalPasswordCredential(ctx, account.TenantID, account.ID)
	if err != nil {
		return fmt.Errorf("read updated credential: %w", err)
	}
	matched, err := security.VerifyPassword(password, credential.HashAlgorithm, credential.PasswordHash, credential.AlgorithmParams)
	if err != nil {
		return fmt.Errorf("verify updated credential: %w", err)
	}
	if !matched {
		return errors.New("updated credential does not match supplied password")
	}
	return nil
}

func readPassword(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("password standard input is unavailable")
	}
	// 标准输入避免密码暴露在 shell 历史和进程参数；多读一字节用于可靠识别超长输入。
	content, err := io.ReadAll(io.LimitReader(reader, maximumPasswordInputBytes+1))
	if err != nil {
		return "", fmt.Errorf("read password from standard input: %w", err)
	}
	if len(content) > maximumPasswordInputBytes {
		return "", fmt.Errorf("password exceeds %d bytes", maximumPasswordInputBytes)
	}
	password := strings.TrimSuffix(string(content), "\n")
	password = strings.TrimSuffix(password, "\r")
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	return password, nil
}

func validateStrongPassword(password string) error {
	length := len([]rune(password))
	if length < 12 || length > 128 {
		return errors.New("password must contain 12 to 128 characters")
	}
	var upper, lower, digit, symbol bool
	for _, character := range password {
		if unicode.IsSpace(character) {
			return errors.New("password must not contain whitespace")
		}
		switch {
		case unicode.IsUpper(character):
			upper = true
		case unicode.IsLower(character):
			lower = true
		case unicode.IsDigit(character):
			digit = true
		default:
			symbol = true
		}
	}
	if !upper || !lower || !digit || !symbol {
		return errors.New("password must contain uppercase, lowercase, digit and symbol characters")
	}
	return nil
}

// Command bootstrap-admin 在迁移完成后初始化首个平台超级管理员。它与公开 HTTP 启动接口分离，
// 使原生部署可以通过受保护的标准输入完成一次性初始化，而无需暴露网络设置入口。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	identityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	identityinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/database"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

const (
	firstSuperAdminNotInitializedExitCode = 3
	maxBootstrapPasswordBytes             = 4096
)

var errFirstSuperAdminNotInitialized = errors.New("first super administrator is not initialized")

type commandOptions struct {
	statusOnly    bool
	displayName   string
	accountName   string
	passwordSTDIN bool
}

func main() {
	if err := run(os.Args[1:], os.Stdin); err != nil {
		if errors.Is(err, errFirstSuperAdminNotInitialized) {
			os.Exit(firstSuperAdminNotInitializedExitCode)
		}
		slog.Error("first super administrator bootstrap stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdin io.Reader) error {
	options, err := parseOptions(arguments)
	if err != nil {
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

	initialized, err := repository.FirstSuperAdminInitialized(ctx)
	if err != nil {
		return err
	}
	if options.statusOnly {
		if !initialized {
			return errFirstSuperAdminNotInitialized
		}
		slog.Info("first super administrator is already initialized")
		return nil
	}
	if initialized {
		// 快速检查只减少重复工作，真正的并发唯一性仍由仓储事务和数据库约束保证。
		slog.Info("first super administrator is already initialized; bootstrap was skipped")
		return nil
	}

	password, err := readPassword(stdin, options.passwordSTDIN)
	if err != nil {
		return err
	}
	defer clearString(&password)

	service, err := identityapplication.NewBootstrapService(
		repository,
		security.Argon2idPasswordHasher{},
		ulid.Generator{},
		identityapplication.SystemClock{},
	)
	if err != nil {
		return err
	}

	result, err := service.InitializeFirstSuperAdmin(ctx, identityapplication.BootstrapInput{
		DisplayName: options.displayName,
		AccountName: options.accountName,
		Password:    password,
	})
	if errors.Is(err, identityapplication.ErrBootstrapAlreadyInitialized) {
		// 两个发布任务并发通过前置检查时，只有事务获胜者写入；失败者视为幂等成功。
		slog.Info("first super administrator is already initialized; concurrent bootstrap was skipped")
		return nil
	}
	if err != nil {
		return err
	}

	slog.Info(
		"first super administrator initialized",
		"user_id", result.UserID,
		"account_id", result.AccountID,
		"role_code", result.RoleCode,
	)
	return nil
}

func parseOptions(arguments []string) (commandOptions, error) {
	flags := flag.NewFlagSet("bootstrap-admin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var options commandOptions
	flags.BoolVar(&options.statusOnly, "status", false, "check whether the first super administrator is initialized")
	flags.StringVar(&options.displayName, "display-name", "", "first super administrator display name")
	flags.StringVar(&options.accountName, "account-name", "", "first super administrator account name")
	flags.BoolVar(&options.passwordSTDIN, "password-stdin", false, "read the first super administrator password from standard input")
	if err := flags.Parse(arguments); err != nil {
		return commandOptions{}, fmt.Errorf("parse command arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return commandOptions{}, errors.New("bootstrap-admin does not accept positional arguments")
	}
	if options.statusOnly {
		if options.displayName != "" || options.accountName != "" || options.passwordSTDIN {
			return commandOptions{}, errors.New("--status cannot be combined with initialization arguments")
		}
		return options, nil
	}
	if strings.TrimSpace(options.displayName) == "" || strings.TrimSpace(options.accountName) == "" || !options.passwordSTDIN {
		return commandOptions{}, errors.New("--display-name, --account-name and --password-stdin are required for initialization")
	}
	return options, nil
}

func readPassword(reader io.Reader, enabled bool) (string, error) {
	if !enabled {
		return "", errors.New("password must be supplied through --password-stdin")
	}
	if reader == nil {
		return "", errors.New("password standard input is unavailable")
	}

	// 多读一个字节才能区分“恰好达到上限”和“输入被截断”，且密码不会进入命令行参数。
	content, err := io.ReadAll(io.LimitReader(reader, maxBootstrapPasswordBytes+1))
	if err != nil {
		return "", fmt.Errorf("read password from standard input: %w", err)
	}
	if len(content) > maxBootstrapPasswordBytes {
		return "", fmt.Errorf("password from standard input exceeds %d bytes", maxBootstrapPasswordBytes)
	}
	password := strings.TrimSuffix(string(content), "\n")
	password = strings.TrimSuffix(password, "\r")
	if password == "" {
		return "", errors.New("password from standard input must not be empty")
	}
	return password, nil
}

func clearString(value *string) {
	if value == nil {
		return
	}
	// Go 字符串不可原地擦除，此操作只能缩短当前引用的生命周期，不能保证底层字节立即清零。
	*value = ""
}

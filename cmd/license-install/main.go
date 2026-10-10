// license-install is an explicit host-root maintenance process, not an HTTP
// endpoint. It never prints signing keys, service credentials or raw licenses.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/bootstrap"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	core "github.com/J-S-Te/license-core"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "离线授权安装未完成：", err)
		os.Exit(1)
	}
}

func readLicense(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("必须提供 --file 签名许可证路径")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("无法读取离线许可证")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > core.MaxTokenBytes {
		return "", errors.New("许可证必须是限制大小的常规文件")
	}
	raw, err := io.ReadAll(io.LimitReader(f, core.MaxTokenBytes+1))
	if err != nil || len(raw) > core.MaxTokenBytes || len(raw) == 0 {
		return "", errors.New("许可证为空、超限或读取失败")
	}
	return strings.TrimSpace(string(raw)), nil
}

func run(args []string) error {
	if len(args) == 0 || (args[0] != "import" && args[0] != "activate" && args[0] != "prepare" && args[0] != "migrate") {
		return errors.New("用法：license-install prepare --scenario fresh|migrate|platform-only|expand --customer ID [--file PATH]；migrate [--timeout 15m]；import|activate --file PATH")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	file := fs.String("file", "", "受控只读签名许可证路径")
	scenario := fs.String("scenario", "", "安装场景")
	customer := fs.String("customer", "", "安装客户标识")
	timeout := fs.Duration("timeout", 15*time.Minute, "全部组件执行确认等待时间")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *timeout <= 0 || *timeout > time.Hour {
		return errors.New("参数无效，超时必须介于0与1小时之间")
	}
	if args[0] == "prepare" {
		if (*scenario != "fresh" && *scenario != "migrate" && *scenario != "platform-only" && *scenario != "expand") || strings.TrimSpace(*customer) == "" {
			return errors.New("prepare 必须提供有效 --scenario 与 --customer")
		}
	} else if *scenario != "" || *customer != "" {
		return errors.New("--scenario 与 --customer 仅适用于 prepare")
	}
	if args[0] == "migrate" && *file != "" {
		return errors.New("migrate 使用已冻结基线；许可证请先执行 import")
	}
	if os.Geteuid() != 0 {
		return errors.New("离线授权维护命令仅允许部署管理员以root运行")
	}
	var raw string
	var err error
	if *file != "" || args[0] == "import" || args[0] == "activate" {
		raw, err = readLicense(*file)
		if err != nil {
			return err
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Environment != "production" || cfg.Audit.EnvironmentCode != "prod" {
		return errors.New("离线授权维护仅支持production/prod安装")
	}
	api, err := bootstrap.NewAPI(cfg)
	if err != nil {
		return err
	}
	defer api.Close()
	if api.OfflineDelivery == nil {
		return errors.New("离线授权维护依赖未装配")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if args[0] == "prepare" {
		out, err := api.OfflineDelivery.Prepare(ctx, *scenario, *customer, raw)
		if err != nil {
			return fmt.Errorf("安装准备未完成：%w", err)
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if args[0] == "migrate" {
		out, err := api.OfflineDelivery.MigrateExisting(ctx)
		if err != nil {
			return fmt.Errorf("旧安装迁移确认未完成：%w", err)
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if args[0] == "import" {
		out, err := api.OfflineDelivery.Import(ctx, raw)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Status     string `json:"status"`
			Idempotent bool   `json:"idempotent"`
		}{out.Status, out.Idempotent})
	}
	out, err := api.OfflineDelivery.Activate(ctx, raw)
	if err != nil {
		return fmt.Errorf("%w；运行确认未完成，可以重新执行同一命令继续，不能认定授权已生效", err)
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

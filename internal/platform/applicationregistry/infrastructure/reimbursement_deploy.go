package infrastructure

import (
	"context"
	"time"
)

// reimbursementMigrationArguments 仅在本次真实备份已成功后给单次迁移注入批准标记。
// 参数：container为受控容器名，service为固定迁移服务，verified为本次备份结果。
// 返回值：一次性Compose运行参数；备份未核验则返回错误。
func reimbursementMigrationArguments(container, service string, verified bool) ([]string, error) {
	if !verified || service != "reimbursement-migrate" {
		return nil, provisioningError("reimbursement migration requires a verified backup in this operation")
	}
	args := []string{"run"}
	if container != "" {
		args = append(args, "--name", container)
	} else {
		args = append(args, "--rm")
	}
	return append(args, "--no-deps", "-e", "REIMBURSEMENT_MIGRATION_BACKUP_VERIFIED=true", service), nil
}

// backupLocalReimbursement 为本地受控报销迁移生成包含摘要的真实数据库备份。
// 参数：ctx控制备份，run为本次统一Compose执行器。
// 返回值：完整备份命令成功才返回nil，不写永久批准配置。
func backupLocalReimbursement(ctx context.Context, run func(context.Context, ...string) error) error {
	name := "reimbursement-controlled-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".sql"
	return run(ctx, "exec", "-T", "reimbursement-mysql", "sh", "-c", productionDatabaseBackupCommand, "_", name, "/backups")
}

// rebuildIntegratedReimbursementStack 按备份、迁移、独立目录发布、API及Worker顺序启动本地报销。
// 参数：ctx为受控操作上下文。
// 返回值：任何真实步骤失败即中止，保留既有数据和诊断现场。
func (p *LocalDockerSubsystemProvisioner) rebuildIntegratedReimbursementStack(ctx context.Context) error {
	run := p.runIntegratedPlatformCompose
	if err := run(ctx, "up", "-d", "--wait", "--no-deps", "reimbursement-mysql"); err != nil {
		return err
	}
	if err := backupLocalReimbursement(ctx, run); err != nil {
		return err
	}
	args, err := reimbursementMigrationArguments("", "reimbursement-migrate", true)
	if err != nil {
		return err
	}
	if err = run(ctx, args...); err != nil {
		return err
	}
	if err = run(ctx, "run", "--rm", "--no-deps", "--entrypoint", "/app/reimbursement-authz-catalog", "reimbursement-api", "publish"); err != nil {
		return err
	}
	return run(ctx, "up", "-d", "--wait", "--no-deps", "--no-build", "reimbursement-api", "reimbursement-worker")
}

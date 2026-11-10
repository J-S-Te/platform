package infrastructure

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

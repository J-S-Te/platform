package evidence

// MigrationInstallationCollector 只证明评审过的升级前运行时边界。镜像摘要不可变匹配
// 仍然强制；缺失发行标签仅在这个被显式选择的迁移操作中允许。
type MigrationInstallationCollector struct {
	*InstallationCollector
}

func NewMigrationInstallationCollector(r Runner, cfg InstallationConfig) (*MigrationInstallationCollector, error) {
	c, err := newInstallationCollector(r, cfg, true)
	if err != nil {
		return nil, err
	}
	return &MigrationInstallationCollector{InstallationCollector: c}, nil
}

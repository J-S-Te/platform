package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

// 迁移清单与目标发布清单相互独立，安装新版本因此无法静默替换已批准的旧镜像。
const migrationLicenseEvidenceApprovalFile = "license-migration-evidence.json"

type migrationInstallationEvidenceProvider interface {
	CollectMigrationInstallationEvidence(context.Context) (licenseevidence.Report, error)
}

// CollectMigrationInstallationEvidence 通过部署 Agent 采集迁移前旧安装的许可证证据。响应必须
// 声明 scope 为 MIGRATION_INSTALLATION，否则按不可用失败，防止以现行安装证据替代迁移前基线。
func (p *UnixSocketSubsystemProvisioner) CollectMigrationInstallationEvidence(ctx context.Context) (licenseevidence.Report, error) {
	if !p.enabled {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: agent disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := dialProvisioningSocket(ctx, p.socketPath)
	if err != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: connection failed")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	watchProvisioningCancellation(ctx, conn)
	if json.NewEncoder(conn).Encode(subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_MIGRATION_INSTALLATION_EVIDENCE"}) != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: send failed")
	}
	var reply subsystemProvisioningReply
	if json.NewDecoder(io.LimitReader(conn, 256*1024)).Decode(&reply) != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: invalid response")
	}
	if !reply.Success || reply.LicenseEvidence == nil {
		return licenseevidence.Report{}, provisioningRejectionError(reply.Message, "")
	}
	if reply.LicenseEvidence.Scope != "MIGRATION_INSTALLATION" {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: invalid scope")
	}
	return *reply.LicenseEvidence, nil
}

// CollectMigrationInstallationEvidence 固定拒绝本地 Docker 迁移：本地部署没有经批准的迁移
// 边界审批，返回 LICENSE_EVIDENCE_UNSUPPORTED，不伪造迁移前证据。
func (p *LocalDockerSubsystemProvisioner) CollectMigrationInstallationEvidence(context.Context) (licenseevidence.Report, error) {
	return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: local migration approval missing")
}

// CollectMigrationInstallationEvidence 基于独立的 license-migration-evidence.json 审批采集迁移前
// 旧安装的证据。全部目标必须属于同一 deploy 根目录与 Compose 项目，边界混杂即失败关闭；审批文件
// 逐项对照受审清单与迁移配置复核，缺审批或不完整时不执行任何采集。
func (p *ProductionComposeSubsystemProvisioner) CollectMigrationInstallationEvidence(ctx context.Context) (licenseevidence.Report, error) {
	if !p.enabled || len(p.targets) == 0 {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: migration boundary not approved")
	}
	var root, project string
	for _, target := range p.targets {
		if root == "" {
			root, project = target.config.DeployRoot, target.config.ComposeProject
		} else if root != target.config.DeployRoot || project != target.config.ComposeProject {
			return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: mixed migration boundaries")
		}
	}
	path, err := productionRuntimeApprovalPath(root, migrationLicenseEvidenceApprovalFile)
	if err != nil {
		return licenseevidence.Report{}, err
	}
	config, err := loadMigrationInstallationEvidenceApproval(path, project, p.profiles)
	if err != nil {
		return licenseevidence.Report{}, err
	}
	collector, err := licenseevidence.NewMigrationInstallationCollector(licenseevidence.ExecRunner{DockerBinary: p.dockerBinary}, config)
	if err != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: invalid migration inventory")
	}
	return collector.Collect(ctx), nil
}

func loadMigrationInstallationEvidenceApproval(path, project string, profiles []productionSubsystemProfile) (licenseevidence.InstallationConfig, error) {
	fail := func() (licenseevidence.InstallationConfig, error) {
		return licenseevidence.InstallationConfig{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: complete installation approval required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return fail()
	}
	f, err := os.Open(path)
	if err != nil {
		return fail()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 256*1024 {
		return fail()
	}
	b, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil || len(b) > 256*1024 {
		return fail()
	}
	var a licenseEvidenceApproval
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&a) != nil || d.Decode(new(any)) != io.EOF || a.Version != 1 || a.Project != project || !a.InstallationBoundary {
		return fail()
	}
	// 被排除的单元必须在受审部署清单中命名为 infrastructure/init；
	// 仅自称基础设施不足以豁免。
	nonbusiness := reviewedPlatformLicenseEvidenceInfrastructure()
	business := map[string]bool{}
	known := map[string]productionSubsystemProfile{}
	for _, p := range profiles {
		known[p.Manifest.Application.Code+"/"+p.Manifest.Application.Environment] = p
		for _, s := range p.Manifest.Compose.RuntimeServices {
			business[s] = true
		}
		for s := range p.Manifest.Compose.ConditionalRuntimeServices {
			business[s] = true
		}
		for _, s := range p.Manifest.Compose.DependencyServices {
			nonbusiness[s] = true
		}
		for _, s := range p.Manifest.Compose.InitializationServices {
			nonbusiness[s] = true
		}
		if s := p.Manifest.Compose.MigrateService; s != "" {
			nonbusiness[s] = true
		}
		if s := p.Manifest.Compose.CatalogSyncService; s != "" {
			nonbusiness[s] = true
		}
	}
	for _, s := range a.ExcludedServices {
		if !nonbusiness[s] || business[s] {
			return fail()
		}
	}
	checked := map[string]bool{}
	for _, s := range a.Services {
		key := s.Application + "/" + s.Environment
		p, ok := known[key]
		if !ok {
			return fail()
		}
		if !checked[key] {
			if _, err := validateMigrationProfileApproval(a, p); err != nil {
				return fail()
			}
			checked[key] = true
		}
	}
	for _, item := range a.Infrastructure {
		key := item.Application + "/" + item.Environment
		p, ok := known[key]
		if !ok || !business[item.Name] || !licenseevidence.ReviewedControlledBIInfrastructure(item.Application, item.Name) {
			return fail()
		}
		if !checked[key] {
			if _, err := validateMigrationProfileApproval(a, p); err != nil {
				return fail()
			}
			checked[key] = true
		}
	}
	config := licenseevidence.InstallationConfig{Project: project, Services: a.Services, Infrastructure: a.Infrastructure, ExcludedServices: a.ExcludedServices, Timeout: 30 * time.Second}
	if _, err := licenseevidence.NewMigrationInstallationCollector(licenseevidence.ExecRunner{}, config); err != nil {
		return fail()
	}
	return config, nil
}

// 旧版发布可能比目标发布少一些组件，但每个选定的服务仍必须属于这个已批准的
// 应用/环境。
func validateMigrationProfileApproval(a licenseEvidenceApproval, p productionSubsystemProfile) (bool, error) {
	known := map[string]bool{}
	for _, name := range p.Manifest.Compose.RuntimeServices {
		known[name] = true
	}
	for name := range p.Manifest.Compose.ConditionalRuntimeServices {
		known[name] = true
	}
	for _, s := range a.Services {
		if s.Application == p.Manifest.Application.Code && s.Environment == p.Manifest.Application.Environment {
			if !known[s.Name] || licenseevidence.ReviewedControlledBIInfrastructure(s.Application, s.Name) {
				return false, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: unapproved legacy runtime service")
			}
		}
	}
	return true, nil
}

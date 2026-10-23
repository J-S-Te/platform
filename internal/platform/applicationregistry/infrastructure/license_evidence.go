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

type licenseEvidenceProvider interface {
	CollectLicenseEvidence(context.Context, string, string) (licenseevidence.Report, error)
}

// CollectLicenseEvidence 通过部署 Agent 采集指定应用/环境的许可证证据，属于固定只读操作
// 而非发现回退路径。仅发送应用与环境选择器，Docker/镜像元数据始终留在主机本地，不出 Agent 边界。
func (p *UnixSocketSubsystemProvisioner) CollectLicenseEvidence(ctx context.Context, app, environment string) (licenseevidence.Report, error) {
	if !p.enabled {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: deployment agent disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := dialProvisioningSocket(ctx, p.socketPath)
	if err != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: agent connection failed")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	watchProvisioningCancellation(ctx, conn)
	if json.NewEncoder(conn).Encode(subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_EVIDENCE", Code: app, Environment: environment}) != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: send failed")
	}
	var reply subsystemProvisioningReply
	if json.NewDecoder(io.LimitReader(conn, 256*1024)).Decode(&reply) != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNAVAILABLE: response invalid")
	}
	if !reply.Success || reply.LicenseEvidence == nil {
		return licenseevidence.Report{}, provisioningRejectionError(reply.Message, "")
	}
	return *reply.LicenseEvidence, nil
}

type licenseEvidenceApproval struct {
	Infrastructure       []licenseevidence.Infrastructure `json:"infrastructure,omitempty"`
	InstallationBoundary bool                             `json:"installation_boundary"`
	ExcludedServices     []string                         `json:"excluded_services"`
	Version              int                              `json:"version"`
	Project              string                           `json:"project"`
	Services             []licenseevidence.Service        `json:"services"`
}

// CollectLicenseEvidence 基于已批准的 license-evidence.json 审批文件采集生产 Compose 目标的
// 许可证证据。目标未在批准清单中或审批文件缺失/校验失败时，一律以 LICENSE_EVIDENCE_UNSUPPORTED
// 失败关闭，不做任何现场推断或降级采集。
func (p *ProductionComposeSubsystemProvisioner) CollectLicenseEvidence(ctx context.Context, app, environment string) (licenseevidence.Report, error) {
	if !p.enabled {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: agent disabled")
	}
	var target *productionComposeTarget
	for _, t := range p.targets {
		if t.config.Profile.Manifest.Application.Code == app && t.config.Profile.Manifest.Application.Environment == environment {
			target = t
			break
		}
	}
	if target == nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: target not approved")
	}
	path, err := productionRuntimeApprovalPath(target.config.DeployRoot, "license-evidence.json")
	if err != nil {
		return licenseevidence.Report{}, err
	}
	config, err := loadLicenseEvidenceApproval(path, target.config.ComposeProject, target.config.Profile)
	if err != nil {
		return licenseevidence.Report{}, err
	}
	collector, err := licenseevidence.NewCollector(licenseevidence.ExecRunner{DockerBinary: p.dockerBinary}, config)
	if err != nil {
		return licenseevidence.Report{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: release evidence approval invalid")
	}
	return collector.Collect(ctx), nil
}

func loadLicenseEvidenceApproval(path, project string, profile productionSubsystemProfile) (licenseevidence.Config, error) {
	unsupported := func() (licenseevidence.Config, error) {
		return licenseevidence.Config{}, provisioningError("LICENSE_EVIDENCE_UNSUPPORTED: approved digest version protocol inventory required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return unsupported()
	}
	f, err := os.Open(path)
	if err != nil {
		return unsupported()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 256*1024 {
		return unsupported()
	}
	b, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil || len(b) > 256*1024 {
		return unsupported()
	}
	var approval licenseEvidenceApproval
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&approval) != nil || decoder.Decode(new(any)) != io.EOF || approval.Version != 1 || approval.Project != project {
		return unsupported()
	}
	// 运行时发布声明保持完整，但安装证据描述实际选定的执行集合。固定清单的条件消费者
	// 可能缺席；每个非条件消费者仍是必需的。后续安装采集会检查项目内每个容器，
	// 因此真实运行的条件进程无法靠从本证据中省略而隐藏。
	expectedSet := map[string]bool{}
	consumers, err := runtimeLicenseConsumerServices(profile)
	if err != nil {
		return unsupported()
	}
	for _, service := range consumers {
		expectedSet[service] = true
	}
	for service := range profile.Manifest.Compose.ConditionalRuntimeServices {
		expectedSet[service] = true
	}
	services := []licenseevidence.Service{}
	actual := map[string]bool{}
	for _, s := range approval.Services {
		if s.Application == profile.Manifest.Application.Code && s.Environment == profile.Manifest.Application.Environment {
			if !expectedSet[s.Name] || actual[s.Name] {
				return unsupported()
			}
			actual[s.Name] = true
			services = append(services, s)
		}
	}
	if len(actual) == 0 {
		return unsupported()
	}
	for service := range expectedSet {
		_, conditional := profile.Manifest.Compose.ConditionalRuntimeServices[service]
		if !conditional && !actual[service] {
			return unsupported()
		}
	}
	infra := []licenseevidence.Infrastructure{}
	for _, item := range approval.Infrastructure {
		if item.Application == profile.Manifest.Application.Code && item.Environment == profile.Manifest.Application.Environment {
			infra = append(infra, item)
		}
	}
	expectedInfra := 0
	for _, name := range profile.Manifest.Compose.RuntimeServices {
		if licenseevidence.ReviewedControlledBIInfrastructure(profile.Manifest.Application.Code, name) {
			expectedInfra++
		}
	}
	if len(infra) != expectedInfra {
		return unsupported()
	}
	for _, item := range infra {
		if !licenseevidence.ReviewedControlledBIInfrastructure(item.Application, item.Name) {
			return unsupported()
		}
	}
	config := licenseevidence.Config{Project: project, Services: services, Infrastructure: infra, Timeout: 30 * time.Second}
	if _, err := licenseevidence.NewCollector(licenseevidence.ExecRunner{}, config); err != nil {
		return unsupported()
	}
	return config, nil
}

var _ licenseEvidenceProvider = (*UnixSocketSubsystemProvisioner)(nil)

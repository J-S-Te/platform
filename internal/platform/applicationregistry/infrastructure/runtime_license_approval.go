package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

var runtimeLicenseServiceIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type runtimeLicenseLifecycleProvider interface {
	ApprovedRuntimeLicenseLifecycle(context.Context, string, string) (application.RuntimeLicenseReleaseApproval, error)
}

// ApprovedRuntimeLicenseLifecycle 汇总生产目标的运行时批准生命周期。组件选择与批准元数据
// 必须逐项来自同一份不可变受审发布文件，读取期间发布被替换即报错；目标不在批准清单时失败关闭。
func (p *ProductionComposeSubsystemProvisioner) ApprovedRuntimeLicenseLifecycle(ctx context.Context, app, env string) (application.RuntimeLicenseReleaseApproval, error) {
	items, err := p.ApprovedRuntimeLicenseComponents(ctx, app, env)
	if err != nil {
		return application.RuntimeLicenseReleaseApproval{}, err
	}
	for _, target := range p.targets {
		if target.config.Profile.Manifest.Application.Code != app || target.config.Profile.Manifest.Application.Environment != env {
			continue
		}
		path, err := productionRuntimeApprovalPath(target.config.DeployRoot, "runtime-license-"+app+"-"+env+".json")
		if err != nil {
			return application.RuntimeLicenseReleaseApproval{}, err
		}
		release, err := loadRuntimeLicenseReleaseDocument(path, target.config.Profile)
		if err != nil {
			return application.RuntimeLicenseReleaseApproval{}, err
		}
		// 选择结果与元数据必须来自同一份不可变受审发布。
		byID := map[string]application.RuntimeLicenseApproval{}
		for _, item := range release.Components {
			byID[item.ServiceID] = item
		}
		out := application.RuntimeLicenseReleaseApproval{Components: items, RetireServiceIDs: release.RetireServiceIDs, ReleaseGeneration: release.ReleaseGeneration}
		for _, item := range items {
			if byID[item.ServiceID] != item {
				return application.RuntimeLicenseReleaseApproval{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: release changed while reading")
			}
			out.RequiredServiceIDs = append(out.RequiredServiceIDs, item.ServiceID)
		}
		return out, nil
	}
	return application.RuntimeLicenseReleaseApproval{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: target not approved")
}

// ApprovedRuntimeLicenseLifecycle 经部署 Agent socket 获取运行时批准生命周期。响应组件必须
// 通过应用层校验，否则按批准不匹配失败，不允许透传未验证的批准内容。
func (p *UnixSocketSubsystemProvisioner) ApprovedRuntimeLicenseLifecycle(ctx context.Context, app, env string) (application.RuntimeLicenseReleaseApproval, error) {
	if !p.enabled {
		return application.RuntimeLicenseReleaseApproval{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: deployment agent disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialProvisioningSocket(ctx, p.socketPath)
	if err != nil {
		return application.RuntimeLicenseReleaseApproval{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: agent connection failed")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return application.RuntimeLicenseReleaseApproval{}, application.ErrRuntimeLicenseApprovalUnavailable
	}
	watchProvisioningCancellation(ctx, conn)
	if json.NewEncoder(conn).Encode(subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_RUNTIME_LIFECYCLE_APPROVAL", Code: app, Environment: env}) != nil {
		return application.RuntimeLicenseReleaseApproval{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: request failed")
	}
	var reply subsystemProvisioningReply
	if json.NewDecoder(io.LimitReader(conn, 65536)).Decode(&reply) != nil {
		return application.RuntimeLicenseReleaseApproval{}, application.ErrRuntimeLicenseApprovalUnavailable
	}
	if !reply.Success {
		return application.RuntimeLicenseReleaseApproval{}, runtimeApprovalReplyError(reply.ErrorCode)
	}
	if reply.RuntimeLicenseLifecycle == nil || application.ValidateRuntimeLicenseApprovals(reply.RuntimeLicenseLifecycle.Components) != nil {
		return application.RuntimeLicenseReleaseApproval{}, application.ErrRuntimeLicenseApprovalMismatch
	}
	return *reply.RuntimeLicenseLifecycle, nil
}

type runtimeLicenseApprovalProvider interface {
	ApprovedRuntimeLicenseComponents(context.Context, string, string) ([]application.RuntimeLicenseApproval, error)
}

type runtimeLicenseRelease struct {
	Version           int                                  `json:"version"`
	Application       string                               `json:"application"`
	Environment       string                               `json:"environment"`
	Components        []application.RuntimeLicenseApproval `json:"components"`
	RetireServiceIDs  []string                             `json:"retire_service_ids,omitempty"`
	ReleaseGeneration string                               `json:"release_generation,omitempty"`
}

// ApprovedRuntimeLicenseComponents 返回生产目标当前选中的已批准运行时组件。只读路径刻意
// 复制 target 以免改动并发部署缓存的服务选择；批准清单与已选服务求交后必须非空，否则按
// 批准不可用失败。
func (p *ProductionComposeSubsystemProvisioner) ApprovedRuntimeLicenseComponents(ctx context.Context, app, env string) ([]application.RuntimeLicenseApproval, error) {
	if !p.enabled {
		return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: deployment agent disabled")
	}
	for _, target := range p.targets {
		if target.config.Profile.Manifest.Application.Code == app && target.config.Profile.Manifest.Application.Environment == env {
			// 只读批准不得改动并发部署已选服务的缓存；其选择会在部署锁下重新计算。
			target = &productionComposeTarget{config: target.config, runner: target.runner}
			if err := target.validateEnabledServices(ctx); err != nil {
				return nil, err
			}
			path, err := productionRuntimeApprovalPath(target.config.DeployRoot, "runtime-license-"+app+"-"+env+".json")
			if err != nil {
				return nil, err
			}
			items, err := loadRuntimeLicenseRelease(path, target.config.Profile)
			if err != nil {
				return nil, err
			}
			selected := map[string]bool{}
			for _, id := range target.selectedRuntimeLicenseConsumers() {
				selected[id] = true
			}
			result := []application.RuntimeLicenseApproval{}
			for _, item := range items {
				if selected[item.ServiceID] {
					result = append(result, item)
				}
			}
			if application.ValidateRuntimeLicenseApprovals(result) != nil {
				return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: no selected components")
			}
			return result, nil
		}
	}
	return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: target not approved")
}
func loadRuntimeLicenseRelease(path string, profile productionSubsystemProfile) ([]application.RuntimeLicenseApproval, error) {
	release, err := loadRuntimeLicenseReleaseDocument(path, profile)
	return release.Components, err
}
func loadRuntimeLicenseReleaseDocument(path string, profile productionSubsystemProfile) (runtimeLicenseRelease, error) {
	failure := func() (runtimeLicenseRelease, error) {
		return runtimeLicenseRelease{}, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: reviewed component inventory required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return failure()
	}
	f, err := os.Open(path)
	if err != nil {
		return failure()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 65536 {
		return failure()
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(raw) > 65536 {
		return failure()
	}
	var release runtimeLicenseRelease
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&release) != nil || d.Decode(new(any)) != io.EOF || release.Version != 1 || release.Application != profile.Manifest.Application.Code || release.Environment != profile.Manifest.Application.Environment || application.ValidateRuntimeLicenseApprovals(release.Components) != nil {
		return failure()
	}
	// 清单校验已强制条件单元必须属于 runtime_services。批准是集合并集，
	// 组件 ID 绝不重复。
	expectedSet := map[string]bool{}
	consumers, err := runtimeLicenseConsumerServices(profile)
	if err != nil {
		return failure()
	}
	for _, service := range consumers {
		expectedSet[service] = true
	}
	for service := range profile.Manifest.Compose.ConditionalRuntimeServices {
		expectedSet[service] = true
	}
	expected := make([]string, 0, len(expectedSet))
	for service := range expectedSet {
		expected = append(expected, service)
	}
	actual := make([]string, 0, len(release.Components))
	for _, c := range release.Components {
		actual = append(actual, c.ServiceID)
	}
	sort.Strings(expected)
	sort.Strings(actual)
	if len(expected) != len(actual) {
		return failure()
	}
	for i := range expected {
		if expected[i] != actual[i] {
			return failure()
		}
	}
	retired := map[string]bool{}
	if release.ReleaseGeneration != "" && !runtimeLicenseServiceIdentifier.MatchString(release.ReleaseGeneration) {
		return failure()
	}
	if len(release.RetireServiceIDs) > 32 {
		return failure()
	}
	for _, id := range release.RetireServiceIDs {
		_, conditional := profile.Manifest.Compose.ConditionalRuntimeServices[id]
		prefix := runtimeLicenseRetirementPrefix(release.Application)
		if prefix == "" || !strings.HasPrefix(id, prefix) || !runtimeLicenseServiceIdentifier.MatchString(id) || (expectedSet[id] && !conditional) || retired[id] || licenseevidence.ReviewedControlledBIInfrastructure(release.Application, id) {
			return failure()
		}
		retired[id] = true
	}
	return release, nil
}

// ApprovedRuntimeLicenseComponents 经部署 Agent socket 获取已批准运行时组件。Agent 不可达、
// 响应损坏或组件校验失败都按批准不可用失败关闭，不做本地推断。
func (p *UnixSocketSubsystemProvisioner) ApprovedRuntimeLicenseComponents(ctx context.Context, app, env string) ([]application.RuntimeLicenseApproval, error) {
	if !p.enabled {
		return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: deployment agent disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialProvisioningSocket(ctx, p.socketPath)
	if err != nil {
		return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: agent connection failed")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, application.ErrRuntimeLicenseApprovalUnavailable
	}
	watchProvisioningCancellation(ctx, conn)
	if json.NewEncoder(conn).Encode(subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_RUNTIME_APPROVAL", Code: app, Environment: env}) != nil {
		return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: request failed")
	}
	var reply subsystemProvisioningReply
	if json.NewDecoder(io.LimitReader(conn, 65536)).Decode(&reply) != nil {
		return nil, application.ErrRuntimeLicenseApprovalUnavailable
	}
	if !reply.Success {
		return nil, runtimeApprovalReplyError(reply.ErrorCode)
	}
	if application.ValidateRuntimeLicenseApprovals(reply.RuntimeLicenseApproval) != nil {
		return nil, application.ErrRuntimeLicenseApprovalMismatch
	}
	return reply.RuntimeLicenseApproval, nil
}

// socket 对端可能更旧或返回任意文本。只有白名单错误码能成为公开诊断；
// Message 与 Detail 绝不跨越该边界。
func runtimeApprovalReplyError(code string) error {
	for _, known := range []error{application.ErrRuntimeLicenseDockerUnavailable, application.ErrRuntimeLicenseApprovalMismatch, application.ErrRuntimeLicenseApprovalUnavailable, application.ErrRuntimeLicenseStartupFailed} {
		if code == application.RuntimeLicenseApprovalErrorCode(known) {
			return known
		}
	}
	return application.ErrRuntimeLicenseApprovalUnavailable
}

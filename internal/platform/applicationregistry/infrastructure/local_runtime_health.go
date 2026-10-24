package infrastructure

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

type localLicenseEvidenceRunner struct {
	p *LocalDockerSubsystemProvisioner
}

// RunOutput 只允许执行 docker 命令并把输出限制在给定字节上限内：非 docker 命令、输出超限或
// 失败一律返回泛化错误，不向证据采集方暴露底层命令细节。
func (r localLicenseEvidenceRunner) RunOutput(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	if name != "docker" {
		return nil, fmt.Errorf("invalid local evidence command")
	}
	if _, ok := r.p.runner.(execSubsystemCommandRunner); ok {
		return (licenseevidence.ExecRunner{DockerBinary: r.p.config.DockerBinary}).RunOutput(ctx, limit, name, args...)
	}
	runner, ok := r.p.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	})
	if !ok {
		return nil, fmt.Errorf("local evidence runner unavailable")
	}
	b, err := runner.RunOutput(ctx, r.p.localPlatformRoot(), os.Environ(), r.p.config.DockerBinary, args...)
	if err != nil || len(b) > limit {
		return nil, fmt.Errorf("local evidence projection unavailable")
	}
	return b, nil
}
func (p *LocalDockerSubsystemProvisioner) verifyLicensedLocalStartup(ctx context.Context, app, env string) error {
	release, err := p.localRelease(ctx, app, env)
	if err != nil {
		return err
	}
	collector, err := licenseevidence.NewCollector(localLicenseEvidenceRunner{p}, licenseevidence.Config{Project: release.Project, Services: release.Services, Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	first := collector.Collect(ctx)
	if !first.Complete {
		return application.ErrRuntimeLicenseStartupFailed
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	second := collector.Collect(ctx)
	if !second.Complete || len(second.Facts) != len(first.Facts) {
		return application.ErrRuntimeLicenseStartupFailed
	}
	for i, f := range first.Facts {
		next := second.Facts[i]
		if f.Service != next.Service || f.ContainerID != next.ContainerID || !f.StartedAt.Equal(next.StartedAt) {
			return application.ErrRuntimeLicenseStartupFailed
		}
	}
	// 已获许可的 Worker 在激活前或过期后暂停业务工作都是正确行为。业务心跳不能证明
	// 部署就绪；API 另行要求每个消费者给出自然的 READY/最新 ACK。
	return nil
}

package infrastructure

import (
	"context"
	"fmt"
	"os"
	"strings"

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

// verifyLicensedLocalStartup 校验内置运行目录中的每个组件容器都已在运行。
// V1 的“按评审发行采集证据”路径已下线：这里只核对真实运行事实，
// 不生成替代证据，也不把缺失容器误判为通过。
func (p *LocalDockerSubsystemProvisioner) verifyLicensedLocalStartup(ctx context.Context, app, env string) error {
	set, err := (BuiltinRuntimeComponentCatalog{}).RuntimeComponentSet(ctx, app, env)
	if err != nil {
		return application.ErrRuntimeLicenseStartupFailed
	}
	runner, ok := p.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	})
	if !ok {
		return application.ErrRuntimeLicenseStartupFailed
	}
	for _, component := range set.Components {
		service := localComposeService(component.ServiceID)
		out, runErr := runner.RunOutput(ctx, p.localPlatformRoot(), os.Environ(), p.config.DockerBinary,
			"ps", "--quiet",
			"--filter", "label=com.docker.compose.project="+p.config.PlatformComposeProject,
			"--filter", "label=com.docker.compose.service="+service)
		if runErr != nil || strings.TrimSpace(string(out)) == "" {
			return application.ErrRuntimeLicenseStartupFailed
		}
	}
	return nil
}

package infrastructure

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"time"
)

// validateRetainedContractRuntime 必须在任何运行时配置写入之前执行。
// 配置文件丢失不会把既有数据库变成首次安装。
func (provisioner *LocalDockerSubsystemProvisioner) validateRetainedContractRuntime(ctx context.Context, environmentPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if provisioner == nil || provisioner.runner == nil || !subsystemDirectoryCodePattern.MatchString(provisioner.config.PlatformComposeProject) {
		return provisioningError("contract retained runtime verification is unavailable")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// compose.local.yaml 显式命名该卷，独立于 -p 项目名。
	volume := "basic-platform-local-contract-mysql-data"
	err := provisioner.runner.Run(checkCtx, provisioner.config.ProjectsRoot, os.Environ(), provisioner.config.DockerBinary, "volume", "inspect", volume)
	if contextErr := checkCtx.Err(); contextErr != nil {
		return contextErr
	}
	if err != nil {
		// Docker 把守护进程失败与卷缺失都报为 inspect 错误。只有一次成功的清点才能确认
		// 卷不存在；绝不能从退出码推断。
		outputRunner, ok := provisioner.runner.(interface {
			RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
		})
		if !ok {
			return provisioningError("contract retained volume verification failed")
		}
		output, listErr := outputRunner.RunOutput(checkCtx, provisioner.config.ProjectsRoot, os.Environ(), provisioner.config.DockerBinary, "volume", "ls", "--filter", "name="+volume, "--format", "{{.Name}}")
		if contextErr := checkCtx.Err(); contextErr != nil {
			return contextErr
		}
		if listErr != nil {
			return provisioningError("contract retained volume verification failed")
		}
		for _, name := range strings.Fields(string(output)) {
			if name == volume {
				return provisioningError("contract retained volume verification failed")
			}
		}
		return nil
	}
	values, err := readEnvironmentValues(environmentPath)
	if err != nil {
		return provisioningError("contract retained runtime configuration must be restored before deployment")
	}
	for _, key := range []string{"CONTRACT_MYSQL_PASSWORD", "CONTRACT_MYSQL_ROOT_PASSWORD"} {
		value := strings.TrimSpace(values[key])
		if value == "" || strings.HasPrefix(value, "REPLACE_WITH_") || value == "PENDING_ONBOARDING" || !validEnvironmentValue(value) {
			return provisioningError("contract retained database credentials must be restored before deployment")
		}
	}
	for _, key := range []string{"OIDC_SESSION_ENCRYPTION_KEY_BASE64", "SIGNING_PHONE_ENCRYPTION_KEY_BASE64"} {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(values[key]))
		if err != nil || len(decoded) != 32 {
			return provisioningError("contract retained encryption keys must be restored before deployment")
		}
	}
	return nil
}

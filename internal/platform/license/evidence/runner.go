package evidence

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// ExecRunner 只应存在于隔离的部署 Agent 上，而不是 platform API 进程内。
// DockerBinary 必须由运维配置，绝不能来自请求参数。
type ExecRunner struct{ DockerBinary string }

func (r ExecRunner) RunOutput(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	if name != "docker" || limit < 1 || limit > maxOutput {
		return nil, errors.New("unsupported evidence command")
	}
	binary := r.DockerBinary
	if binary == "" {
		binary = "docker"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	output := &boundedBuffer{limit: limit}
	cmd.Stdout = output
	// 刻意丢弃 stderr：守护进程错误文本可能携带凭据等敏感信息。
	if cmd.Run() != nil {
		return nil, errors.New("evidence command failed")
	}
	return output.Bytes(), nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("evidence output limit exceeded")
	}
	return b.Buffer.Write(p)
}

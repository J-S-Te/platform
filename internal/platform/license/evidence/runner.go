package evidence

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// ExecRunner belongs on the isolated deployment Agent, not the platform API.
// DockerBinary must be configured by the operator, never request parameters.
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
	// stderr is intentionally discarded: daemon errors can contain credentials.
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

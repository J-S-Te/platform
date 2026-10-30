package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSubsystemCommandOutputHelper 为真实子进程输出机器结果及独立诊断流。
// 参数：t为测试上下文；专用环境变量限定子进程行为。
// 返回值：无；失败样本以非零状态退出。
func TestSubsystemCommandOutputHelper(t *testing.T) {
	mode := os.Getenv("QA_SUBSYSTEM_OUTPUT_HELPER")
	if mode == "" {
		return
	}
	fmt.Fprintln(os.Stdout, `{"services":{"project-api":{}}}`)
	fmt.Fprintln(os.Stderr, "warning: optional configuration is unset")
	if mode == "failure" {
		os.Exit(7)
	}
	os.Exit(0)
}

// TestSubsystemCommandOutputSeparatesDiagnostics 防止成功命令的诊断破坏结构化结果。
// 参数：t为测试上下文；使用本测试二进制启动真实独立进程。
// 返回值：无；成功结果污染或失败诊断丢失时测试失败。
func TestSubsystemCommandOutputSeparatesDiagnostics(t *testing.T) {
	for _, mode := range []string{"success", "failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			env := append(os.Environ(), "QA_SUBSYSTEM_OUTPUT_HELPER="+mode)
			output, err := (execSubsystemCommandRunner{}).RunOutput(ctx, t.TempDir(),
				env, os.Args[0], "-test.run=^TestSubsystemCommandOutputHelper$")
			if mode == "success" {
				if err != nil || !json.Valid(output) {
					t.Fatalf("successful machine output must contain only JSON: err=%v output=%q", err, output)
				}
				return
			}
			if err == nil || !strings.Contains(string(output), `"project-api"`) ||
				!strings.Contains(string(output), "warning: optional configuration is unset") {
				t.Fatalf("failed command must retain both diagnostics and output: err=%v output=%q", err, output)
			}
		})
	}
}

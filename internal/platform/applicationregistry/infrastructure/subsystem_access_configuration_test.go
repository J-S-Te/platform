package infrastructure

import (
	"strings"
	"testing"
)

// V1 配置指纹已随审批路径下线；漂移判定仍只比较"本次候选"与"上次成功部署证据"。
// 这里用不透明摘要字符串直接覆盖判定分支，不复制指纹实现。
func TestSubsystemAccessConfigurationDetectsUpdates(t *testing.T) {
	// 证据里的候选必须是 64 位十六进制摘要。
	baseline := strings.Repeat("a", 64)
	for _, tt := range []struct {
		name, candidate, manifest, applied, stored string
		drift                                      bool
	}{
		{"unchanged", baseline, "manifest-1", "manifest-1", baseline, false},
		{"new image", "other-digest", "manifest-1", "manifest-1", baseline, true},
		{"new manifest", baseline, "manifest-2", "manifest-1", baseline, true},
		{"unknown baseline", baseline, "manifest-1", "manifest-1", "", true},
		{"not applied", baseline, "manifest-1", "", baseline, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			drift, summary := accessConfigurationDrift(tt.candidate, tt.stored, tt.manifest, tt.applied)
			if drift != tt.drift || (drift && len(summary) == 0) {
				t.Fatalf("drift=%v summary=%v", drift, summary)
			}
		})
	}
	if got := accessCandidateFromEvidence(accessCandidatePrefix + baseline + ";provision:manifest-1"); got != baseline {
		t.Fatal(got)
	}
	if accessCandidateFromEvidence("provision:manifest-1") != "" || accessCandidateFromEvidence(accessCandidatePrefix+"invalid") != "" {
		t.Fatal("legacy or malformed evidence accepted")
	}
}

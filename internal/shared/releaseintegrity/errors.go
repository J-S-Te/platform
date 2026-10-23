package releaseintegrity

import "fmt"

// Code 是稳定的发布完整性诊断代码，刻意
// 不复用 LICENSE_* 代码：发布完整性与商业授权、
// 运行时确认回答的是不同问题。
type Code string

const (
	CodeTrustUnavailable     Code = "RELEASE_TRUST_UNAVAILABLE"
	CodeSignatureInvalid     Code = "RELEASE_SIGNATURE_INVALID"
	CodeProvenanceInvalid    Code = "RELEASE_PROVENANCE_INVALID"
	CodeArtifactMismatch     Code = "RELEASE_ARTIFACT_MISMATCH"
	CodePlatformMismatch     Code = "RELEASE_PLATFORM_MISMATCH"
	CodeComponentSetMismatch Code = "RELEASE_COMPONENT_SET_MISMATCH"
	CodeAgentIncompatible    Code = "RELEASE_AGENT_INCOMPATIBLE"
	CodeRollbackRejected     Code = "RELEASE_ROLLBACK_REJECTED"
	CodeRecoveryRequired     Code = "RELEASE_RECOVERY_REQUIRED"
	CodeReleaseInvalid       Code = "RELEASE_INVALID"
)

// Error 是发布完整性策略失败时返回的唯一错误类型。
// Details 只能包含不敏感且已验证的值，例如
// 相对产物路径或公开摘要。
type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func fail(code Code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func failDetails(code Code, message string, details map[string]any) error {
	return &Error{Code: code, Message: message, Details: details}
}

package application

import (
	"errors"
	"time"
)

var ErrServiceCredentialRetryRequired = errors.New(
	"new service credential delivery requires controlled RETRY",
)

const (
	AccessDeploymentRejected      = "REJECTED_BEFORE_DEPLOYMENT"
	AccessDeploymentAbandoned     = "ABANDONED"
	AccessCredentialRetryRequired = "SERVICE_CREDENTIAL_RETRY_REQUIRED"
	AccessOperatorAbandoned       = "OPERATOR_ABANDONED"
	// AccessDeploymentEffectUnknown 表示派发之后没有取得确定结果：只能人工核验，不能重排重试。
	AccessDeploymentEffectUnknown = "DEPLOYMENT_EFFECT_UNKNOWN"
	// AccessDeploymentSupersededUnknown 表示人工核验确认原代次已被新的审核清单取代，
	// 且既无成功回执也无终止结果：只作失败收口，不声明部署成功、也不自动回滚。
	AccessDeploymentSupersededUnknown = "DEPLOYMENT_SUPERSEDED_UNKNOWN"
)

// SubsystemAccessDeploymentOutcome 是Agent真实拒绝或人工终止的持久事实，不是成功回执。
type SubsystemAccessDeploymentOutcome struct {
	Receipt        SubsystemAccessDeploymentReceipt   `json:"receipt"`
	State          string                             `json:"state"`
	Code           string                             `json:"code"`
	OperatorID     string                             `json:"operator_id,omitempty"`
	Reason         string                             `json:"reason,omitempty"`
	EvidenceDigest string                             `json:"evidence_digest,omitempty"`
	RecordedAt     time.Time                          `json:"recorded_at"`
	RuntimeFiles   []SubsystemAccessRecoveryFile      `json:"runtime_files,omitempty"`
	Containers     []SubsystemAccessRecoveryContainer `json:"containers,omitempty"`
}

type SubsystemAccessRecoveryFile struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	SHA256  string `json:"sha256,omitempty"`
}

type SubsystemAccessRecoveryContainer struct {
	ServiceID   string `json:"service_id"`
	Present     bool   `json:"present"`
	ContainerID string `json:"container_id,omitempty"`
	Image       string `json:"image,omitempty"`
	State       string `json:"state,omitempty"`
	Health      string `json:"health,omitempty"`
}

type SubsystemAccessRecoveryEvidence struct {
	Receipt        SubsystemAccessDeploymentReceipt   `json:"receipt"`
	EvidenceDigest string                             `json:"evidence_digest"`
	RuntimeFiles   []SubsystemAccessRecoveryFile      `json:"runtime_files"`
	Containers     []SubsystemAccessRecoveryContainer `json:"containers"`
	AlreadyFenced  bool                               `json:"already_fenced"`
	// CurrentProfileChecksum 是 Agent 当前审核清单的摘要；与原回执清单摘要不同即表示
	// 原代次已被新的发布取代（"审核基准已变更"），此时不再要求旧清单与当前清单一致。
	CurrentProfileChecksum string `json:"current_profile_checksum,omitempty"`
	// SuccessReceipt 与 TerminalOutcome 是 Agent 对原代次的否定证明：
	// 两者都为假才能证明"确实没有成功、也没有终止"；任一为真都不得按取代收口。
	SuccessReceipt  bool `json:"success_receipt,omitempty"`
	TerminalOutcome bool `json:"terminal_outcome,omitempty"`
}

type SubsystemAccessAbandonRequest struct {
	ReleaseDigest  string                           `json:"release_digest,omitempty"`
	Receipt        SubsystemAccessDeploymentReceipt `json:"receipt"`
	EvidenceDigest string                           `json:"evidence_digest"`
	OperatorID     string                           `json:"operator_id"`
	Reason         string                           `json:"reason"`
}

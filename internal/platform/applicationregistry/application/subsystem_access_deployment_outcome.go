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
}

type SubsystemAccessAbandonRequest struct {
	ReleaseDigest  string                           `json:"release_digest,omitempty"`
	Receipt        SubsystemAccessDeploymentReceipt `json:"receipt"`
	EvidenceDigest string                           `json:"evidence_digest"`
	OperatorID     string                           `json:"operator_id"`
	Reason         string                           `json:"reason"`
}

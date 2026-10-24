package application

import (
	"context"
	"time"
)

// HandoverItem 是子系统适配器发现的一项责任；平台只保存交接控制记录，业务数据仍由各子系统负责。
type HandoverItem struct {
	ID             string `json:"id"`
	System         string `json:"system"`
	ResourceType   string `json:"resource_type"`
	ResourceID     string `json:"resource_id"`
	ResourceName   string `json:"resource_name,omitempty"`
	Category       string `json:"category,omitempty"`
	CurrentOwnerID string `json:"current_owner_id"`
	TargetOwnerID  string `json:"target_owner_id"`
	Status         string `json:"status"`
	BatchID        string `json:"batch_id,omitempty"`
	// 编排字段：幂等键、租约与重试计划由平台控制面维护。
	ExpectedVersion uint64     `json:"expected_version"`
	AppliedVersion  uint64     `json:"applied_version"`
	IdempotencyKey  string     `json:"idempotency_key,omitempty"`
	LeaseID         string     `json:"lease_id,omitempty"`
	LeaseDeadline   *time.Time `json:"lease_deadline,omitempty"`
	Attempts        uint64     `json:"attempts"`
	NextRetryAt     *time.Time `json:"next_retry_at,omitempty"`
	LastErrorCode   string     `json:"last_error_code,omitempty"`
	EvidenceRef     string     `json:"evidence_ref,omitempty"`
	Version         uint64     `json:"version"`
	CompletedBy     string     `json:"completed_by,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

// HandoverReport 是交接就绪检查结果；Ready 只有在无未完成责任项时为真。
type HandoverReport struct {
	Ready       bool           `json:"ready"`
	Outstanding []HandoverItem `json:"outstanding"`
}

// HandoverChecker 保持为窄适配边界：子系统发布责任快照，人员变更服务不直接访问子系统数据库。
type HandoverChecker interface {
	Check(context.Context, PersonnelChangeRequest) (HandoverReport, error)
}

// HandoverManager 暴露平台持有的交接控制面。每个持久责任项完成之前，离职不能排期。
type HandoverManager interface {
	HandoverChecker
	List(context.Context, string, string) ([]HandoverItem, error)
	Complete(context.Context, string, string, string, string, string) (HandoverItem, error)
}

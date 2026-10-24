package application

import (
	"context"
	"errors"
	"time"
)

// 统一接入向导的操作实体与仓储契约。
// 只保存进度与证据引用：不保存密钥材料、凭据封套或完整命令。

var (
	// ErrSubsystemAccessOperationNotFound 表示操作不存在（或不属于当前租户）。
	ErrSubsystemAccessOperationNotFound = errors.New("subsystem access operation not found")
	// ErrSubsystemAccessOperationConflict 表示同幂等键的内容不同，或同目标已有执行中操作。
	ErrSubsystemAccessOperationConflict = errors.New("subsystem access operation conflict")
	// ErrSubsystemAccessOperationLeaseLost 表示租约已失效：过期执行者不得覆盖新结果。
	ErrSubsystemAccessOperationLeaseLost = errors.New("subsystem access operation lease lost")
)

// SubsystemAccessOperation 是一次向导操作（非秘密信息）。
type SubsystemAccessOperation struct {
	ID              string
	TenantID        string
	ApplicationID   string
	EnvironmentID   string
	ApplicationCode string
	EnvironmentCode string
	Action          SubsystemAccessAction
	Stage           SubsystemAccessStage
	Status          SubsystemAccessStatus
	PreviewVersion  string
	RequestDigest   string
	// PublicOrigin / AuthProvider / InitialAdminID 是执行参数（非敏感），Worker 据此执行。
	PublicOrigin   string
	AuthProvider   string
	InitialAdminID string
	IdempotencyKey string
	OperatorID     string
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	AttemptCount   uint32
	ErrorCode      string
	ErrorMessage   string
	ResultRef      string
	Version        uint64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SubsystemAccessOperationStage 记录单个阶段的执行结果（同样只存引用）。
type SubsystemAccessOperationStage struct {
	ID           string
	TenantID     string
	OperationID  string
	Stage        SubsystemAccessStage
	Status       SubsystemAccessStatus
	AttemptCount uint32
	ErrorCode    string
	ErrorMessage string
	EvidenceRef  string
	StartedAt    *time.Time
	FinishedAt   *time.Time
	UpdatedAt    time.Time
}

// SubsystemAccessOperationCreate 是创建请求（幂等键 + 请求摘要 + 目标 + 动作）。
type SubsystemAccessOperationCreate struct {
	ID              string
	TenantID        string
	ApplicationID   string
	EnvironmentID   string
	ApplicationCode string
	EnvironmentCode string
	Action          SubsystemAccessAction
	Stage           SubsystemAccessStage
	PreviewVersion  string
	RequestDigest   string
	// PublicOrigin / AuthProvider / InitialAdminID 是执行参数（非敏感），Worker 据此执行。
	PublicOrigin   string
	AuthProvider   string
	InitialAdminID string
	IdempotencyKey string
	OperatorID     string
	Now            time.Time
}

// SubsystemAccessOperationCompletion 是收口写入（终态或等待）。
type SubsystemAccessOperationCompletion struct {
	TenantID     string
	ID           string
	LeaseOwner   string
	Status       SubsystemAccessStatus
	Stage        SubsystemAccessStage
	ErrorCode    string
	ErrorMessage string
	ResultRef    string
	Now          time.Time
}

// SubsystemAccessOperationRepository 是操作与阶段的持久化契约。
// 实现必须用条件更新实现领取/续租/收口：过期执行者不能覆盖新结果。
type SubsystemAccessOperationRepository interface {
	CreateOperation(context.Context, SubsystemAccessOperationCreate) (SubsystemAccessOperation, bool, error)
	GetOperation(context.Context, string, string) (SubsystemAccessOperation, error)
	FindOperationByTarget(context.Context, string, string, string) (SubsystemAccessOperation, bool, error)
	ListOperationsByTarget(context.Context, string, string, string, int) ([]SubsystemAccessOperation, error)
	ClaimOperation(context.Context, string, string, string, time.Duration, time.Time) (SubsystemAccessOperation, bool, error)
	RenewOperationLease(context.Context, string, string, string, time.Duration, time.Time) (bool, error)
	CompleteOperation(context.Context, SubsystemAccessOperationCompletion) (bool, error)
	// RequeueOperation 把等待/失败的操作放回可领取状态（保留槽位；条件更新，重复调用幂等）。
	RequeueOperation(context.Context, string, string, time.Time) (bool, error)
	// AdvanceOperationStage 在持有租约时推进阶段（不改状态、不清租约）；过期执行者调用返回 false。
	AdvanceOperationStage(context.Context, string, string, string, SubsystemAccessStage, time.Time) (bool, error)
	// ListClaimableOperations 列出可领取的操作（待处理，或运行中但租约已过期）；有界、跨租户按时间排序。
	ListClaimableOperations(context.Context, int, time.Time) ([]SubsystemAccessOperation, error)
	UpsertOperationStage(context.Context, string, string, SubsystemAccessStage, SubsystemAccessStatus, string, string, string, time.Time) error
	ListOperationStages(context.Context, string, string) ([]SubsystemAccessOperationStage, error)
}

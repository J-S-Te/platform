// Package application 提供 Keycloak 授权投影面向运维的、租户隔离的恢复契约。
// 它刻意不暴露 worker 队列本身：调用方可以查看持久化失败并请求一次安全重放，
// 但不能改变事件的租户、主体或目标应用。
package application

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	ErrOperationsValidation = errors.New("invalid Keycloak projection operation")
	ErrProjectionNotFound   = errors.New("Keycloak projection event not found")
	ErrProjectionConflict   = errors.New("Keycloak projection event cannot be replayed")
)

const (
	defaultFailurePageSize = 20
	maxFailurePageSize     = 100
)

// FailurePageRequest 把管理端视图限定在租户加可选的应用/环境过滤之内。
// 它绝不接受浏览器传入的任意 SQL 排序字段或身份选择器。
type FailurePageRequest struct {
	Page, PageSize               int
	ApplicationCode, Environment string
}

// FailedProjection 是一次持久化失败的 Keycloak 投影事件的运维视图；错误内容已
// 脱敏（不含 token、secret 或 outbox 载荷），BlocksCutover 表示该失败是否阻塞
// 所属环境的 issuer 切换。
type FailedProjection struct {
	EventID         string    `json:"event_id"`
	IdentityID      string    `json:"identity_id"`
	ApplicationID   string    `json:"application_id"`
	EnvironmentID   string    `json:"environment_id,omitempty"`
	ApplicationCode string    `json:"application_code"`
	Environment     string    `json:"environment,omitempty"`
	EventType       string    `json:"event_type"`
	Attempts        uint      `json:"attempts"`
	FailedAt        time.Time `json:"failed_at"`
	ErrorCode       string    `json:"error_code,omitempty"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	BlocksCutover   bool      `json:"blocks_cutover"`
}

// FailurePageResult 是失败投影的分页结果；Total 为满足过滤条件的失败事件总数。
type FailurePageResult struct {
	Items    []FailedProjection `json:"items"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
	Total    int64              `json:"total"`
}

// AlertStatus 是无敏感信息的运维信号：FAILED 事件是持久化死信，会阻塞其所属
// 环境的 Keycloak issuer 切换。该表示中不含任何 Client secret、token 或 outbox 载荷。
type AlertStatus struct {
	Severity                 string     `json:"severity"`
	State                    string     `json:"state"`
	Summary                  string     `json:"summary"`
	FailedCount              int64      `json:"failed_count"`
	AffectedEnvironmentCount int64      `json:"affected_environment_count"`
	OldestFailedAt           *time.Time `json:"oldest_failed_at,omitempty"`
}

// ReplayInput 是重放一次失败投影的请求；Confirmation 必须原样回填 EventID，
// Reason 用于运维审计留痕。
type ReplayInput struct {
	TenantID, EventID, OperatorID, Confirmation, Reason string
}

// ReplayResult 报告重放结果：Replayed 表示本次已转回 PENDING，AlreadyPending
// 表示事件本就在队列中（视为幂等成功），AvailableAt 为下一次可投递时间。
type ReplayResult struct {
	EventID        string    `json:"event_id"`
	Replayed       bool      `json:"replayed"`
	AlreadyPending bool      `json:"already_pending"`
	AvailableAt    time.Time `json:"available_at"`
}

// OperationsStore 持久化失败投影的查询、告警与重放状态；实现必须以租户为边界，
// 并在行锁下完成 FAILED -> PENDING 的条件转移，防止重复入队。
type OperationsStore interface {
	ListFailedProjections(context.Context, string, FailurePageRequest) (FailurePageResult, error)
	GetProjectionAlertStatus(context.Context, string) (AlertStatus, error)
	ReplayFailedProjection(context.Context, ReplayInput, time.Time) (ReplayResult, error)
}

// Operations 是 Keycloak 投影的运维门面：查询失败死信、聚合告警状态和受控重放，
// 供管理端接口调用。
type Operations struct {
	store OperationsStore
	clock Clock
}

// NewOperations 构造运维服务；store 为空或提供超过一个时钟时返回错误。
func NewOperations(store OperationsStore, clocks ...Clock) (*Operations, error) {
	if store == nil || len(clocks) > 1 {
		return nil, errors.New("Keycloak projection operations dependencies are invalid")
	}
	clock := Clock(systemClock{})
	if len(clocks) == 1 {
		if clocks[0] == nil {
			return nil, errors.New("Keycloak projection operations clock must not be nil")
		}
		clock = clocks[0]
	}
	return &Operations{store: store, clock: clock}, nil
}

// ListFailed 返回租户内失败投影的分页列表；租户 ID 缺失时返回 ErrOperationsValidation。
func (service *Operations) ListFailed(ctx context.Context, tenantID string, query FailurePageRequest) (FailurePageResult, error) {
	if strings.TrimSpace(tenantID) == "" {
		return FailurePageResult{}, ErrOperationsValidation
	}
	query = normalizeFailurePageQuery(query)
	return service.store.ListFailedProjections(ctx, strings.TrimSpace(tenantID), query)
}

// AlertStatus 返回租户内失败投影的聚合告警状态，供运维轮询与切换门禁检查。
func (service *Operations) AlertStatus(ctx context.Context, tenantID string) (AlertStatus, error) {
	if strings.TrimSpace(tenantID) == "" {
		return AlertStatus{}, ErrOperationsValidation
	}
	return service.store.GetProjectionAlertStatus(ctx, strings.TrimSpace(tenantID))
}

// Replay 只做 FAILED -> PENDING 的状态变更。存储层在行锁下执行条件转移；
// 重复 POST 看到的是 PENDING/RUNNING，将返回幂等成功而不会入队重复事件。
func (service *Operations) Replay(ctx context.Context, input ReplayInput) (ReplayResult, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.EventID = strings.TrimSpace(input.EventID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.Confirmation = strings.TrimSpace(input.Confirmation)
	input.Reason = strings.TrimSpace(input.Reason)
	if input.TenantID == "" || input.EventID == "" || input.OperatorID == "" || input.Confirmation != input.EventID || len(input.Reason) < 6 || len(input.Reason) > 500 {
		return ReplayResult{}, ErrOperationsValidation
	}
	return service.store.ReplayFailedProjection(ctx, input, service.clock.Now().UTC())
}

func normalizeFailurePageQuery(query FailurePageRequest) FailurePageRequest {
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = defaultFailurePageSize
	}
	if query.PageSize > maxFailurePageSize {
		query.PageSize = maxFailurePageSize
	}
	query.ApplicationCode = strings.TrimSpace(query.ApplicationCode)
	query.Environment = strings.ToLower(strings.TrimSpace(query.Environment))
	return query
}

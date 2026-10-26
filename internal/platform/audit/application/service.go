// Package application 编排只追加审计写入、租户隔离查询和异步导出任务。
package application

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

var (
	correlationIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	traceIDPattern               = regexp.MustCompile(`^[0-9a-f]{32}$`)

	// ErrNotFound 表示租户范围内不存在目标审计事件或导出任务。
	ErrNotFound = errors.New("audit resource not found")
	// ErrConflict 表示状态机不允许的转换或唯一键冲突。
	ErrConflict = errors.New("audit resource conflict")
	// ErrValidation 表示调用方可自行修正的输入错误。
	ErrValidation = errors.New("audit validation failed")
)

// IdentifierGenerator 生成审计事件与导出任务的稳定公开标识。
type IdentifierGenerator interface {
	New(time.Time) (string, error)
}

// Clock 抽象时间源，使接收时间与导出调度可测试。
type Clock interface{ Now() time.Time }

// SystemClock 是生产环境的 UTC 时钟。
type SystemClock struct{}

// Now 返回当前 UTC 时间。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// PageRequest 是审计事件列表查询的分页与过滤参数。
type PageRequest struct {
	Page, PageSize                                                                       int
	Keyword, ApplicationCode, EnvironmentCode, Action, ActionCategory, Result, RiskLevel string
	// 批次 3：按操作地址、对象与链路 ID 精确检索，便于从一条审计追到整条链路。
	ClientIP, ResourceID, RequestID string
	// 统一审计方案 §2.4：按事件、操作者、关联 ID 与登录地址精确检索。
	EventID, ActorID, CorrelationID, LoginIP string
	OccurredFrom, OccurredTo                 *time.Time
}

// PageResult 是审计查询的单页结果。
type PageResult[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// EventInput 是单个审计事件的完整输入，包含主体、动作、资源、链路与来源协议字段。
type EventInput struct {
	EventID, ApplicationCode, EnvironmentCode, ActorType, ActorID, ActorName, SessionID, ClientID string
	OccurredAt                                                                                    time.Time
	Action, ResourceType, ResourceID, ResourceName, BusinessID, RequestID, TraceID, CorrelationID string
	Result, ReasonCode, RiskLevel, Classification, Summary                                        string
	Metadata                                                                                      map[string]any
	Changes                                                                                       []domain.FieldChange
	SourceIP, UserAgent, EventCategory, EventType                                                 string
	// 统一审计来源协议字段。SourceIP 是旧协议兼容列（历史行为：user_login_ip，缺省为投递地址），
	// 新发布方应提供 LoginIP/ClientIP/IPSource；IngestPeerIP 只由平台摄取端写入。
	LoginIP, ClientIP, IngestPeerIP, IPSource string
	// LoginIPSource 记录登录地址的获取方式（会话期固定）；历史记录为空。
	LoginIPSource string
}

// BatchDeliveryInput 描述一次传输批次。这里的关联标识属于“子系统到平台接收端”的投递链路；
// EventInput 中的同名字段仍保留被审计业务操作自己的链路，二者不能互相覆盖。
type BatchDeliveryInput struct {
	ApplicationCode, EnvironmentCode, ClientID string
	RequestID, TraceID, CorrelationID          string
	SourceIP, UserAgent                        string
}

// IngestionHealth 是平台侧审计摄取健康度（批次 3）。
//
// 口径说明：各子系统的审计 Outbox 位于各自数据库，平台无法读取，因此这里统计的是
// "平台已接收的事件"——近期接收量、来源未采集数与最近接收时间，用于发现采集/上报停摆。
type IngestionHealth struct {
	TotalEvents       int64      `json:"total_events"`
	RecentEvents      int64      `json:"recent_events"`
	UnknownProvenance int64      `json:"unknown_provenance"`
	LastReceivedAt    *time.Time `json:"last_received_at,omitempty"`
	OldestUnknownAt   *time.Time `json:"oldest_unknown_at,omitempty"`
}

// Repository 持久化审计事件、摄取健康度与导出任务；实现必须强制租户范围并保证
// 事件只追加、不更新。
type Repository interface {
	Ingest(context.Context, string, EventInput, time.Time) (domain.Receipt, error)
	IngestBatch(context.Context, string, BatchDeliveryInput, []EventInput, time.Time) ([]domain.Receipt, error)
	List(context.Context, string, PageRequest) (PageResult[domain.Event], error)
	Get(context.Context, string, string) (domain.Event, error)
	IngestionHealth(context.Context, string, time.Time) (IngestionHealth, error)
	CreateExportJob(context.Context, string, string, PageRequest, string, time.Time) (domain.ExportJob, error)
	GetExportJob(context.Context, string, string) (domain.ExportJob, error)
	ClaimExportJob(context.Context, string, time.Time, time.Time) (domain.ExportWork, bool, error)
	ListExportEvents(context.Context, string, domain.ExportQuery) ([]domain.Event, error)
	CompleteExportJob(context.Context, domain.ExportWork, domain.ExportFile, time.Time) error
	FailExportJob(context.Context, domain.ExportWork, string, string, time.Time, time.Time) error
	GetExportFile(context.Context, string, string) (domain.ExportFile, error)
}

// Service 是审计应用服务：统一执行输入校验、链路补全与租户范围约束。
type Service struct {
	repository Repository
	ids        IdentifierGenerator
	clock      Clock
}

// NewService 构造审计服务；任一依赖为 nil 时失败关闭。
func NewService(repository Repository, ids IdentifierGenerator, clock Clock) (*Service, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("audit service dependencies must not be nil")
	}
	return &Service{repository: repository, ids: ids, clock: clock}, nil
}

// Ingest 校验并补全链路字段后持久化单个审计事件；事件只追加，不更新。
func (s *Service) Ingest(ctx context.Context, tenantID string, input EventInput) (domain.Receipt, error) {
	input = completeCorrelation(ctx, input)
	if err := validateInput(tenantID, input); err != nil {
		return domain.Receipt{}, err
	}
	return s.repository.Ingest(ctx, tenantID, normalizeInput(input), s.clock.Now())
}

// IngestBatch 在持久化前校验整批事件，并把“首次接收/幂等重复”的判定交给同一个仓储事务。
// 任一事件无效或事务失败都不会留下部分事件或批次回执；重复事件则作为逐条成功结果返回，
// 使采用事务发件箱重试的子系统无需把已接收事件误判为失败。
func (s *Service) IngestBatch(ctx context.Context, tenantID string, delivery BatchDeliveryInput, inputs []EventInput) ([]domain.Receipt, error) {
	if len(inputs) == 0 || len(inputs) > 100 {
		return nil, validation("events must contain 1 to 100 items")
	}
	if err := validateBatchDelivery(tenantID, delivery); err != nil {
		return nil, err
	}

	normalized := make([]EventInput, 0, len(inputs))
	for _, input := range inputs {
		if err := validateInput(tenantID, input); err != nil {
			return nil, err
		}
		if input.ApplicationCode != delivery.ApplicationCode || input.EnvironmentCode != delivery.EnvironmentCode || input.ClientID != delivery.ClientID {
			return nil, validation("batch event source does not match delivery principal")
		}
		normalized = append(normalized, normalizeInput(input))
	}
	return s.repository.IngestBatch(ctx, tenantID, normalizeBatchDelivery(delivery), normalized, s.clock.Now())
}

// List 分页返回租户内审计事件，过滤条件先规范化再校验。
func (s *Service) List(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Event], error) {
	query = normalizePage(query)
	if err := validatePage(query); err != nil {
		return PageResult[domain.Event]{}, err
	}
	return s.repository.List(ctx, tenantID, query)
}

// Get 返回租户内单个审计事件详情。
func (s *Service) Get(ctx context.Context, tenantID, eventID string) (domain.Event, error) {
	if strings.TrimSpace(eventID) == "" {
		return domain.Event{}, validation("event_id is required")
	}
	return s.repository.Get(ctx, tenantID, strings.TrimSpace(eventID))
}

// CreateExportJob 以规范化后的查询快照创建异步导出任务。
func (s *Service) CreateExportJob(ctx context.Context, tenantID, operatorID string, query PageRequest) (domain.ExportJob, error) {
	if strings.TrimSpace(operatorID) == "" {
		return domain.ExportJob{}, validation("operator is required")
	}
	query = normalizePage(query)
	if err := validatePage(query); err != nil {
		return domain.ExportJob{}, err
	}
	id, err := s.ids.New(s.clock.Now())
	if err != nil {
		return domain.ExportJob{}, fmt.Errorf("generate export job ID: %w", err)
	}
	return s.repository.CreateExportJob(ctx, tenantID, operatorID, query, id, s.clock.Now())
}

// GetExportJob 返回导出任务状态与产物元数据。
func (s *Service) GetExportJob(ctx context.Context, tenantID, jobID string) (domain.ExportJob, error) {
	if strings.TrimSpace(jobID) == "" {
		return domain.ExportJob{}, validation("job_id is required")
	}
	return s.repository.GetExportJob(ctx, tenantID, strings.TrimSpace(jobID))
}
func normalizePage(query PageRequest) PageRequest {
	query.Keyword = strings.TrimSpace(query.Keyword)
	query.ApplicationCode = strings.TrimSpace(query.ApplicationCode)
	query.EnvironmentCode = strings.TrimSpace(query.EnvironmentCode)
	query.Action = strings.TrimSpace(query.Action)
	query.ActionCategory = strings.ToUpper(strings.TrimSpace(query.ActionCategory))
	query.Result = strings.ToUpper(strings.TrimSpace(query.Result))
	query.RiskLevel = strings.ToUpper(strings.TrimSpace(query.RiskLevel))
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = 20
	}
	if query.PageSize > 100 {
		query.PageSize = 100
	}
	return query
}

func validActionCategory(category string) bool {
	return category == "" || oneOf(category, "LOGIN", "CREATE", "UPDATE", "DELETE", "EXPORT", "STATUS_CHANGE", "AUTHORIZATION_CHANGE", "SECRET_ROTATION", "PASSWORD_RESET", "CATALOG_SYNC", "AUDIT_ACCESS", "IMPORT")
}

func validatePage(query PageRequest) error {
	if !validActionCategory(query.ActionCategory) {
		return validation("action_category is invalid")
	}
	if query.Result != "" && !oneOf(query.Result, "SUCCESS", "FAILURE", "DENIED") {
		return validation("result is invalid")
	}
	if query.RiskLevel != "" && !oneOf(query.RiskLevel, "LOW", "MEDIUM", "HIGH", "CRITICAL") {
		return validation("risk_level is invalid")
	}
	if query.OccurredFrom != nil && query.OccurredTo != nil && query.OccurredFrom.After(*query.OccurredTo) {
		return validation("occurred_from must not be after occurred_to")
	}
	return nil
}

// completeCorrelation 只用可信请求上下文补齐缺失的关联字段。子系统主动上报的 request_id 与
// trace_id 描述原业务操作，存在时必须保留；应用、环境、客户端及网络来源则由 HTTP 适配层
// 根据已验证主体绑定，不能在这里从事件正文推断。
func completeCorrelation(ctx context.Context, input EventInput) EventInput {
	input.RequestID = strings.TrimSpace(input.RequestID)
	input.TraceID = strings.TrimSpace(input.TraceID)
	input.CorrelationID = strings.TrimSpace(input.CorrelationID)

	if input.RequestID == "" {
		input.RequestID = strings.TrimSpace(requestctx.RequestID(ctx))
	}
	if input.TraceID == "" {
		input.TraceID = strings.TrimSpace(requestctx.TraceID(ctx))
	}
	if input.CorrelationID == "" {
		input.CorrelationID = strings.TrimSpace(requestctx.CorrelationID(ctx))
	}
	return input
}

func validateBatchDelivery(tenantID string, delivery BatchDeliveryInput) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(delivery.ApplicationCode) == "" || strings.TrimSpace(delivery.EnvironmentCode) == "" || strings.TrimSpace(delivery.ClientID) == "" {
		return validation("tenant, application_code, environment_code and client_id are required for batch delivery")
	}
	if strings.TrimSpace(delivery.RequestID) == "" || strings.TrimSpace(delivery.TraceID) == "" || strings.TrimSpace(delivery.CorrelationID) == "" {
		return validation("delivery request_id, trace_id and correlation_id are required")
	}
	if len(delivery.ClientID) > 128 || len(delivery.RequestID) > 128 || len(delivery.CorrelationID) > 128 {
		return validation("batch delivery field exceeds storage limit")
	}
	if !correlationIdentifierPattern.MatchString(delivery.RequestID) {
		return validation("delivery request_id is invalid")
	}
	if !correlationIdentifierPattern.MatchString(delivery.CorrelationID) {
		return validation("delivery correlation_id is invalid")
	}
	if !traceIDPattern.MatchString(delivery.TraceID) || delivery.TraceID == strings.Repeat("0", 32) {
		return validation("delivery trace_id must be a non-zero 32-character lowercase hexadecimal value")
	}
	return nil
}

func normalizeBatchDelivery(delivery BatchDeliveryInput) BatchDeliveryInput {
	delivery.ApplicationCode = strings.TrimSpace(delivery.ApplicationCode)
	delivery.EnvironmentCode = strings.TrimSpace(delivery.EnvironmentCode)
	delivery.ClientID = strings.TrimSpace(delivery.ClientID)
	delivery.RequestID = strings.TrimSpace(delivery.RequestID)
	delivery.TraceID = strings.TrimSpace(delivery.TraceID)
	delivery.CorrelationID = strings.TrimSpace(delivery.CorrelationID)
	delivery.SourceIP = strings.TrimSpace(delivery.SourceIP)
	delivery.UserAgent = strings.TrimSpace(delivery.UserAgent)
	return delivery
}

func validateInput(tenantID string, input EventInput) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(input.EventID) == "" || strings.TrimSpace(input.ApplicationCode) == "" || strings.TrimSpace(input.EnvironmentCode) == "" || input.OccurredAt.IsZero() || strings.TrimSpace(input.Action) == "" || strings.TrimSpace(input.Result) == "" {
		return validation("event_id, application_code, environment_code, occurred_at, action and result are required")
	}
	if len(input.EventID) > 128 || len(input.Action) > 128 || len(input.Summary) > 1000 || len(input.RequestID) > 128 || len(input.CorrelationID) > 128 {
		return validation("audit field exceeds storage limit")
	}
	if input.RequestID != "" && !correlationIdentifierPattern.MatchString(input.RequestID) {
		return validation("request_id is invalid")
	}
	if input.CorrelationID != "" && !correlationIdentifierPattern.MatchString(input.CorrelationID) {
		return validation("correlation_id is invalid")
	}
	if input.TraceID != "" && (!traceIDPattern.MatchString(input.TraceID) || input.TraceID == strings.Repeat("0", 32)) {
		return validation("trace_id must be a non-zero 32-character lowercase hexadecimal value")
	}
	if !oneOf(input.Result, "SUCCESS", "FAILURE", "DENIED") {
		return validation("result is invalid")
	}
	if input.ResourceType == "" {
		return validation("resource_type is required")
	}
	if input.RiskLevel != "" && !oneOf(input.RiskLevel, "LOW", "MEDIUM", "HIGH", "CRITICAL") {
		return validation("risk_level is invalid")
	}
	return nil
}
func normalizeInput(input EventInput) EventInput {
	input.EventID = strings.TrimSpace(input.EventID)
	input.ApplicationCode = strings.TrimSpace(input.ApplicationCode)
	input.EnvironmentCode = strings.TrimSpace(input.EnvironmentCode)
	input.Action = strings.TrimSpace(input.Action)
	input.ResourceType = strings.TrimSpace(input.ResourceType)
	input.Result = strings.TrimSpace(input.Result)
	if input.ActorType == "" {
		input.ActorType = "SYSTEM"
	}
	if input.RiskLevel == "" {
		input.RiskLevel = "LOW"
	}
	if input.Classification == "" {
		input.Classification = "INTERNAL"
	}
	if input.EventCategory == "" {
		input.EventCategory = "EXTERNAL"
	}
	if input.EventType == "" {
		input.EventType = input.Action
	}
	return input
}
func validation(message string) error { return fmt.Errorf("%w: %s", ErrValidation, message) }
func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

// ClaimExportJob 为指定 worker 领取一个到期任务。领取后只有持有该 worker 标识的执行者
// 可以完成或失败该任务；超时租约由仓储允许其他实例重新领取。
func (s *Service) ClaimExportJob(ctx context.Context, workerID string, staleBefore time.Time) (domain.ExportWork, bool, error) {
	if strings.TrimSpace(workerID) == "" {
		return domain.ExportWork{}, false, validation("worker ID is required")
	}
	return s.repository.ClaimExportJob(ctx, strings.TrimSpace(workerID), s.clock.Now(), staleBefore.UTC())
}

// ListExportEvents 返回导出快照使用的完整过滤结果；这里有硬上限，防止一次导出把审计表
// 和本地内存拖垮，超限任务由 worker 标记为不可自动重试的失败。
func (s *Service) ListExportEvents(ctx context.Context, tenantID string, query domain.ExportQuery) ([]domain.Event, error) {
	return s.repository.ListExportEvents(ctx, tenantID, query)
}

// CompleteExportJob 由导出 worker 记录任务完成与产物元数据。
func (s *Service) CompleteExportJob(ctx context.Context, work domain.ExportWork, file domain.ExportFile) error {
	return s.repository.CompleteExportJob(ctx, work, file, s.clock.Now())
}

// FailExportJob 由导出 worker 记录失败码与安全消息，并按退避时间安排重试。
func (s *Service) FailExportJob(ctx context.Context, work domain.ExportWork, code, message string, retryAt time.Time) error {
	return s.repository.FailExportJob(ctx, work, strings.TrimSpace(code), strings.TrimSpace(message), retryAt.UTC(), s.clock.Now())
}

// GetExportFile 只读取已完成任务关联的文件元数据。物理路径仍是仓储/下载适配器内部信息，
// API 调用方不能通过提交路径选择服务器上的任意文件。
func (s *Service) GetExportFile(ctx context.Context, tenantID, jobID string) (domain.ExportFile, error) {
	if strings.TrimSpace(jobID) == "" {
		return domain.ExportFile{}, validation("job_id is required")
	}
	return s.repository.GetExportFile(ctx, tenantID, strings.TrimSpace(jobID))
}

// IngestionHealth 返回平台侧审计摄取健康度；recentAfter 之前的窗口由调用方决定。
func (service *Service) IngestionHealth(ctx context.Context, tenantID string, recentAfter time.Time) (IngestionHealth, error) {
	if strings.TrimSpace(tenantID) == "" {
		return IngestionHealth{}, ErrValidation
	}
	return service.repository.IngestionHealth(ctx, tenantID, recentAfter)
}

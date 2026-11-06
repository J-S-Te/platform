// Package application 编排只追加审计写入、租户隔离查询和异步导出任务。
package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

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
	// ErrExpired 表示资源曾经存在但已超过保留期或产物已被清理，调用方不应重试。
	ErrExpired = errors.New("audit resource expired")
	// ErrDownloadTicketInvalid 表示下载票据不存在、跨租户/跨任务或已被使用。
	// 三种情形统一语义，避免向调用方区分“猜错的票据”与“已兑换的票据”。
	ErrDownloadTicketInvalid = errors.New("audit export download ticket invalid")
	// ErrDownloadTicketExpired 表示票据存在且未使用，但已超过有效期；调用方应重新申请。
	ErrDownloadTicketExpired = errors.New("audit export download ticket expired")
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
	// DeadLetterPending 是摄取死信表中仍待重放的行数（审计 P1）：平台侧摄取写不进去时
	// 事件进入 audit_dead_letter，该计数大于零即表示存在未被平台持久化的审计事件。
	DeadLetterPending int64 `json:"dead_letter_pending"`
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
	// ListExportEvents 按 (occurred_at DESC, id DESC) 稳定排序读取一批事件；cursor 为零值
	// 时从头开始，limit 有硬上限，保证单批内存可控。
	ListExportEvents(context.Context, string, domain.ExportQuery, domain.ExportCursor, int) (domain.ExportEventPage, error)
	CompleteExportJob(context.Context, domain.ExportWork, domain.ExportFile, time.Time) error
	FailExportJob(context.Context, domain.ExportWork, string, string, time.Time, time.Time) error
	GetExportFile(context.Context, string, string) (domain.ExportFile, error)
	// ListExpiredExportArtifacts 只选择已完成且完成时间早于 completedBefore 的 AUDIT_EXPORT
	// 产物版本；不得返回其他 job_type 或其他模块的文件版本。
	ListExpiredExportArtifacts(context.Context, time.Time, int) ([]domain.ExportArtifact, error)
	// MarkExportArtifactsDeleted 把给定 file_version 行标记为 DELETED；不删除 file_object
	// 或 async_job 行，保留下载审计历史。
	MarkExportArtifactsDeleted(context.Context, []string) error
	// VerifyPayloadHash 按存储行的算法标签重算候选规范 payload 的摘要并比对（HMAC 密钥由实现持有）。
	VerifyPayloadHash(context.Context, string, string, []byte) (domain.PayloadHashVerification, error)
	VerifyEventChain(context.Context, string, string, string, uint64, int) (domain.ChainVerification, error)
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

// SpoolErrorCodeWriteExhausted 是中间件把“同步写入 + 重试都失败”的结果事件落死信时
// 使用的稳定错误码（BIZ-RUNTIME-11）。死信是运营可重放/可忽略的故障证据，因此错误码
// 必须稳定，不能把上游驱动的原始错误文本当分类。
const SpoolErrorCodeWriteExhausted = "AUDIT_EVENT_WRITE_RETRY_EXHAUSTED"

// Spool 把同步写入与重试都失败的事件按审计死信语义（PENDING + 脱敏载荷 + 租户边界）保存，
// 供运营重放或忽略。它与 DeadLetterService.RecordFailure 使用同一套载荷编解码与初始状态，
// 区别只在入口：审计管道故障时，中间件不能让已经生效的业务事实失去审计结论。
//
// 死信仓储与审计仓储通常是同一实现（如 MySQL 仓储）；实现不支持死信时必须失败关闭并
// 返回错误，由调用方按“审计不可用”处理。
func (s *Service) Spool(ctx context.Context, tenantID string, input EventInput) error {
	input = completeCorrelation(ctx, input)
	if err := validateInput(tenantID, input); err != nil {
		return err
	}
	repository, ok := s.repository.(DeadLetterRepository)
	if !ok {
		return errors.New("audit repository does not support dead-letter spooling")
	}
	input = normalizeInput(input)
	now := s.clock.Now().UTC().Truncate(time.Millisecond)
	deadLetterID, err := s.ids.New(now)
	if err != nil {
		return err
	}
	payload, err := marshalDeadLetterEvent(input)
	if err != nil {
		return err
	}
	_, err = repository.CreateDeadLetter(ctx, domain.DeadLetter{
		DeadLetterID:     deadLetterID,
		TenantID:         strings.TrimSpace(tenantID),
		ApplicationCode:  input.ApplicationCode,
		EnvironmentCode:  input.EnvironmentCode,
		EventID:          input.EventID,
		Status:           domain.DeadLetterPending,
		Payload:          payload,
		LastErrorCode:    []byte(SpoolErrorCodeWriteExhausted),
		LastErrorMessage: []byte("audit event write failed after synchronous retry"),
		Attempts:         1,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	return err
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

// VerifyPayloadHash 重算候选事件输入的完整性摘要并与存储行比对（审计 P0 · 防篡改取证）。
// 规范化方式与摄取一致：先 normalizeInput、再对 EventInput 做结构体 JSON 序列化
// （字段顺序由结构体固定），因此调用方只需提供与摄取时相同的字段值，不必复刻原始字节。
func (s *Service) VerifyPayloadHash(ctx context.Context, tenantID, eventID string, input EventInput) (domain.PayloadHashVerification, error) {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return domain.PayloadHashVerification{}, validation("event_id is required")
	}
	if strings.TrimSpace(input.EventID) != "" && strings.TrimSpace(input.EventID) != eventID {
		return domain.PayloadHashVerification{}, validation("payload EventID must match the path event_id")
	}
	payload, err := json.Marshal(normalizeInput(input))
	if err != nil {
		return domain.PayloadHashVerification{}, fmt.Errorf("marshal audit payload: %w", err)
	}
	return s.repository.VerifyPayloadHash(ctx, tenantID, eventID, payload)
}

// VerifyEventChain 校验（租户、应用、环境）维度的哈希链分段：自证每行 chain_hash 可由
// chain_prev_hash 与 payload_hash 重算，且窗口内 chain_sequence 连续。链首（sequence=1）
// 额外要求 prev 为全零。校验调用本身会被通用审计轨迹记录。
func (s *Service) VerifyEventChain(ctx context.Context, tenantID, applicationCode, environmentCode string, fromSequence uint64, limit int) (domain.ChainVerification, error) {
	if strings.TrimSpace(applicationCode) == "" || strings.TrimSpace(environmentCode) == "" {
		return domain.ChainVerification{}, validation("application and environment are required")
	}
	return s.repository.VerifyEventChain(ctx, tenantID, strings.TrimSpace(applicationCode), strings.TrimSpace(environmentCode), fromSequence, limit)
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
	if query.Result != "" && !oneOf(query.Result, "SUCCESS", "FAILURE", "DENIED", "REQUESTED") {
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

// 变更摘要与自由文本快照的存储上限。超限一律按校验失败拒绝（HTTP 422），不做静默
// 截断：截断会让审计记录"看起来像"原始事实，却丢失真实取值。
const (
	// MaxChangeSummaryEntries 是单事件 change_summary 的最大条数。
	MaxChangeSummaryEntries = 100
	// MaxChangeSummaryValueRunes 是 change_summary 单个 before/after 值的最大长度。
	MaxChangeSummaryValueRunes = 4096
	// 以下上限与 audit_event 的 actor_name_snapshot / resource_name_snapshot /
	// reason_code / business_id 列宽（字符数）对齐，避免把超长自由文本留给 DB 报错。
	MaxActorNameRunes    = 128
	MaxResourceNameRunes = 255
	MaxReasonCodeRunes   = 128
	MaxBusinessIDRunes   = 128
)

// ValidateChangeSummary 限制变更摘要的条数与单值长度。摄取边界（HTTP 适配层打码后）
// 与应用服务都会调用它：前者尽早返回 422，后者覆盖平台内部写入。
func ValidateChangeSummary(changes []domain.FieldChange) error {
	if len(changes) > MaxChangeSummaryEntries {
		return validation(fmt.Sprintf("change_summary must not exceed %d entries", MaxChangeSummaryEntries))
	}
	for _, change := range changes {
		if length, err := changeSummaryValueRunes(change.Before); err != nil {
			return err
		} else if length > MaxChangeSummaryValueRunes {
			return validation(fmt.Sprintf("change_summary before of field %q exceeds %d characters", change.Field, MaxChangeSummaryValueRunes))
		}
		if length, err := changeSummaryValueRunes(change.After); err != nil {
			return err
		} else if length > MaxChangeSummaryValueRunes {
			return validation(fmt.Sprintf("change_summary after of field %q exceeds %d characters", change.Field, MaxChangeSummaryValueRunes))
		}
	}
	return nil
}

// changeSummaryValueRunes 统计变更值的字符数：字符串按 rune 计，其它 JSON 值按序列化
// 结果计——避免调用方用嵌套对象绕过单值长度上限。
func changeSummaryValueRunes(value any) (int, error) {
	switch typed := value.(type) {
	case nil:
		return 0, nil
	case string:
		return utf8.RuneCountInString(typed), nil
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return 0, validation("change_summary value must be JSON serializable")
		}
		return utf8.RuneCountInString(string(encoded)), nil
	}
}

func validateInput(tenantID string, input EventInput) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(input.EventID) == "" || strings.TrimSpace(input.ApplicationCode) == "" || strings.TrimSpace(input.EnvironmentCode) == "" || input.OccurredAt.IsZero() || strings.TrimSpace(input.Action) == "" || strings.TrimSpace(input.Result) == "" {
		return validation("event_id, application_code, environment_code, occurred_at, action and result are required")
	}
	if len(input.EventID) > 128 || len(input.Action) > 128 || len(input.Summary) > 1000 || len(input.RequestID) > 128 || len(input.CorrelationID) > 128 {
		return validation("audit field exceeds storage limit")
	}
	if err := ValidateChangeSummary(input.Changes); err != nil {
		return err
	}
	// 自由文本快照字段同样限长：它们会原样落库并展示给审计读者，超长既是存储风险
	// 也是日志/报表放大面。
	if utf8.RuneCountInString(input.ActorName) > MaxActorNameRunes {
		return validation("actor_name exceeds storage limit")
	}
	if utf8.RuneCountInString(input.ResourceName) > MaxResourceNameRunes {
		return validation("resource_name exceeds storage limit")
	}
	if utf8.RuneCountInString(input.ReasonCode) > MaxReasonCodeRunes {
		return validation("reason_code exceeds storage limit")
	}
	if utf8.RuneCountInString(input.BusinessID) > MaxBusinessIDRunes {
		return validation("business_id exceeds storage limit")
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
	if !oneOf(input.Result, "SUCCESS", "FAILURE", "DENIED", "REQUESTED") {
		// REQUESTED 是 BIZ-RUNTIME-11 的“意图事件”结论：业务尚未执行、结果待定。
		// 它只在必开模式的意图/结果配对里出现，消费方应按 metadata.phase 区分。
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

// ExportEventBatchSize 是审计导出分批读取的默认单批行数，同时作为单批上限。
// 导出不再对总量设硬失败阈值：worker 按游标边读边写，内存只与单批大小相关。
const ExportEventBatchSize = 1000

// ExportArtifactCleanupBatchSize 是保留期清理单次选择的产物上限，避免一次锁住过多行。
const ExportArtifactCleanupBatchSize = 200

// ListExportEvents 分批返回导出快照的过滤结果。total 不再设上限，因此超过任意总量的
// 导出都能完成；limit 被收敛到 [1, ExportEventBatchSize]，防止调用方绕过分批约束。
func (s *Service) ListExportEvents(ctx context.Context, tenantID string, query domain.ExportQuery, cursor domain.ExportCursor, limit int) (domain.ExportEventPage, error) {
	if strings.TrimSpace(tenantID) == "" {
		return domain.ExportEventPage{}, validation("tenant_id is required")
	}
	if limit < 1 || limit > ExportEventBatchSize {
		limit = ExportEventBatchSize
	}
	return s.repository.ListExportEvents(ctx, strings.TrimSpace(tenantID), query, cursor, limit)
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
// API 调用方不能通过提交路径选择服务器上的任意文件。产物版本被保留期清理标记为
// DELETED/REMOVED 后返回 ErrExpired，下载端据此返回明确的“已过期/不可用”。
func (s *Service) GetExportFile(ctx context.Context, tenantID, jobID string) (domain.ExportFile, error) {
	if strings.TrimSpace(jobID) == "" {
		return domain.ExportFile{}, validation("job_id is required")
	}
	file, err := s.repository.GetExportFile(ctx, tenantID, strings.TrimSpace(jobID))
	if err != nil {
		return domain.ExportFile{}, err
	}
	if !exportFileAvailable(file.Status) {
		return domain.ExportFile{}, ErrExpired
	}
	return file, nil
}

// exportFileAvailable 判定导出产物是否仍可下载：只有明确标记为已删除/已移除的版本
// 视为过期；空状态（历史数据或未选择该列）按可用处理，避免误伤既有导出。
func exportFileAvailable(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "DELETED", "REMOVED":
		return false
	default:
		return true
	}
}

// ExportDownloadTicketTTL 是一次性导出下载票据的有效期。票据只覆盖“点下载 → 取字节”
// 这一小段窗口，短 TTL 让被复制的票据几乎无法再被利用；过期后必须重新申请。
const ExportDownloadTicketTTL = 2 * time.Minute

// ExportDownloadTicketRepository 是一次性导出下载票据的持久化能力。
//
// 与 DeadLetterRepository 一样按能力接口声明：只需要读写审计事件的实现不必伪造票据存储；
// 生产仓储（MySQL）必须同时实现两者。
type ExportDownloadTicketRepository interface {
	// CreateExportDownloadTicket 只写入票据摘要；实现不得保存或返回明文 token。
	CreateExportDownloadTicket(context.Context, domain.ExportDownloadTicket) error
	// ConsumeExportDownloadTicket 必须用一条条件 UPDATE（used_at IS NULL 且未过期）
	// 原子核销，返回是否命中。并发兑换同一票据时最多一个调用返回 Consumed。
	ConsumeExportDownloadTicket(context.Context, string, string, []byte, time.Time) (domain.ExportDownloadTicketState, error)
}

// IssueExportDownloadTicket 为已完成的导出任务签发一次性下载票据，返回票据记录与明文
// token。明文 token 只在这里出现一次：调用方必须只把它放进签发响应，不得记录日志、
// 写入审计载荷或持久化；服务端只保存 SHA-256 摘要。
//
// 签发前置条件：任务属于当前租户、状态 SUCCEEDED，且产物版本仍可下载。磁盘文件是否
// 真实存在由 HTTP 适配层（持有存储根）检查，避免服务层假设本地存储布局。
func (s *Service) IssueExportDownloadTicket(ctx context.Context, tenantID, jobID string) (domain.ExportDownloadTicket, string, error) {
	tenantID = strings.TrimSpace(tenantID)
	jobID = strings.TrimSpace(jobID)
	if tenantID == "" {
		return domain.ExportDownloadTicket{}, "", validation("tenant_id is required")
	}
	if jobID == "" {
		return domain.ExportDownloadTicket{}, "", validation("job_id is required")
	}
	repository, ok := s.repository.(ExportDownloadTicketRepository)
	if !ok {
		return domain.ExportDownloadTicket{}, "", errors.New("audit repository does not support export download tickets")
	}
	// GetExportFile 的查询同时强制（租户、SUCCEEDED、AUDIT_EXPORT）三个条件，因此这里
	// 不需要再单独判断任务状态：未完成、失败或属于其他租户的任务都是 ErrNotFound。
	file, err := s.repository.GetExportFile(ctx, tenantID, jobID)
	if err != nil {
		return domain.ExportDownloadTicket{}, "", err
	}
	if !exportFileAvailable(file.Status) {
		return domain.ExportDownloadTicket{}, "", ErrExpired
	}
	now := s.clock.Now().UTC()
	id, err := s.ids.New(now)
	if err != nil {
		return domain.ExportDownloadTicket{}, "", fmt.Errorf("generate audit export download ticket ID: %w", err)
	}
	token, digest, err := newExportDownloadTicketToken()
	if err != nil {
		return domain.ExportDownloadTicket{}, "", err
	}
	ticket := domain.ExportDownloadTicket{ID: id, TenantID: tenantID, JobID: jobID, TokenHash: digest, ExpiresAt: now.Add(ExportDownloadTicketTTL), CreatedAt: now}
	if err := repository.CreateExportDownloadTicket(ctx, ticket); err != nil {
		return domain.ExportDownloadTicket{}, "", err
	}
	return ticket, token, nil
}

// ConsumeExportDownloadTicket 原子核销一次性下载票据。只有未使用且未过期的、属于
// （租户, 任务）的票据能核销成功；其余情形分别返回 ErrDownloadTicketInvalid（不存在/
// 跨范围/已用）与 ErrDownloadTicketExpired（已过期），由 HTTP 层映射为 403/410。
func (s *Service) ConsumeExportDownloadTicket(ctx context.Context, tenantID, jobID, token string) error {
	tenantID = strings.TrimSpace(tenantID)
	jobID = strings.TrimSpace(jobID)
	if tenantID == "" {
		return validation("tenant_id is required")
	}
	if jobID == "" {
		return validation("job_id is required")
	}
	repository, ok := s.repository.(ExportDownloadTicketRepository)
	if !ok {
		return errors.New("audit repository does not support export download tickets")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrDownloadTicketInvalid
	}
	digest := sha256.Sum256([]byte(token))
	state, err := repository.ConsumeExportDownloadTicket(ctx, tenantID, jobID, digest[:], s.clock.Now().UTC())
	if err != nil {
		return err
	}
	switch state {
	case domain.ExportDownloadTicketConsumed:
		return nil
	case domain.ExportDownloadTicketExpired:
		return ErrDownloadTicketExpired
	default:
		return ErrDownloadTicketInvalid
	}
}

// newExportDownloadTicketToken 生成 32 字节随机 token（base64url 无填充）并返回其
// SHA-256 摘要。32 字节熵远高于可暴力猜测的范围；摘要用于入库，明文只返回给签发调用方。
func newExportDownloadTicketToken() (string, []byte, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", nil, fmt.Errorf("generate audit export download ticket: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(data)
	digest := sha256.Sum256([]byte(token))
	return token, digest[:], nil
}

// ListExpiredExportArtifacts 选择超过保留期的已完成 AUDIT_EXPORT 产物，供 worker 删除
// 磁盘文件并标记版本。只处理审计导出任务，其他 job_type 与其他模块的文件不受影响。
func (s *Service) ListExpiredExportArtifacts(ctx context.Context, completedBefore time.Time, limit int) ([]domain.ExportArtifact, error) {
	if completedBefore.IsZero() {
		return nil, validation("completed_before is required")
	}
	if limit < 1 || limit > ExportArtifactCleanupBatchSize {
		limit = ExportArtifactCleanupBatchSize
	}
	return s.repository.ListExpiredExportArtifacts(ctx, completedBefore.UTC(), limit)
}

// MarkExportArtifactsDeleted 把已清理磁盘文件的版本标记为 DELETED。只改写 file_version.status，
// 保留 file_object/async_job 行，因而下载审计历史与任务状态不受影响。
func (s *Service) MarkExportArtifactsDeleted(ctx context.Context, fileVersionIDs []string) error {
	normalized := make([]string, 0, len(fileVersionIDs))
	for _, id := range fileVersionIDs {
		if id = strings.TrimSpace(id); id != "" {
			normalized = append(normalized, id)
		}
	}
	if len(normalized) == 0 {
		return nil
	}
	return s.repository.MarkExportArtifactsDeleted(ctx, normalized)
}

// IngestionHealth 返回平台侧审计摄取健康度；recentAfter 之前的窗口由调用方决定。
func (service *Service) IngestionHealth(ctx context.Context, tenantID string, recentAfter time.Time) (IngestionHealth, error) {
	if strings.TrimSpace(tenantID) == "" {
		return IngestionHealth{}, ErrValidation
	}
	return service.repository.IngestionHealth(ctx, tenantID, recentAfter)
}

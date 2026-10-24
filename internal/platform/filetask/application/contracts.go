// Package application 协调安全的本地文件存储与通用的 MySQL 异步任务。
package application

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

// FileRepository 持久化文件元数据。文件系统有意被挡在 LocalStore 之后：
// MySQL 与本地文件系统无法共享同一个分布式事务，元数据与字节必须分开提交。
type FileRepository interface {
	CreateWriting(context.Context, domain.File, domain.FileVersion) error
	ReserveUpload(context.Context, domain.File, domain.FileVersion) (stored domain.StoredFile, created bool, err error)
	MarkValidating(context.Context, string, string, uint64, []byte, time.Time) error
	MarkReady(context.Context, string, string, time.Time) error
	MarkRejected(context.Context, string, string, time.Time) error
	MarkFailed(context.Context, string, string, time.Time) error
	GetAvailable(context.Context, string, string) (domain.StoredFile, error)
	// ClaimExpiredUnbound 领取"临时类 + 保留期已到 + 创建早于截止点 + 无活动绑定"的一条文件。
	ClaimExpiredUnbound(context.Context, string, time.Time, time.Time) (domain.StoredFile, bool, error)
	MarkDeleted(context.Context, string, string, uint64, time.Time) error
	ReleaseCleanupClaim(context.Context, string, string, string, uint64, time.Time) error
	// ListStaleDeleting 列出超过截止时间仍停留在 DELETING 的遗留记录，供受控对账逐条处理。
	ListStaleDeleting(context.Context, string, time.Time, int) ([]domain.StoredFile, error)
	CreateBinding(context.Context, domain.FileBinding) (domain.FileBinding, error)
	DeactivateBinding(context.Context, string, string, string, string, time.Time) error
	HasActiveBinding(context.Context, string, string, string, string, string) (bool, error)
	HasAnyActiveBinding(context.Context, string, string, string) (bool, error)
	ListRecoveryCandidates(context.Context, string, time.Time, int) ([]domain.StoredFile, error)
	// PendingUploadSession 返回与该文件关联且仍停留在 UPLOADING 的上传会话。
	PendingUploadSession(context.Context, string, string) (domain.UploadSessionRecovery, bool, error)
	// MarkUploadSession 只收敛仍处于 UPLOADING 的会话（READY/REJECTED/FAILED），
	// 已终结的会话不会被覆盖，重复对账保持幂等。
	MarkUploadSession(context.Context, string, string, string, string, time.Time) error
}

// JobRepository 拥有通用 async_job 的状态迁移。领取必须在短事务内加行锁完成；
// worker 在执行慢操作期间绝不持有数据库锁。
type JobRepository interface {
	CreateJob(context.Context, domain.Job) (domain.Job, error)
	ListJobs(context.Context, string, domain.PageRequest) (domain.PageResult[domain.Job], error)
	GetJob(context.Context, string, string) (domain.Job, error)
	ClaimJob(context.Context, string, []string, time.Time, time.Time) (domain.Job, bool, error)
	CompleteJob(context.Context, domain.Job, time.Time) error
	FailJob(context.Context, domain.Job, string, string, bool, time.Time, time.Time) error
	CancelJob(context.Context, string, string, time.Time) error
	RetryJob(context.Context, string, string, time.Time) error
	CreateRerun(context.Context, domain.Job) (domain.Job, error)
}

// LocalStore 在单一可信根之下写入与打开文件。实现必须拒绝路径穿越和符号链接逃逸，
// 即使存储元数据已被破坏。
type LocalStore interface {
	WriteAtomically(context.Context, string, io.Reader, int64) (size uint64, sha256 []byte, err error)
	OpenVerified(string) (io.ReadSeekCloser, error)
	Remove(string) error
	CleanupTemporary(time.Time) (int, error)
	// Exists 判断命名空间内是否仍存在该常规文件；对账用它区分"磁盘已删"与"字节仍在"。
	Exists(string) (bool, error)
}

// StagedLocalStore 让不可信字节在验证通过前保持在正式命名空间之外。Stage 标识是
// 不透明的实现值，绝不能持久化或返回给客户端。
type StagedLocalStore interface {
	Stage(context.Context, io.Reader, int64) (stageID string, size uint64, sha256 []byte, err error)
	OpenStaged(string) (io.ReadSeekCloser, error)
	PublishStaged(string, string) error
	QuarantineStaged(string) error
	RemoveStaged(string) error
}

// IDGenerator 给应用层提供单调的公开标识符，而不把它耦合到具体数据库实现。
type IDGenerator interface {
	New(time.Time) (string, error)
}

// Clock 使模块测试中的过期与重试行为可确定。
type Clock interface{ Now() time.Time }

// SystemClock 是生产环境的 UTC 时钟。
type SystemClock struct{}

// Now 返回当前 UTC 时间。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// UploadPolicy 控制模块的上传边界。平台有意采用白名单，而不信任浏览器 MIME 头。
type UploadPolicy struct {
	MaxBytes          int64
	AllowedMediaTypes map[string]struct{}
}

// UploadInput 是请求本地的二进制输入。内容只被消费一次，绝不写入日志。
type UploadInput struct {
	TenantID, ApplicationID, OwnerUserID      string
	Namespace, Purpose, AuthenticatedClientID string
	PolicyVersion, RetentionClass             string
	RetentionUntil                            *time.Time
	OriginalName, DeclaredMediaType           string
	Classification, RequestID                 string
	PreallocatedFileID, PreallocatedVersionID string
	ExpectedSize                              uint64
	ExpectedSHA256                            []byte
	Content                                   io.Reader
}

// DownloadAccess 携带服务端认证出的主体及其已解析的权限。HTTP 客户端不能直接填充；
// 处理器从 authctx 读取。
type DownloadAccess struct {
	TenantID, UserID            string
	ApplicationID, ResourceType string
	ResourceID                  string
	PermissionCodes             []string
	ResourceAccessVerified      bool
}

// BindingInput 创建文件到业务资源的 ACTIVE 绑定。ApplicationID 必须与文件归属一致，
// 以免一个子系统借绑定接口接管另一个子系统的文件。
type BindingInput struct {
	TenantID, ApplicationID, FileID string
	ResourceType, ResourceID        string
	BindingType, DisplayName        string
	SortOrder                       int
	OperatorUserID                  string
}

// JobCreateInput 创建应用自有的后台任务。Payload 必须是合法 JSON，且不得包含
// 密码、令牌、上传原始内容或其他凭据材料。
type JobCreateInput struct {
	TenantID, ApplicationID string
	JobType, AggregateType  string
	AggregateID             string
	IdempotencyKey          string
	RequestID, TraceID      string
	CorrelationID           string
	BusinessRef             string
	Payload                 json.RawMessage
	Priority                int
	MaxAttempts             uint
	AvailableAt             *time.Time
}

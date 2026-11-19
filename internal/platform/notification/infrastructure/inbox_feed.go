package infrastructure

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

const (
	inboxFeedInterval = 750 * time.Millisecond
	inboxFeedBatch    = 128
)

type inboxFeedKey struct {
	TenantID, RecipientUserID string
}

type inboxFeedVersion struct {
	Key       inboxFeedKey `gorm:"embedded"`
	Revision  uint64
	VisibleAt *time.Time
}

type inboxFeedAudience struct {
	last   inboxFeedVersion
	loaded bool
	tabs   map[chan struct{}]struct{}
}

// InboxFeed 在每个 API 进程中汇总在线订阅，并批量读取同一份已提交的收件箱版本。
type InboxFeed struct {
	database  *gorm.DB
	logger    *slog.Logger
	context   context.Context
	cancel    context.CancelFunc
	lock      sync.Mutex
	audiences map[inboxFeedKey]*inboxFeedAudience
	running   bool
}

// NewInboxFeed 用途：创建平台收件箱的在线变更分发器。
// 参数：database 为平台数据库，logger 为投递日志记录器。
// 返回值：按收件人合并读取的分发器。
func NewInboxFeed(database *gorm.DB, logger *slog.Logger) *InboxFeed {
	ctx, cancel := context.WithCancel(context.Background())
	return &InboxFeed{
		database: database, logger: logger, context: ctx, cancel: cancel,
		audiences: make(map[inboxFeedKey]*inboxFeedAudience),
	}
}

// Subscribe 用途：登记当前进程的一个连接，并按需启动后台查询。
// 参数：tenantID 和 userID 为经过认证的租户和收件人。
// 返回值：容量为一的可合并变更通道，以及连接退出时的注销函数。
func (feed *InboxFeed) Subscribe(tenantID, userID string) (<-chan struct{}, func()) {
	key := inboxFeedKey{tenantID, userID}
	changes := make(chan struct{}, 1)
	feed.lock.Lock()
	if feed.context.Err() != nil {
		feed.lock.Unlock()
		close(changes)
		return changes, func() {}
	}
	audience := feed.audiences[key]
	if audience == nil {
		audience = &inboxFeedAudience{tabs: make(map[chan struct{}]struct{})}
		feed.audiences[key] = audience
	}
	audience.tabs[changes] = struct{}{}
	if !feed.running {
		feed.running = true
		go feed.run()
	}
	feed.lock.Unlock()
	return changes, func() {
		feed.lock.Lock()
		defer feed.lock.Unlock()
		delete(audience.tabs, changes)
		if len(audience.tabs) == 0 && feed.audiences[key] == audience {
			delete(feed.audiences, key)
		}
	}
}

// Close 用途：停止数据库查询并结束所有 SSE 订阅，供 HTTP 排空前调用。
// 参数：无。
// 返回值：无；可安全重复调用。
func (feed *InboxFeed) Close() {
	feed.lock.Lock()
	defer feed.lock.Unlock()
	feed.cancel()
	for _, audience := range feed.audiences {
		for tab := range audience.tabs {
			close(tab)
		}
	}
	clear(feed.audiences)
}

// run 用途：定期收集在线收件人并读取变更，最后一个连接退出后停止。
// 参数：无。
// 返回值：无。
func (feed *InboxFeed) run() {
	ticker := time.NewTicker(inboxFeedInterval)
	defer ticker.Stop()
	for {
		feed.lock.Lock()
		if len(feed.audiences) == 0 {
			feed.running = false
			feed.lock.Unlock()
			return
		}
		keys := make([]inboxFeedKey, 0, len(feed.audiences))
		for key := range feed.audiences {
			keys = append(keys, key)
		}
		feed.lock.Unlock()
		for offset := 0; offset < len(keys) && feed.context.Err() == nil; offset += inboxFeedBatch {
			feed.poll(keys[offset:min(offset+inboxFeedBatch, len(keys))])
		}
		select {
		case <-ticker.C:
		case <-feed.context.Done():
			return
		}
	}
}

// poll 用途：将一批当前收件人的版本与上次成功快照比较，合并通知在线连接。
// 参数：keys 为本批在线的租户及收件人键。
// 返回值：无。
func (feed *InboxFeed) poll(keys []inboxFeedKey) {
	ctx, cancel := context.WithTimeout(feed.context, 2*time.Second)
	defer cancel()
	versions, err := feed.readVersions(ctx, keys)
	if err != nil {
		if feed.context.Err() == nil {
			feed.logger.Warn("read notification inbox revisions", "error", err)
		}
		return
	}
	feed.lock.Lock()
	defer feed.lock.Unlock()
	for _, version := range versions {
		audience := feed.audiences[version.Key]
		if audience == nil {
			continue
		}
		if !audience.loaded || version.changedSince(audience.last) {
			for tab := range audience.tabs {
				select {
				case tab <- struct{}{}:
				default:
				}
			}
		}
		audience.last, audience.loaded = version, true
	}
}

// changedSince 用途：检测已提交的写入或计划提醒自然到期。
// 参数：previous 为该收件人上次成功读取的快照。
// 返回值：需要浏览器重新读取时为真。
func (version inboxFeedVersion) changedSince(previous inboxFeedVersion) bool {
	if version.Revision != previous.Revision {
		return true
	}
	if version.VisibleAt == nil || previous.VisibleAt == nil {
		return version.VisibleAt != previous.VisibleAt
	}
	return !version.VisibleAt.Equal(*previous.VisibleAt)
}

// readVersions 用途：在同一个语句快照里读取即时变更版本与最近到期的计划提醒。
// 参数：ctx 为本次批查询截止上下文，keys 为非空的在线收件人键集合。
// 返回值：包含无历史通知用户的完整快照，或查询错误。
func (feed *InboxFeed) readVersions(ctx context.Context, keys []inboxFeedKey) ([]inboxFeedVersion, error) {
	rows := make([]string, 0, len(keys))
	args := make([]any, 0, 2*len(keys))
	for _, key := range keys {
		rows = append(rows, "SELECT CAST(? AS CHAR CHARACTER SET ascii) COLLATE ascii_bin AS tenant_id, "+
			"CAST(? AS CHAR CHARACTER SET ascii) COLLATE ascii_bin AS recipient_user_id")
		args = append(args, key.TenantID, key.RecipientUserID)
	}
	query := `SELECT active.tenant_id, active.recipient_user_id, COALESCE(r.revision, 0) AS revision,
		(SELECT MAX(d.scheduled_visible_at) FROM notification_delivery d
		 WHERE d.tenant_id = active.tenant_id AND d.recipient_user_id = active.recipient_user_id
		 AND d.scheduled_visible_at <= UTC_TIMESTAMP(3)) AS visible_at
		FROM (` + strings.Join(rows, " UNION ALL ") + `) active
		LEFT JOIN notification_inbox_revision r
		  ON r.tenant_id = active.tenant_id AND r.recipient_user_id = active.recipient_user_id`
	var versions []inboxFeedVersion
	err := feed.database.WithContext(ctx).Raw(query, args...).Scan(&versions).Error
	return versions, err
}

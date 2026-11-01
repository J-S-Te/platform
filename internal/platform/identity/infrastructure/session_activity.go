package infrastructure

import (
	"gorm.io/gorm"
	"time"
)

// withSessionActivity 在账号并发登录判定中使用同一中央活动窗口。
// 参数：query为IAM会话查询，now为时钟，cutoff为空闲边界，active选择有效或无效窗口。
// 返回值：叠加活动条件的查询；启用中央后不会用本地last_seen推断续期。
func (repository *GORMRepository) withSessionActivity(query *gorm.DB, now, cutoff time.Time, active bool) *gorm.DB {
	if repository.central == nil {
		if active {
			return query.Where("COALESCE(last_interactive_at,last_seen_at) > ?", cutoff)
		}
		return query.Where("COALESCE(last_interactive_at,last_seen_at) <= ?", cutoff)
	}
	predicate := `EXISTS (SELECT 1 FROM sgov_platform_link AS link
		JOIN sgov_session AS central ON central.id=link.session_id
		WHERE link.iam_session_id=iam_session.id AND central.revoked_at IS NULL
		AND central.expires_at > ? AND central.last_interactive_at > ?)`
	if !active {
		predicate = "NOT " + predicate
	}
	return query.Where(predicate, now, cutoff)
}

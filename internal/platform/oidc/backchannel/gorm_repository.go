package backchannel

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// GORMRepository 持久化 Back-Channel Logout 投递队列，并以数据库租约防止多 Worker 重复发送。
type GORMRepository struct {
	DB          *gorm.DB
	Lease       time.Duration
	MaxAttempts int
}

// claimSelectSQL 只领取"端点已登记且未达上限"的到期任务。它有意排除 NO_ENDPOINT：
// 缺少登记是配置缺陷，不是可无限重试的传输故障（BIZ-XEVT2-07）。
const claimSelectSQL = `SELECT o.id,c.client_id,o.session_id,o.subject_id,o.jti,o.attempt_count AS attempts,u.logout_uri FROM platform_oauth_backchannel_logout_outbox o JOIN platform_oauth_backchannel_logout_uri u ON u.oauth_client_id=o.oauth_client_id JOIN platform_oauth_client c ON c.id=o.oauth_client_id AND c.status='ACTIVE' WHERE ((o.status IN ('PENDING','RETRY') AND o.next_attempt_at<=?) OR (o.status='PROCESSING' AND o.locked_until<?)) AND o.attempt_count<? ORDER BY o.next_attempt_at,o.id LIMIT ? FOR UPDATE SKIP LOCKED`

// demoteMissingEndpointSQL 把"待投递但端点已被撤销"的行降级为 NO_ENDPOINT。
// 否则这些行会被 Claim 的 JOIN 静默跳过：既不会投递，也不会计入任何告警。
const demoteMissingEndpointSQL = `UPDATE platform_oauth_backchannel_logout_outbox o SET o.status=?,o.locked_until=NULL,o.last_error=? WHERE o.status IN (?,?) AND NOT EXISTS (SELECT 1 FROM platform_oauth_backchannel_logout_uri u WHERE u.oauth_client_id=o.oauth_client_id)`

// rearmRegisteredEndpointSQL 在补齐登记后把 NO_ENDPOINT 行重新武装为可投递。
// 这消除了"事后补登记也无法补投"的遗留缺口：logout_token 在发送时签发，延迟投递安全。
const rearmRegisteredEndpointSQL = `UPDATE platform_oauth_backchannel_logout_outbox o SET o.status=?,o.next_attempt_at=?,o.last_error=NULL WHERE o.status=? AND EXISTS (SELECT 1 FROM platform_oauth_backchannel_logout_uri u WHERE u.oauth_client_id=o.oauth_client_id AND u.logout_uri<>'')`

// Claim 在短事务中使用 FOR UPDATE SKIP LOCKED 抢占到期任务；同一事务内先同步
// NO_ENDPOINT 状态，保证"缺少登记"始终可见而不会被 JOIN 静默吞掉。
func (r *GORMRepository) Claim(ctx context.Context, now time.Time, limit int) ([]Message, error) {
	if r == nil || r.DB == nil || limit <= 0 {
		return nil, errors.New("back-channel logout repository is not configured")
	}
	lease := r.Lease
	if lease <= 0 {
		lease = 30 * time.Second
	}
	max := r.MaxAttempts
	if max <= 0 {
		max = 8
	}
	var result []Message
	err := r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 端点缺失/补齐的状态迁移先于领取执行：单次抓取即可让"不可投递"与
		// "可投递"两侧都收敛，无需额外的补偿 worker。
		if err := tx.Exec(demoteMissingEndpointSQL, StatusNoEndpoint, ReasonMissingLogoutURI, StatusPending, StatusRetry).Error; err != nil {
			return err
		}
		if err := tx.Exec(rearmRegisteredEndpointSQL, StatusPending, now.UTC(), StatusNoEndpoint).Error; err != nil {
			return err
		}
		var rows []backchannelOutboxRow
		if err := tx.Raw(claimSelectSQL, now.UTC(), now.UTC(), max, limit).Scan(&rows).Error; err != nil {
			return err
		}
		until := now.UTC().Add(lease)
		for _, row := range rows {
			if err := tx.Exec(`UPDATE platform_oauth_backchannel_logout_outbox SET status=?, locked_until=?, attempt_count=attempt_count+1 WHERE id=?`, StatusProcessing, until, row.ID).Error; err != nil {
				return err
			}
			result = append(result, Message{ID: row.ID, Audience: row.ClientID, Subject: row.SubjectID, Session: row.SessionID, JTI: row.JTI, URI: row.URI, Attempt: row.Attempts + 1})
		}
		return nil
	})
	return result, err
}

type backchannelOutboxRow struct {
	ID, ClientID, SessionID, SubjectID, JTI, URI string
	Attempts                                     int
}

// Complete 将处理中的任务标记为已投递。
func (r *GORMRepository) Complete(ctx context.Context, id string, now time.Time) error {
	if r == nil || r.DB == nil || id == "" {
		return errors.New("back-channel logout repository is not configured")
	}
	result := r.DB.WithContext(ctx).Exec(`UPDATE platform_oauth_backchannel_logout_outbox SET status=?, delivered_at=?, locked_until=NULL, last_error=NULL WHERE id=? AND status=?`, StatusDelivered, now.UTC(), id, StatusProcessing)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("back-channel logout delivery lease is no longer owned")
	}
	return nil
}

// Fail 递增尝试次数并以指数退避安排下一次投递；达到上限后进入 FAILED。
func (r *GORMRepository) Fail(ctx context.Context, id, summary string, next time.Time) error {
	if r == nil || r.DB == nil || id == "" {
		return errors.New("back-channel logout repository is not configured")
	}
	max := r.MaxAttempts
	if max <= 0 {
		max = 8
	}
	if len(summary) > 255 {
		summary = summary[:255]
	}
	result := r.DB.WithContext(ctx).Exec(`UPDATE platform_oauth_backchannel_logout_outbox SET status=CASE WHEN attempt_count>=? THEN ? ELSE ? END, next_attempt_at=?, locked_until=NULL, last_error=? WHERE id=? AND status=?`, max, StatusFailed, StatusRetry, next.UTC(), summary, id, StatusProcessing)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("back-channel logout delivery lease is no longer owned")
	}
	return nil
}

// RetryDelay 返回有上限的指数退避间隔。
func RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

var _ Delivery = (*GORMRepository)(nil)

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

// Claim 在短事务中使用 FOR UPDATE SKIP LOCKED 抢占到期任务。
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
		var rows []backchannelOutboxRow
		query := `SELECT o.id,c.client_id,o.session_id,o.subject_id,o.jti,o.attempt_count AS attempts,u.logout_uri FROM platform_oauth_backchannel_logout_outbox o JOIN platform_oauth_backchannel_logout_uri u ON u.oauth_client_id=o.oauth_client_id JOIN platform_oauth_client c ON c.id=o.oauth_client_id AND c.status='ACTIVE' WHERE ((o.status IN ('PENDING','RETRY') AND o.next_attempt_at<=?) OR (o.status='PROCESSING' AND o.locked_until<?)) AND o.attempt_count<? ORDER BY o.next_attempt_at,o.id LIMIT ? FOR UPDATE SKIP LOCKED`
		if err := tx.Raw(query, now.UTC(), now.UTC(), max, limit).Scan(&rows).Error; err != nil {
			return err
		}
		until := now.UTC().Add(lease)
		for _, row := range rows {
			if err := tx.Exec(`UPDATE platform_oauth_backchannel_logout_outbox SET status='PROCESSING', locked_until=?, attempt_count=attempt_count+1 WHERE id=?`, until, row.ID).Error; err != nil {
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
	result := r.DB.WithContext(ctx).Exec(`UPDATE platform_oauth_backchannel_logout_outbox SET status='DELIVERED', delivered_at=?, locked_until=NULL, last_error=NULL WHERE id=? AND status='PROCESSING'`, now.UTC(), id)
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
	result := r.DB.WithContext(ctx).Exec(`UPDATE platform_oauth_backchannel_logout_outbox SET status=CASE WHEN attempt_count>=? THEN 'FAILED' ELSE 'RETRY' END, next_attempt_at=?, locked_until=NULL, last_error=? WHERE id=? AND status='PROCESSING'`, max, next.UTC(), summary, id)
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

package infrastructure

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// incrementUnreadStat is called in the same transaction as a delivery state transition. The
// GREATEST guard makes repeated repair/replay paths unable to produce a negative counter.
func incrementUnreadStat(tx *gorm.DB, tenantID, userID string, delta int64, now time.Time) error {
	if delta == 0 {
		return nil
	}
	statement := `INSERT INTO notification_user_stat (tenant_id, user_id, unread_count, updated_at)
VALUES (?, ?, GREATEST(?, 0), ?)
ON DUPLICATE KEY UPDATE unread_count = GREATEST(unread_count + ?, 0), updated_at = VALUES(updated_at)`
	if err := tx.Exec(statement, tenantID, userID, delta, now, delta).Error; err != nil {
		return fmt.Errorf("update notification unread statistic: %w", err)
	}
	return nil
}

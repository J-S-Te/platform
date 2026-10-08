package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
	"gorm.io/gorm"
)

// InboxPolicy 读取租户站内信开关，但不让 notification 依赖 settings 的内部模型。
type InboxPolicy struct{ database *gorm.DB }

// NewInboxPolicy constructs the settings read adapter used by notification creation.
func NewInboxPolicy(database *gorm.DB) (*InboxPolicy, error) {
	if database == nil {
		return nil, fmt.Errorf("notification policy database must not be nil")
	}
	return &InboxPolicy{database: database}, nil
}

type notificationSettingRow struct {
	InboxEnabled      bool   `gorm:"column:inbox_enabled"`
	ReminderFrequency string `gorm:"column:reminder_frequency"`
}

// DeliveryVisibility 解析租户通知创建时的可见性计划：站内信是否启用，以及新投递何时
// 对收件人可见。行不存在（租户从未保存设置）按 IMMEDIATE 处理，保持现状行为；
// 数据库故障不能伪装成默认值。
func (policy *InboxPolicy) DeliveryVisibility(ctx context.Context, tenantID string, now time.Time) (bool, time.Time, error) {
	var row notificationSettingRow
	err := policy.database.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&row).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, now, fmt.Errorf("read notification delivery visibility: %w", err)
	}
	if err != nil || !row.InboxEnabled {
		return false, now, nil
	}
	switch strings.ToUpper(strings.TrimSpace(row.ReminderFrequency)) {
	case "NEVER":
		return false, now, nil
	case "DAILY":
		return true, nextDailyReleasePoint(now), nil
	case "WEEKLY":
		return true, nextWeeklyReleasePoint(now), nil
	default:
		// IMMEDIATE 与历史/未知值：立即可见。
		return true, now, nil
	}
}

// notificationReleaseZone 是通知释放点使用的时区：北京时间（UTC+8）。Asia/Shanghai 无
// 夏令时，固定偏移即可精确表达；这是全平台确认的统一业务时区，不引入按租户配置。
var notificationReleaseZone = time.FixedZone("Asia/Shanghai", 8*60*60)

// nextDailyReleasePoint 返回下一个每日释放点（北京时间 09:00）。DAILY 语义是“到点集中
// 可见”而非“24 小时延迟”：同一日内多次创建都在同一释放点浮现，形成每日汇总效果。
func nextDailyReleasePoint(now time.Time) time.Time {
	local := now.In(notificationReleaseZone)
	next := time.Date(local.Year(), local.Month(), local.Day(), 9, 0, 0, 0, notificationReleaseZone)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// nextWeeklyReleasePoint 返回下一个每周释放点（北京时间周一 09:00）。
func nextWeeklyReleasePoint(now time.Time) time.Time {
	next := nextDailyReleasePoint(now)
	for next.In(notificationReleaseZone).Weekday() != time.Monday {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// RecipientResolver 只解析当前租户的有效用户。角色和组织在这里仅用于选择通知受众，
// 绝不产生角色绑定或业务权限。
type RecipientResolver struct{ database *gorm.DB }

// NewRecipientResolver constructs the GORM tenant audience resolver.
func NewRecipientResolver(database *gorm.DB) (*RecipientResolver, error) {
	if database == nil {
		return nil, fmt.Errorf("notification recipient resolver database must not be nil")
	}
	return &RecipientResolver{database: database}, nil
}

type recipientUserRow struct {
	ID string `gorm:"column:id"`
}

// ResolveRecipients resolves USER, ORGANIZATION and ROLE targets to enabled tenant users.
func (resolver *RecipientResolver) ResolveRecipients(ctx context.Context, tenantID string, targets []domain.RecipientTarget, at time.Time) ([]string, error) {
	users := make(map[string]struct{})
	for _, target := range targets {
		targetID := strings.TrimSpace(target.ID)
		if targetID == "" {
			continue
		}
		var rows []recipientUserRow
		var err error
		switch target.Type {
		case domain.RecipientTypeUser:
			err = resolver.database.WithContext(ctx).Table("iam_user AS u").Select("u.id").Where("u.tenant_id = ? AND u.id = ? AND u.status = ?", tenantID, targetID, "ACTIVE").Find(&rows).Error
		case domain.RecipientTypeOrganization:
			err = resolver.database.WithContext(ctx).Table("iam_membership AS m").Joins("JOIN iam_user AS u ON u.id = m.user_id AND u.tenant_id = m.tenant_id").Select("DISTINCT u.id").Where("m.tenant_id = ? AND m.org_unit_id = ? AND m.status = ? AND u.status = ? AND (m.valid_from IS NULL OR m.valid_from <= ?) AND (m.valid_until IS NULL OR m.valid_until >= ?)", tenantID, targetID, "ACTIVE", "ACTIVE", at, at).Find(&rows).Error
		case domain.RecipientTypeRole:
			err = resolver.database.WithContext(ctx).Table("authz_role_binding AS b").Joins("JOIN iam_user AS u ON u.id = b.subject_id AND u.tenant_id = b.tenant_id").Select("DISTINCT u.id").Where("b.tenant_id = ? AND b.role_id = ? AND b.status = ? AND b.subject_type = ? AND u.status = ?", tenantID, targetID, "ACTIVE", "USER", "ACTIVE").Find(&rows).Error
		default:
			return nil, fmt.Errorf("unsupported notification recipient target %q", target.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve notification recipients: %w", err)
		}
		for _, row := range rows {
			if row.ID != "" {
				users[row.ID] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(users))
	for userID := range users {
		result = append(result, userID)
	}
	sort.Strings(result)
	return result, nil
}

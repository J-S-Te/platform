package publictransport

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ReconcileBrokerCallbacks 收敛已完成切换中遗漏的 Keycloak Broker 登记回调。
// 参数：ctx 为部署操作上下文，控制查询和事务取消。
// 返回值：成功或无需处理返回 nil，状态或事务不匹配时返回错误。
func (c *Coordinator) ReconcileBrokerCallbacks(ctx context.Context) error {
	return c.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx)
		if err != nil {
			return err
		}
		if state.State != StateHTTPS || state.ActiveTransitionID != nil {
			return errors.New("broker callback reconciliation requires a stable HTTPS state")
		}
		transition, err := latestCompletedTransition(tx, state)
		if err != nil {
			return err
		}
		return stageBrokerCallbacks(tx, transition, c.now().UTC(), false)
	})
}

// LastCompleted 返回与当前稳定公开入口一致的最近已完成切换。
// 参数：ctx 为数据库查询上下文。
// 返回值：返回切换与原入口，当前有活动切换或无匹配完成记录时返回错误。
func (c *Coordinator) LastCompleted(ctx context.Context) (Transition, error) {
	state, err := c.Status(ctx)
	if err != nil {
		return Transition{}, err
	}
	if state.ActiveTransitionID != nil || state.State != state.ActiveMode {
		return Transition{}, errors.New("the public transport state is not stable")
	}
	return latestCompletedTransition(c.database.WithContext(ctx), state)
}

// 用途：按当前稳定模式及两个实际 Origin 找到相关已完成切换。
// 参数：tx 为数据库句柄，state 为已读取的稳定状态。
// 返回值：最近的匹配切换记录或查询错误。
func latestCompletedTransition(tx *gorm.DB, state State) (Transition, error) {
	var transition Transition
	err := tx.Table("platform_public_transport_transition").
		Where("phase = ? AND target_mode = ? AND target_platform_origin = ? AND target_sso_origin = ?",
			PhaseCompleted, state.ActiveMode, state.PlatformOrigin, state.SSOOrigin).
		Order("completed_at DESC").Take(&transition).Error
	return transition, err
}

// 用途：将平台与客户门户 Broker 在旧 SSO 来源上的精确回调登记到目标来源。
// 参数：tx 为当前事务，transition 为受控切换，now 为写入时间，keepSource 表示保留旧回调。
// 返回值：成功返回 nil，任一资源无法登记或更新时返回错误。
func stageBrokerCallbacks(tx *gorm.DB, transition Transition, now time.Time, keepSource bool) error {
	if transition.SourceSSOOrigin == transition.TargetSSOOrigin {
		return nil
	}
	aliases := map[string]string{
		"keycloak-broker":                 "basic-platform",
		"keycloak-customer-portal-broker": "basic-platform-customer",
	}
	var rows []struct{ ClientID, Identifier, Value string }
	if err := tx.Table("platform_oauth_redirect_uri AS redirect").
		Select("redirect.oauth_client_id AS client_id, client.client_id AS identifier, redirect.redirect_uri AS value").
		Joins("JOIN platform_oauth_client AS client ON client.id = redirect.oauth_client_id").
		Where("client.status = 'ACTIVE' AND client.client_id IN ?", []string{"keycloak-broker", "keycloak-customer-portal-broker"}).
		Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		path, ok := strings.CutPrefix(row.Value, transition.SourceSSOOrigin)
		if !ok || !strings.HasPrefix(path, "/realms/") || !strings.HasSuffix(path, "/broker/"+aliases[row.Identifier]+"/endpoint") {
			continue
		}
		target := transition.TargetSSOOrigin + path
		if err := stageBrokerCallback(tx, transition.ID, row.ClientID, row.Value, target, now, keepSource); err != nil {
			return err
		}
	}
	return nil
}

// 用途：以现有回调资源类型记录迁移，并在同一事务中更新精确登记。
// 参数：切换 ID、客户端内部 ID、新旧回调、当前时间和保留旧值标志。
// 返回值：写入成功返回 nil，登记或删除失败时返回错误。
func stageBrokerCallback(tx *gorm.DB, transitionID, clientID, source, target string, now time.Time, keepSource bool) error {
	if err := insertResource(tx, transitionID, "OAUTH_REDIRECT", clientID, "", "", "", source, target, now); err != nil {
		return err
	}
	table := tx.Table("platform_oauth_redirect_uri")
	values := map[string]any{"oauth_client_id": clientID, "redirect_uri": target, "created_at": now}
	if err := table.Clauses(clause.Insert{Modifier: "IGNORE"}).Create(values).Error; err != nil {
		return err
	}
	if keepSource {
		return nil
	}
	return tx.Table("platform_oauth_redirect_uri").
		Where("oauth_client_id = ? AND redirect_uri = ?", clientID, source).Delete(nil).Error
}

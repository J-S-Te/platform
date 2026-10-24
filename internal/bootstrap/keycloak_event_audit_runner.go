package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	keycloakapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/infrastructure"
)

// eventCursorStore 持久化采集进度（批次 3）。实现必须按 realm 隔离。
type eventCursorStore interface {
	LoadEventCursor(ctx context.Context, realm string) (infrastructure.EventCursor, error)
	SaveEventCursor(ctx context.Context, cursor infrastructure.EventCursor, now time.Time) error
	RecordEventCursorFailure(ctx context.Context, realm, message string, now time.Time) (uint, error)
}

// 采集窗口：首次从回看窗口开始；之后从游标减一个重叠区，重复事件由审计 EventID 去重。
const (
	keycloakEventInitialLookback = 5 * time.Minute
	keycloakEventOverlap         = time.Minute
	// 连续失败达到该次数后按 ERROR 级记录"采集异常"，避免长期静默漏采。
	keycloakEventFailureAlertThreshold = 3
)

// keycloakEventAuditRunner 轮询 Keycloak 标准的用户/管理员事件端点。
// 批次 3：改用持久游标，停机超过五分钟也不会永久漏采；连续失败升级为异常告警。
type keycloakEventAuditRunner struct {
	collector *keycloakapplication.KeycloakEventAuditCollector
	cursors   eventCursorStore
	realm     string
	logger    *slog.Logger
	poll      time.Duration
	lastWarn  time.Time
}

func newKeycloakEventAuditRunner(collector *keycloakapplication.KeycloakEventAuditCollector, cursors eventCursorStore, realm string, logger *slog.Logger, poll time.Duration) (*keycloakEventAuditRunner, error) {
	if collector == nil || logger == nil || poll <= 0 {
		return nil, errors.New("Keycloak event audit runner configuration is invalid")
	}
	return &keycloakEventAuditRunner{collector: collector, cursors: cursors, realm: strings.TrimSpace(realm), logger: logger, poll: poll}, nil
}

// Run 按 poll 周期持续采集，依赖 context 终止信号退出；单次采集失败不终止循环。
func (runner *keycloakEventAuditRunner) Run(ctx context.Context) {
	ticker := time.NewTicker(runner.poll)
	defer ticker.Stop()
	for {
		runner.collectOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// collectOnce 读游标 → 采集 → 推进游标；失败时累计连续失败次数。
func (runner *keycloakEventAuditRunner) collectOnce(ctx context.Context) {
	now := time.Now().UTC()
	since := now.Add(-keycloakEventInitialLookback)
	cursor := infrastructure.EventCursor{Realm: runner.realm}
	if runner.cursors != nil {
		loaded, err := runner.cursors.LoadEventCursor(ctx, runner.realm)
		if err != nil {
			runner.warn(ctx, "load Keycloak event cursor", err)
			return
		}
		cursor = loaded
		if !cursor.LastEventTime.IsZero() {
			// 减去重叠区：晚到的事件仍会被采到，重复由审计 EventID 去重。
			since = cursor.LastEventTime.Add(-keycloakEventOverlap)
		}
	}
	if _, err := runner.collector.Collect(ctx, since); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		runner.recordFailure(ctx, err, now)
		return
	}
	if runner.cursors == nil {
		return
	}
	cursor.Realm = runner.realm
	cursor.LastEventTime = now
	if err := runner.cursors.SaveEventCursor(ctx, cursor, now); err != nil {
		runner.warn(ctx, "save Keycloak event cursor", err)
	}
}

// recordFailure 累计连续失败；达到阈值后按 ERROR 级告警（每分钟最多一次）。
func (runner *keycloakEventAuditRunner) recordFailure(ctx context.Context, collectErr error, now time.Time) {
	failures := uint(0)
	if runner.cursors != nil {
		count, err := runner.cursors.RecordEventCursorFailure(ctx, runner.realm, collectErr.Error(), now)
		if err != nil {
			runner.warn(ctx, "record Keycloak event collection failure", err)
		}
		failures = count
	}
	level := slog.LevelWarn
	if failures >= keycloakEventFailureAlertThreshold {
		level = slog.LevelError
	}
	if runner.lastWarn.IsZero() || now.Sub(runner.lastWarn) >= time.Minute {
		runner.lastWarn = now
		runner.logger.Log(ctx, level, "collect Keycloak audit events deferred", "error", collectErr, "consecutive_failures", failures, "realm", runner.realm)
	}
}

func (runner *keycloakEventAuditRunner) warn(ctx context.Context, message string, err error) {
	now := time.Now().UTC()
	if runner.lastWarn.IsZero() || now.Sub(runner.lastWarn) >= time.Minute {
		runner.lastWarn = now
		runner.logger.WarnContext(ctx, message, "error", err, "realm", runner.realm)
	}
}

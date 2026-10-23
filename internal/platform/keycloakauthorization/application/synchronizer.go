// Package application 协调 Basic Platform 到 Keycloak 的单向授权投影。
package application

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	projectionworker "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/worker"
)

// Snapshot 是某个租户、身份、应用和环境在同一授权修订上的完整投影输入，
// 字段在加载后不可部分更新：整个快照要么全部写入 Keycloak，要么整体失败重试。
type Snapshot struct {
	TenantID, IdentityID, ApplicationID, EnvironmentID, ApplicationCode, KeycloakClientID string
	PersonID, DisplayName, Email, PrimaryOrganizationID                                   string
	UserEnabled                                                                           bool
	OrganizationIDs, Roles, Permissions                                                   []string
	RoleConfigHash                                                                        string
	AuthorizationRevision                                                                 uint64
}

// Source 是投影事件的平台侧读取抽象；实现方负责校验事件与快照的租户/资源边界。
type Source interface {
	// Source 从平台事实源读取快照；Keycloak 不反向成为授权事实源。
	LoadAuthorizationProjection(context.Context, projectionworker.Event) (Snapshot, error)
}

// KeycloakAdmin 封装对 Keycloak 控制面的外部写入口，实现方必须把写入限制在
// 当前快照声明的用户与 Client 范围内。
type KeycloakAdmin interface {
	// KeycloakAdmin 是受限的外部写边界，只能写入当前快照声明的用户与 Client 范围。
	EnsureUser(context.Context, Snapshot) error
	EnsureOrganizationGroups(context.Context, Snapshot) error
	AssignClientRoles(context.Context, Snapshot) error
	SetClientAuthorizationAttributes(context.Context, Snapshot) error
}

// ProjectionStore 持久化每次投影的最终状态，供重试调度、运维诊断和 readiness 门禁读取。
type ProjectionStore interface {
	// ProjectionStore 持久化同步结果，供重试、运维诊断和 readiness 门禁读取。
	// 成功时间参数是快照加载前的对账水位，只有外部同步全部成功后才保存。
	MarkSynchronized(context.Context, Snapshot, time.Time) error
	MarkFailed(context.Context, projectionworker.Event, string, string, time.Time) error
}

// FailureNotifier 是对重复投影失败进行告警的可选接口。实现必须并发安全，
// 且绝不能阻塞同步循环。
type FailureNotifier interface {
	NotifyProjectionFailure(ctx context.Context, tenantID, applicationID, identityID string, err error)
}

// Clock 提供当前时间；测试可注入确定性时钟使重试与水位断言可复现。
type Clock interface{ Now() time.Time }
type systemClock struct{}

// Now 返回当前 UTC 时间。
func (systemClock) Now() time.Time { return time.Now().UTC() }

// Synchronizer 按事件把平台授权快照单向投影到 Keycloak，并持久化成功/失败状态；
// 连续失败按 scope 计数并触发一次告警，防止故障期间告警风暴。
type Synchronizer struct {
	source    Source
	admin     KeycloakAdmin
	store     ProjectionStore
	clock     Clock
	notifier  FailureNotifier
	failMu    sync.Mutex
	failCount map[string]int
}

// NewSynchronizer 构造投影同步器；任一核心依赖为空或提供超过一个时钟时返回错误。
func NewSynchronizer(source Source, admin KeycloakAdmin, store ProjectionStore, clocks ...Clock) (*Synchronizer, error) {
	if source == nil || admin == nil || store == nil || len(clocks) > 1 {
		return nil, errors.New("Keycloak authorization synchronizer dependencies are invalid")
	}
	clock := Clock(systemClock{})
	if len(clocks) == 1 {
		if clocks[0] == nil {
			return nil, errors.New("Keycloak authorization synchronizer clock must not be nil")
		}
		clock = clocks[0]
	}
	return &Synchronizer{source: source, admin: admin, store: store, clock: clock, failCount: make(map[string]int)}, nil
}

// SetFailureNotifier 挂接可选的告警出口。必须在启动同步循环之前调用。
func (s *Synchronizer) SetFailureNotifier(notifier FailureNotifier) {
	s.notifier = notifier
}

// SyncAuthorization 执行一次完整的投影：先取对账水位，再加载快照、校验事件边界，
// 然后按用户、组织组、Client 角色、属性顺序写入；任一步失败即记录失败并返回，
// 只有全部外部步骤成功后才推进水位。
func (s *Synchronizer) SyncAuthorization(ctx context.Context, event projectionworker.Event) error {
	// 在加载前取对账水位，确保加载或外部调用期间跨过的有效期边界仍会被补偿。
	// 只有所有外部步骤成功后才保存该水位；失败不能推进。
	// DATETIME(3) 持久化前向下截断，避免数据库舍入越过尚未读取的边界。
	snapshotAt := s.clock.Now().UTC().Truncate(time.Millisecond)
	// 先校验事件与快照的租户/资源边界，再按用户、组织组、Client 角色、属性顺序投影。
	snapshot, err := s.source.LoadAuthorizationProjection(ctx, event)
	if err != nil {
		return s.fail(ctx, event, err)
	}
	if err := validateSnapshot(snapshot, event); err != nil {
		return s.fail(ctx, event, err)
	}
	for _, operation := range []struct {
		name string
		run  func(context.Context, Snapshot) error
	}{
		{"ensure user", s.admin.EnsureUser}, {"ensure organization groups", s.admin.EnsureOrganizationGroups},
		{"assign Client roles", s.admin.AssignClientRoles}, {"set Client authorization attributes", s.admin.SetClientAuthorizationAttributes},
	} {
		if err := operation.run(ctx, snapshot); err != nil {
			return s.fail(ctx, event, fmt.Errorf("%s: %w", operation.name, err))
		}
	}
	if err := s.store.MarkSynchronized(ctx, snapshot, snapshotAt); err != nil {
		return fmt.Errorf("mark Keycloak authorization projection synchronized: %w", err)
	}
	// 成功后清零失败计数；worker 可能并发处理多个投影事件，访问 map 必须持锁。
	s.failMu.Lock()
	delete(s.failCount, failureKey(event))
	s.failMu.Unlock()
	return nil
}

func (s *Synchronizer) fail(ctx context.Context, event projectionworker.Event, err error) error {
	_ = s.store.MarkFailed(ctx, event, "KEYCLOAK_SYNC_FAILED", trim(err.Error(), 1000), s.clock.Now().UTC())
	// 按 scope 追踪连续失败次数，仅在跨越阈值时通知一次，
	// 避免故障持续期间出现告警风暴。
	key := failureKey(event)
	s.failMu.Lock()
	s.failCount[key]++
	shouldNotify := s.failCount[key] == 3
	s.failMu.Unlock()
	if s.notifier != nil && shouldNotify {
		s.notifier.NotifyProjectionFailure(ctx, event.TenantID, event.ApplicationID, event.IdentityID, err)
	}
	return err
}

func failureKey(event projectionworker.Event) string {
	return event.TenantID + "/" + event.ApplicationID + "/" + event.EnvironmentID + "/" + event.IdentityID
}

func validateSnapshot(snapshot Snapshot, event projectionworker.Event) error {
	// 事件中的复合键是不可越界的安全约束，避免过期或串租户快照写入 Keycloak。
	if snapshot.TenantID != event.TenantID || snapshot.IdentityID != event.IdentityID || snapshot.ApplicationID != event.ApplicationID || snapshot.EnvironmentID != event.EnvironmentID || strings.TrimSpace(snapshot.ApplicationCode) == "" || strings.TrimSpace(snapshot.KeycloakClientID) == "" {
		return errors.New("Keycloak authorization snapshot does not match outbox event")
	}
	if len(snapshot.Roles) != len(unique(snapshot.Roles)) || len(snapshot.Permissions) != len(unique(snapshot.Permissions)) || len(snapshot.OrganizationIDs) != len(unique(snapshot.OrganizationIDs)) {
		return errors.New("Keycloak authorization snapshot contains duplicate values")
	}
	return nil
}
func unique(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			seen[v] = struct{}{}
		}
	}
	for v := range seen {
		result = append(result, v)
	}
	sort.Strings(result)
	return result
}
func trim(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		return value[:max]
	}
	return value
}

var _ projectionworker.Synchronizer = (*Synchronizer)(nil)

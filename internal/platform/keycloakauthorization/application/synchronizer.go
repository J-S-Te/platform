// Package application coordinates the one-way Basic Platform to Keycloak authorization projection.
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

type Snapshot struct {
	// Snapshot 是某个租户、身份、应用和环境在同一授权修订上的完整投影输入。
	TenantID, IdentityID, ApplicationID, EnvironmentID, ApplicationCode, KeycloakClientID string
	PersonID, DisplayName, Email, PrimaryOrganizationID                                   string
	UserEnabled                                                                           bool
	OrganizationIDs, Roles, Permissions                                                   []string
	RoleConfigHash                                                                        string
	AuthorizationRevision                                                                 uint64
}

type Source interface {
	// Source 从平台事实源读取快照；Keycloak 不反向成为授权事实源。
	LoadAuthorizationProjection(context.Context, projectionworker.Event) (Snapshot, error)
}

type KeycloakAdmin interface {
	// KeycloakAdmin 是受限的外部写边界，只能写入当前快照声明的用户与 Client 范围。
	EnsureUser(context.Context, Snapshot) error
	EnsureOrganizationGroups(context.Context, Snapshot) error
	AssignClientRoles(context.Context, Snapshot) error
	SetClientAuthorizationAttributes(context.Context, Snapshot) error
}

type ProjectionStore interface {
	// ProjectionStore 持久化同步结果，供重试、运维诊断和 readiness 门禁读取。
	MarkSynchronized(context.Context, Snapshot, time.Time) error
	MarkFailed(context.Context, projectionworker.Event, string, string, time.Time) error
}

// FailureNotifier is an optional interface for alerting on repeated projection
// failures. Implementations should be safe for concurrent use and must not
// block the synchronization loop.
type FailureNotifier interface {
	NotifyProjectionFailure(ctx context.Context, tenantID, applicationID, identityID string, err error)
}

type Clock interface{ Now() time.Time }
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type Synchronizer struct {
	source    Source
	admin     KeycloakAdmin
	store     ProjectionStore
	clock     Clock
	notifier  FailureNotifier
	failMu    sync.Mutex
	failCount map[string]int
}

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

// SetFailureNotifier attaches an optional alerting sink. Call this before
// starting the synchronization loop.
func (s *Synchronizer) SetFailureNotifier(notifier FailureNotifier) {
	s.notifier = notifier
}

func (s *Synchronizer) SyncAuthorization(ctx context.Context, event projectionworker.Event) error {
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
	if err := s.store.MarkSynchronized(ctx, snapshot, s.clock.Now().UTC()); err != nil {
		return fmt.Errorf("mark Keycloak authorization projection synchronized: %w", err)
	}
	// Reset failure counter on success; protect the map because the worker may
	// process multiple projection events concurrently.
	s.failMu.Lock()
	delete(s.failCount, failureKey(event))
	s.failMu.Unlock()
	return nil
}

func (s *Synchronizer) fail(ctx context.Context, event projectionworker.Event, err error) error {
	_ = s.store.MarkFailed(ctx, event, "KEYCLOAK_SYNC_FAILED", trim(err.Error(), 1000), s.clock.Now().UTC())
	// Track consecutive failures per scope and notify only when the threshold is
	// crossed, avoiding a notification storm while an outage persists.
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

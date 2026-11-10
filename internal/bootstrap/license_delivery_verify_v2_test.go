package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	registryinfra "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	license "github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	core "github.com/J-S-Te/license-core"
	"github.com/J-S-Te/license-core/catalog"
	runtime "github.com/J-S-Te/license-core/runtime"
)

// V1 审批路径下线后，离线交付验证只接受 BUILTIN_COMPONENT_V2 目标态。
// 本文件覆盖：完整 V2 通过、以及各关键事实缺失/不一致时失败关闭。

type v2VerifyInitializer struct{ state domain.State }

func (s v2VerifyInitializer) Read(context.Context) (domain.State, error) { return s.state, nil }
func (s v2VerifyInitializer) Initialize(context.Context, license.InitializeInput, domain.Actor) (domain.State, error) {
	return domain.State{}, errors.New("verification must never initialize")
}

type v2VerifyCoordinator struct{ status coordination.Status }

func (s v2VerifyCoordinator) Status(context.Context, string) (coordination.Status, error) {
	return s.status, nil
}
func (s v2VerifyCoordinator) BeginActivation(context.Context, string, uint64, domain.Actor) error {
	return errors.New("verification must never activate")
}

type v2VerifyPrincipal struct{}

func (v2VerifyPrincipal) OfflineBootstrapPrincipal(context.Context, time.Time) (authctx.Principal, error) {
	// 离线验证要求已持久化的初始化超级管理员：租户 default + 超管角色 + 全部离线权限。
	return authctx.Principal{
		Tenant:          authctx.ReferenceName{ID: "tenant", Code: "default"},
		User:            authctx.ReferenceName{ID: "user"},
		Account:         authctx.ReferenceName{ID: "account"},
		Roles:           []authctx.ReferenceName{{ID: "role", Code: "platform-super-admin"}},
		PermissionCodes: offlinePermissions,
	}, nil
}

func hashHex(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

const v2VerifyApp = "settlement"

// v2VerifyFixture 构造一个完整的 V2 目标态：内置目录、已登记的 2 个成员、当前快照与新鲜确认。
func v2VerifyFixture(t *testing.T) (*OfflineDeliveryRunner, offlineSnapshotVerification, time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	set, err := (registryinfra.BuiltinRuntimeComponentCatalog{}).RuntimeComponentSet(context.Background(), v2VerifyApp, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Components) != 2 {
		t.Fatalf("unexpected catalog size %d", len(set.Components))
	}
	const currentRaw = "current-license-jws"
	license := core.License{
		ProtocolVersion: 1, Issuer: core.Issuer, ID: "verify-license", Version: 7,
		CustomerID: "customer", ProductID: core.Product, Environment: "production", InstanceID: "instance",
		IssuedAt: now.Unix() - 200, NotBefore: now.Unix() - 100,
		Applications: []core.Application{{Code: v2VerifyApp, NotBefore: now.Unix() - 100, ExpiresAt: now.Unix() + 3600, Kind: "FULL"}},
	}
	state := domain.State{
		InstanceID: "instance", CustomerID: "customer", Environment: "production",
		Revision: 5, CurrentVersion: 7, HighestVersion: 7, HighestObservedAt: now.Unix(), Current: &license, CurrentDigest: hashHex(currentRaw),
	}
	members := make([]coordination.Member, 0, len(set.Components))
	for _, component := range set.Components {
		raw := "member-" + component.ServiceID
		members = append(members, coordination.Member{
			Application: v2VerifyApp, Environment: "prod", ServiceID: component.ServiceID,
			OAuthClientID:        registry.RuntimeLicenseClientIDV2(state.InstanceID, v2VerifyApp, "prod", component.ServiceID, component.CredentialGeneration),
			RegistrationProtocol: component.RegistrationProtocol, CatalogVersion: component.CatalogVersion,
			CredentialGeneration: component.CredentialGeneration,
			IssuedRevision:       state.Revision, AckRevision: state.Revision,
			RawJWS: raw, IssuedDigest: hashHex(raw),
			ReadyAt: &now, AckAt: &now,
		})
	}
	status := coordination.Status{
		Application: coordination.Application{
			Application: v2VerifyApp, RegistrationMode: catalog.BuiltinComponentV2, CatalogVersion: set.CatalogVersion,
			State: runtime.Enforced, Revision: state.Revision,
		},
		DeploymentRevision: state.Revision, SnapshotCurrent: true, Members: members,
	}
	verify := func(string, runtime.Binding) (runtime.PlatformSnapshot, error) {
		return runtime.PlatformSnapshot{
			Revision: state.Revision, EnforcementState: runtime.Enforced,
			HighestLicenseVersion: state.HighestVersion, SnapshotIssuedAt: now.Unix() - 10,
			CurrentLicenseJWS: currentRaw,
		}, nil
	}
	runner := &OfflineDeliveryRunner{
		identity: v2VerifyPrincipal{}, initializer: v2VerifyInitializer{state: state},
		coordinator: v2VerifyCoordinator{status: status}, catalog: registryinfra.BuiltinRuntimeComponentCatalog{},
		environment: "production",
	}
	return runner, verify, now
}

func TestOfflineVerifyEnforcementAcceptsBuiltinV2Only(t *testing.T) {
	runner, verify, now := v2VerifyFixture(t)
	out, err := runner.verifyEnforcement(context.Background(), verify, func() time.Time { return now })
	if err != nil {
		t.Fatalf("complete V2 topology must verify: %v", err)
	}
	if out.Status != string(runtime.Enforced) || len(out.Runtime) != 1 || len(out.Runtime[0].Components) != 2 {
		t.Fatalf("unexpected verification output: %+v", out)
	}
}

func TestOfflineVerifyEnforcementRejectsLegacyOrBridgingMode(t *testing.T) {
	for _, mode := range []catalog.RegistrationMode{catalog.LegacyImageV1, catalog.BridgingComponentV2, ""} {
		t.Run(string(mode), func(t *testing.T) {
			runner, verify, now := v2VerifyFixture(t)
			runner.coordinator = v2VerifyCoordinator{status: func() coordination.Status {
				status := runner.coordinator.(v2VerifyCoordinator).status
				status.Application.RegistrationMode = mode
				return status
			}()}
			if _, err := runner.verifyEnforcement(context.Background(), verify, func() time.Time { return now }); !errors.Is(err, ErrOfflineEnforcementIncomplete) {
				t.Fatalf("mode %q must fail closed, got %v", mode, err)
			}
		})
	}
}

func TestOfflineVerifyEnforcementRejectsIncompleteV2Facts(t *testing.T) {
	for _, mutation := range []string{"missing-ack", "stale-ready", "catalog-size", "snapshot-drift"} {
		t.Run(mutation, func(t *testing.T) {
			runner, verify, now := v2VerifyFixture(t)
			coordinator := runner.coordinator.(v2VerifyCoordinator)
			status := coordinator.status
			switch mutation {
			case "missing-ack":
				status.Members[0].AckAt = nil
			case "stale-ready":
				old := now.Add(-2 * time.Minute)
				status.Members[0].ReadyAt = &old
			case "catalog-size":
				status.Members = status.Members[:1]
			case "snapshot-drift":
				status.SnapshotCurrent = false
			}
			runner.coordinator = v2VerifyCoordinator{status: status}
			if _, err := runner.verifyEnforcement(context.Background(), verify, func() time.Time { return now }); !errors.Is(err, ErrOfflineEnforcementIncomplete) {
				t.Fatalf("%s must fail closed, got %v", mutation, err)
			}
		})
	}
}

// TestOfflineVerifyEnforcementReportsFailureCause 用途：证明验收能定位失败事实且不泄漏验签材料。
// 参数：t 为测试上下文。
// 返回值：无；诊断缺失、误放行或泄密时失败。
func TestOfflineVerifyEnforcementReportsFailureCause(t *testing.T) {
	cases := []struct{ change, want string }{
		{"deployment", "DEPLOYMENT_REVISION_CHANGED"},
		{"content", "SNAPSHOT_CONTENT_STALE"},
		{"revision", "MEMBER_REVISION_MISMATCH"},
		{"ready-missing", "READY_MISSING"},
		{"ready-stale", "READY_STALE"},
		{"ready-future", "READY_FUTURE"},
		{"ack-missing", "ACK_MISSING"},
		{"ack-stale", "ACK_STALE"},
		{"ack-future", "ACK_FUTURE"},
		{"signature", "SNAPSHOT_SIGNATURE_INVALID"},
		{"digest", "SNAPSHOT_DIGEST_MISMATCH"},
		{"snapshot-revision", "SNAPSHOT_PAYLOAD_MISMATCH"},
		{"application-revision", "APPLICATION_REVISION_INVALID"},
		{"exact-minute", ""},
		{"future-tolerance", ""},
		{"other-application-revision", ""},
	}
	for _, item := range cases {
		t.Run(item.change, func(t *testing.T) {
			runner, verify, now := v2VerifyFixture(t)
			status := runner.coordinator.(v2VerifyCoordinator).status
			member := &status.Members[0]
			old, future := now.Add(-61*time.Second), now.Add(2*time.Second)
			switch item.change {
			case "deployment":
				status.DeploymentRevision++
			case "content":
				status.SnapshotCurrent = false
			case "revision":
				member.AckRevision--
			case "ready-missing":
				member.ReadyAt = nil
			case "ready-stale":
				member.ReadyAt = &old
			case "ready-future":
				member.ReadyAt = &future
			case "ack-missing":
				member.AckAt = nil
			case "ack-stale":
				member.AckAt = &old
			case "ack-future":
				member.AckAt = &future
			case "signature":
				verify = func(string, runtime.Binding) (runtime.PlatformSnapshot, error) {
					return runtime.PlatformSnapshot{}, errors.New("private-signature-material")
				}
			case "digest":
				member.IssuedDigest = "invalid"
			case "snapshot-revision":
				original := verify
				verify = func(raw string, binding runtime.Binding) (runtime.PlatformSnapshot, error) {
					snapshot, err := original(raw, binding)
					snapshot.Revision--
					return snapshot, err
				}
			case "other-application-revision":
				status.Revision--
				for index := range status.Members {
					status.Members[index].IssuedRevision--
					status.Members[index].AckRevision--
				}
				original := verify
				verify = func(raw string, binding runtime.Binding) (runtime.PlatformSnapshot, error) {
					snapshot, err := original(raw, binding)
					snapshot.Revision--
					return snapshot, err
				}
			case "application-revision":
				status.Revision = 0
			case "exact-minute":
				edge := now.Add(-time.Minute)
				member.ReadyAt, member.AckAt = &edge, &edge
			case "future-tolerance":
				edge := now.Add(time.Second)
				member.ReadyAt, member.AckAt = &edge, &edge
			}
			runner.coordinator = v2VerifyCoordinator{status: status}
			_, err := runner.verifyEnforcement(context.Background(), verify, func() time.Time { return now })
			if item.want == "" {
				if err != nil {
					t.Fatalf("existing heartbeat boundary changed: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrOfflineEnforcementIncomplete) ||
				!strings.Contains(err.Error(), item.want) || !strings.Contains(err.Error(), v2VerifyApp) {
				t.Fatalf("want application and %s diagnosis, got %v", item.want, err)
			}
			if strings.Contains(err.Error(), "private-signature-material") ||
				strings.Contains(err.Error(), member.RawJWS) {
				t.Fatal("diagnosis leaked snapshot or verification material")
			}
		})
	}
}

package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/trust"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	core "github.com/J-S-Te/license-core"
	"github.com/J-S-Te/license-core/catalog"
	runtime "github.com/J-S-Te/license-core/runtime"
)

// ErrOfflineEnforcementIncomplete 表示当前商业许可与运行时确认不完整，部署校验
// 不能放行。
var ErrOfflineEnforcementIncomplete = errors.New("current commercial license and runtime confirmations are incomplete")

// OfflineEnforcementVerification 刻意不包含 JWS、机器密钥或拟议的组件选择。
// 每个应用都来自已验证的当前购买，每个运行时成员都来自隔离的 Agent。
type OfflineEnforcementVerification struct {
	Status         string                               `json:"status"`
	InstanceID     string                               `json:"instance_id"`
	CustomerID     string                               `json:"customer_id"`
	Environment    string                               `json:"environment"`
	LicenseVersion uint64                               `json:"license_version"`
	LicenseDigest  string                               `json:"license_digest"`
	Applications   []string                             `json:"applications"`
	Runtime        []OfflineEnforcementApplicationProof `json:"runtime"`
	VerifiedAt     time.Time                            `json:"verified_at"`
}

// OfflineEnforcementApplicationProof 是单个应用的运行时成员证明：应用、环境、
// 授权修订、发布摘要与组件清单均来自隔离部署 Agent 的采集结果。
type OfflineEnforcementApplicationProof struct {
	Application   string   `json:"application"`
	Environment   string   `json:"environment"`
	Revision      uint64   `json:"revision"`
	ReleaseDigest string   `json:"release_digest"`
	Components    []string `json:"components"`
}

type offlineSnapshotVerification func(string, runtime.Binding) (runtime.PlatformSnapshot, error)

// VerifyEnforcement 只观察实际持久化的授权状态；它绝不导入许可证、注册/部署
// 系统、发起激活、签发快照或发送 READY/ACK。常规的许可证 Read/Status 时钟观察
// 与到期待激活许可证的激活逻辑保持不变。它刻意独立于严格本地只读的安装日志
// status 命令。
func (r *OfflineDeliveryRunner) VerifyEnforcement(ctx context.Context, cfg config.Config) (OfflineEnforcementVerification, error) {
	if cfg.Environment != "production" || cfg.Audit.EnvironmentCode != "prod" || r == nil || r.environment != cfg.Environment {
		return OfflineEnforcementVerification{}, fmt.Errorf("%w: production/prod verification is required", ErrOfflineEnforcementIncomplete)
	}
	verifier, err := security.LoadApplicationJWTVerifier(cfg.Auth.JWTIssuer, cfg.Auth.ApplicationJWTAudience, cfg.Auth.JWTPublicKeyPath)
	if err != nil {
		return OfflineEnforcementVerification{}, fmt.Errorf("%w: platform snapshot verification key unavailable", ErrOfflineEnforcementIncomplete)
	}
	vendor := trust.Keys()
	return r.verifyEnforcement(ctx, func(raw string, binding runtime.Binding) (runtime.PlatformSnapshot, error) {
		return verifier.VerifyLicenseSnapshot(raw, binding, vendor)
	}, time.Now)
}

func (r *OfflineDeliveryRunner) verifyEnforcement(ctx context.Context, verify offlineSnapshotVerification, clock func() time.Time) (OfflineEnforcementVerification, error) {
	out := OfflineEnforcementVerification{Status: "INCOMPLETE", Applications: []string{}, Runtime: []OfflineEnforcementApplicationProof{}}
	if ctx == nil || r == nil || r.initializer == nil || r.coordinator == nil || r.identity == nil || verify == nil || clock == nil {
		return out, fmt.Errorf("%w: verification dependencies unavailable", ErrOfflineEnforcementIncomplete)
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if _, err := r.principal(ctx); err != nil {
		return out, fmt.Errorf("%w: active offline administrator is required", ErrOfflineEnforcementIncomplete)
	}
	state, err := r.initializer.Read(ctx)
	if err != nil {
		return out, fmt.Errorf("%w: verified current license unavailable", ErrOfflineEnforcementIncomplete)
	}
	apps, err := offlineCurrentPurchase(state, r.environment, clock().UTC())
	if err != nil {
		return out, err
	}
	type approvedApplication struct {
		builtin bool
		digest  string
		specs   []coordination.ServiceSpec
		status  coordination.Status
	}
	approved := make(map[string]approvedApplication, len(apps))
	var source registry.RuntimeLicenseLifecycleApprovalSource
	for _, app := range apps {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		status, e := r.coordinator.Status(ctx, app)
		if e != nil {
			return out, fmt.Errorf("%w: %s runtime status unavailable", ErrOfflineEnforcementIncomplete, app)
		}
		switch status.Application.RegistrationMode {
		case catalog.BuiltinComponentV2:
			set, e := r.builtinRuntimeComponentSet(ctx, app)
			if e != nil {
				return out, fmt.Errorf("%w: %s runtime component catalog unavailable", ErrOfflineEnforcementIncomplete, app)
			}
			specs, e := builtinRuntimeSpecs(state, app, status, set)
			if e != nil {
				return out, fmt.Errorf("%w: %s runtime component set is incomplete", ErrOfflineEnforcementIncomplete, app)
			}
			if e = offlineEnforcementStatusV2(state, app, status, specs, verify, clock().UTC()); e != nil {
				return out, e
			}
			digest, e := runtimeComponentSetDigest(set)
			if e != nil {
				return out, fmt.Errorf("%w: %s runtime component set is invalid", ErrOfflineEnforcementIncomplete, app)
			}
			approved[app] = approvedApplication{builtin: true, digest: digest, specs: specs, status: status}
		case catalog.BridgingComponentV2:
			return out, fmt.Errorf("%w: %s bridging runtime proof is not supported", ErrOfflineEnforcementIncomplete, app)
		default:
			if source == nil {
				var ok bool
				source, ok = r.source.(registry.RuntimeLicenseLifecycleApprovalSource)
				if !ok {
					return out, fmt.Errorf("%w: isolated release approval reader unavailable", ErrOfflineEnforcementIncomplete)
				}
			}
			release, e := source.ApprovedRuntimeLicenseLifecycle(ctx, app, "prod")
			if e != nil {
				return out, fmt.Errorf("%w: %s release approval unavailable", ErrOfflineEnforcementIncomplete, app)
			}
			digest, e := registry.RuntimeLicenseReleaseDigest(release)
			if e != nil {
				return out, fmt.Errorf("%w: %s release approval invalid", ErrOfflineEnforcementIncomplete, app)
			}
			lifecycle, e := registry.NewRuntimeLicenseLifecycleVerifier(source)
			if e != nil {
				return out, fmt.Errorf("%w: release verification unavailable", ErrOfflineEnforcementIncomplete)
			}
			// 复用真实的、感知代际的 OAuth 绑定。第二次可信读取必须与完整规范发布
			// 一致，包括条件选择逻辑。
			binding, e := lifecycle.VerifyRuntimeLifecycle(ctx, coordination.LifecycleInput{Application: app, Environment: "prod", ReleaseDigest: digest})
			if e != nil {
				return out, fmt.Errorf("%w: %s release approval changed", ErrOfflineEnforcementIncomplete, app)
			}
			if e = offlineEnforcementStatus(state, app, status, binding.Specs, verify, clock().UTC()); e != nil {
				return out, e
			}
			approved[app] = approvedApplication{digest: digest, specs: binding.Specs, status: status}
		}
	}
	// 后一个应用的检查不得掩盖前一个应用的续期、成员变化或心跳丢失。
	// 重新观察整个集合；绝不代替消费方把这些观察变成 ACK。
	for _, app := range apps {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		first := approved[app]
		var digest string
		if first.builtin {
			set, e := r.builtinRuntimeComponentSet(ctx, app)
			if e != nil {
				return out, fmt.Errorf("%w: %s runtime component catalog changed during verification", ErrOfflineEnforcementIncomplete, app)
			}
			digest, e = runtimeComponentSetDigest(set)
			if e != nil || digest != first.digest {
				return out, fmt.Errorf("%w: %s release changed during verification", ErrOfflineEnforcementIncomplete, app)
			}
		} else {
			release, e := source.ApprovedRuntimeLicenseLifecycle(ctx, app, "prod")
			if e != nil {
				return out, fmt.Errorf("%w: %s final release approval unavailable", ErrOfflineEnforcementIncomplete, app)
			}
			digest, e = registry.RuntimeLicenseReleaseDigest(release)
			if e != nil || digest != first.digest {
				return out, fmt.Errorf("%w: %s release changed during verification", ErrOfflineEnforcementIncomplete, app)
			}
		}
		status, e := r.coordinator.Status(ctx, app)
		if e != nil || status.Revision != first.status.Revision {
			return out, fmt.Errorf("%w: %s runtime changed during verification", ErrOfflineEnforcementIncomplete, app)
		}
		if first.builtin {
			e = offlineEnforcementStatusV2(state, app, status, first.specs, verify, clock().UTC())
		} else {
			e = offlineEnforcementStatus(state, app, status, first.specs, verify, clock().UTC())
		}
		if e != nil {
			return out, e
		}
		proof := OfflineEnforcementApplicationProof{Application: app, Environment: "prod", Revision: status.Revision, ReleaseDigest: digest}
		for _, spec := range first.specs {
			proof.Components = append(proof.Components, spec.ServiceID)
		}
		sort.Strings(proof.Components)
		out.Runtime = append(out.Runtime, proof)
	}
	final, err := r.initializer.Read(ctx)
	if err != nil {
		return out, fmt.Errorf("%w: final verified license unavailable", ErrOfflineEnforcementIncomplete)
	}
	if _, err = offlineCurrentPurchase(final, r.environment, clock().UTC()); err != nil {
		return out, err
	}
	if final.InstanceID != state.InstanceID || final.CustomerID != state.CustomerID || final.Environment != state.Environment || final.Revision != state.Revision || final.CurrentVersion != state.CurrentVersion || final.HighestVersion != state.HighestVersion || final.CurrentDigest != state.CurrentDigest || final.PendingDigest != state.PendingDigest {
		return out, fmt.Errorf("%w: license changed during verification; retry observation", ErrOfflineEnforcementIncomplete)
	}
	if _, err = r.principal(ctx); err != nil {
		return out, fmt.Errorf("%w: offline administrator authority changed", ErrOfflineEnforcementIncomplete)
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	out.Status, out.InstanceID, out.CustomerID, out.Environment = string(runtime.Enforced), state.InstanceID, state.CustomerID, state.Environment
	out.LicenseVersion, out.LicenseDigest, out.Applications, out.VerifiedAt = state.CurrentVersion, state.CurrentDigest, apps, clock().UTC()
	return out, nil
}

func offlineLicenseDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func offlineCurrentPurchase(state domain.State, environment string, now time.Time) ([]string, error) {
	if environment != "production" || state.Environment != environment || state.Current == nil || state.ClockBlocked || state.Revision == 0 || state.HighestObservedAt <= 0 || now.Unix() < state.HighestObservedAt-300 || !offlineLicenseDigest(state.CurrentDigest) || state.CurrentVersion == 0 || state.CurrentVersion != state.Current.Version || state.CurrentVersion > state.HighestVersion || state.Current.InstanceID != state.InstanceID || state.Current.CustomerID != state.CustomerID || state.Current.Environment != state.Environment || core.Validate(*state.Current) != nil {
		return nil, fmt.Errorf("%w: current license binding or clock is invalid", ErrOfflineEnforcementIncomplete)
	}
	if (state.Pending == nil) != (state.PendingDigest == "") || (state.Pending == nil && state.HighestVersion != state.CurrentVersion) || (state.Pending != nil && (!offlineLicenseDigest(state.PendingDigest) || state.Pending.InstanceID != state.InstanceID || state.Pending.CustomerID != state.CustomerID || state.Pending.Environment != state.Environment || state.Pending.Version != state.HighestVersion || core.CompareVersions(*state.Pending, *state.Current) != nil || now.Unix() >= state.Pending.NotBefore)) {
		return nil, fmt.Errorf("%w: pending license state is inconsistent", ErrOfflineEnforcementIncomplete)
	}
	apps := make([]string, 0, len(state.Current.Applications))
	for _, grant := range state.Current.Applications {
		if _, ok := offlineTargets[grant.Code]; !ok || core.Evaluate(*state.Current, grant.Code, core.MUTATE_BUSINESS, now, state.InstanceID, state.Environment) != nil {
			return nil, fmt.Errorf("%w: %s current entitlement is not effective", ErrOfflineEnforcementIncomplete, grant.Code)
		}
		apps = append(apps, grant.Code)
	}
	sort.Strings(apps)
	return apps, nil
}

func offlineEnforcementStatus(state domain.State, app string, status coordination.Status, specs []coordination.ServiceSpec, verify offlineSnapshotVerification, now time.Time) error {
	failure := func() error {
		return fmt.Errorf("%w: %s requires the full approved current snapshot and fresh consumer ACKs", ErrOfflineEnforcementIncomplete, app)
	}
	if status.Application.Application != app || status.State != runtime.Enforced || status.MigrationEligible || status.Revision == 0 || status.DeploymentRevision != state.Revision || !status.SnapshotCurrent || len(specs) == 0 || len(status.Members) != len(specs) {
		return failure()
	}
	expected := make(map[string]coordination.ServiceSpec, len(specs))
	for _, spec := range specs {
		if spec.Application != app || spec.Environment != "prod" || spec.ServiceID == "" || expected[spec.ServiceID].ServiceID != "" {
			return failure()
		}
		expected[spec.ServiceID] = spec
	}
	for _, member := range status.Members {
		spec, ok := expected[member.ServiceID]
		if !ok || member.Application != app || member.Environment != spec.Environment || member.OAuthClientID != spec.OAuthClientID || member.ImageDigest != spec.ImageDigest || member.CoverageDigest != spec.CoverageDigest || member.RetiredAt != nil || member.IssuedRevision != status.Revision || member.AckRevision != status.Revision || member.ReadyAt == nil || member.AckAt == nil {
			return failure()
		}
		for _, heartbeat := range []*time.Time{member.ReadyAt, member.AckAt} {
			if heartbeat.IsZero() || now.Sub(*heartbeat) > time.Minute || heartbeat.After(now.Add(time.Second)) {
				return failure()
			}
		}
		digest := sha256.Sum256([]byte(member.RawJWS))
		if member.RawJWS == "" || member.IssuedDigest != hex.EncodeToString(digest[:]) {
			return failure()
		}
		snapshot, err := verify(member.RawJWS, runtime.Binding{InstanceID: state.InstanceID, Environment: state.Environment, Application: app, ServiceID: member.ServiceID})
		if err != nil || snapshot.Revision != status.Revision || snapshot.EnforcementState != runtime.Enforced || snapshot.MigrationEligible || snapshot.HighestLicenseVersion != state.HighestVersion || snapshot.SnapshotIssuedAt > now.Unix() {
			return failure()
		}
		for _, slot := range []struct{ raw, digest string }{{snapshot.CurrentLicenseJWS, state.CurrentDigest}, {snapshot.PendingLicenseJWS, state.PendingDigest}} {
			hash := sha256.Sum256([]byte(slot.raw))
			if (slot.digest == "" && slot.raw != "") || (slot.digest != "" && (slot.raw == "" || hex.EncodeToString(hash[:]) != slot.digest)) {
				return failure()
			}
		}
		delete(expected, member.ServiceID)
	}
	return nil
}

func (r *OfflineDeliveryRunner) builtinRuntimeComponentSet(ctx context.Context, app string) (registry.RuntimeComponentSet, error) {
	if r.catalog == nil {
		return registry.RuntimeComponentSet{}, errors.New("runtime component catalog is not configured")
	}
	set, err := r.catalog.RuntimeComponentSet(ctx, app, "prod")
	if err != nil {
		return registry.RuntimeComponentSet{}, err
	}
	if set.Application != app || set.Environment != "prod" {
		return registry.RuntimeComponentSet{}, errors.New("runtime component catalog scope mismatch")
	}
	if err = registry.ValidateRuntimeComponentSet(set); err != nil {
		return registry.RuntimeComponentSet{}, err
	}
	return set, nil
}

func builtinRuntimeSpecs(state domain.State, app string, status coordination.Status, set registry.RuntimeComponentSet) ([]coordination.ServiceSpec, error) {
	if state.InstanceID == "" || status.Application.Application != app || status.Application.RegistrationMode != catalog.BuiltinComponentV2 || status.Application.CatalogVersion != set.CatalogVersion || set.Application != app || set.Environment != "prod" || len(set.Components) == 0 || len(status.Members) != len(set.Components) {
		return nil, errors.New("builtin runtime set does not match the registered application")
	}
	specs := make([]coordination.ServiceSpec, 0, len(set.Components))
	for _, component := range set.Components {
		if component.ServiceID == "" || component.RegistrationProtocol != catalog.RegistrationProtocolV2 || component.CatalogVersion != set.CatalogVersion || component.CredentialGeneration == "" {
			return nil, errors.New("builtin runtime component is invalid")
		}
		specs = append(specs, coordination.ServiceSpec{
			Application: app, Environment: "prod", ServiceID: component.ServiceID,
			OAuthClientID:        registry.RuntimeLicenseClientIDV2(state.InstanceID, app, "prod", component.ServiceID, component.CredentialGeneration),
			RegistrationProtocol: component.RegistrationProtocol, CatalogVersion: component.CatalogVersion, CredentialGeneration: component.CredentialGeneration,
		})
	}
	return specs, nil
}

func runtimeComponentSetDigest(set registry.RuntimeComponentSet) (string, error) {
	components := append([]registry.RuntimeComponentSpec(nil), set.Components...)
	sort.Slice(components, func(i, j int) bool { return components[i].ServiceID < components[j].ServiceID })
	raw, err := json.Marshal(struct {
		Application    string                          `json:"application"`
		Environment    string                          `json:"environment"`
		CatalogVersion int                             `json:"catalog_version"`
		Components     []registry.RuntimeComponentSpec `json:"components"`
	}{Application: set.Application, Environment: set.Environment, CatalogVersion: set.CatalogVersion, Components: components})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func offlineEnforcementStatusV2(state domain.State, app string, status coordination.Status, specs []coordination.ServiceSpec, verify offlineSnapshotVerification, now time.Time) error {
	failure := func() error {
		return fmt.Errorf("%w: %s requires the full approved current snapshot and fresh consumer ACKs", ErrOfflineEnforcementIncomplete, app)
	}
	if status.Application.Application != app || status.Application.RegistrationMode != catalog.BuiltinComponentV2 || status.State != runtime.Enforced || status.MigrationEligible || status.Revision == 0 || status.DeploymentRevision != state.Revision || !status.SnapshotCurrent || len(specs) == 0 || len(status.Members) != len(specs) {
		return failure()
	}
	expected := make(map[string]coordination.ServiceSpec, len(specs))
	for _, spec := range specs {
		if spec.Application != app || spec.Environment != "prod" || spec.ServiceID == "" || spec.RegistrationProtocol != catalog.RegistrationProtocolV2 || spec.CatalogVersion == 0 || spec.CredentialGeneration == "" || expected[spec.ServiceID].ServiceID != "" {
			return failure()
		}
		expected[spec.ServiceID] = spec
	}
	for _, member := range status.Members {
		spec, ok := expected[member.ServiceID]
		if !ok || member.Application != app || member.Environment != spec.Environment || member.OAuthClientID != spec.OAuthClientID || member.RegistrationProtocol != spec.RegistrationProtocol || member.CatalogVersion != spec.CatalogVersion || member.CredentialGeneration != spec.CredentialGeneration || member.RetiredAt != nil || member.IssuedRevision != status.Revision || member.AckRevision != status.Revision || member.ReadyAt == nil || member.AckAt == nil {
			return failure()
		}
		// V2 的 image/coverage 只保留为诊断字段，不是商业授权等值门槛。
		for _, heartbeat := range []*time.Time{member.ReadyAt, member.AckAt} {
			if heartbeat.IsZero() || now.Sub(*heartbeat) > time.Minute || heartbeat.After(now.Add(time.Second)) {
				return failure()
			}
		}
		digest := sha256.Sum256([]byte(member.RawJWS))
		if member.RawJWS == "" || member.IssuedDigest != hex.EncodeToString(digest[:]) {
			return failure()
		}
		snapshot, err := verify(member.RawJWS, runtime.Binding{InstanceID: state.InstanceID, Environment: state.Environment, Application: app, ServiceID: member.ServiceID})
		if err != nil || snapshot.Revision != status.Revision || snapshot.EnforcementState != runtime.Enforced || snapshot.MigrationEligible || snapshot.HighestLicenseVersion != state.HighestVersion || snapshot.SnapshotIssuedAt > now.Unix() {
			return failure()
		}
		for _, slot := range []struct{ raw, digest string }{{snapshot.CurrentLicenseJWS, state.CurrentDigest}, {snapshot.PendingLicenseJWS, state.PendingDigest}} {
			hash := sha256.Sum256([]byte(slot.raw))
			if (slot.digest == "" && slot.raw != "") || (slot.digest != "" && (slot.raw == "" || hex.EncodeToString(hash[:]) != slot.digest)) {
				return failure()
			}
		}
		delete(expected, member.ServiceID)
	}
	if len(expected) != 0 {
		return failure()
	}
	return nil
}

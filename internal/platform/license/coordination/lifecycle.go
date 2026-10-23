package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	runtime "github.com/J-S-Te/license-core/runtime"
	"gorm.io/gorm"
)

// LifecycleApproval 只能由受信任的发布清单读取器返回。必需成员与显式退役许可
// 绝不能来自运行时令牌，也不能照抄浏览器提交的候选组件清单。
type LifecycleApproval struct {
	Specs              []ServiceSpec
	RequiredServiceIDs []string
	RetireServiceIDs   []string
}

// LifecycleInput 是一次运行时生命周期对账请求的入参；ExpectedRevision 提供
// 乐观并发控制，ReleaseDigest 标识目标发布。
type LifecycleInput struct {
	Application      string
	Environment      string
	ExpectedRevision uint64
	OperationID      string
	ReleaseDigest    string
}

// LifecycleApprovalVerifier 校验生命周期请求并返回经审计批准的成员变更。
type LifecycleApprovalVerifier interface {
	VerifyRuntimeLifecycle(context.Context, LifecycleInput) (LifecycleApproval, error)
}

// WithLifecycleApprovalVerifier 注入生命周期审批验证器；v 为空时构造失败。
func WithLifecycleApprovalVerifier(v LifecycleApprovalVerifier) Option {
	return func(s *Service) error {
		if v == nil {
			return ErrInvalid
		}
		s.lifecycleApproval = v
		return nil
	}
}

// LifecycleEvent 记录一次已完成的生命周期对账；以 OperationID 幂等，重复提交
// 同一请求摘要直接成功。
type LifecycleEvent struct {
	OperationID   string `gorm:"primaryKey;size:128"`
	Application   string
	RequestDigest string
	ReleaseDigest string
	OldSpecs      string
	NewSpecs      string
	Revision      uint64
	TenantID      string
	UserID        string
	CreatedAt     time.Time
}

// TableName 绑定 GORM 模型到 license_runtime_lifecycle_event 表。
func (LifecycleEvent) TableName() string { return "license_runtime_lifecycle_event" }

// RetiredClient 永久记录已退役的 OAuth client 身份，防止其被任何后续注册复用。
type RetiredClient struct {
	OAuthClientID string `gorm:"primaryKey;column:oauth_client_id;size:128"`
	ServiceID     string
	RetiredAt     time.Time
}

// TableName 绑定 GORM 模型到 license_runtime_retired_client 表。
func (RetiredClient) TableName() string { return "license_runtime_retired_client" }

func canonicalApproval(in LifecycleInput, approval LifecycleApproval) (LifecycleApproval, error) {
	if !validApplication(in.Application) || !identifier.MatchString(in.Environment) || in.ExpectedRevision == 0 || !identifier.MatchString(in.OperationID) || !digestPattern.MatchString(in.ReleaseDigest) || len(approval.Specs) == 0 || len(approval.Specs) > 64 || len(approval.RequiredServiceIDs) == 0 {
		return approval, ErrInvalid
	}
	approval.Specs = append([]ServiceSpec(nil), approval.Specs...)
	for index := range approval.Specs {
		approval.Specs[index] = normalizeSpecRegistration(approval.Specs[index])
	}
	approval.RequiredServiceIDs = append([]string(nil), approval.RequiredServiceIDs...)
	approval.RetireServiceIDs = append([]string(nil), approval.RetireServiceIDs...)
	specs, clients := map[string]bool{}, map[string]bool{}
	for _, spec := range approval.Specs {
		if !validSpec(spec) || spec.Application != in.Application || spec.Environment != in.Environment || specs[spec.ServiceID] || clients[spec.OAuthClientID] {
			return approval, ErrInvalid
		}
		specs[spec.ServiceID], clients[spec.OAuthClientID] = true, true
	}
	seen := map[string]bool{}
	for _, id := range approval.RequiredServiceIDs {
		if !identifier.MatchString(id) || !specs[id] || seen[id] {
			return approval, ErrInvalid
		}
		seen[id] = true
	}
	seen = map[string]bool{}
	for _, id := range approval.RetireServiceIDs {
		if !identifier.MatchString(id) || specs[id] || seen[id] {
			return approval, ErrInvalid
		}
		seen[id] = true
	}
	sort.Slice(approval.Specs, func(i, j int) bool { return approval.Specs[i].ServiceID < approval.Specs[j].ServiceID })
	sort.Strings(approval.RequiredServiceIDs)
	sort.Strings(approval.RetireServiceIDs)
	return approval, nil
}
func memberSpec(m Member) ServiceSpec {
	return ServiceSpec{
		Application: m.Application, Environment: m.Environment, ServiceID: m.ServiceID, OAuthClientID: m.OAuthClientID,
		RegistrationProtocol: normalizeSpecRegistration(ServiceSpec{RegistrationProtocol: m.RegistrationProtocol}).RegistrationProtocol,
		CatalogVersion:       m.CatalogVersion, CredentialGeneration: m.CredentialGeneration,
		CoverageDigest: m.CoverageDigest, ImageDigest: m.ImageDigest,
	}
}
func validRetirementOwner(in LifecycleInput, m Member, requireTombstone bool) bool {
	return m.Application == in.Application && m.Environment == in.Environment && (!requireTombstone || m.RetiredAt != nil)
}

// ReconcileComponents 是内部受控发布操作，不是用户或机器端点。它原子替换整份
// 已批准的成员清单。已激活的应用在滚动部署期间绝不回到迁移兼容；新 client
// 绝不继承前一个 client 的 ACK。
func (s *Service) ReconcileComponents(ctx context.Context, in LifecycleInput, actor domain.Actor) error {
	if s.lifecycleApproval == nil || !identifier.MatchString(actor.TenantID) || !identifier.MatchString(actor.UserID) {
		return ErrForbidden
	}
	if principal, ok := appctx.PrincipalFromContext(ctx); ok && principal.HasScope("license.runtime") {
		return ErrForbidden
	}
	approved, err := s.lifecycleApproval.VerifyRuntimeLifecycle(ctx, in)
	if err != nil {
		return err
	}
	approved, err = canonicalApproval(in, approved)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(struct {
		Input    LifecycleInput
		Approval LifecycleApproval
		Actor    domain.Actor
	}{in, approved, actor})
	if err != nil {
		return err
	}
	requestDigest := hash(string(encoded))
	return s.lock(ctx, func(tx *gorm.DB, d *domain.Deployment, _ domain.State) error {
		var event LifecycleEvent
		err := tx.First(&event, "operation_id = ?", in.OperationID).Error
		if err == nil {
			if event.RequestDigest != requestDigest {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var app Application
		if err = tx.First(&app, "application = ?", in.Application).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotReady
			}
			return err
		}
		if app.Revision != in.ExpectedRevision {
			return ErrConflict
		}
		if app.State != runtime.Pending && app.State != runtime.Applying && app.State != runtime.Enforced {
			return domain.ErrCorrupt
		}
		var previous []Member
		if err = tx.Where("application = ? AND retired_at IS NULL", in.Application).Order("service_id ASC").Find(&previous).Error; err != nil {
			return err
		}
		current := map[string]Member{}
		desired := map[string]ServiceSpec{}
		retire := map[string]bool{}
		for _, member := range previous {
			current[member.ServiceID] = member
		}
		for _, spec := range approved.Specs {
			desired[spec.ServiceID] = spec
		}
		for _, id := range approved.RetireServiceIDs {
			retire[id] = true
			// 退役许可仅限本应用范围，包括幂等的墓碑操作。发布绝不能指名无关或
			// 臆造的服务，否则部署 Agent 可能据此停用本应用之外的进程。
			if member, present := current[id]; present {
				if !validRetirementOwner(in, member, false) {
					return ErrForbidden
				}
				continue
			}
			var tombstone Member
			lookup := tx.First(&tombstone, "service_id = ?", id).Error
			if errors.Is(lookup, gorm.ErrRecordNotFound) {
				return ErrForbidden
			}
			if lookup != nil {
				return lookup
			}
			if !validRetirementOwner(in, tombstone, true) {
				return ErrForbidden
			}
		}
		for _, member := range previous {
			spec, remains := desired[member.ServiceID]
			if !remains && !retire[member.ServiceID] {
				return ErrForbidden
			}
			if remains && memberSpec(member) != spec && member.OAuthClientID == spec.OAuthClientID {
				return ErrInvalid
			}
		}
		oldSpecs := make([]ServiceSpec, 0, len(previous))
		for _, member := range previous {
			oldSpecs = append(oldSpecs, memberSpec(member))
		}
		oldJSON, err := json.Marshal(oldSpecs)
		if err != nil {
			return err
		}
		newJSON, err := json.Marshal(approved.Specs)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		for _, spec := range approved.Specs {
			var count int64
			if err = tx.Model(&RetiredClient{}).Where("oauth_client_id = ?", spec.OAuthClientID).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return ErrConflict
			}
		}
		for _, member := range previous {
			replacement, remains := desired[member.ServiceID]
			if !remains || memberSpec(member) != replacement {
				if err = tx.Create(&RetiredClient{OAuthClientID: member.OAuthClientID, ServiceID: member.ServiceID, RetiredAt: now}).Error; err != nil {
					return err
				}
			}
			if _, remains := desired[member.ServiceID]; !remains {
				if err = tx.Model(&member).Updates(map[string]any{"retired_at": now, "ready_at": nil, "ack_at": nil, "ack_revision": 0, "issued_revision": 0, "issued_digest": "", "raw_jws": ""}).Error; err != nil {
					return err
				}
			}
		}
		environment := s.applicationEnvironment
		if environment == "" {
			environment = d.Environment
		}
		for _, spec := range approved.Specs {
			if spec.Environment != environment {
				return ErrInvalid
			}
			old, exists := current[spec.ServiceID]
			if exists {
				if memberSpec(old) == spec {
					continue
				}
				if err = tx.Model(&old).Updates(map[string]any{"oauth_client_id": spec.OAuthClientID, "registration_protocol": spec.RegistrationProtocol, "catalog_version": spec.CatalogVersion, "credential_generation": spec.CredentialGeneration, "coverage_digest": spec.CoverageDigest, "image_digest": spec.ImageDigest, "ready_at": nil}).Error; err != nil {
					return err
				}
			} else {
				// 只有这份显式批准的发布才能恢复一个逻辑服务；其旧 OAuth 身份
				// 保持永久退役。
				var retired Member
				lookup := tx.First(&retired, "service_id = ?", spec.ServiceID).Error
				if lookup == nil {
					if retired.RetiredAt == nil || retired.Application != spec.Application || retired.Environment != spec.Environment || retired.OAuthClientID == spec.OAuthClientID {
						return ErrConflict
					}
					if err = tx.Model(&retired).Updates(map[string]any{"retired_at": nil, "oauth_client_id": spec.OAuthClientID, "registration_protocol": spec.RegistrationProtocol, "catalog_version": spec.CatalogVersion, "credential_generation": spec.CredentialGeneration, "coverage_digest": spec.CoverageDigest, "image_digest": spec.ImageDigest, "ready_at": nil, "ack_at": nil, "ack_revision": 0, "issued_revision": 0, "issued_digest": "", "raw_jws": ""}).Error; err != nil {
						return err
					}
					continue
				}
				if !errors.Is(lookup, gorm.ErrRecordNotFound) {
					return lookup
				}
				var count int64
				if err = tx.Model(&Member{}).Where("service_id = ? OR oauth_client_id = ?", spec.ServiceID, spec.OAuthClientID).Count(&count).Error; err != nil {
					return err
				}
				if count != 0 {
					return ErrConflict
				}
				if err = tx.Create(&Member{ServiceID: spec.ServiceID, Application: spec.Application, Environment: spec.Environment, OAuthClientID: spec.OAuthClientID, RegistrationProtocol: spec.RegistrationProtocol, CatalogVersion: spec.CatalogVersion, CredentialGeneration: spec.CredentialGeneration, CoverageDigest: spec.CoverageDigest, ImageDigest: spec.ImageDigest, CreatedAt: now}).Error; err != nil {
					return err
				}
			}
		}
		// 强制执行是单调策略，不是发布就绪指示器。既有消费方会持久化 ENFORCED
		// 并正确把 APPLYING 快照判为回滚。保持该策略的同时，下文撤销当前发布的
		// 全部 ACK；部署完成仍要求整份已批准成员（含未变化的镜像）给出新鲜
		// READY/ACK。只有最初的待激活迁移能保留其冻结资格；APPLYING/ENFORCED
		// 在替换期间绝不恢复兼容。
		if app.State != runtime.Pending {
			app.MigrationEligible = false
		}
		app.ContentDigest = contentDigest(app, d)
		if err = s.bump(tx, d, &app); err != nil {
			return err
		}
		if err = tx.Model(&Member{}).Where("application = ? AND retired_at IS NULL", in.Application).Updates(map[string]any{"ack_revision": 0, "ack_at": nil, "issued_revision": 0, "issued_digest": "", "raw_jws": ""}).Error; err != nil {
			return err
		}
		event = LifecycleEvent{OperationID: in.OperationID, Application: in.Application, RequestDigest: requestDigest, ReleaseDigest: in.ReleaseDigest, OldSpecs: string(oldJSON), NewSpecs: string(newJSON), Revision: app.Revision, TenantID: actor.TenantID, UserID: actor.UserID, CreatedAt: now}
		if err = tx.Create(&event).Error; err != nil {
			return err
		}
		return tx.Create(&domain.Event{Kind: "RUNTIME_COMPONENTS_RECONCILED", Digest: strings.TrimPrefix(in.ReleaseDigest, "sha256:"), Revision: app.Revision, TenantID: actor.TenantID, UserID: actor.UserID, CreatedAt: now}).Error
	})
}

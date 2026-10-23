package coordination

import (
	"context"
	"errors"
	"sort"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/license-core/catalog"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BuiltinComponentSource 提供随二进制编译的内置目录；绝不是运行时批准文件，
// 也不是浏览器提交的组件清单。
type BuiltinComponentSource interface {
	BuiltinComponents(context.Context, string, string) ([]catalog.Component, error)
}

// WithBuiltinComponentSource 注入内置目录来源；source 为空时构造失败。
func WithBuiltinComponentSource(source BuiltinComponentSource) Option {
	return func(s *Service) error {
		if source == nil {
			return ErrInvalid
		}
		s.builtinCatalog = source
		return nil
	}
}

// BuiltinReconcileInput 是内部受控更新动作的入参。它不携带 client ID 或任意规格；
// 稳定的 V2 身份由内置目录与部署实例共同确定。
type BuiltinReconcileInput struct {
	Application          string
	Environment          string
	OperationID          string
	ExpectedRevision     uint64
	CatalogVersion       int
	CredentialGeneration string
	// ServiceIDs 把桥接范围限定到所选成员；为空表示完整目录。
	// 未入选的成员保持既有 V1/V2 状态不变。
	ServiceIDs []string
}

// ReconcileBuiltinComponents 依据内置目录把应用迁移到 V2 桥接：V1 成员被目录身份
// 替换，新成员从无 READY/ACK 起步；ENFORCED 状态与迁移资格保持不变。
func (s *Service) ReconcileBuiltinComponents(ctx context.Context, in BuiltinReconcileInput, actor domain.Actor) error {
	if s.builtinCatalog == nil || !validApplication(in.Application) || !identifier.MatchString(in.Environment) || !identifier.MatchString(in.OperationID) || in.ExpectedRevision == 0 || in.CatalogVersion != catalog.Version || !identifier.MatchString(in.CredentialGeneration) || !identifier.MatchString(actor.TenantID) || !identifier.MatchString(actor.UserID) {
		return ErrInvalid
	}
	components, err := s.builtinCatalog.BuiltinComponents(ctx, in.Application, in.Environment)
	if err != nil || len(components) == 0 || len(components) > 64 {
		return ErrNotReady
	}
	components, err = builtinBusinessComponents(in.Application, components)
	if err != nil {
		return err
	}
	selected := map[string]bool{}
	for _, serviceID := range in.ServiceIDs {
		if !identifier.MatchString(serviceID) || selected[serviceID] {
			return ErrInvalid
		}
		selected[serviceID] = true
	}
	all := make(map[string]catalog.Component, len(components))
	desired := make(map[string]ServiceSpec, len(components))
	required := make([]string, 0, len(components))
	for _, component := range components {
		if component.Role == catalog.RoleInfrastructure || component.RegistrationProtocol != catalog.RegistrationProtocolV2 || component.Application != in.Application {
			return ErrInvalid
		}
		if _, exists := all[component.ServiceID]; exists {
			return ErrInvalid
		}
		all[component.ServiceID] = component
		if len(selected) > 0 && !selected[component.ServiceID] {
			continue
		}
		desired[component.ServiceID] = ServiceSpec{
			Application: in.Application, Environment: in.Environment, ServiceID: component.ServiceID,
			OAuthClientID:        catalog.ClientIDV2("", in.Application, in.Environment, component.ServiceID, in.CredentialGeneration),
			RegistrationProtocol: catalog.RegistrationProtocolV2, CatalogVersion: in.CatalogVersion, CredentialGeneration: in.CredentialGeneration,
		}
		if !component.Conditional {
			required = append(required, component.ServiceID)
		}
	}
	if len(selected) > 0 {
		for serviceID := range selected {
			if _, ok := all[serviceID]; !ok {
				return ErrInvalid
			}
		}
	}
	sort.Strings(required)
	if len(desired) == 0 || len(required) == 0 {
		return ErrInvalid
	}
	return s.lock(ctx, func(tx *gorm.DB, d *domain.Deployment, _ domain.State) error {
		var app Application
		if e := tx.First(&app, "application = ?", in.Application).Error; e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				return ErrNotReady
			}
			return e
		}
		if app.Revision != in.ExpectedRevision {
			return ErrConflict
		}
		targetMode := app.RegistrationMode
		switch app.RegistrationMode {
		case "", catalog.LegacyImageV1:
			targetMode = catalog.BridgingComponentV2
		case catalog.BridgingComponentV2, catalog.BuiltinComponentV2:
			targetMode = app.RegistrationMode
		default:
			return ErrInvalid
		}
		var previous []Member
		if e := tx.Where("application = ? AND retired_at IS NULL", in.Application).Order("service_id ASC").Find(&previous).Error; e != nil {
			return e
		}
		current := map[string]Member{}
		for _, member := range previous {
			current[member.ServiceID] = member
		}
		changed := app.RegistrationMode != targetMode || app.CatalogVersion != in.CatalogVersion
		for serviceID, spec := range desired {
			spec.OAuthClientID = catalog.ClientIDV2(d.InstanceID, in.Application, in.Environment, serviceID, in.CredentialGeneration)
			desired[serviceID] = spec
			member, exists := current[serviceID]
			if !exists {
				changed = true
				continue
			}
			if memberSpec(member) != spec {
				changed = true
			}
		}
		for serviceID := range current {
			if _, catalogMember := all[serviceID]; !catalogMember {
				changed = true
			}
		}
		if !changed {
			return nil
		}
		// 桥接只是注册模式，绝不是迁移资格。
		now := s.now().UTC()
		for serviceID, member := range current {
			spec, selectedForBridge := desired[serviceID]
			if !selectedForBridge {
				if _, catalogMember := all[serviceID]; catalogMember {
					// 部分桥接之外的目录成员：保留其当前协议与 ready/ack 状态。
					continue
				}
				if e := tx.Create(&RetiredClient{OAuthClientID: member.OAuthClientID, ServiceID: member.ServiceID, RetiredAt: now}).Error; e != nil && !errors.Is(e, gorm.ErrDuplicatedKey) {
					return e
				}
				if e := tx.Model(&member).Updates(map[string]any{"retired_at": now, "ready_at": nil, "ack_at": nil, "ack_revision": 0, "issued_revision": 0, "issued_digest": "", "raw_jws": ""}).Error; e != nil {
					return e
				}
				continue
			}
			if memberSpec(member) == spec {
				continue
			}
			if member.OAuthClientID != spec.OAuthClientID {
				if e := tx.Create(&RetiredClient{OAuthClientID: member.OAuthClientID, ServiceID: member.ServiceID, RetiredAt: now}).Error; e != nil && !errors.Is(e, gorm.ErrDuplicatedKey) {
					return e
				}
			}
			if e := tx.Model(&member).Updates(map[string]any{
				"oauth_client_id": spec.OAuthClientID, "registration_protocol": spec.RegistrationProtocol,
				"catalog_version": spec.CatalogVersion, "credential_generation": spec.CredentialGeneration,
				"coverage_digest": "", "image_digest": "", "ready_at": nil, "ack_at": nil,
				"ack_revision": 0, "issued_revision": 0, "issued_digest": "", "raw_jws": "",
			}).Error; e != nil {
				return e
			}
		}
		for serviceID, spec := range desired {
			if _, exists := current[serviceID]; exists {
				continue
			}
			var retired RetiredClient
			if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&retired, "oauth_client_id = ?", spec.OAuthClientID).Error; e == nil {
				return ErrConflict
			} else if !errors.Is(e, gorm.ErrRecordNotFound) {
				return e
			}
			if e := tx.Create(&Member{ServiceID: spec.ServiceID, Application: spec.Application, Environment: spec.Environment, OAuthClientID: spec.OAuthClientID,
				RegistrationProtocol: spec.RegistrationProtocol, CatalogVersion: spec.CatalogVersion, CredentialGeneration: spec.CredentialGeneration,
				CoverageDigest: "", ImageDigest: "", CreatedAt: now}).Error; e != nil {
				return e
			}
		}
		app.RegistrationMode = targetMode
		app.CatalogVersion = in.CatalogVersion
		app.ContentDigest = contentDigest(app, d)
		if e := s.bump(tx, d, &app); e != nil {
			return e
		}
		return tx.Create(&domain.Event{Kind: "RUNTIME_COMPONENTS_BRIDGED", Revision: d.Revision, TenantID: actor.TenantID, UserID: actor.UserID, CreatedAt: now}).Error
	})
}

// builtinBusinessComponents 校验内置目录组件并剔除基础设施角色。基础设施组件
// （当前为 Metabase）由部署拓扑管理，不签发运行时凭据，也不进入桥接成员集合；
// 目录中存在基础设施组件不应让整个应用无法桥接。
func builtinBusinessComponents(app string, components []catalog.Component) ([]catalog.Component, error) {
	seen := make(map[string]struct{}, len(components))
	result := make([]catalog.Component, 0, len(components))
	for _, component := range components {
		if component.RegistrationProtocol != catalog.RegistrationProtocolV2 || component.Application != app {
			return nil, ErrInvalid
		}
		if _, exists := seen[component.ServiceID]; exists {
			return nil, ErrInvalid
		}
		seen[component.ServiceID] = struct{}{}
		if component.Role == catalog.RoleInfrastructure {
			continue
		}
		result = append(result, component)
	}
	return result, nil
}

// Package infrastructure persists external identity operations in the platform schema.
package infrastructure

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/audit/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const activeStatus = "ACTIVE"

type GORMRepository struct {
	database *gorm.DB
	audit    *auditapplication.Service
}

func NewGORMRepository(database *gorm.DB, audit *auditapplication.Service) (*GORMRepository, error) {
	if database == nil || audit == nil {
		return nil, errors.New("external identity repository dependencies must not be nil")
	}
	return &GORMRepository{database: database, audit: audit}, nil
}

type identityModel struct {
	ID, TenantID, PlatformUserID, AccountNo, Status string
	EmailDigest, MobileDigest                       []byte
}

func (identityModel) TableName() string { return "iam_external_identity" }

type idempotencyModel struct {
	ID, TenantID, OAuthClientID, Operation, IdempotencyKey, ResourceID, ResultJSON string
	RequestHash                                                                    []byte
}

func (idempotencyModel) TableName() string { return "iam_external_identity_idempotency" }

func (repository *GORMRepository) Provision(ctx context.Context, command application.ProvisionCommand) (application.ProvisionResult, error) {
	var response application.ProvisionResult
	// nonce、幂等结果、用户/账号/外部身份和审计事件在同一事务中提交，避免平台返回成功却缺少任一安全记录。
	err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := recordNonce(tx, command.Principal.TenantID, command.NonceHash[:], command.NonceExpiresAt, command.OccurredAt); err != nil {
			return err
		}
		var err error
		response, err = replayProvision(tx, command)
		if err == nil {
			return ensureExternalLoginAccount(tx, command, response.PlatformUserID, response.AccountNo)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		// 邮箱和手机号独立查重；若二者命中不同身份，不能猜测合并，必须显式报冲突。
		byEmail, err := findIdentityByDigest(tx, command.Principal.TenantID, "email_digest", command.EmailDigest)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		byMobile, mobileErr := findIdentityByDigest(tx, command.Principal.TenantID, "mobile_digest", command.MobileDigest)
		if mobileErr != nil && !errors.Is(mobileErr, gorm.ErrRecordNotFound) {
			return mobileErr
		}
		identity := identityModel{}
		if err == nil {
			identity = byEmail
		}
		if mobileErr == nil {
			if identity.ID != "" && identity.ID != byMobile.ID {
				return application.ErrConflict
			}
			identity = byMobile
		}
		loginAccountCreated := false
		if identity.ID == "" {
			operator := command.Principal.OAuthClientID
			employeeNo := "EXT-" + strings.ToUpper(command.PlatformUserID)
			if err := tx.Table("iam_user").Create(map[string]any{
				"id": command.PlatformUserID, "tenant_id": command.Principal.TenantID, "employee_no": employeeNo,
				"display_name": command.DisplayName, "email": command.Email, "mobile_ciphertext": nullableBytes(command.MobileCipher),
				"mobile_hash": nullableBytes(command.MobileDigest), "employment_status": "EXTERNAL_CUSTOMER", "status": activeStatus,
				"version": 1, "created_at": command.OccurredAt, "created_by": operator, "updated_at": command.OccurredAt, "updated_by": operator,
			}).Error; err != nil {
				return mapWriteError(err)
			}
			// iam_external_identity.login_account_id is NOT NULL since
			// migration 000071, so the HUMAN/LOCAL login account must exist
			// before the identity row is inserted. No password credential is
			// created; initialization stays an explicit admin lifecycle action.
			accountID, accountErr := createExternalLoginAccount(tx, command, command.PlatformUserID, command.AccountNo)
			if accountErr != nil {
				return mapWriteError(accountErr)
			}
			identity = identityModel{ID: command.IdentityID, TenantID: command.Principal.TenantID, PlatformUserID: command.PlatformUserID, AccountNo: command.AccountNo, EmailDigest: command.EmailDigest, MobileDigest: command.MobileDigest, Status: domain.IdentityPendingActivation}
			if err := tx.Table(identityModel{}.TableName()).Create(map[string]any{
				"id": identity.ID, "tenant_id": identity.TenantID, "platform_user_id": identity.PlatformUserID, "account_no": identity.AccountNo,
				"login_account_id": accountID, "email_digest": nullableBytes(identity.EmailDigest), "mobile_digest": nullableBytes(identity.MobileDigest), "status": identity.Status,
				"created_at": command.OccurredAt, "created_by": operator, "updated_at": command.OccurredAt, "updated_by": operator,
			}).Error; err != nil {
				return mapWriteError(err)
			}
			loginAccountCreated = true
		} else {
			if identity.Status == domain.IdentityDisabled {
				return application.ErrConflict
			}
			if len(command.EmailDigest) > 0 && len(identity.EmailDigest) > 0 && !application.SameDigest(command.EmailDigest, identity.EmailDigest) {
				return application.ErrConflict
			}
			if len(command.MobileDigest) > 0 && len(identity.MobileDigest) > 0 && !application.SameDigest(command.MobileDigest, identity.MobileDigest) {
				return application.ErrConflict
			}
			updates := map[string]any{"updated_at": command.OccurredAt, "updated_by": command.Principal.OAuthClientID}
			if len(identity.EmailDigest) == 0 && len(command.EmailDigest) > 0 {
				updates["email_digest"] = command.EmailDigest
			}
			if len(identity.MobileDigest) == 0 && len(command.MobileDigest) > 0 {
				updates["mobile_digest"] = command.MobileDigest
			}
			if len(updates) > 2 {
				if err := tx.Model(&identityModel{}).Where("id = ? AND tenant_id = ?", identity.ID, identity.TenantID).Updates(updates).Error; err != nil {
					return mapWriteError(err)
				}
			}
		}
		// 这里只预留 HUMAN/LOCAL 登录账号，不创建密码凭据。凭据初始化仍由平台管理员显式完成，
		// 因而 CRM 和门户永远不接触密码。新身份已在插入时关联账号；该补偿只服务历史或重放身份，
		// 避免同值 UPDATE 的零受影响行被误判为冲突。
		if !loginAccountCreated {
			if err := ensureExternalLoginAccount(tx, command, identity.PlatformUserID, identity.AccountNo); err != nil {
				return err
			}
		}
		response = application.ProvisionResult{PlatformUserID: identity.PlatformUserID, AccountNo: identity.AccountNo}
		if err := createIdempotency(tx, command.Principal.TenantID, command.Principal.OAuthClientID, "PROVISION", command.IdempotencyKey, command.RequestHash[:], identity.ID, encodeProvision(response), command.OccurredAt); err != nil {
			return err
		}
		return ingestAudit(ctx, tx, repository.audit, command.EventID, command.Principal, command.OccurredAt, "external_identity.provision", identity.PlatformUserID, "外部客户身份已预置，等待平台凭据激活", map[string]any{"account_no": identity.AccountNo, "identity_status": identity.Status})
	})
	return response, err
}

func ensureExternalLoginAccount(tx *gorm.DB, command application.ProvisionCommand, platformUserID, accountNo string) error {
	var identity identityModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND platform_user_id = ?", command.Principal.TenantID, platformUserID).
		Take(&identity).Error; err != nil {
		return err
	}
	accountID, err := createExternalLoginAccount(tx, command, platformUserID, accountNo)
	if err != nil {
		return err
	}
	update := tx.Model(&identityModel{}).
		Where("id = ? AND tenant_id = ? AND (login_account_id IS NULL OR login_account_id = ?)", identity.ID, identity.TenantID, accountID).
		Update("login_account_id", accountID)
	if update.Error != nil {
		return mapWriteError(update.Error)
	}
	if update.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

// createExternalLoginAccount 为外部客户预留唯一 ACTIVE HUMAN/LOCAL 账号，但不创建密码凭据。
// 已存在的同构账号可幂等复用；账号类型或认证源不一致时拒绝接管，防止覆盖其他登录体系。
func createExternalLoginAccount(tx *gorm.DB, command application.ProvisionCommand, platformUserID, accountNo string) (string, error) {
	var account struct {
		ID, Username, AccountType, AuthSource string
	}
	result := tx.Table("iam_account").
		Select("id, username, account_type, auth_source").
		Where("tenant_id = ? AND user_id = ? AND username = ?", command.Principal.TenantID, platformUserID, accountNo).
		Take(&account)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		operator := command.Principal.OAuthClientID
		if err := tx.Table("iam_account").Create(map[string]any{
			"id": command.AccountID, "tenant_id": command.Principal.TenantID, "user_id": platformUserID,
			"username": accountNo, "account_type": "HUMAN", "auth_source": "LOCAL", "status": activeStatus,
			"version": 1, "created_at": command.OccurredAt, "created_by": operator, "updated_at": command.OccurredAt, "updated_by": operator,
		}).Error; err != nil {
			return "", mapWriteError(err)
		}
		return command.AccountID, nil
	}
	if result.Error != nil {
		return "", result.Error
	}
	if account.Username != accountNo || account.AccountType != "HUMAN" || account.AuthSource != "LOCAL" {
		return "", application.ErrConflict
	}
	return account.ID, nil
}

func (repository *GORMRepository) AssignRole(ctx context.Context, command application.RoleCommand) (domain.RoleResult, error) {
	return repository.changeRole(ctx, command, false)
}

func (repository *GORMRepository) RevokeRole(ctx context.Context, command application.RoleCommand) (domain.RoleResult, error) {
	return repository.changeRole(ctx, command, true)
}

func (repository *GORMRepository) changeRole(ctx context.Context, command application.RoleCommand, revoke bool) (domain.RoleResult, error) {
	operation, action, summary := "ASSIGN_ROLE", "application_role.assign", "Portal 客户角色已分配"
	if revoke {
		operation, action, summary = "REVOKE_ROLE", "application_role.revoke", "Portal 客户角色已回收"
	}
	var response domain.RoleResult
	// 角色绑定、策略修订号、幂等结果和高风险审计必须原子提交。策略修订号用于让子系统尽快淘汰旧权限缓存。
	err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := recordNonce(tx, command.Principal.TenantID, command.NonceHash[:], command.NonceExpiresAt, command.OccurredAt); err != nil {
			return err
		}
		var err error
		response, err = replayRole(tx, command, operation)
		if err == nil || !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var identity identityModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND platform_user_id = ? AND status <> ?", command.Principal.TenantID, command.PlatformUserID, domain.IdentityDisabled).Take(&identity).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return application.ErrNotFound
			}
			return err
		}
		// 角色必须来自已同步的应用授权目录，而不是仅按客户端提交的 code 命中任意同名角色。
		var target struct{ ApplicationID, RoleID string }
		if err := tx.Table("platform_application AS application").Select("application.id AS application_id, role.id AS role_id").Joins("JOIN authz_role AS role ON role.tenant_id = application.tenant_id AND role.application_id = application.id AND role.code = ? AND role.role_type = ? AND role.status = ?", command.RoleCode, "APPLICATION", activeStatus).Joins("JOIN authz_authorization_catalog AS catalog ON catalog.tenant_id = application.tenant_id AND catalog.application_id = application.id AND catalog.sync_status = ?", "SYNCED").Where("application.tenant_id = ? AND application.code = ? AND application.status = ?", command.Principal.TenantID, command.ApplicationCode, activeStatus).Take(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return application.ErrUnavailable
			}
			return err
		}
		operator := command.Principal.OAuthClientID
		if revoke {
			result := tx.Table("authz_role_binding").Where("tenant_id = ? AND application_id = ? AND role_id = ? AND subject_type = ? AND subject_id = ? AND scope_type = ? AND scope_id = '' AND status = ?", command.Principal.TenantID, target.ApplicationID, target.RoleID, "USER", command.PlatformUserID, "TENANT", activeStatus).Updates(map[string]any{"status": domain.IdentityDisabled, "updated_at": command.OccurredAt, "updated_by": operator, "version": gorm.Expr("version + 1")})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return application.ErrNotFound
			}
		} else {
			binding := map[string]any{"id": command.BindingID, "tenant_id": command.Principal.TenantID, "application_id": target.ApplicationID, "role_id": target.RoleID, "subject_type": "USER", "subject_id": command.PlatformUserID, "scope_type": "TENANT", "scope_id": "", "status": activeStatus, "grant_origin": "SYSTEM", "origin_id": identity.ID, "origin_item_id": "", "version": 1, "created_at": command.OccurredAt, "created_by": operator, "updated_at": command.OccurredAt, "updated_by": operator}
			if err := tx.Table("authz_role_binding").Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}, {Name: "application_id"}, {Name: "role_id"}, {Name: "subject_type"}, {Name: "subject_id"}, {Name: "scope_type"}, {Name: "scope_id"}}, DoUpdates: clause.Assignments(map[string]any{"status": activeStatus, "updated_at": command.OccurredAt, "updated_by": operator, "version": gorm.Expr("version + 1")})}).Create(binding).Error; err != nil {
				return mapWriteError(err)
			}
		}
		revision := tx.Table("authz_policy_revision").Where("tenant_id = ? AND application_id = ?", command.Principal.TenantID, target.ApplicationID).Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "changed_at": command.OccurredAt, "change_reason": summary})
		if revision.Error != nil {
			return revision.Error
		}
		if revision.RowsAffected != 1 {
			return application.ErrUnavailable
		}
		status := activeStatus
		if revoke {
			status = domain.IdentityDisabled
		}
		response = domain.RoleResult{PlatformUserID: command.PlatformUserID, ApplicationCode: command.ApplicationCode, RoleCode: command.RoleCode, Status: status}
		if err := createIdempotency(tx, command.Principal.TenantID, command.Principal.OAuthClientID, operation, command.IdempotencyKey, command.RequestHash[:], identity.ID, encodeRole(response), command.OccurredAt); err != nil {
			return err
		}
		return ingestAudit(ctx, tx, repository.audit, command.EventID, command.Principal, command.OccurredAt, action, command.PlatformUserID, summary, map[string]any{"application_code": command.ApplicationCode, "role_code": command.RoleCode, "status": status})
	})
	return response, err
}

func recordNonce(tx *gorm.DB, tenantID string, digest []byte, expiresAt, now any) error {
	// 唯一键承担并发防重放：两个相同 nonce 同时到达时，只有一个事务能够插入成功。
	if err := tx.Exec("DELETE FROM iam_external_identity_nonce_replay WHERE expires_at <= ?", now).Error; err != nil {
		return err
	}
	if err := tx.Table("iam_external_identity_nonce_replay").Create(map[string]any{"tenant_id": tenantID, "nonce_hash": digest, "expires_at": expiresAt, "created_at": now}).Error; err != nil {
		if isDuplicate(err) {
			return application.ErrReplay
		}
		return err
	}
	return nil
}

func replayProvision(tx *gorm.DB, command application.ProvisionCommand) (application.ProvisionResult, error) {
	row, err := loadIdempotency(tx, command.Principal.TenantID, command.Principal.OAuthClientID, "PROVISION", command.IdempotencyKey, command.RequestHash[:])
	if err != nil {
		return application.ProvisionResult{}, err
	}
	parts := strings.Split(row.ResultJSON, "\x00")
	if len(parts) != 2 {
		return application.ProvisionResult{}, application.ErrConflict
	}
	return application.ProvisionResult{PlatformUserID: parts[0], AccountNo: parts[1]}, nil
}

func replayRole(tx *gorm.DB, command application.RoleCommand, operation string) (domain.RoleResult, error) {
	row, err := loadIdempotency(tx, command.Principal.TenantID, command.Principal.OAuthClientID, operation, command.IdempotencyKey, command.RequestHash[:])
	if err != nil {
		return domain.RoleResult{}, err
	}
	parts := strings.Split(row.ResultJSON, "\x00")
	if len(parts) != 4 {
		return domain.RoleResult{}, application.ErrConflict
	}
	return domain.RoleResult{PlatformUserID: parts[0], ApplicationCode: parts[1], RoleCode: parts[2], Status: parts[3]}, nil
}

func loadIdempotency(tx *gorm.DB, tenantID, clientID, operation, key string, digest []byte) (idempotencyModel, error) {
	var row idempotencyModel
	if err := tx.Where("tenant_id = ? AND oauth_client_id = ? AND operation = ? AND idempotency_key = ?", tenantID, clientID, operation, key).Take(&row).Error; err != nil {
		return row, err
	}
	// 相同幂等键只允许重放同一规范化请求；否则返回冲突而不是泄漏或复用旧结果。
	if !application.SameDigest(row.RequestHash, digest) {
		return row, application.ErrConflict
	}
	return row, nil
}

func createIdempotency(tx *gorm.DB, tenantID, clientID, operation, key string, digest []byte, resourceID, result string, now any) error {
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(tenantID+"\x00"+clientID+"\x00"+operation+"\x00"+key)))
	return tx.Table("iam_external_identity_idempotency").Create(map[string]any{"id": sum, "tenant_id": tenantID, "oauth_client_id": clientID, "operation": operation, "idempotency_key": key, "request_hash": digest, "resource_id": resourceID, "result_json": result, "created_at": now, "completed_at": now}).Error
}

func findIdentityByDigest(tx *gorm.DB, tenantID, column string, digest []byte) (identityModel, error) {
	if len(digest) == 0 {
		return identityModel{}, gorm.ErrRecordNotFound
	}
	var row identityModel
	return row, tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND "+column+" = ?", tenantID, digest).Take(&row).Error
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
func encodeProvision(value application.ProvisionResult) string {
	return value.PlatformUserID + "\x00" + value.AccountNo
}
func encodeRole(value domain.RoleResult) string {
	return value.PlatformUserID + "\x00" + value.ApplicationCode + "\x00" + value.RoleCode + "\x00" + value.Status
}

func mapWriteError(err error) error {
	if isDuplicate(err) {
		return application.ErrConflict
	}
	return err
}
func isDuplicate(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate")
}

func ingestAudit(ctx context.Context, tx *gorm.DB, _ *auditapplication.Service, eventID string, principal appctx.Principal, occurredAt time.Time, action, resourceID, summary string, metadata map[string]any) error {
	// 使用当前事务重新装配审计仓储，使审计落库与身份变更共享提交/回滚边界。
	repository, err := auditinfrastructure.NewRepository(tx)
	if err != nil {
		return err
	}
	service, err := auditapplication.NewService(repository, fixedAuditID{value: eventID}, fixedClock{value: occurredAt})
	if err != nil {
		return err
	}
	_, err = service.Ingest(ctx, principal.TenantID, auditapplication.EventInput{
		EventID: eventID, ApplicationCode: principal.ApplicationCode, EnvironmentCode: principal.EnvironmentCode,
		ActorType: "APPLICATION", ActorID: principal.OAuthClientID, ClientID: principal.ClientID,
		OccurredAt: occurredAt, Action: action, ResourceType: "external_identity", ResourceID: resourceID,
		Result: "SUCCESS", RiskLevel: "HIGH", Classification: "CONFIDENTIAL", Summary: summary,
		Metadata: metadata, EventCategory: "PLATFORM_IDENTITY", EventType: action,
	})
	return err
}

type fixedAuditID struct{ value string }

func (generator fixedAuditID) New(time.Time) (string, error) { return generator.value, nil }

type fixedClock struct{ value time.Time }

func (clock fixedClock) Now() time.Time { return clock.value }

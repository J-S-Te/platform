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
	platformulid "github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
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
	ID, TenantID, PlatformUserID, AccountNo, LoginAccountID, Status string
	EmailDigest, MobileDigest                                       []byte
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
			var migrated bool
			response.AccountNo, migrated, err = ensureExternalLoginAccount(tx, command, response.PlatformUserID)
			if err != nil || !migrated {
				return err
			}
			// 老幂等记录中的结果可能仍是 EXT-...。迁移成功后以当前事务读取到的
			// 手机号账号响应，并单独记录安全审计；不改写历史幂等记录和历史审计。
			return ingestAudit(ctx, tx, repository.audit, command.EventID, command.Principal, command.OccurredAt,
				"external_identity.login_name_migrate", response.PlatformUserID, "外部客户存量登录账号已迁移",
				map[string]any{"login_name_migrated": true})
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
			if credentialErr := ensureExternalPasswordCredential(tx, command, accountID); credentialErr != nil {
				return mapWriteError(credentialErr)
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
		loginNameMigrated := false
		// 新身份已在插入时关联账号；该补偿服务历史身份或预置重放，并在身份、账号
		// 同时加锁后把旧 EXT-... 登录名原地迁移为本次已校验的联系人手机号。
		if !loginAccountCreated {
			var ensuredAccountNo string
			ensuredAccountNo, loginNameMigrated, err = ensureExternalLoginAccount(tx, command, identity.PlatformUserID)
			if err != nil {
				return err
			}
			identity.AccountNo = ensuredAccountNo
		}
		response = application.ProvisionResult{PlatformUserID: identity.PlatformUserID, AccountNo: identity.AccountNo}
		if err := createIdempotency(tx, command.Principal.TenantID, command.Principal.OAuthClientID, "PROVISION", command.IdempotencyKey, command.RequestHash[:], identity.ID, encodeProvision(response), command.OccurredAt); err != nil {
			return err
		}
		return ingestAudit(ctx, tx, repository.audit, command.EventID, command.Principal, command.OccurredAt, "external_identity.provision", identity.PlatformUserID, "外部客户身份已预置，等待平台凭据激活", map[string]any{"account_no": identity.AccountNo, "identity_status": identity.Status, "login_name_migrated": loginNameMigrated})
	})
	return response, err
}

type externalLoginAccount struct {
	ID, UserID, Username, AccountType, AuthSource, Status string
}

// ensureExternalLoginAccount 锁定外部身份及其已绑定账号，补偿缺失关联，并在满足
// 严格条件时把存量 EXT-... 登录名原地迁移为联系人手机号。返回最终登录名、是否迁移和错误。
func ensureExternalLoginAccount(tx *gorm.DB, command application.ProvisionCommand, platformUserID string) (string, bool, error) {
	var identity identityModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND platform_user_id = ?", command.Principal.TenantID, platformUserID).
		Take(&identity).Error; err != nil {
		return "", false, err
	}
	accountID := identity.LoginAccountID
	if accountID == "" {
		// 仅兼容迁移 000071 之前可能遗留的空关联。先按身份原账号创建或复用，
		// 再以条件更新建立关联；不能直接按新手机号创建第二个客户账号。
		var err error
		accountID, err = createExternalLoginAccount(tx, command, platformUserID, identity.AccountNo)
		if err != nil {
			return "", false, err
		}
		update := tx.Model(&identityModel{}).
			Where("id = ? AND tenant_id = ? AND login_account_id IS NULL", identity.ID, identity.TenantID).
			Update("login_account_id", accountID)
		if update.Error != nil {
			return "", false, mapWriteError(update.Error)
		}
		if update.RowsAffected != 1 {
			return "", false, application.ErrConflict
		}
		identity.LoginAccountID = accountID
	}

	account, err := lockExternalLoginAccount(tx, identity.TenantID, accountID)
	if err != nil {
		return "", false, err
	}
	if account.UserID != platformUserID || account.AccountType != "HUMAN" || account.AuthSource != "LOCAL" || account.Status != activeStatus {
		return "", false, application.ErrConflict
	}

	migrated, err := migrateLegacyExternalLoginName(tx, command, &identity, account)
	if err != nil {
		return "", false, err
	}
	if err := ensureExternalPasswordCredential(tx, command, accountID); err != nil {
		return "", false, err
	}
	return identity.AccountNo, migrated, nil
}

func lockExternalLoginAccount(tx *gorm.DB, tenantID, accountID string) (externalLoginAccount, error) {
	var account externalLoginAccount
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("iam_account").
		Select("id, user_id, username, account_type, auth_source, status").
		Where("tenant_id = ? AND id = ?", tenantID, accountID).Take(&account).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return account, application.ErrConflict
	}
	return account, err
}

// migrateLegacyExternalLoginName 只迁移仍同时使用同一 EXT-... 名称的身份和关联账号。
// 已经改名、跨账号占用、手机号占用或身份/账号漂移均失败关闭；密码凭据不在更新范围内。
func migrateLegacyExternalLoginName(tx *gorm.DB, command application.ProvisionCommand, identity *identityModel, account externalLoginAccount) (bool, error) {
	desired, migrate, err := decideLegacyExternalLoginNameMigration(command, *identity, account)
	if err != nil || !migrate {
		return false, err
	}
	if err := ensureExternalLoginNameAvailable(tx, identity.TenantID, identity.ID, account.ID, desired); err != nil {
		return false, err
	}

	accountUpdate := tx.Table("iam_account").
		Where("tenant_id = ? AND id = ? AND user_id = ? AND username = ?", identity.TenantID, account.ID, identity.PlatformUserID, account.Username).
		Updates(map[string]any{"username": desired, "updated_at": command.OccurredAt, "updated_by": command.Principal.OAuthClientID, "version": gorm.Expr("version + 1")})
	if accountUpdate.Error != nil {
		return false, mapWriteError(accountUpdate.Error)
	}
	if accountUpdate.RowsAffected != 1 {
		return false, application.ErrConflict
	}
	identityUpdate := tx.Model(&identityModel{}).
		Where("tenant_id = ? AND id = ? AND login_account_id = ? AND account_no = ?", identity.TenantID, identity.ID, account.ID, identity.AccountNo).
		Updates(map[string]any{"account_no": desired, "updated_at": command.OccurredAt, "updated_by": command.Principal.OAuthClientID})
	if identityUpdate.Error != nil {
		return false, mapWriteError(identityUpdate.Error)
	}
	if identityUpdate.RowsAffected != 1 {
		return false, application.ErrConflict
	}
	identity.AccountNo = desired
	return true, nil
}

// decideLegacyExternalLoginNameMigration 集中验证迁移前状态，便于在执行任何 UPDATE 前
// 拒绝非存量账号、身份与账号名称漂移及缺少已验证手机号的请求。
func decideLegacyExternalLoginNameMigration(command application.ProvisionCommand, identity identityModel, account externalLoginAccount) (string, bool, error) {
	desired := strings.TrimSpace(command.AccountNo)
	if len(command.MobileDigest) == 0 || desired == "" || isLegacyExternalLoginName(desired) {
		if account.Username != identity.AccountNo {
			return "", false, application.ErrConflict
		}
		return identity.AccountNo, false, nil
	}
	if identity.AccountNo == desired {
		if account.Username != desired {
			return "", false, application.ErrConflict
		}
		return desired, false, nil
	}
	if !isLegacyExternalLoginName(identity.AccountNo) || account.Username != identity.AccountNo || !isLegacyExternalLoginName(account.Username) {
		return "", false, application.ErrConflict
	}
	return desired, true, nil
}

func ensureExternalLoginNameAvailable(tx *gorm.DB, tenantID, identityID, accountID, desired string) error {
	var conflictingAccount struct{ ID string }
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("iam_account").Select("id").
		Where("tenant_id = ? AND username = ? AND id <> ?", tenantID, desired, accountID).Take(&conflictingAccount).Error
	if err == nil {
		return application.ErrConflict
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	var conflictingIdentity struct{ ID string }
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table(identityModel{}.TableName()).Select("id").
		Where("tenant_id = ? AND account_no = ? AND id <> ?", tenantID, desired, identityID).Take(&conflictingIdentity).Error
	if err == nil {
		return application.ErrConflict
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return nil
}

func isLegacyExternalLoginName(value string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(value)), "EXT-")
}

// ensureExternalPasswordCredential 仅在显式启用客户初始口令且账号还没有凭据时创建。
// 预置重放绝不覆盖已存在凭据，避免把客户修改后的密码重置为默认口令。
func ensureExternalPasswordCredential(tx *gorm.DB, command application.ProvisionCommand, accountID string) error {
	if command.CredentialID == "" {
		return nil
	}
	if len(command.PasswordDigest) == 0 || len(command.PasswordParams) == 0 || accountID == "" {
		return application.ErrConflict
	}
	var existing struct{ ID string }
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("iam_password_credential").
		Select("id").Where("account_id = ?", accountID).Take(&existing).Error
	if err == nil {
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return tx.Table("iam_password_credential").Create(map[string]any{
		"id": command.CredentialID, "account_id": accountID,
		"password_hash": command.PasswordDigest, "hash_algorithm": "argon2id", "algorithm_params": command.PasswordParams,
		"must_change": true, "failed_attempts": 0, "status": activeStatus,
		"password_changed_at": command.OccurredAt, "created_at": command.OccurredAt, "updated_at": command.OccurredAt,
	}).Error
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
		// 外部客户角色不是经通用授权服务写入，因此必须在同一事务中补投影任务。
		// 否则数据库中的 portal_customer 已生效，但 Keycloak 不会创建/更新用户，
		// 客户首次通过邀请登录时会因 identity_id 等声明缺失而失败。
		if err := enqueueKeycloakRoleProjection(tx, command.Principal.TenantID, command.PlatformUserID, target.ApplicationID, command.EventID, command.OccurredAt); err != nil {
			return err
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

// enqueueKeycloakRoleProjection 为每个已同步的 Keycloak Client 环境写入持久化投影任务。
// 调用方已持有角色变更事务，因此外部客户角色一旦提交，就不会漏掉 Keycloak 对账。
func enqueueKeycloakRoleProjection(tx *gorm.DB, tenantID, platformUserID, applicationID, initialEventID string, now time.Time) error {
	var environmentIDs []string
	if err := tx.Table("keycloak_application_client_mapping").
		Where("tenant_id = ? AND application_id = ? AND status = ?", tenantID, applicationID, "SYNCED").
		Order("environment_id ASC").
		Pluck("environment_id", &environmentIDs).Error; err != nil {
		return fmt.Errorf("load Keycloak Client mapping targets for external customer role: %w", err)
	}
	for index, environmentID := range environmentIDs {
		eventID := initialEventID
		if index > 0 {
			var err error
			eventID, err = platformulid.New(now)
			if err != nil {
				return fmt.Errorf("generate external customer Keycloak projection event ID: %w", err)
			}
		}
		if err := tx.Table("keycloak_authorization_outbox").Create(map[string]any{
			"id": eventID, "tenant_id": tenantID, "identity_id": platformUserID,
			"application_id": applicationID, "environment_id": environmentID,
			"event_type": "AUTHORIZATION_CHANGED", "authorization_revision": 0,
			"status": "PENDING", "available_at": now, "attempts": 0, "created_at": now,
		}).Error; err != nil {
			return fmt.Errorf("enqueue external customer Keycloak authorization projection: %w", err)
		}
	}
	return nil
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

// BindCustomer 在单个事务内完成绑定写入/禁用：nonce、幂等、身份存在性、绑定 upsert、
// 审计原子提交。同租户同客户标识的跨身份冲突由唯一键触发并映射为显式冲突。
func (repository *GORMRepository) BindCustomer(ctx context.Context, command application.BindingCommand) (domain.BindingResult, error) {
	operation, action, summary := "BIND", "external_customer.binding.bind", "外部客户绑定已建立"
	if command.Status == domain.BindingDisabled {
		operation, action, summary = "DISABLE_BIND", "external_customer.binding.disable", "外部客户绑定已禁用"
	}
	var response domain.BindingResult
	err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := recordNonce(tx, command.Principal.TenantID, command.NonceHash[:], command.NonceExpiresAt, command.OccurredAt); err != nil {
			return err
		}
		var err error
		response, err = replayBinding(tx, command, operation)
		if err == nil || !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		// 绑定只允许挂在仍可用的外部身份上；DISABLED 身份不能重新建立或恢复绑定。
		var identity identityModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND platform_user_id = ? AND status <> ?", command.Principal.TenantID, command.PlatformUserID, domain.IdentityDisabled).
			Take(&identity).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return application.ErrNotFound
			}
			return err
		}

		var existing struct {
			ID                string
			CustomerRefDigest []byte
			Status            string
		}
		findErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Table("iam_external_customer_binding").
			Select("id, customer_ref_digest, status").
			Where("tenant_id = ? AND identity_id = ? AND application_code = ?", command.Principal.TenantID, identity.ID, command.ApplicationCode).
			Take(&existing).Error
		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			if command.Status == domain.BindingDisabled {
				return application.ErrNotFound
			}
			operator := command.Principal.OAuthClientID
			if err := tx.Table("iam_external_customer_binding").Create(map[string]any{
				"id": command.BindingID, "tenant_id": command.Principal.TenantID, "identity_id": identity.ID,
				"application_code": command.ApplicationCode, "customer_ref_digest": command.CustomerRefDigest,
				"customer_ref_cipher": command.CustomerRefCipher, "status": command.Status, "version": 1,
				"created_at": command.OccurredAt, "created_by": operator, "updated_at": command.OccurredAt, "updated_by": operator,
			}).Error; err != nil {
				return mapWriteError(err)
			}
		} else if findErr != nil {
			return findErr
		} else if command.Status == domain.BindingDisabled {
			// 禁用必须以同一客户标识定位绑定，防止只凭 platform_user_id 就冻结其他客户的映射。
			if !application.SameDigest(existing.CustomerRefDigest, command.CustomerRefDigest) {
				return application.ErrNotFound
			}
			// 禁用不刷新密文/摘要，保留绑定历史供审计与轮换对照。
			update := tx.Table("iam_external_customer_binding").
				Where("id = ? AND tenant_id = ? AND status = ?", existing.ID, command.Principal.TenantID, domain.BindingActive).
				Updates(map[string]any{"status": domain.BindingDisabled, "updated_at": command.OccurredAt, "updated_by": command.Principal.OAuthClientID, "version": gorm.Expr("version + 1")})
			if update.Error != nil {
				return mapWriteError(update.Error)
			}
			if update.RowsAffected != 1 {
				return application.ErrConflict
			}
		} else {
			// 恢复绑定（禁用后重新开通）或密钥轮换后刷新密文/摘要。
			update := tx.Table("iam_external_customer_binding").
				Where("id = ? AND tenant_id = ? AND (status = ? OR status = ?)", existing.ID, command.Principal.TenantID, domain.BindingActive, domain.BindingDisabled).
				Updates(map[string]any{"customer_ref_digest": command.CustomerRefDigest, "customer_ref_cipher": command.CustomerRefCipher, "status": domain.BindingActive, "updated_at": command.OccurredAt, "updated_by": command.Principal.OAuthClientID, "version": gorm.Expr("version + 1")})
			if update.Error != nil {
				return mapWriteError(update.Error)
			}
			if update.RowsAffected != 1 {
				return application.ErrConflict
			}
		}
		response = domain.BindingResult{PlatformUserID: command.PlatformUserID, ApplicationCode: command.ApplicationCode, Status: command.Status}
		if err := createIdempotency(tx, command.Principal.TenantID, command.Principal.OAuthClientID, operation, command.IdempotencyKey, command.RequestHash[:], identity.ID, encodeBinding(response), command.OccurredAt); err != nil {
			return err
		}
		return ingestAudit(ctx, tx, repository.audit, command.EventID, command.Principal, command.OccurredAt, action, command.PlatformUserID, summary, map[string]any{"application_code": command.ApplicationCode, "binding_status": command.Status})
	})
	return response, err
}

// ResolveCustomerBinding 只读解析 ACTIVE 绑定记录，明文解密由应用层完成。
func (repository *GORMRepository) ResolveCustomerBinding(ctx context.Context, query application.BindingQuery) (domain.CustomerBinding, error) {
	var row struct {
		ApplicationCode   string
		CustomerRefCipher []byte
		CustomerRefDigest []byte
		Status            string
	}
	statement := repository.database.WithContext(ctx).
		Table("iam_external_customer_binding AS binding").
		Select("binding.application_code, binding.customer_ref_cipher, binding.customer_ref_digest, binding.status").
		Joins("JOIN iam_external_identity AS identity ON identity.id = binding.identity_id AND identity.tenant_id = binding.tenant_id").
		Where("binding.tenant_id = ? AND identity.platform_user_id = ? AND binding.application_code = ?", query.TenantID, query.PlatformUserID, query.ApplicationCode)
	if query.Status != "" {
		statement = statement.Where("binding.status = ?", query.Status)
	}
	err := statement.Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.CustomerBinding{}, application.ErrNotFound
	}
	if err != nil {
		return domain.CustomerBinding{}, fmt.Errorf("query external customer binding: %w", err)
	}
	return domain.CustomerBinding{
		ApplicationCode: row.ApplicationCode, CustomerRefCipher: row.CustomerRefCipher,
		CustomerRefDigest: row.CustomerRefDigest, Status: row.Status,
	}, nil
}

func replayBinding(tx *gorm.DB, command application.BindingCommand, operation string) (domain.BindingResult, error) {
	row, err := loadIdempotency(tx, command.Principal.TenantID, command.Principal.OAuthClientID, operation, command.IdempotencyKey, command.RequestHash[:])
	if err != nil {
		return domain.BindingResult{}, err
	}
	parts := strings.Split(row.ResultJSON, "\x00")
	if len(parts) != 3 {
		return domain.BindingResult{}, application.ErrConflict
	}
	return domain.BindingResult{PlatformUserID: parts[0], ApplicationCode: parts[1], Status: parts[2]}, nil
}

func encodeBinding(value domain.BindingResult) string {
	return value.PlatformUserID + "\x00" + value.ApplicationCode + "\x00" + value.Status
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

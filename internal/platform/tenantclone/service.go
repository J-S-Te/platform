// Package tenantclone 提供新租户授权目录的事务性、幂等克隆能力。
package tenantclone

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
)

var (
	ErrValidation = errors.New("tenant authorization clone validation failed")
	ErrNotFound   = errors.New("tenant authorization clone tenant not found")
)

// Input 描述一次授权目录克隆；目标租户必须已由租户生命周期流程创建。
type Input struct{ SourceTenantID, TargetTenantID, IdempotencyKey, OperatorID string }

// Result 是不包含任何用户、Client Secret 或环境配置的克隆结果。
type Result struct {
	OperationID, SourceTenantID, TargetTenantID                  string
	Applications, Resources, Permissions, Roles, RolePermissions int
	Status                                                       string
}

// Service 执行授权目录克隆。所有 ID 都在事务内重新生成，绑定表不会被复制。
type Service struct {
	DB    *gorm.DB
	Clock func() time.Time
}

// Clone 在目标租户和幂等键上加锁，复制应用、资源、权限、角色及角色权限并原子提交。
func (s *Service) Clone(ctx context.Context, in Input) (Result, error) {
	in.SourceTenantID, in.TargetTenantID, in.IdempotencyKey = strings.TrimSpace(in.SourceTenantID), strings.TrimSpace(in.TargetTenantID), strings.TrimSpace(in.IdempotencyKey)
	if s == nil || s.DB == nil || in.SourceTenantID == "" || in.TargetTenantID == "" || in.IdempotencyKey == "" || in.SourceTenantID == in.TargetTenantID {
		return Result{}, ErrValidation
	}
	now := time.Now().UTC()
	if s.Clock != nil {
		now = s.Clock().UTC()
	}
	result := Result{SourceTenantID: in.SourceTenantID, TargetTenantID: in.TargetTenantID}
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var operation struct{ ID, SourceTenantID, TargetTenantID, IdempotencyKey, Status string }
		if err := tx.Raw("SELECT id, source_tenant_id, target_tenant_id, idempotency_key, status FROM iam_tenant_authorization_clone WHERE target_tenant_id=? AND idempotency_key=? FOR UPDATE", in.TargetTenantID, in.IdempotencyKey).Scan(&operation).Error; err != nil {
			return err
		}
		if operation.ID != "" {
			if operation.Status == "COMPLETED" {
				result.OperationID, result.Status = operation.ID, operation.Status
				return s.countResult(tx, in, &result)
			}
			return fmt.Errorf("clone operation is already %s", operation.Status)
		}
		for _, tenant := range []string{in.SourceTenantID, in.TargetTenantID} {
			var n int64
			if err := tx.Table("iam_tenant").Where("id=? AND status NOT IN ('DELETED','DISABLED')", tenant).Count(&n).Error; err != nil || n != 1 {
				return ErrNotFound
			}
		}
		opID, err := ulid.New(now)
		if err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO iam_tenant_authorization_clone (id,source_tenant_id,target_tenant_id,idempotency_key,status,created_at,updated_at) VALUES (?,?,?,?,?,?,?)", opID, in.SourceTenantID, in.TargetTenantID, in.IdempotencyKey, "RUNNING", now, now).Error; err != nil {
			return err
		}
		if err := cloneCatalog(tx, in.SourceTenantID, in.TargetTenantID, in.OperatorID, now, &result); err != nil {
			return err
		}
		if err := tx.Exec("UPDATE iam_tenant_authorization_clone SET status='COMPLETED',updated_at=? WHERE id=?", now, opID).Error; err != nil {
			return err
		}
		result.OperationID, result.Status = opID, "COMPLETED"
		return nil
	})
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		var operation struct{ ID, Status string }
		if lookupErr := s.DB.WithContext(ctx).Raw("SELECT id,status FROM iam_tenant_authorization_clone WHERE target_tenant_id=? AND idempotency_key=?", in.TargetTenantID, in.IdempotencyKey).Scan(&operation).Error; lookupErr == nil && operation.ID != "" {
			if operation.Status == "COMPLETED" {
				result.OperationID, result.Status = operation.ID, operation.Status
				err = s.countResult(s.DB.WithContext(ctx), in, &result)
			}
		}
	}
	return result, err
}

func cloneCatalog(tx *gorm.DB, source, target, operator string, now time.Time, result *Result) error {
	type row struct {
		ID, Code, Name, ApplicationType, Description, Status, ResourceType, Action, RoleType, Effect string
		ApplicationID, ResourceID                                                                    string
		BuiltIn                                                                                      bool
	}
	apps := []row{}
	if err := tx.Raw("SELECT id,code,name,application_type,status FROM platform_application WHERE tenant_id=? AND status NOT IN ('DELETED','DISABLED') ORDER BY id", source).Scan(&apps).Error; err != nil {
		return err
	}
	appMap := map[string]string{}
	for _, a := range apps {
		id, err := ulid.New(now)
		if err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO platform_application (id,tenant_id,code,name,application_type,owner_org_id,owner_user_id,homepage_url,description,status,version,created_at,updated_at,created_by,updated_by)
			SELECT ?,?,code,name,application_type,NULL,NULL,homepage_url,description,status,1,?,?,?,? FROM platform_application WHERE tenant_id=? AND id=?`, id, target, now, now, nullableOperator(operator), nullableOperator(operator), source, a.ID).Error; err != nil {
			return err
		}
		appMap[a.ID] = id
		result.Applications++
	}
	resources := []row{}
	if err := tx.Raw("SELECT id,application_id,code,name,resource_type,status FROM authz_resource WHERE tenant_id=? ORDER BY id", source).Scan(&resources).Error; err != nil {
		return err
	}
	resMap := map[string]string{}
	for _, r := range resources {
		appID := appMap[r.ApplicationID]
		if appID == "" {
			return ErrValidation
		}
		id, err := ulid.New(now)
		if err != nil {
			return err
		}
		if err = tx.Exec(`INSERT INTO authz_resource (id,tenant_id,application_id,code,name,resource_type,attribute_schema,status,version,created_at,updated_at,created_by,updated_by)
			SELECT ?,?,?,code,name,resource_type,attribute_schema,status,1,?,?,?,? FROM authz_resource WHERE tenant_id=? AND id=?`, id, target, appID, now, now, nullableOperator(operator), nullableOperator(operator), source, r.ID).Error; err != nil {
			return err
		}
		resMap[r.ID] = id
		result.Resources++
	}
	perms := []row{}
	if err := tx.Raw("SELECT id,application_id,resource_id,code,action,name,status FROM authz_permission WHERE tenant_id=? ORDER BY id", source).Scan(&perms).Error; err != nil {
		return err
	}
	permMap := map[string]string{}
	for _, p := range perms {
		appID, resID := appMap[p.ApplicationID], resMap[p.ResourceID]
		if appID == "" || resID == "" {
			return ErrValidation
		}
		id, err := ulid.New(now)
		if err != nil {
			return err
		}
		if err = tx.Exec("INSERT INTO authz_permission (id,tenant_id,application_id,resource_id,code,action,name,description,risk_level,status,version,created_at,updated_at,created_by,updated_by) SELECT ?,?,?,?,?,?,name,description,risk_level,status,1,?,?,?,? FROM authz_permission WHERE id=? AND tenant_id=?", id, target, appID, resID, p.Code, p.Action, now, now, nullableOperator(operator), nullableOperator(operator), p.ID, source).Error; err != nil {
			return err
		}
		permMap[p.ID] = id
		result.Permissions++
	}
	roles := []row{}
	if err := tx.Raw("SELECT id,application_id,code,name,role_type,built_in,status FROM authz_role WHERE tenant_id=? ORDER BY id", source).Scan(&roles).Error; err != nil {
		return err
	}
	roleMap := map[string]string{}
	for _, r := range roles {
		appID := appMap[r.ApplicationID]
		if appID == "" {
			return ErrValidation
		}
		id, err := ulid.New(now)
		if err != nil {
			return err
		}
		if err = tx.Exec(`INSERT INTO authz_role (id,tenant_id,application_id,code,name,role_type,description,built_in,status,version,created_at,updated_at,created_by,updated_by)
			SELECT ?,?,?,code,name,role_type,description,built_in,status,1,?,?,?,? FROM authz_role WHERE tenant_id=? AND id=?`, id, target, appID, now, now, nullableOperator(operator), nullableOperator(operator), source, r.ID).Error; err != nil {
			return err
		}
		roleMap[r.ID] = id
		result.Roles++
	}
	var links []struct{ RoleID, PermissionID, Effect string }
	if err := tx.Raw("SELECT role_id,permission_id,effect FROM authz_role_permission WHERE role_id IN (SELECT id FROM authz_role WHERE tenant_id=?)", source).Scan(&links).Error; err != nil {
		return err
	}
	for _, l := range links {
		if roleMap[l.RoleID] == "" || permMap[l.PermissionID] == "" {
			return ErrValidation
		}
		if err := tx.Exec("INSERT INTO authz_role_permission (role_id,permission_id,effect,created_at,created_by) VALUES (?,?,?,?,?)", roleMap[l.RoleID], permMap[l.PermissionID], l.Effect, now, nullableOperator(operator)).Error; err != nil {
			return err
		}
		result.RolePermissions++
	}
	return nil
}

func (s *Service) countResult(tx *gorm.DB, in Input, result *Result) error {
	queries := []struct {
		table string
		value *int
	}{
		{"platform_application", &result.Applications},
		{"authz_resource", &result.Resources},
		{"authz_permission", &result.Permissions},
		{"authz_role", &result.Roles},
	}
	for _, query := range queries {
		if err := tx.Raw("SELECT COUNT(*) FROM "+query.table+" WHERE tenant_id=?", in.TargetTenantID).Scan(query.value).Error; err != nil {
			return err
		}
	}
	return tx.Raw(`SELECT COUNT(*) FROM authz_role_permission rp JOIN authz_role r ON r.id=rp.role_id WHERE r.tenant_id=?`, in.TargetTenantID).Scan(&result.RolePermissions).Error
}

func nullableOperator(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

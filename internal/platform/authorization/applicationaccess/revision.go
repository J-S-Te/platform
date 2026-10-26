// edit_revision 是"用户 × 应用"可编辑授权状态的摘要版本，供整组保存接口做
// 条件写入（乐观并发控制）。它与应用级 authz_revision 是两个独立概念：
// authz_revision 随目录/任意授权变化整体推进，无法表达"这份可编辑集合是否
// 被别人改过"；edit_revision 只由本接口可编辑的内容（MANUAL 直绑与历史自定义
// 权限）决定。
//
// 版本输入（方案第三阶段第 3 节）：
//   - 租户、用户、应用身份；
//   - 该集合全部原始人工绑定，包括停用、未来生效与历史空来源兼容记录；
//   - 绑定 ID、版本、角色、范围、状态、起止时间及来源标记（grant_origin/origin_id/origin_item_id）；
//   - DELETE 会清理的历史用户自定义权限（authz_user_permission）。
//
// 明确不包含：角色名称等纯展示信息、按当前时间推算的"是否生效"、其他用户授权、
// 岗位模板等非本接口可编辑来源。无人工绑定时仍返回合法版本（空集合参与哈希），
// 防止"从空新增"绕过并发保护。
//
// 锁序（与方案一致）：iam_tenant 行锁 → 目录/目标校验 → 人工绑定与历史权限
// 读取 → 版本比较 → 变更 → 授权 revision 与 Outbox。写入该 MANUAL 集合的路径
// 当前仅有 UpdateAccess 与 DeleteAccess（主体手写入口恒拒绝、岗位模板只产生
// TEMPLATE 来源），两条路径均已接入同一锁序；后续新增写路径必须沿用。
package applicationaccess

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EditRevisionPrefix 是 edit_revision 的版本前缀；字段或语义变化时必须递增，
// 使旧版本天然不匹配新计算结果。
const EditRevisionPrefix = "v1:"

// editRevisionBinding 是参与版本摘要的绑定行投影。字段顺序即 JSON 序列化
// 顺序（Go 结构体字段序固定），是哈希确定性的一部分，不得调整或增删。
type editRevisionBinding struct {
	ID           string `json:"id"`
	Version      uint64 `json:"version"`
	RoleID       string `json:"role_id"`
	ScopeType    string `json:"scope_type"`
	ScopeID      string `json:"scope_id"`
	Status       string `json:"status"`
	ValidFrom    string `json:"valid_from"`
	ValidUntil   string `json:"valid_until"`
	GrantOrigin  string `json:"grant_origin"`
	OriginID     string `json:"origin_id"`
	OriginItemID string `json:"origin_item_id"`
}

// editRevisionPermission 是参与版本摘要的历史自定义权限行投影。
type editRevisionPermission struct {
	PermissionID string `json:"permission_id"`
	CreatedAt    string `json:"created_at"`
}

type editRevisionPayload struct {
	Version         string                   `json:"version"`
	TenantID        string                   `json:"tenant_id"`
	UserID          string                   `json:"user_id"`
	ApplicationID   string                   `json:"application_id"`
	Bindings        []editRevisionBinding    `json:"bindings"`
	Permissions     []editRevisionPermission `json:"permissions"`
}

// revisionTimeFormat 统一时间精度：UTC 毫秒精度固定格式，空值用空串参与哈希。
func revisionTimeFormat(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// buildEditRevision 由行投影构造版本摘要，是纯函数以便确定性测试。
// 绑定按 ID 排序、权限按权限 ID 排序，输入顺序不影响结果。
func buildEditRevision(tenantID, userID, applicationID string, bindings []editRevisionBinding, permissions []editRevisionPermission) string {
	sortedBindings := append([]editRevisionBinding(nil), bindings...)
	for i := 1; i < len(sortedBindings); i++ {
		for j := i; j > 0 && sortedBindings[j].ID < sortedBindings[j-1].ID; j-- {
			sortedBindings[j], sortedBindings[j-1] = sortedBindings[j-1], sortedBindings[j]
		}
	}
	sortedPermissions := append([]editRevisionPermission(nil), permissions...)
	for i := 1; i < len(sortedPermissions); i++ {
		for j := i; j > 0 && sortedPermissions[j].PermissionID < sortedPermissions[j-1].PermissionID; j-- {
			sortedPermissions[j], sortedPermissions[j-1] = sortedPermissions[j-1], sortedPermissions[j]
		}
	}
	if sortedBindings == nil {
		sortedBindings = []editRevisionBinding{}
	}
	if sortedPermissions == nil {
		sortedPermissions = []editRevisionPermission{}
	}
	payload := editRevisionPayload{
		Version:       "v1",
		TenantID:      tenantID,
		UserID:        userID,
		ApplicationID: applicationID,
		Bindings:      sortedBindings,
		Permissions:   sortedPermissions,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// 结构体编码不会失败；兜底保持签名稳定。
		return EditRevisionPrefix + "invalid"
	}
	sum := sha256.Sum256(encoded)
	return EditRevisionPrefix + hex.EncodeToString(sum[:])
}

// loadEditRevisionRows 读取参与版本计算的全部原始行：人工绑定（含停用/未来/
// 历史记录）与历史自定义权限。必须在本接口事务锁内调用，保证版本与后续变更
// 基于同一份数据。
func loadEditRevisionRows(database *gorm.DB, tenantID, userID, applicationID string) ([]editRevisionBinding, []editRevisionPermission, error) {
	var rows []struct {
		ID           string     `gorm:"column:id"`
		Version      uint64     `gorm:"column:version"`
		RoleID       string     `gorm:"column:role_id"`
		ScopeType    string     `gorm:"column:scope_type"`
		ScopeID      string     `gorm:"column:scope_id"`
		Status       string     `gorm:"column:status"`
		ValidFrom    *time.Time `gorm:"column:valid_from"`
		ValidUntil   *time.Time `gorm:"column:valid_until"`
		GrantOrigin  string     `gorm:"column:grant_origin"`
		OriginID     string     `gorm:"column:origin_id"`
		OriginItemID string     `gorm:"column:origin_item_id"`
	}
	err := database.Table("authz_role_binding AS rb").
		Select("rb.id, rb.version, rb.role_id, rb.scope_type, rb.scope_id, rb.status, rb.valid_from, rb.valid_until, rb.grant_origin, rb.origin_id, rb.origin_item_id").
		// 版本边界必须与可编辑集合一致：只统计该主体本人的绑定。少了 subject_id 时，
		// 同应用下他人（如用户 B）的授权变化会推高用户 A 的编辑版本，制造无关 409。
		// 版本边界必须与可编辑集合一致：只统计该主体本人的绑定。少了 subject_id 时，
		// 同应用下他人（如用户 B）的授权变化会推高用户 A 的编辑版本，制造无关 409。
		Where("rb.tenant_id = ? AND rb.application_id = ? AND rb.subject_type = ? AND rb.subject_id = ? AND rb.grant_origin = ?", tenantID, applicationID, subjectTypeUser, userID, grantOriginManual).
		Order("rb.id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, nil, fmt.Errorf("load manual bindings for edit revision: %w", err)
	}
	bindings := make([]editRevisionBinding, 0, len(rows))
	for _, row := range rows {
		bindings = append(bindings, editRevisionBinding{
			ID: row.ID, Version: row.Version, RoleID: row.RoleID,
			ScopeType: row.ScopeType, ScopeID: row.ScopeID, Status: row.Status,
			ValidFrom: revisionTimeFormat(row.ValidFrom), ValidUntil: revisionTimeFormat(row.ValidUntil),
			GrantOrigin: row.GrantOrigin, OriginID: row.OriginID, OriginItemID: row.OriginItemID,
		})
	}

	var permissionRows []struct {
		PermissionID string    `gorm:"column:permission_id"`
		CreatedAt    time.Time `gorm:"column:created_at"`
	}
	err = database.Table("authz_user_permission").
		Select("permission_id, created_at").
		Where("tenant_id = ? AND application_id = ? AND user_id = ?", tenantID, applicationID, userID).
		Order("permission_id ASC").
		Find(&permissionRows).Error
	if err != nil {
		return nil, nil, fmt.Errorf("load legacy user permissions for edit revision: %w", err)
	}
	permissions := make([]editRevisionPermission, 0, len(permissionRows))
	for _, row := range permissionRows {
		created := row.CreatedAt
		permissions = append(permissions, editRevisionPermission{
			PermissionID: row.PermissionID, CreatedAt: revisionTimeFormat(&created),
		})
	}
	return bindings, permissions, nil
}

// computeEditRevision 在给定数据库句柄（事务或一致读）上计算 edit_revision。
// 管理端 GET 与写入校验必须使用同一函数与同一锁/快照边界。
func (s *Service) computeEditRevision(database *gorm.DB, tenantID, userID, applicationID string) (string, error) {
	bindings, permissions, err := loadEditRevisionRows(database, tenantID, userID, applicationID)
	if err != nil {
		return "", err
	}
	return buildEditRevision(tenantID, userID, applicationID, bindings, permissions), nil
}

// ensureExpectedRevision 在事务锁内比较调用方携带的版本条件。expectedRevision
// 为空时：strictRequired 为 false（兼容发布第一阶段）跳过比较并返回 false，
// 为 true（全量切换后的严格模式）返回 422 校验错误。携带但陈旧返回
// ErrRevisionConflict，格式非法返回 ErrValidation（422）。
// 返回 true 表示条件有效且已通过比较。
func (s *Service) ensureExpectedRevision(database *gorm.DB, tenantID, userID, applicationID, expectedRevision string, strictRequired bool) (bool, error) {
	expected := strings.TrimSpace(expectedRevision)
	if expected == "" {
		if strictRequired {
			return false, validation("expected_revision is required for application access writes")
		}
		return false, nil
	}
	if !strings.HasPrefix(expected, EditRevisionPrefix) || len(expected) != len(EditRevisionPrefix)+64 {
		return false, validation("expected_revision format is invalid")
	}
	current, err := s.computeEditRevision(database, tenantID, userID, applicationID)
	if err != nil {
		return false, err
	}
	if current != expected {
		return false, ErrRevisionConflict
	}
	return true, nil
}

// lockTenantRow 以 iam_tenant 行锁串行化同租户的整组保存/撤销（方案第一轮
// 不新增版本表；锁粒度评估与后续演进见方案第 4 节）。调用方必须在事务内、
// 且在任何目标校验之前调用它。
func lockTenantRow(tx *gorm.DB, tenantID string) error {
	var row struct {
		ID string `gorm:"column:id"`
	}
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Table("iam_tenant").
		Select("id").
		Where("id = ?", tenantID).
		Take(&row).Error
	if err != nil {
		return fmt.Errorf("lock tenant for application access write: %w", err)
	}
	return nil
}

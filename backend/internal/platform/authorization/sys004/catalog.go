// Package sys004 implements the fixed contract-management authorization profile.
package sys004

import (
	"encoding/hex"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/sm3"
	"sort"
	"strings"
)

const ApplicationCode = "contract_management"

type RoleDefinition struct {
	Code        string
	Name        string
	Permissions []string
}

var PermissionNames = map[string]string{
	"all": "全部权限", "dashboard": "仪表盘访问",
	"contract.read": "查看合同", "contract.create": "创建合同", "contract.edit": "编辑合同", "contract.delete": "删除合同",
	"customer.read": "查看客户", "customer.create": "创建客户", "customer.edit": "编辑客户", "customer.delete": "删除客户",
	"contract_type.manage": "管理合同类型", "contract_template.read": "查看合同模板", "contract_template.manage": "管理合同模板",
	"approval.view": "查看审批", "approval.process": "处理审批", "user.manage": "管理用户",
	"audit.view": "查看审计日志", "audit.read": "审计只读",
}

var Roles = []RoleDefinition{
	{Code: "admin", Name: "超级管理员", Permissions: []string{"all"}},
	{Code: "sales_director", Name: "销售总监", Permissions: []string{"dashboard", "contract.read", "customer.read", "approval.view", "approval.process"}},
	{Code: "tech_director", Name: "技术总监", Permissions: []string{"dashboard", "contract.read", "customer.read", "approval.view", "approval.process"}},
	{Code: "finance_director", Name: "财务总监", Permissions: []string{"dashboard", "contract.read", "customer.read", "approval.view", "approval.process"}},
	{Code: "sales", Name: "销售人员", Permissions: []string{"dashboard", "contract.read", "contract.create", "contract.edit", "customer.read", "customer.create", "customer.edit", "contract_template.read"}},
	{Code: "audit_admin", Name: "审计管理员", Permissions: []string{"dashboard", "contract.read", "customer.read", "approval.view", "audit.view", "audit.read"}},
}

func PermissionCodes() []string {
	codes := make([]string, 0, len(PermissionNames))
	for code := range PermissionNames {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

func RoleCodes() []string {
	codes := make([]string, 0, len(Roles))
	for _, role := range Roles {
		codes = append(codes, role.Code)
	}
	sort.Strings(codes)
	return codes
}

func Role(code string) (RoleDefinition, bool) {
	for _, role := range Roles {
		if role.Code == code {
			return role, true
		}
	}
	return RoleDefinition{}, false
}

func IsCustomPermissionAllowed(code string) bool {
	_, exists := PermissionNames[code]
	return exists && code != "all" && code != "user.manage"
}

// RoleConfigHash returns a deterministic digest of role codes and their sorted permission sets.
func RoleConfigHash() string {
	roles := append([]RoleDefinition(nil), Roles...)
	sort.Slice(roles, func(i, j int) bool { return roles[i].Code < roles[j].Code })
	var canonical strings.Builder
	for _, role := range roles {
		permissions := append([]string(nil), role.Permissions...)
		sort.Strings(permissions)
		canonical.WriteString(role.Code)
		canonical.WriteByte('=')
		canonical.WriteString(strings.Join(permissions, ","))
		canonical.WriteByte('\n')
	}
	digest := sm3.Sum([]byte(canonical.String()))
	return hex.EncodeToString(digest[:])
}

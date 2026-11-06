package filetaskhttp

import (
	"strconv"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

const portalLocalReportPurpose = "portal.local_report"

// portalLocalReportUploadCaller 限制本地报告上传只能来自精确门户机器主体。
// 参数：p为现有认证中间件提供的可信主体。
// 返回值：主体租户、应用完整且没有浏览器用户时返回true，不接受应用别名。
func portalLocalReportUploadCaller(p authctx.Principal) bool {
	return p.Account.Code == "customer_portal" && p.User.ID == "" &&
		p.Tenant.ID != "" && p.Account.ID != ""
}

// portalLocalReportUploadBinding 只允许本地报告绑定规范正整数申请标识。
// 参数：input为已规范化的上传会话声明。
// 返回值：资源类型、申请ID及绑定类型完全匹配时返回true。
func portalLocalReportUploadBinding(input createUploadRequest) bool {
	id, err := strconv.ParseUint(input.ResourceID, 10, 64)
	return err == nil && id > 0 && strconv.FormatUint(id, 10) == input.ResourceID &&
		input.ResourceType == "PORTAL_REPORT_REQUEST" && input.BindingType == "REPORT"
}

// Package domain 定义面向应用方的最小负责人目录契约。
package domain

// Organization 是一个可用于负责人范围过滤的有效组织归属。
type Organization struct {
	ID        string `json:"organization_id"`
	Name      string `json:"organization_name"`
	IsPrimary bool   `json:"is_primary"`
}

// User 是对调用方应用授权的有效内部用户。刻意不包含账号名、
// 邮箱地址与手机号，目录接口不得成为个人信息导出通道。
type User struct {
	ID            string         `json:"user_id"`
	DisplayName   string         `json:"display_name"`
	Organizations []Organization `json:"organizations"`
}

// Page 是负责人目录用户的确定性分页结果。
type Page struct {
	Items    []User `json:"items"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
	Total    int64  `json:"total"`
}

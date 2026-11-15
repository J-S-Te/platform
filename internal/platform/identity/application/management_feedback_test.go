package application

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

// TestUserValidationExplainsExistingRejections 验证已有人员校验保留具体业务原因。
// 参数：t 为测试上下文，所有资料均为内存夹具。
// 返回值：无；错误链或公开提示不准确时失败。
func TestUserValidationExplainsExistingRejections(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	email, employee, mobile := "invalid-email", strings.Repeat("x", 65), strings.Repeat("1", 33)
	base := UserCreateInput{TenantID: "t", OperatorID: "operator",
		DisplayName: "测试人员", Status: domain.StatusActive}
	cases := []struct {
		name, want string
		change     func(*UserCreateInput)
	}{
		{"姓名", "姓名", func(in *UserCreateInput) { in.DisplayName = "" }},
		{"邮箱", "邮箱", func(in *UserCreateInput) { in.Email = &email }},
		{"工号", "工号", func(in *UserCreateInput) { in.EmployeeNo = &employee }},
		{"手机号", "手机号", func(in *UserCreateInput) { in.Mobile = &mobile }},
		{"有效期", "有效期", func(in *UserCreateInput) { in.ValidUntil = &now }},
	}
	for _, test := range cases {
		input := base
		test.change(&input)
		err := validateUserCreate(input, now)
		if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s 没有具体校验原因：%v", test.name, err)
		}
		if test.name == "工号" {
			update := UserUpdateInput{TenantID: "t", OperatorID: "operator", UserID: "u",
				DisplayName: "测试人员", Version: 1, EmployeeNo: &employee}
			for _, rejection := range []error{err, validateUserUpdate(update, now)} {
				if !errors.Is(rejection, ErrValidation) ||
					!strings.Contains(rejection.Error(), "管理员") ||
					strings.Contains(rejection.Error(), "缩短") {
					t.Errorf("系统生成的工号不能要求用户在只读页面修改：%v", rejection)
				}
			}
		}
	}
	if err := validateUserCreate(base, now); err != nil {
		t.Fatalf("原有合法资料被新增门禁拒绝：%v", err)
	}
}

// TestMembershipValidationExplainsDateRange 验证任职日期错误明确成对填写与先后关系。
// 参数：t 为测试上下文。
// 返回值：无；长期任职规则变化或短期错误原因不明确时失败。
func TestMembershipValidationExplainsDateRange(t *testing.T) {
	from := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	before := from.AddDate(0, 0, -1)
	for _, dates := range [][2]*time.Time{{&from, nil}, {nil, &from}, {&from, &before}} {
		err := validateMembership("t", "operator", "u", "org", "position",
			domain.MembershipPrimary, dates[0], dates[1])
		if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "日期") {
			t.Fatalf("任职日期没有明确原因：%v", err)
		}
	}
	if err := validateMembership("t", "operator", "u", "org", "position",
		domain.MembershipPrimary, nil, nil); err != nil {
		t.Fatalf("长期任职仍应允许省略日期：%v", err)
	}
}

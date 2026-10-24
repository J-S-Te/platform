package identityhttp

import "testing"

// 空字符串是表单默认值，不是「清空手机号」的指令。此前 UpdateUser 把「字段存在」
// 当成修改，只改状态或姓名的保存会把手机号密文和摘要写成 NULL。
func TestMobileUpdateRequested(t *testing.T) {
	blank := ""
	spaces := "   "
	value := "13800000000"
	padded := " 13800000000 "

	cases := []struct {
		name   string
		mobile *string
		want   bool
	}{
		{name: "未提交字段", mobile: nil, want: false},
		{name: "空字符串", mobile: &blank, want: false},
		{name: "仅空白字符", mobile: &spaces, want: false},
		{name: "有效手机号", mobile: &value, want: true},
		{name: "带空白的手机号", mobile: &padded, want: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mobileUpdateRequested(testCase.mobile); got != testCase.want {
				t.Fatalf("mobileUpdateRequested(%v) = %v, want %v", testCase.mobile, got, testCase.want)
			}
		})
	}
}

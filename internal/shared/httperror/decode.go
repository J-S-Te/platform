package httperror

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// DecodeValidation 把严格解码失败转换为公开的协议分类，不输出原始解析诊断。
// 参数：err 为解码或尾随内容检查错误，nil 表示存在额外 JSON 值等契约问题。
// 返回值：兼容既有校验错误码的安全错误，不包含字段输入值或未知字段名。
func DecodeValidation(err error) Error {
	reason := "invalid_json"
	message := "提交的信息格式不正确，请刷新页面后重新操作；仍失败时请联系管理员。"
	var mismatch *json.UnmarshalTypeError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		reason = "request_too_large"
		message = "提交内容超过当前接口允许大小，请减少本次提交数量后重试。"
	case errors.As(err, &mismatch):
		reason = "invalid_type"
	case err != nil && strings.HasPrefix(err.Error(), "json: unknown field "):
		reason = "unknown_field"
	}
	return New(Validation.Code, message, map[string]string{"reason": reason})
}

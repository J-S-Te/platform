package application

import "errors"

// ValidationIssue 只携带可公开的字段名、既有规则及静态业务提示，不包含被拒绝的值。
type ValidationIssue struct {
	Field   string `json:"field"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

type fieldValidationError struct{ issue ValidationIssue }

// Error 返回明确的校验原因供日志与应用调用方定位。
// 参数：无。
// 返回值：原校验分类和安全业务说明，不包含原始输入。
func (e *fieldValidationError) Error() string { return ErrValidation.Error() + ": " + e.issue.Message }

// Unwrap 保持现有 ErrValidation 的兼容判断。
// 参数：无。
// 返回值：既有校验错误哨兵。
func (e *fieldValidationError) Unwrap() error { return ErrValidation }

// invalidField 为已经存在的拒绝条件附加静态字段提示。
// 参数：field 为请求字段，rule 为既有规则，message 为代码中明确的安全文案。
// 返回值：兼容原错误分类且不携带输入值的校验错误。
func invalidField(field, rule, message string) error {
	return &fieldValidationError{ValidationIssue{Field: field, Rule: rule, Message: message}}
}

// ValidationIssues 提取显式声明为安全的字段问题，拒绝从任意包装异常推造文案。
// 参数：err 为应用错误，可包含原有错误链。
// 返回值：安全问题列表；普通哨兵或内部异常返回空列表。
func ValidationIssues(err error) []ValidationIssue {
	var invalid *fieldValidationError
	if errors.As(err, &invalid) {
		return []ValidationIssue{invalid.issue}
	}
	return nil
}

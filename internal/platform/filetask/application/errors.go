package application

import "errors"

var (
	// ErrNotFound 表示租户范围内不存在目标文件或任务。
	ErrNotFound = errors.New("filetask resource not found")
	// ErrConflict 表示状态机不允许的转换或唯一键冲突。
	ErrConflict = errors.New("filetask resource conflict")
	// ErrValidation 表示调用方可自行修正的输入错误。
	ErrValidation = errors.New("filetask validation failed")
	// ErrForbidden 表示调用方无权访问目标文件或任务。
	ErrForbidden = errors.New("filetask access forbidden")
	// ErrStorage 表示本地文件存储读写失败，属基础设施错误。
	ErrStorage = errors.New("filetask local storage failure")
	// ErrPayloadUnsafe 表示任务载荷超出安全约束（尺寸或结构），拒绝入队。
	ErrPayloadUnsafe = errors.New("filetask job payload is unsafe")
)

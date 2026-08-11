// Package domain defines configuration aggregates independent of the HTTP and database layers.
package domain

import "time"

// Reference identifies a configuration-related resource in API responses.
type Reference struct {
	ID   string
	Code string
	Name string
}

// Namespace 是配置生命周期的隔离边界。同一应用在不同租户或环境中的草稿与发布版本
// 必须通过该边界分开，运行时不能绕过命名空间读取其他环境的数据。
type Namespace struct {
	ID          string
	Application Reference
	Code        string
	Name        string
	Description string
	Version     uint64
}

// Item 表示尚可编辑的配置草稿。当前实现不接收明文密钥；Secret 字段用于阻止调用方
// 将敏感值误当成普通文本存入配置表，而不是一个可绕过密钥管理的开关。
type Item struct {
	ID        string
	Namespace Reference
	Key       string
	ValueType string
	Value     any
	Secret    bool
	Version   uint64
	UpdatedAt time.Time
}

// VersionedItem selects one specific draft version for a release snapshot.
type VersionedItem struct {
	ItemID  string
	Version uint64
}

// Release 是一次不可变发布的元数据；发布后运行时读取的是快照，而不是可能继续变化的草稿。
type Release struct {
	ID          string
	Namespace   Reference
	VersionNo   uint64
	Status      string
	Comment     string
	CreatedAt   time.Time
	PublishedAt *time.Time
}

// PublishedConfig is the safe runtime representation of a published namespace.
type PublishedConfig struct {
	ApplicationCode string
	NamespaceCode   string
	ReleaseVersion  uint64
	Values          map[string]any
}

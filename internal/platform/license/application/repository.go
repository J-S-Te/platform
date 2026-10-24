// Package application 编排商业授权的安装状态机：初始化、双阶段导入、时钟回拨
// 闩锁与签名恢复。所有状态变更都在仓储事务锁内完成。
package application

import (
	"context"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
)

// Transaction 是一次授权状态事务内的读写集合；实现必须保证其中全部操作
// 随外层 Lock 的提交/回滚一起生效。
type Transaction interface {
	Deployment() *domain.Deployment
	SaveDeployment(*domain.Deployment) error
	Artifact(string) (domain.Artifact, error)
	AddArtifact(domain.Artifact) error
	AddEvent(domain.Event) error
	ConsumeRecovery(domain.ClockRecovery) error
}

// Repository 提供授权聚合的事务锁与事件分页查询。
type Repository interface {
	Lock(context.Context, func(Transaction) error) error
	Events(context.Context, int, int) (domain.EventPage, error)
}

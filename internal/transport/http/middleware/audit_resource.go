package middleware

import (
	"context"
	"net/http"
	"strings"
)

// 安全审查 SEC-D5：体寻址（body-addressed）的创建类操作没有路径 *_id，通用审计只能记录
// 操作者与路由，缺少“目标对象”。本文件提供请求作用域的可变目标 id 槽位：AuditTrail
// 中间件在执行业务链之前挂载槽位，net/http 处理器在创建成功后通过 MarkCreatedResource
// 登记对象 id，链路同步执行，读写不存在并发窗口。
type auditResourceTargetKey struct{}

type auditResourceTarget struct{ id string }

// AttachAuditResourceTarget 挂载可变目标 id 槽位（由 AuditTrail 在 Next 之前调用；
// 测试可直接调用以模拟中间件链的挂载阶段）。
func AttachAuditResourceTarget(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, auditResourceTargetKey{}, &auditResourceTarget{})
}

// MarkCreatedResource 供业务处理器在成功创建资源后登记目标对象 id（安全审查 SEC-D5）。
// 请求上下文未挂载槽位（例如不经 AuditTrail 的集成路由）时为安全空操作。
func MarkCreatedResource(request *http.Request, id string) {
	if request == nil {
		return
	}
	value := strings.TrimSpace(id)
	if value == "" {
		return
	}
	if target, ok := request.Context().Value(auditResourceTargetKey{}).(*auditResourceTarget); ok {
		target.id = value
	}
}

// AuditResourceIDFromRequest 返回处理器登记的目标对象 id；AuditTrail 用它填充审计事件的
// ResourceID 与 metadata["resource_id"]，测试用它断言接线。
func AuditResourceIDFromRequest(request *http.Request) string {
	if request == nil {
		return ""
	}
	if target, ok := request.Context().Value(auditResourceTargetKey{}).(*auditResourceTarget); ok {
		return target.id
	}
	return ""
}

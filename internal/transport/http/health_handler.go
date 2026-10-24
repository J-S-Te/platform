package httptransport

import (
	"context"
	"net/http"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// HealthHandler 承载进程存活与依赖就绪两类探针端点。
type HealthHandler struct {
	database *gorm.DB
	appName  string
}

// NewHealthHandler 基于应用共享的 GORM 连接创建探针处理器。
func NewHealthHandler(database *gorm.DB, appName string) HealthHandler {
	return HealthHandler{database: database, appName: appName}
}

// Liveness 报告 HTTP 进程是否存活。有意不查询 MySQL：存活探针若依赖外部服务，
// 依赖抖动会让编排层误杀进程。
func (handler HealthHandler) Liveness(ginContext *gin.Context) {
	httpresponse.WriteSuccess(ginContext.Writer, ginContext.Request, http.StatusOK, "服务运行正常", map[string]string{
		"status":  "ok",
		"service": handler.appName,
	})
}

// Readiness 报告 API 必需的数据库依赖当前是否可达；不可达时返回 503，
// 由负载均衡把实例摘出流量。
func (handler HealthHandler) Readiness(ginContext *gin.Context) {
	contextWithTimeout, cancel := context.WithTimeout(ginContext.Request.Context(), 2*time.Second)
	defer cancel()

	if err := handler.database.WithContext(contextWithTimeout).Exec("SELECT 1").Error; err != nil {
		httpresponse.WriteError(ginContext.Writer, ginContext.Request, http.StatusServiceUnavailable, httperror.DependencyUnavailable)
		return
	}

	httpresponse.WriteSuccess(ginContext.Writer, ginContext.Request, http.StatusOK, "依赖服务可用", map[string]string{
		"status": "ok",
		"mysql":  "available",
	})
}

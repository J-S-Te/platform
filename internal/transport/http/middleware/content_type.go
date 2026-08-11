package middleware

import (
	"mime"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// RequireSafeWriteContentType 拒绝浏览器可无预检发送的 text/form 写请求，缩小 CSRF 攻击面。
// multipart 只为认证文件上传保留；无请求体动作仍由 Origin/认证中间件继续保护。
func RequireSafeWriteContentType() gin.HandlerFunc {
	return func(context *gin.Context) {
		request := context.Request
		if request == nil || !isUnsafeMethod(request.Method) || request.ContentLength == 0 {
			context.Next()
			return
		}

		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(request.Header.Get("Content-Type")))
		if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") && mediaType != "multipart/form-data") {
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusUnsupportedMediaType, httperror.New("PLATFORM_UNSUPPORTED_MEDIA_TYPE", "请求内容类型不受支持", nil))
			return
		}
		context.Next()
	}
}

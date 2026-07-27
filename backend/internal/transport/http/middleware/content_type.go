package middleware

import (
	"mime"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// RequireSafeWriteContentType rejects browser-simple text/form bodies on JSON APIs. Multipart is
// retained for the authenticated file upload endpoint. Empty-body action endpoints remain valid.
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

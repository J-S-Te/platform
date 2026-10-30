package filetaskhttp

import (
	"bytes"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// TestV1ContractPDFPurposeEnforcesMedia 验证新增PDF用途不依赖旧v1策略缺口。
// 参数：t为网关上传边界测试上下文。
// 返回值：无；正式PDF拒收或不匹配文件放行时测试失败。
func TestV1ContractPDFPurposeEnforcesMedia(t *testing.T) {
	for _, tt := range []struct {
		media        string
		size, status int
	}{
		{"application/pdf", 30, http.StatusCreated},
		{"image/png", 30, http.StatusUnprocessableEntity},
		{"application/pdf", (20 << 20) + 1, http.StatusUnprocessableEntity},
	} {
		files := &stubV1FileService{file: domain.File{ID: "pdf-1", Status: domain.FileStatusReady}}
		handler, err := NewHandler(files, &stubV1JobService{}, slog.New(&captureLogger{}), newAuditTestDB(t, &auditScript{}))
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("application_id", "app-1"); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteField("purpose", "contract.external.pdf-source"); err != nil {
			t.Fatal(err)
		}
		part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="file"; filename="source.pdf"`}, "Content-Type": {tt.media}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(bytes.Repeat([]byte("x"), tt.size)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/files", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request = request.WithContext(authctx.WithPrincipal(request.Context(), v1Principal()))
		result := httptest.NewRecorder()
		handler.Upload(result, request)
		if result.Code != tt.status {
			t.Fatalf("media=%s size=%d status=%d want=%d", tt.media, tt.size, result.Code, tt.status)
		}
	}
}

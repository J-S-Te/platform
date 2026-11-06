package filetaskhttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/gorm"
)

type portalLocalReadService struct {
	recordingV2FileService
	stored  domain.StoredFile
	content []byte
}

// OpenDownload 记录handler传递的真实授权声明并返回固定合成版本。
// 参数：ctx为请求，access为受控下载范围，fileID为路径文件标识。
// 返回值：仅单测文件/流或注入错误，不冒充真实存储完整性验收。
func (s *portalLocalReadService) OpenDownload(ctx context.Context, access application.DownloadAccess, fileID string) (domain.StoredFile, io.ReadSeekCloser, error) {
	s.accesses = append(s.accesses, access)
	if s.openErr != nil {
		return domain.StoredFile{}, nil, s.openErr
	}
	return s.stored, &nopReadSeekCloser{Reader: bytes.NewReader(s.content)}, nil
}

// portalLocalSourceFixture 构造固定版本的本地报告读取声明和文件服务替身。
// 参数：无。
// 返回值：完整声明及合成文件；摘要来自同一内容，不使用业务文件。
func portalLocalSourceFixture() (portalReportSourceRequest, *portalLocalReadService) {
	content := []byte("test-pdf")
	digest := sha256.Sum256(content)
	input := portalReportSourceRequest{SourceType: "PORTAL_LOCAL", RequestID: 12,
		ExpectedSHA256: hex.EncodeToString(digest[:]), ExpectedSize: 8,
		ExpectedMIME: "application/pdf", ExpectedName: "local-report.pdf"}
	files := &portalLocalReadService{content: content, stored: domain.StoredFile{
		File: domain.File{ID: "file", TenantID: "tenant-1", ApplicationID: "portal-app",
			Namespace: "portal", Purpose: "local-report", Status: domain.FileStatusReady},
		Version: domain.FileVersion{ID: "version", FileID: "file", SizeBytes: 8,
			SHA256: digest[:], MediaType: "application/pdf", OriginalName: "local-report.pdf",
			Status: domain.FileVersionStatusReady}}}
	return input, files
}

// portalLocalSessionRows 为正式GORM读取提供受控会话行，不连接数据库。
// 参数：input为完整性声明；应用/租户/用途与成功案例一致。
// 返回值：用于HTTP单元测试的SQL结果。
func portalLocalSessionRows(input portalReportSourceRequest) driver.Rows {
	digest, _ := hex.DecodeString(input.ExpectedSHA256)
	return &fakeRows{columns: []string{"tenant_id", "application_id", "application_code", "file_id",
		"version_id", "namespace", "purpose", "status", "resource_type", "resource_id",
		"binding_type", "expected_size", "expected_sha256"}, values: [][]driver.Value{{
		"tenant-1", "portal-app", "customer_portal", "file", "version", "portal", "local-report",
		"READY", "PORTAL_REPORT_REQUEST", "12", "REPORT", int64(8), digest}}}
}

// TestPortalLocalSourceQueryPinsAuthenticatedApplication 锁定实际SQL的来源、租户和当前申请。
// 参数：t为测试上下文，使用dry-run不发起任何数据库连接。
// 返回值：无；跨应用、错namespace或错request能放宽查询时失败。
func TestPortalLocalSourceQueryPinsAuthenticatedApplication(t *testing.T) {
	input, _ := portalLocalSourceFixture()
	p := portalLocalWriter()
	db := newAuditTestDB(t, &auditScript{})
	query := portalReportSourceSessionQuery(db.Session(&gorm.Session{DryRun: true}), p, input, "file")
	statement := query.Take(&uploadV2Session{}).Statement
	expected := []any{"tenant-1", "file", "customer_portal", "portal", "local-report", "READY",
		"PORTAL_REPORT_REQUEST", "12", "portal-app", "REPORT", 1}
	if !reflect.DeepEqual(statement.Vars, expected) {
		t.Fatalf("local query lost exact boundary: %s vars=%#v", statement.SQL.String(), statement.Vars)
	}
	for _, fragment := range []string{"tenant_id = ?", "application_id = ?", "application_code = ?",
		"namespace = ?", "purpose = ?", "status = ?", "resource_id = ?", "binding_type = ?"} {
		if !strings.Contains(statement.SQL.String(), fragment) {
			t.Fatalf("missing %s in %s", fragment, statement.SQL.String())
		}
	}
	for _, source := range []string{"", "PROJECT"} {
		input.SourceType = source
		legacy := portalReportSourceSessionQuery(db.Session(&gorm.Session{DryRun: true}), p, input, "file").Take(&uploadV2Session{}).Statement
		if strings.Contains(legacy.SQL.String(), "application_id = ?") ||
			!reflect.DeepEqual(legacy.Vars[:8], []any{"tenant-1", "file", "project_management", "project", "report", "READY", "PORTAL_REPORT_REQUEST", "12"}) {
			t.Fatalf("legacy project boundary changed: %s %#v", legacy.SQL.String(), legacy.Vars)
		}
	}
}

// TestPortalLocalContentEnforcesSourceAndAudit 验证正式HTTP读取流程、错误与输出审计。
// 参数：t为测试上下文，SQL及文件服务只在单元边界替换。
// 返回值：无；失败不释放文件字节，成功保留专用主体归属。
func TestPortalLocalContentEnforcesSourceAndAudit(t *testing.T) {
	for _, mode := range []string{"success", "ordinary_writer", "wildcard", "unknown_source",
		"missing_session", "wrong_namespace", "changed_hash", "revoked_binding", "open_failure", "audit_failure"} {
		t.Run(mode, func(t *testing.T) {
			input, files := portalLocalSourceFixture()
			p := portalLocalWriter()
			p.SessionID = "customer_portal-test-portal-report-source"
			p.PermissionCodes = []string{portalReportFileScope}
			status, queried := http.StatusOK, 0
			script := &auditScript{rows: func(query string) (driver.Rows, error) {
				queried++
				if strings.Contains(query, "file_upload_v2_session") {
					if mode == "missing_session" {
						return &fakeRows{columns: []string{"file_id"}}, nil
					}
					return portalLocalSessionRows(input), nil
				}
				if strings.Contains(query, "file_binding") {
					count := int64(1)
					if mode == "revoked_binding" {
						count = 0
					}
					return &fakeRows{columns: []string{"count"}, values: [][]driver.Value{{count}}}, nil
				}
				return nil, errors.New("unexpected SQL")
			}}
			switch mode {
			case "ordinary_writer":
				p.SessionID, status = "customer_portal-test-file-gateway", http.StatusForbidden
			case "wildcard":
				p.PermissionCodes, status = []string{"*"}, http.StatusForbidden
			case "unknown_source":
				input.SourceType, status = "ANY", http.StatusBadRequest
			case "missing_session", "revoked_binding":
				status = http.StatusForbidden
			case "wrong_namespace":
				files.stored.File.Namespace, status = "project", http.StatusConflict
			case "changed_hash":
				files.stored.Version.SHA256, status = make([]byte, 32), http.StatusConflict
			case "open_failure":
				files.openErr, status = errors.New("unavailable"), http.StatusForbidden
			case "audit_failure":
				script.execError, status = errors.New("audit unavailable"), http.StatusServiceUnavailable
			}
			handler, err := NewUploadV2Handler(newAuditTestDB(t, script), files,
				func(time.Time) (string, error) { return "test-id", nil }, slog.Default())
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(input)
			request := httptest.NewRequest(http.MethodPost, "/api/v2/portal-report-files/file/content", bytes.NewReader(body))
			request.SetPathValue("file_id", "file")
			request = request.WithContext(authctx.WithPrincipal(request.Context(), p))
			response := httptest.NewRecorder()
			handler.PortalReportContent(response, request)
			if response.Code != status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
			}
			if status != http.StatusOK {
				if bytes.Equal(response.Body.Bytes(), files.content) {
					t.Fatal("denied response released file")
				}
				if mode == "ordinary_writer" || mode == "wildcard" || mode == "unknown_source" {
					if queried != 0 || len(files.accesses) != 0 {
						t.Fatal("rejected identity reached source lookup")
					}
				}
				return
			}
			if !bytes.Equal(response.Body.Bytes(), files.content) || response.Header().Get("X-File-Version-ID") != "version" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("successful response lost immutable content or headers")
			}
			if len(files.accesses) != 1 || files.accesses[0].ApplicationID != "portal-app" || files.accesses[0].TenantID != "tenant-1" || files.accesses[0].ResourceID != "12" || files.accesses[0].ResourceAccessVerified {
				t.Fatalf("download access=%+v", files.accesses)
			}
			args := strings.Join(script.insertArgs(), "|")
			for _, marker := range []string{"portal-app", p.SessionID, "PORTAL_REPORT_SOURCE_AUTHORIZED", "PORTAL_REPORT_SOURCE_READ"} {
				if !strings.Contains(args, marker) {
					t.Fatalf("audit missing %s: %s", marker, args)
				}
			}
		})
	}
}

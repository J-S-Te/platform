package filetaskhttp

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 安全（SEC-D9）负向测试：HTTP 边界不得再自证 ResourceAccessVerified。
// 断言：①IssueDownloadTicket 传给服务层的 access.ResourceAccessVerified 恒为 false；
// ②请求体无法注入该标志（未知字段被拒绝）；③缺失资源标识的请求在触达服务层之前被拒。

type recordingV2FileService struct {
	accesses []application.DownloadAccess
	openErr  error
}

func (service *recordingV2FileService) Upload(context.Context, application.UploadInput) (domain.File, error) {
	return domain.File{}, errors.New("not implemented")
}

func (service *recordingV2FileService) BindResource(context.Context, application.BindingInput) (domain.FileBinding, error) {
	return domain.FileBinding{}, errors.New("not implemented")
}

func (service *recordingV2FileService) UnbindResource(context.Context, string, string, string, string) error {
	return errors.New("not implemented")
}

func (service *recordingV2FileService) OpenDownload(_ context.Context, access application.DownloadAccess, fileID string) (domain.StoredFile, io.ReadSeekCloser, error) {
	service.accesses = append(service.accesses, access)
	if service.openErr != nil {
		return domain.StoredFile{}, nil, service.openErr
	}
	return domain.StoredFile{
		File:    domain.File{ID: fileID, TenantID: access.TenantID, ApplicationID: access.ApplicationID, Status: domain.FileStatusReady},
		Version: domain.FileVersion{MediaType: "text/plain", OriginalName: "f.txt", Status: domain.FileVersionStatusReady},
	}, &nopReadSeekCloser{Reader: bytes.NewReader([]byte("ok"))}, nil
}

type ticketScript struct {
	executed []string
}

type ticketDriver struct{ script *ticketScript }
type ticketConn struct{ script *ticketScript }
type ticketTx struct{}
type ticketResult int64

func (result ticketResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result ticketResult) RowsAffected() (int64, error) { return int64(result), nil }

func (driver *ticketDriver) Open(string) (driver.Conn, error) {
	return &ticketConn{script: driver.script}, nil
}
func (connection *ticketConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported by ticket test driver")
}
func (*ticketConn) Close() error              { return nil }
func (*ticketConn) Begin() (driver.Tx, error) { return ticketTx{}, nil }
func (*ticketConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return ticketTx{}, nil
}
func (ticketTx) Commit() error   { return nil }
func (ticketTx) Rollback() error { return nil }
func (connection *ticketConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	connection.script.executed = append(connection.script.executed, query)
	return ticketResult(1), nil
}
func (connection *ticketConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, errors.New("unexpected query in ticket test")
}

// nopReadSeekCloser 满足服务层 io.ReadSeekCloser 返回类型（测试桩不需要真实内容校验）。
type nopReadSeekCloser struct{ *bytes.Reader }

func (nopReadSeekCloser) Close() error { return nil }

var ticketDriverCounter uint64

func newTicketTestHandler(t *testing.T, script *ticketScript, files *recordingV2FileService) *UploadV2Handler {
	t.Helper()
	driverName := fmt.Sprintf("fileticket-test-%d", atomic.AddUint64(&ticketDriverCounter, 1))
	sql.Register(driverName, &ticketDriver{script: script})
	sqlDatabase, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open ticket test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDatabase, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open ticket GORM database: %v", err)
	}
	handler, err := NewUploadV2Handler(database, files, func(time.Time) (string, error) { return "01K10C00000000000000000TI1", nil })
	if err != nil {
		t.Fatalf("NewUploadV2Handler: %v", err)
	}
	return handler
}

func ticketRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/file-gateway/api/v2/files/file-1/download-ticket", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.SetPathValue("file_id", "file-1")
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant:          authctx.ReferenceName{ID: "tenant-1"},
		Account:         authctx.ReferenceName{ID: "app-1", Code: "contract_management"},
		User:            authctx.ReferenceName{ID: "user-1"},
		SessionID:       "session-1",
		PermissionCodes: []string{"platform:file:download"},
	}))
}

func TestIssueDownloadTicketNeverSelfCertifiesResourceVerification(t *testing.T) {
	script := &ticketScript{}
	files := &recordingV2FileService{}
	handler := newTicketTestHandler(t, script, files)
	response := httptest.NewRecorder()

	handler.IssueDownloadTicket(response, ticketRequest(`{"resource_type":"REPORT","resource_id":"report-1"}`))

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(files.accesses) != 1 {
		t.Fatalf("OpenDownload 调用次数 = %d, want 1", len(files.accesses))
	}
	access := files.accesses[0]
	if access.ResourceAccessVerified {
		t.Fatal("HTTP 边界不得再自证 ResourceAccessVerified（SEC-D9）")
	}
	if access.ResourceType != "REPORT" || access.ResourceID != "report-1" {
		t.Fatalf("access 资源声明 = %q/%q", access.ResourceType, access.ResourceID)
	}
	if access.TenantID != "tenant-1" || access.ApplicationID != "app-1" {
		t.Fatalf("access 租户/应用 = %q/%q", access.TenantID, access.ApplicationID)
	}
}

func TestIssueDownloadTicketRejectsInjectedVerificationFlag(t *testing.T) {
	script := &ticketScript{}
	files := &recordingV2FileService{}
	handler := newTicketTestHandler(t, script, files)
	response := httptest.NewRecorder()

	// 请求体注入自证标志必须被未知字段校验直接拒绝，且不触达服务层。
	handler.IssueDownloadTicket(response, ticketRequest(`{"resource_type":"REPORT","resource_id":"report-1","resource_access_verified":true}`))

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(files.accesses) != 0 {
		t.Fatalf("注入标志的请求不得触达 OpenDownload: %+v", files.accesses)
	}
}

func TestIssueDownloadTicketRequiresCompleteResourceClaim(t *testing.T) {
	for _, body := range []string{
		`{"resource_type":"","resource_id":""}`,
		`{"resource_type":"REPORT"}`,
		`{"resource_id":"report-1"}`,
	} {
		script := &ticketScript{}
		files := &recordingV2FileService{}
		handler := newTicketTestHandler(t, script, files)
		response := httptest.NewRecorder()

		handler.IssueDownloadTicket(response, ticketRequest(body))

		if response.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status = %d, want 400", body, response.Code)
		}
		if len(files.accesses) != 0 {
			t.Fatalf("body=%s 缺失资源标识的请求不得触达 OpenDownload", body)
		}
	}
}

func TestIssueDownloadTicketMapsForbiddenServiceRejection(t *testing.T) {
	script := &ticketScript{}
	files := &recordingV2FileService{openErr: application.ErrForbidden}
	handler := newTicketTestHandler(t, script, files)
	response := httptest.NewRecorder()

	handler.IssueDownloadTicket(response, ticketRequest(`{"resource_type":"REPORT","resource_id":"report-1"}`))

	// 服务层按具体绑定证明拒绝时，HTTP 边界返回 403——自证标志无法跳过资源校验。
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, query := range script.executed {
		if strings.HasPrefix(strings.TrimSpace(query), "INSERT") {
			t.Fatalf("被拒绝的请求不得写入下载票据: %s", query)
		}
	}
}

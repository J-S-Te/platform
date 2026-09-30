package filetaskhttp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// AUD-2026-021 回归：①auditDirect 写库失败必须输出带可告警字段的结构化 error 日志；
// ②无效上传/下载票据必须落 FAILED 访问审计，且主流程错误响应保持既有错误码不变。

type auditExec struct {
	query string
	args  []driver.NamedValue
}

// auditScript 记录 Exec 语句并在需要时注入失败/返回查询行。
type auditScript struct {
	mu        sync.Mutex
	execs     []auditExec
	execError error
	rows      func(query string) (driver.Rows, error)
}

func (script *auditScript) recordExec(query string, args []driver.NamedValue) {
	script.mu.Lock()
	defer script.mu.Unlock()
	script.execs = append(script.execs, auditExec{query: query, args: args})
}

func (script *auditScript) inserts() []auditExec {
	script.mu.Lock()
	defer script.mu.Unlock()
	var result []auditExec
	for _, item := range script.execs {
		if strings.HasPrefix(strings.TrimSpace(strings.ToUpper(item.query)), "INSERT") {
			result = append(result, item)
		}
	}
	return result
}

func (script *auditScript) insertArgs() []string {
	inserts := script.inserts()
	var args []string
	for _, item := range inserts {
		for _, arg := range item.args {
			args = append(args, fmt.Sprintf("%v", arg.Value))
		}
	}
	return args
}

type auditDriver struct{ script *auditScript }
type auditConn struct{ script *auditScript }
type auditTx struct{}

func (driver *auditDriver) Open(string) (driver.Conn, error) {
	return &auditConn{script: driver.script}, nil
}
func (connection *auditConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported by audit test driver")
}
func (*auditConn) Close() error              { return nil }
func (*auditConn) Begin() (driver.Tx, error) { return auditTx{}, nil }
func (*auditConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return auditTx{}, nil
}
func (auditTx) Commit() error   { return nil }
func (auditTx) Rollback() error { return nil }

func (connection *auditConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	connection.script.recordExec(query, args)
	if connection.script.execError != nil {
		return nil, connection.script.execError
	}
	return auditResult{}, nil
}

// auditResult 返回完整的 Result（含 LastInsertId），否则 GORM 自增主键回填在
// 脚本驱动上会得到 driver.RowsAffected 的固定 LastInsertId 错误并使 Create 失败。
type auditResult struct{}

func (auditResult) LastInsertId() (int64, error) { return 1, nil }
func (auditResult) RowsAffected() (int64, error) { return 1, nil }

func (connection *auditConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if connection.script.rows == nil {
		return nil, errors.New("unexpected query in audit test: " + query)
	}
	return connection.script.rows(query)
}

// fakeRows 返回预置的查询结果，GORM 按列名扫描，缺失列保持零值。
type fakeRows struct {
	columns []string
	values  [][]driver.Value
	pos     int
}

func (rows *fakeRows) Columns() []string { return rows.columns }
func (rows *fakeRows) Close() error      { return nil }
func (rows *fakeRows) Next(dest []driver.Value) error {
	if rows.pos >= len(rows.values) {
		return io.EOF
	}
	copy(dest, rows.values[rows.pos])
	rows.pos++
	return nil
}

var auditDriverCounter uint64

func newAuditTestDB(t *testing.T, script *auditScript) *gorm.DB {
	t.Helper()
	driverName := "fileaudit-test-" + strconv.FormatUint(atomic.AddUint64(&auditDriverCounter, 1), 10)
	sql.Register(driverName, &auditDriver{script: script})
	sqlDatabase, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open audit test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDatabase, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open audit GORM database: %v", err)
	}
	return database
}

// captureLogger 捕获 slog 记录，用于断言审计写失败日志的字段。
type captureLogger struct {
	mu      sync.Mutex
	records []slog.Record
}

func (capture *captureLogger) Enabled(context.Context, slog.Level) bool { return true }
func (capture *captureLogger) Handle(_ context.Context, record slog.Record) error {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.records = append(capture.records, record.Clone())
	return nil
}
func (capture *captureLogger) WithAttrs([]slog.Attr) slog.Handler { return capture }
func (capture *captureLogger) WithGroup(string) slog.Handler      { return capture }

func (capture *captureLogger) find(message string) (slog.Record, bool) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	for _, record := range capture.records {
		if record.Message == message {
			return record, true
		}
	}
	return slog.Record{}, false
}

func recordAttrs(record slog.Record) map[string]string {
	fields := map[string]string{}
	record.Attrs(func(attr slog.Attr) bool {
		fields[attr.Key] = attr.Value.String()
		return true
	})
	return fields
}

func TestAuditDirectWriteFailureLogsStructuredError(t *testing.T) {
	script := &auditScript{execError: errors.New("audit table unavailable")}
	capture := &captureLogger{}
	handler, err := NewUploadV2Handler(newAuditTestDB(t, script), &recordingV2FileService{}, func(time.Time) (string, error) { return "01K10C00000000000000000TI1", nil }, slog.New(capture))
	if err != nil {
		t.Fatalf("NewUploadV2Handler: %v", err)
	}

	handler.auditDirect(context.Background(), "tenant-1", "app-1", "file-1", "user-1", "client-1", "UPLOAD_COMPLETED", "SUCCESS", "req-1")

	record, ok := capture.find("file access audit write failed")
	if !ok {
		t.Fatalf("审计写库失败必须输出 error 日志，实际记录 %+v", capture.records)
	}
	if record.Level != slog.LevelError {
		t.Fatalf("日志级别 = %v, want Error", record.Level)
	}
	fields := recordAttrs(record)
	for _, key := range []string{"error", "tenant_id", "application_id", "file_id", "action", "result"} {
		if _, exists := fields[key]; !exists {
			t.Fatalf("审计失败日志缺少可告警字段 %q: %v", key, fields)
		}
	}
	if fields["tenant_id"] != "tenant-1" || fields["application_id"] != "app-1" || fields["file_id"] != "file-1" || fields["action"] != "UPLOAD_COMPLETED" || fields["result"] != "SUCCESS" {
		t.Fatalf("审计失败日志字段不完整: %v", fields)
	}
	if !strings.Contains(fields["error"], "audit table unavailable") {
		t.Fatalf("审计失败日志必须携带底层错误: %v", fields)
	}
}

func TestUploadContentInvalidTicketWritesFailedAudit(t *testing.T) {
	digest := sha256.Sum256([]byte("invalid-ticket-value"))
	mismatched := append([]byte(nil), digest[:]...)
	mismatched[0] ^= 0xFF // 与请求票据哈希必不相等
	script := &auditScript{rows: func(query string) (driver.Rows, error) {
		if strings.Contains(query, "file_upload_v2_session") {
			return &fakeRows{
				columns: []string{"id", "tenant_id", "application_id", "file_id", "actor_user_id", "authenticated_client_id", "status", "ticket_hash", "ticket_expires_at"},
				values: [][]driver.Value{{
					"01K10C00000000000000000UP1", "tenant-1", "app-1", "file-1", nil,
					"client-1", "CREATED", mismatched, time.Now().UTC().Add(time.Minute),
				}},
			}, nil
		}
		return nil, errors.New("unexpected query: " + query)
	}}
	capture := &captureLogger{}
	handler, err := NewUploadV2Handler(newAuditTestDB(t, script), &recordingV2FileService{}, func(time.Time) (string, error) { return "01K10C00000000000000000TI1", nil }, slog.New(capture))
	if err != nil {
		t.Fatalf("NewUploadV2Handler: %v", err)
	}
	request := httptest.NewRequest(http.MethodPut, "/file-gateway/api/v2/upload-sessions/01K10C00000000000000000UP1/content", nil)
	request.Header.Set("Authorization", "UploadTicket invalid-ticket-value")
	request.Header.Set("X-Request-ID", "req-bad-upload-ticket")
	request.SetPathValue("upload_id", "01K10C00000000000000000UP1")
	response := httptest.NewRecorder()

	handler.UploadContent(response, request)

	// 主流程错误响应保持既有错误码不变。
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "FILE_TICKET_INVALID") {
		t.Fatalf("body = %s", response.Body.String())
	}
	// 无效票据必须落 FAILED 访问审计，且携带会话真实身份。
	inserts := script.inserts()
	if len(inserts) != 1 {
		t.Fatalf("无效上传票据应产生恰好一条审计 INSERT，实际 %d: %v", len(inserts), script.execs)
	}
	if !strings.Contains(strings.ToUpper(inserts[0].query), "FILE_ACCESS_AUDIT") {
		t.Fatalf("审计写入目标表错误: %s", inserts[0].query)
	}
	joined := strings.Join(script.insertArgs(), "|")
	for _, expected := range []string{"UPLOAD_TICKET_REJECTED", "FAILED", "tenant-1", "app-1", "file-1", "client-1", "req-bad-upload-ticket"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("FAILED 审计缺少字段值 %q: %v", expected, script.insertArgs())
		}
	}
}

func TestUploadContentUnknownSessionStillAuditsRejectedTicket(t *testing.T) {
	script := &auditScript{rows: func(query string) (driver.Rows, error) {
		// 会话不存在（伪造 upload_id）：零行结果。
		return &fakeRows{columns: []string{"id"}}, nil
	}}
	capture := &captureLogger{}
	handler, err := NewUploadV2Handler(newAuditTestDB(t, script), &recordingV2FileService{}, func(time.Time) (string, error) { return "01K10C00000000000000000TI1", nil }, slog.New(capture))
	if err != nil {
		t.Fatalf("NewUploadV2Handler: %v", err)
	}
	request := httptest.NewRequest(http.MethodPut, "/file-gateway/api/v2/upload-sessions/01KFAKE0000000000000000001/content", nil)
	request.Header.Set("Authorization", "UploadTicket invalid-ticket-value")
	request.SetPathValue("upload_id", "01KFAKE0000000000000000001")
	response := httptest.NewRecorder()

	handler.UploadContent(response, request)

	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "FILE_TICKET_INVALID") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	joined := strings.Join(script.insertArgs(), "|")
	if !strings.Contains(joined, "UPLOAD_TICKET_REJECTED") || !strings.Contains(joined, "FAILED") || !strings.Contains(joined, "upload-ticket") {
		t.Fatalf("会话未命中的无效票据也必须留 FAILED 审计: %v", script.insertArgs())
	}
}

func TestDownloadContentInvalidTicketWritesFailedAudit(t *testing.T) {
	script := &auditScript{rows: func(query string) (driver.Rows, error) {
		// 票据查询未命中（无效/过期/已使用票据）：零行结果。
		return &fakeRows{columns: []string{"id"}}, nil
	}}
	capture := &captureLogger{}
	handler, err := NewUploadV2Handler(newAuditTestDB(t, script), &recordingV2FileService{}, func(time.Time) (string, error) { return "01K10C00000000000000000TI1", nil }, slog.New(capture))
	if err != nil {
		t.Fatalf("NewUploadV2Handler: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/file-gateway/api/v2/files/file-1/content", nil)
	request.Header.Set("X-Request-ID", "req-bad-download-ticket")
	request.SetPathValue("file_id", "file-1")
	response := httptest.NewRecorder()

	handler.DownloadContent(response, request)

	// 主流程错误响应保持既有错误码不变。
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "FILE_DOWNLOAD_TICKET_INVALID") {
		t.Fatalf("body = %s", response.Body.String())
	}
	inserts := script.inserts()
	if len(inserts) != 1 {
		t.Fatalf("无效下载票据应产生恰好一条审计 INSERT，实际 %d: %v", len(inserts), script.execs)
	}
	if !strings.Contains(strings.ToUpper(inserts[0].query), "FILE_ACCESS_AUDIT") {
		t.Fatalf("审计写入目标表错误: %s", inserts[0].query)
	}
	joined := strings.Join(script.insertArgs(), "|")
	for _, expected := range []string{"DOWNLOAD_TICKET_REJECTED", "FAILED", "file-1", "download-ticket", "req-bad-download-ticket"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("FAILED 审计缺少字段值 %q: %v", expected, script.insertArgs())
		}
	}
}

func TestUploadContentValidTicketStillSucceedsAgainstScriptedDriver(t *testing.T) {
	// 守护测试：脚本驱动返回的会话票据哈希与请求票据一致时，不得走拒绝路径，
	// 防止上述负向测试因脚本配置错误而假绿。
	digest := sha256.Sum256([]byte("valid-ticket"))
	script := &auditScript{rows: func(query string) (driver.Rows, error) {
		return &fakeRows{
			columns: []string{"id", "tenant_id", "application_id", "file_id", "actor_user_id", "authenticated_client_id", "status", "ticket_hash", "ticket_expires_at"},
			values: [][]driver.Value{{
				"01K10C00000000000000000UP1", "tenant-1", "app-1", "file-1", nil,
				"client-1", "CREATED", digest[:], time.Now().UTC().Add(time.Minute),
			}},
		}, nil
	}}
	capture := &captureLogger{}
	handler, err := NewUploadV2Handler(newAuditTestDB(t, script), &recordingV2FileService{}, func(time.Time) (string, error) { return "01K10C00000000000000000TI1", nil }, slog.New(capture))
	if err != nil {
		t.Fatalf("NewUploadV2Handler: %v", err)
	}
	request := httptest.NewRequest(http.MethodPut, "/file-gateway/api/v2/upload-sessions/01K10C00000000000000000UP1/content", nil)
	request.Header.Set("Authorization", "UploadTicket valid-ticket")
	request.SetPathValue("upload_id", "01K10C00000000000000000UP1")
	response := httptest.NewRecorder()

	handler.UploadContent(response, request)

	if strings.Contains(response.Body.String(), "FILE_TICKET_INVALID") {
		t.Fatalf("匹配票据不应被拒绝: status=%d body=%s", response.Code, response.Body.String())
	}
}

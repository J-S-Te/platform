package filetaskhttp

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// FILE 上传会话的操作人信任边界（v2）：actor 只能由已认证主体 p.User.ID 决定。
// createUploadRequest.ActorUserID 为兼容客户端而保留，但服务端不再据此落库：声明值与
// 主体不一致（含机器主体替用户声明）时必须 422 且不创建会话。
//
// 以下用例用本地 SQL 驱动记录实际下发的语句与参数，不连接任何数据库。

type actorProbeStatement struct {
	query string
	args  []driver.NamedValue
}

type actorProbeScript struct {
	mutex      sync.Mutex
	statements []actorProbeStatement
}

func (script *actorProbeScript) record(query string, args []driver.NamedValue) {
	script.mutex.Lock()
	defer script.mutex.Unlock()
	script.statements = append(script.statements, actorProbeStatement{query: query, args: append([]driver.NamedValue(nil), args...)})
}

func (script *actorProbeScript) all() []actorProbeStatement {
	script.mutex.Lock()
	defer script.mutex.Unlock()
	return append([]actorProbeStatement(nil), script.statements...)
}

func (script *actorProbeScript) insertInto(table string) []actorProbeStatement {
	matched := make([]actorProbeStatement, 0, 1)
	for _, statement := range script.all() {
		if strings.HasPrefix(strings.TrimSpace(statement.query), "INSERT") && strings.Contains(statement.query, table) {
			matched = append(matched, statement)
		}
	}
	return matched
}

func (script *actorProbeScript) inserts() []actorProbeStatement {
	matched := make([]actorProbeStatement, 0, 1)
	for _, statement := range script.all() {
		if strings.HasPrefix(strings.TrimSpace(statement.query), "INSERT") {
			matched = append(matched, statement)
		}
	}
	return matched
}

func statementArgumentText(statement actorProbeStatement) string {
	values := make([]string, 0, len(statement.args))
	for _, argument := range statement.args {
		values = append(values, fmt.Sprintf("%v", argument.Value))
	}
	return strings.Join(values, "|")
}

type actorProbeDriver struct{ script *actorProbeScript }
type actorProbeConn struct{ script *actorProbeScript }
type actorProbeTx struct{}
type actorProbeResult int64

func (result actorProbeResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result actorProbeResult) RowsAffected() (int64, error) { return int64(result), nil }

func (driver *actorProbeDriver) Open(string) (driver.Conn, error) {
	return &actorProbeConn{script: driver.script}, nil
}
func (connection *actorProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported by actor probe driver")
}
func (*actorProbeConn) Close() error              { return nil }
func (*actorProbeConn) Begin() (driver.Tx, error) { return actorProbeTx{}, nil }
func (*actorProbeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return actorProbeTx{}, nil
}
func (actorProbeTx) Commit() error   { return nil }
func (actorProbeTx) Rollback() error { return nil }
func (connection *actorProbeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	connection.script.record(query, args)
	return actorProbeResult(1), nil
}

// QueryContext 一律返回“查不到”：幂等键回查不得命中，CreateSession 才会走到落库分支。
func (*actorProbeConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, errors.New("actor probe driver has no persistent rows")
}

var actorProbeDriverCounter uint64

func newActorProbeHandler(t *testing.T) (*UploadV2Handler, *actorProbeScript) {
	t.Helper()
	script := &actorProbeScript{}
	driverName := fmt.Sprintf("file-actor-probe-%d", atomic.AddUint64(&actorProbeDriverCounter, 1))
	sql.Register(driverName, &actorProbeDriver{script: script})
	sqlDatabase, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open actor probe database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	// 幂等回查在脚本化驱动上必然“查不到”，这里静默 GORM 的预期错误日志，保持测试输出可读。
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDatabase, SkipInitializeWithVersion: true}), &gorm.Config{
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open actor probe GORM database: %v", err)
	}
	handler, err := NewUploadV2Handler(database, &recordingV2FileService{}, func(time.Time) (string, error) {
		return "01K10C00000000000000000AC1", nil
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewUploadV2Handler: %v", err)
	}
	return handler, script
}

func authenticatedHumanPrincipal() authctx.Principal {
	return authctx.Principal{
		Tenant:    authctx.ReferenceName{ID: "tenant-1"},
		Account:   authctx.ReferenceName{ID: "app-1", Code: "contract_management"},
		User:      authctx.ReferenceName{ID: "user-1"},
		SessionID: "session-1",
	}
}

// machinePrincipal 模拟只带应用机器身份、没有用户身份的调用方。
func machinePrincipal() authctx.Principal {
	return authctx.Principal{
		Tenant:    authctx.ReferenceName{ID: "tenant-1"},
		Account:   authctx.ReferenceName{ID: "app-1", Code: "contract_management"},
		SessionID: "machine-contract_management",
	}
}

// createSessionRequest 构造一个除 actor 声明外完全合法的创建会话请求。
// declaredActor 为空表示请求体不带 actor_user_id 字段。
func createSessionRequest(principal authctx.Principal, declaredActor string) *http.Request {
	actorField := ""
	if declaredActor != "" {
		actorField = `,"actor_user_id":"` + declaredActor + `"`
	}
	body := `{"purpose":"contract.template","original_name":"template.docx",` +
		`"media_type":"application/vnd.openxmlformats-officedocument.wordprocessingml.document",` +
		`"sha256":"` + strings.Repeat("a", 64) + `","size_bytes":1024,` +
		`"resource_type":"CONTRACT","resource_id":"contract-1","binding_type":"PRIMARY",` +
		`"idempotency_key":"idem-actor-1"` + actorField + `}`
	request := httptest.NewRequest(http.MethodPost, "/file-gateway/api/v2/upload-sessions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request.WithContext(authctx.WithPrincipal(request.Context(), principal))
}

// 请求体声明的操作人与已认证主体不一致时失败关闭，且不创建会话。
func TestCreateSessionRejectsDeclaredActorMismatch(t *testing.T) {
	handler, script := newActorProbeHandler(t)
	response := httptest.NewRecorder()

	handler.CreateSession(response, createSessionRequest(authenticatedHumanPrincipal(), "user-2"))

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "FILE_ACTOR_MISMATCH") {
		t.Fatalf("error code = %s, want FILE_ACTOR_MISMATCH", response.Body.String())
	}
	if inserts := script.inserts(); len(inserts) != 0 {
		t.Fatalf("被拒绝的请求不得创建上传会话: %v", inserts)
	}
}

// 机器主体没有用户身份，不得在请求体里替某个用户声明归属。
func TestCreateSessionRejectsMachineActorDeclaration(t *testing.T) {
	handler, script := newActorProbeHandler(t)
	response := httptest.NewRecorder()

	handler.CreateSession(response, createSessionRequest(machinePrincipal(), "user-1"))

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "FILE_ACTOR_MISMATCH") {
		t.Fatalf("error code = %s, want FILE_ACTOR_MISMATCH", response.Body.String())
	}
	if inserts := script.inserts(); len(inserts) != 0 {
		t.Fatalf("机器主体替用户声明时不得创建上传会话: %v", inserts)
	}
}

// 请求体省略 actor 时，落库的操作人取自已认证主体。
func TestCreateSessionUsesAuthenticatedActorWhenBodyOmitsIt(t *testing.T) {
	handler, script := newActorProbeHandler(t)
	response := httptest.NewRecorder()

	handler.CreateSession(response, createSessionRequest(authenticatedHumanPrincipal(), ""))

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	sessions := script.insertInto("file_upload_v2_session")
	if len(sessions) != 1 {
		t.Fatalf("upload session insert count = %d, want 1", len(sessions))
	}
	if arguments := statementArgumentText(sessions[0]); !strings.Contains(arguments, "user-1") {
		t.Fatalf("会话操作人未取自已认证主体: args = %s", arguments)
	}
	if !strings.Contains(sessions[0].query, "actor_user_id") {
		t.Fatalf("上传会话必须落库操作人列: SQL = %s", sessions[0].query)
	}
}

// 正常路径不变：请求体声明与主体一致时按主体落库，请求返回 201。
func TestCreateSessionAcceptsMatchingDeclaredActor(t *testing.T) {
	handler, script := newActorProbeHandler(t)
	response := httptest.NewRecorder()

	handler.CreateSession(response, createSessionRequest(authenticatedHumanPrincipal(), "user-1"))

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	sessions := script.insertInto("file_upload_v2_session")
	if len(sessions) != 1 {
		t.Fatalf("upload session insert count = %d, want 1", len(sessions))
	}
	if arguments := statementArgumentText(sessions[0]); !strings.Contains(arguments, "user-1") {
		t.Fatalf("会话操作人 = %s, want user-1", arguments)
	}
}

// 机器主体且请求体不带 actor 时保持既有行为：会话创建成功但不记录操作人。
func TestCreateSessionAllowsMachinePrincipalWithoutActorClaim(t *testing.T) {
	handler, script := newActorProbeHandler(t)
	response := httptest.NewRecorder()

	handler.CreateSession(response, createSessionRequest(machinePrincipal(), ""))

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	sessions := script.insertInto("file_upload_v2_session")
	if len(sessions) != 1 {
		t.Fatalf("upload session insert count = %d, want 1", len(sessions))
	}
	if arguments := statementArgumentText(sessions[0]); strings.Contains(arguments, "user-") {
		t.Fatalf("机器主体不得落库用户操作人: args = %s", arguments)
	}
}

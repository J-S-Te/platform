//go:build portal_report_mysql

package filetaskhttp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"gorm.io/gorm"
)

type portalLocalMySQLFixture struct {
	db                  *gorm.DB
	tenant, application string
	content             []byte
	newID               func() string
	request             func(*testing.T, string, string, string, []byte) (int, http.Header, []byte)
	token               func(string, string, string, string, []string) string
}

// portalLocalGatewayField 解析真实网关响应中的固定字符串字段。
// 参数：t为测试上下文，body为响应，name为预期字段。
// 返回值：字段内容；响应不合法或字段缺失时终止当前用例。
func portalLocalGatewayField(t *testing.T, body []byte, name string) string {
	t.Helper()
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := json.Unmarshal(envelope.Data[name], &value); err != nil {
		t.Fatal(err)
	}
	return value
}

// portalLocalMySQLCase 在原独占MySQL进程夹具内验证本地PDF真实上传与OpenDownload。
// 参数：t为测试上下文，f复用原测试的受控进程、真实令牌与独占库。
// 返回值：无；不会寻找其他DSN或连接既有业务环境。
func portalLocalMySQLCase(t *testing.T, f portalLocalMySQLFixture) {
	digest := sha256.Sum256(f.content)
	hash := hex.EncodeToString(digest[:])
	writer := f.token(f.tenant, f.application, "customer_portal", "customer_portal-test-file-gateway", []string{"platform:file:upload"})
	reader := f.token(f.tenant, f.application, "customer_portal", "customer_portal-test-portal-report-source", []string{portalReportFileScope})
	body, _ := json.Marshal(map[string]any{"purpose": "portal.local_report", "original_name": "local-report.pdf",
		"media_type": "application/pdf", "size_bytes": len(f.content), "sha256": hash,
		"resource_type": "PORTAL_REPORT_REQUEST", "resource_id": "123", "binding_type": "REPORT",
		"idempotency_key": "portal-local-" + f.newID()})
	status, _, out := f.request(t, "POST", "/api/v2/upload-sessions", "Bearer "+writer, body)
	if status != http.StatusCreated {
		t.Fatalf("local session status=%d", status)
	}
	sessionID, fileID := portalLocalGatewayField(t, out, "upload_id"), portalLocalGatewayField(t, out, "file_id")
	t.Cleanup(func() {
		for _, table := range []string{"file_access_audit", "file_validation_event", "file_download_ticket", "file_upload_v2_session", "file_binding", "file_upload_session", "file_version"} {
			if err := f.db.Table(table).Where("file_id=?", fileID).Delete(nil).Error; err != nil {
				t.Error("owned local fixture cleanup failed", table)
			}
		}
		if err := f.db.Table("file_object").Where("id=?", fileID).Delete(nil).Error; err != nil {
			t.Error("owned local object cleanup failed")
		}
	})
	status, _, out = f.request(t, "POST", "/api/v2/upload-sessions/"+sessionID+"/tickets", "Bearer "+writer, []byte(`{}`))
	if status != http.StatusCreated {
		t.Fatalf("local ticket status=%d", status)
	}
	ticket := portalLocalGatewayField(t, out, "ticket")
	status, _, _ = f.request(t, "PUT", "/api/v2/upload-sessions/"+sessionID+"/content", "UploadTicket "+ticket, f.content)
	if status != http.StatusCreated {
		t.Fatalf("local content status=%d", status)
	}
	var session uploadV2Session
	if err := f.db.Where("id=?", sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.Namespace != "portal" || session.Purpose != "local-report" || session.ApplicationID != f.application || session.Status != "READY" {
		t.Fatal("local publication namespace or owner changed")
	}
	claim := portalReportSourceRequest{SourceType: "PORTAL_LOCAL", RequestID: 123, ExpectedSHA256: hash,
		ExpectedSize: int64(len(f.content)), ExpectedMIME: "application/pdf", ExpectedName: "local-report.pdf"}
	path := "/api/v2/portal-report-files/" + fileID + "/content"
	claimBody, _ := json.Marshal(claim)
	status, headers, out := f.request(t, "POST", path, "Bearer "+reader, claimBody)
	if status != http.StatusOK || !bytes.Equal(out, f.content) || headers.Get("X-File-Version-ID") != session.VersionID || headers.Get("X-File-SHA256") != hash {
		t.Fatalf("actual LOCAL immutable source failed status=%d", status)
	}
	portalLocalMySQLDeniedCases(t, f, path, reader, claim, sessionID, fileID)
	var authorized int64
	if err := f.db.Model(&accessAudit{}).Where("file_id=? AND action='PORTAL_REPORT_SOURCE_AUTHORIZED'", fileID).Count(&authorized).Error; err != nil || authorized != 1 {
		t.Fatal("LOCAL denials wrote successful authorization audit")
	}
}

// portalLocalMySQLDeniedCases 核对真实SQL的跨应用、错来源、错申请和当前绑定拒绝。
// 参数：t/f为独占夹具，path/reader/claim为成功固定声明，sessionID/fileID为本次数据。
// 返回值：无；拒绝用例不得释放报告内容，修改仅限本次夹具且必须恢复。
func portalLocalMySQLDeniedCases(t *testing.T, f portalLocalMySQLFixture, path, reader string, claim portalReportSourceRequest, sessionID, fileID string) {
	deny := func(t *testing.T, token string, input portalReportSourceRequest) {
		t.Helper()
		body, _ := json.Marshal(input)
		status, _, out := f.request(t, "POST", path, "Bearer "+token, body)
		if status < 400 || bytes.Equal(out, f.content) {
			t.Fatalf("invalid LOCAL source released content status=%d", status)
		}
	}
	t.Run("other_application", func(t *testing.T) {
		deny(t, f.token(f.tenant, f.newID(), "customer_portal", "customer_portal-test-portal-report-source", []string{portalReportFileScope}), claim)
	})
	t.Run("ordinary_writer", func(t *testing.T) {
		deny(t, f.token(f.tenant, f.application, "customer_portal", "customer_portal-test-file-gateway", []string{portalReportFileScope}), claim)
	})
	t.Run("wrong_request", func(t *testing.T) { wrong := claim; wrong.RequestID++; deny(t, reader, wrong) })
	t.Run("project_default", func(t *testing.T) { wrong := claim; wrong.SourceType = ""; deny(t, reader, wrong) })
	for _, test := range []struct{ name, table, key, id, column, wrong, restore string }{
		{"wrong_namespace", "file_upload_v2_session", "id", sessionID, "namespace", "project", "portal"},
		{"wrong_purpose", "file_upload_v2_session", "id", sessionID, "purpose", "report", "local-report"},
		{"wrong_application_code", "file_upload_v2_session", "id", sessionID, "application_code", "project_management", "customer_portal"},
		{"wrong_binding_type", "file_binding", "file_id", fileID, "binding_type", "ATTACHMENT", "REPORT"},
		{"revoked_binding", "file_binding", "file_id", fileID, "status", "INACTIVE", "ACTIVE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := f.db.Table(test.table).Where(test.key+"=?", test.id)
			if err := query.Update(test.column, test.wrong).Error; err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := f.db.Table(test.table).Where(test.key+"=?", test.id).Update(test.column, test.restore).Error; err != nil {
					t.Error("owned fixture restore failed")
				}
			}()
			deny(t, reader, claim)
		})
	}
}

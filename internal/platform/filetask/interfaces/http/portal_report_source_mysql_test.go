//go:build portal_report_mysql

package filetaskhttp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	mysqlconfig "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 使用本测试独占的临时 schema 和实际 file-gateway 进程。
// 测试不伪造认证主体，也不替换生产令牌中间件、
// FileService、SQL 仓储、本地内容存储或 HTTP 处理器。
func TestPortalReportSourceProductionProcessOnMySQL(t *testing.T) {
	path := os.Getenv("UIP_P1_FILE_GATEWAY_TEST_DSN_FILE")
	if path == "" {
		t.Fatal("owned MySQL DSN file is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read owned MySQL DSN file failed")
	}
	dsn := strings.TrimSpace(string(raw))
	parsed, err := mysqlconfig.ParseDSN(dsn)
	if err != nil || parsed.DBName != "file_gateway_p1_test_20261013" {
		t.Fatal("refuse non-owned MySQL schema")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("connect owned MySQL schema failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	module, err := filepath.Abs("../../../../..")
	if err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	storage := filepath.Join(temp, "store")
	binary := filepath.Join(temp, "file-gateway")
	build := exec.Command("go", "build", "-race", "-o", binary, "./cmd/file-gateway")
	build.Dir = module
	build.Env = append(cleanPortalGatewayTestEnv(), "GOCACHE=/private/tmp/uip-business-review-go-cache", "GOWORK=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real file-gateway: %v\n%s", err, output)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	privPath, pubPath := filepath.Join(temp, "private.pem"), filepath.Join(temp, "public.pem")
	for p, b := range map[string][]byte{privPath: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), pubPath: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})} {
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := security.LoadApplicationJWTManager("portal-report-p1-test", "portal-report-gateway-test", privPath, pubPath)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	command := exec.Command(binary)
	command.Env = append(cleanPortalGatewayTestEnv(), "FILE_GATEWAY_DATABASE_DSN="+dsn, "FILE_GATEWAY_HTTP_ADDRESS="+address, "FILE_GATEWAY_STORAGE_BACKEND=local", "FILE_GATEWAY_STORAGE_ROOT="+storage, "FILE_GATEWAY_TOKEN_ISSUER=portal-report-p1-test", "FILE_GATEWAY_TOKEN_AUDIENCE=portal-report-gateway-test", "FILE_GATEWAY_TOKEN_PUBLIC_KEY_PATH="+pubPath)
	// 子进程日志可能包含数据库错误，须保持私密且不得打印。
	var childLogs bytes.Buffer
	command.Stdout = &childLogs
	command.Stderr = &childLogs
	if err := command.Start(); err != nil {
		t.Fatal("start real file-gateway process failed")
	}
	defer func() {
		_ = command.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = command.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
	}()
	base := "http://" + address
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ready := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Get(base + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("real file-gateway did not become ready; child logs withheld")
	}
	newID := func() string {
		id, err := ulid.New(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	tenant, projectApp, portalApp := newID(), newID(), newID()
	token := func(tenant, app, code, subject string, scopes []string) string {
		now := time.Now().UTC()
		raw, err := manager.Issue(security.ApplicationTokenClaims{OAuthClientID: newID(), ClientID: subject, TenantID: tenant, ApplicationID: app, ApplicationCode: code, EnvironmentID: newID(), EnvironmentCode: "test", Scopes: scopes, IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(5 * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	projectToken := token(tenant, projectApp, "project_management", "project-test-report-publisher", []string{"platform:file:upload"})
	portalToken := token(tenant, portalApp, "customer_portal", "customer_portal-test-portal-report-source", []string{portalReportFileScope})
	request := func(t *testing.T, method, path, authorization string, body []byte) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), method, base+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", authorization)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("real gateway request failed")
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header, out
	}
	decode := func(raw []byte) map[string]json.RawMessage {
		t.Helper()
		var envelope struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal("gateway JSON malformed")
		}
		return envelope.Data
	}
	field := func(data map[string]json.RawMessage, key string) string {
		t.Helper()
		var s string
		if err := json.Unmarshal(data[key], &s); err != nil {
			t.Fatalf("missing gateway field %s", key)
		}
		return s
	}
	content := portalGatewayPDF()
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	input := map[string]any{"purpose": "project.report", "original_name": "report.pdf", "media_type": "application/pdf", "size_bytes": len(content), "sha256": hash, "resource_type": "PORTAL_REPORT_REQUEST", "resource_id": "123", "binding_type": "REPORT", "idempotency_key": "portal-p1-" + newID()}
	body, _ := json.Marshal(input)
	status, _, out := request(t, "POST", "/api/v2/upload-sessions", "Bearer "+projectToken, body)
	if status != 201 {
		t.Fatalf("actual V2 session create failed status=%d", status)
	}
	data := decode(out)
	sessionID, fileID := field(data, "upload_id"), field(data, "file_id")
	// 只移除本次运行拥有的行，绝不清空共享或旧数据库。
	defer func() {
		for _, table := range []string{"file_access_audit", "file_validation_event", "file_download_ticket", "file_upload_v2_session", "file_binding", "file_upload_session", "file_version"} {
			if err := db.Table(table).Where("file_id = ?", fileID).Delete(nil).Error; err != nil {
				t.Errorf("owned fixture cleanup %s failed", table)
			}
		}
		if err := db.Table("file_object").Where("id = ?", fileID).Delete(nil).Error; err != nil {
			t.Error("owned file-object cleanup failed")
		}

		var remaining int64
		if err := db.Table("file_object").Where("id=?", fileID).Count(&remaining).Error; err != nil || remaining != 0 {
			t.Error("owned fixture cleanup did not complete")
		}
	}()
	status, _, out = request(t, "POST", "/api/v2/upload-sessions/"+sessionID+"/tickets", "Bearer "+projectToken, []byte(`{}`))
	if status != 201 {
		t.Fatalf("actual upload ticket failed %d", status)
	}
	uploadTicket := field(decode(out), "ticket")
	status, _, out = request(t, "PUT", "/api/v2/upload-sessions/"+sessionID+"/content", "UploadTicket "+uploadTicket, content)
	if status != 201 {
		t.Fatalf("actual upload/store/validation/bind failed %d", status)
	}
	bindingID := field(decode(out), "binding_id")
	var session uploadV2Session
	if err := db.Where("id=?", sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.Status != "READY" || session.ApplicationCode != "project_management" || session.VersionID == "" {
		t.Fatal("actual V2 publication not ready")
	}
	var validations int64
	if err := db.Model(&validationEvent{}).Where("file_id=? AND result='PASSED'", fileID).Count(&validations).Error; err != nil || validations != 1 {
		t.Fatal("actual static-validation evidence absent")
	}
	source := portalReportSourceRequest{RequestID: 123, ExpectedSHA256: hash, ExpectedSize: int64(len(content)), ExpectedMIME: "application/pdf", ExpectedName: "report.pdf"}
	sourceBody, _ := json.Marshal(source)
	sourcePath := "/api/v2/portal-report-files/" + fileID + "/content"
	status, headers, out := request(t, "POST", sourcePath, "Bearer "+portalToken, sourceBody)
	if status != 200 || !bytes.Equal(out, content) || headers.Get("X-File-Version-ID") != session.VersionID || headers.Get("X-File-SHA256") != hash || headers.Get("Content-Type") != "application/pdf" {
		t.Fatalf("actual delegated immutable file read failed status=%d", status)
	}
	var audits []accessAudit
	if err := db.Where("file_id=? AND action IN ?", fileID, []string{"PORTAL_REPORT_SOURCE_AUTHORIZED", "PORTAL_REPORT_SOURCE_READ"}).Find(&audits).Error; err != nil || len(audits) != 2 {
		t.Fatal("source authorization/read audits absent")
	}
	for _, audit := range audits {
		if audit.TenantID != tenant || audit.ApplicationID != portalApp || audit.AuthenticatedClientID != "customer_portal-test-portal-report-source" || audit.Result != "SUCCESS" {
			t.Fatal("source audit attribution mismatch")
		}
	}
	assertDenied := func(t *testing.T, auth string, claim portalReportSourceRequest) {
		t.Helper()
		body, _ := json.Marshal(claim)
		status, headers, out := request(t, "POST", sourcePath, "Bearer "+auth, body)
		if status < 400 || bytes.Contains(out, []byte("%PDF")) || bytes.Equal(out, content) || headers.Get("X-File-Version-ID") != "" {
			t.Fatalf("unauthorized source exposed content status=%d", status)
		}
	}
	t.Run("tenant", func(t *testing.T) {
		assertDenied(t, token(newID(), portalApp, "customer_portal", "customer_portal-test-portal-report-source", []string{portalReportFileScope}), source)
	})
	t.Run("caller_application", func(t *testing.T) {
		assertDenied(t, token(tenant, projectApp, "project_management", "customer_portal-test-portal-report-source", []string{portalReportFileScope}), source)
	})
	t.Run("scope", func(t *testing.T) {
		assertDenied(t, token(tenant, portalApp, "customer_portal", "customer_portal-test-portal-report-source", []string{"platform:file:download"}), source)
	})
	t.Run("wildcard_scope", func(t *testing.T) {
		assertDenied(t, token(tenant, portalApp, "customer_portal", "customer_portal-test-portal-report-source", []string{"*"}), source)
	})
	t.Run("wrong_purpose_client", func(t *testing.T) {
		assertDenied(t, token(tenant, portalApp, "customer_portal", "customer_portal-test-file-gateway", []string{portalReportFileScope}), source)
	})
	t.Run("request", func(t *testing.T) { wrong := source; wrong.RequestID++; assertDenied(t, portalToken, wrong) })
	t.Run("hash", func(t *testing.T) {
		wrong := source
		wrong.ExpectedSHA256 = strings.Repeat("a", 64)
		assertDenied(t, portalToken, wrong)
	})
	t.Run("size", func(t *testing.T) { wrong := source; wrong.ExpectedSize++; assertDenied(t, portalToken, wrong) })
	t.Run("mime", func(t *testing.T) {
		wrong := source
		wrong.ExpectedMIME = "text/plain"
		assertDenied(t, portalToken, wrong)
	})
	t.Run("name", func(t *testing.T) {
		wrong := source
		wrong.ExpectedName = "other.pdf"
		assertDenied(t, portalToken, wrong)
	})
	t.Run("content_tamper", func(t *testing.T) {
		var row struct{ StorageRelativePath string }
		if err := db.Table("file_version").Select("storage_relative_path").Where("id=?", session.VersionID).Take(&row).Error; err != nil {
			t.Fatal(err)
		}
		if !filepath.IsLocal(row.StorageRelativePath) {
			t.Fatal("fixture content path not local")
		}
		p := filepath.Join(storage, row.StorageRelativePath)
		if err := os.WriteFile(p, bytes.Repeat([]byte("X"), len(content)), 0600); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.WriteFile(p, content, 0600); err != nil {
				t.Error(err)
			}
		}()
		assertDenied(t, portalToken, source)
	})

	mutate := func(t *testing.T, table, key, id, column string, wrong, restore any) {
		t.Helper()
		if err := db.Table(table).Where(key+"=?", id).Update(column, wrong).Error; err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := db.Table(table).Where(key+"=?", id).Update(column, restore).Error; err != nil {
				t.Error(err)
			}
		}()
		assertDenied(t, portalToken, source)
	}
	t.Run("source_owner", func(t *testing.T) {
		mutate(t, "file_upload_v2_session", "id", sessionID, "application_id", portalApp, projectApp)
	})
	t.Run("source_application", func(t *testing.T) {
		mutate(t, "file_upload_v2_session", "id", sessionID, "application_code", "customer_portal", "project_management")
	})
	t.Run("version", func(t *testing.T) {
		mutate(t, "file_upload_v2_session", "id", sessionID, "version_id", newID(), session.VersionID)
	})
	t.Run("revoked_binding", func(t *testing.T) { mutate(t, "file_binding", "id", bindingID, "status", "INACTIVE", "ACTIVE") })
	t.Run("quarantined", func(t *testing.T) { mutate(t, "file_object", "id", fileID, "status", "QUARANTINED", "READY") })
	t.Run("signature", func(t *testing.T) {
		parts := strings.Split(portalToken, ".")
		parts[2] = strings.Repeat("A", len(parts[2]))
		assertDenied(t, strings.Join(parts, "."), source)
	})
	var authorized int64
	if err := db.Model(&accessAudit{}).Where("file_id=? AND action='PORTAL_REPORT_SOURCE_AUTHORIZED'", fileID).Count(&authorized).Error; err != nil || authorized != 1 {
		t.Fatal("denied requests created successful authorization audit")
	}
}

func cleanPortalGatewayTestEnv() []string {
	var out []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.Contains(strings.ToUpper(name), "DSN") || strings.HasPrefix(name, "FILE_GATEWAY_") || name == "GOWORK" || name == "GOCACHE" {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func portalGatewayPDF() []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << >> /Contents 4 0 R >>", "<< /Length 0 >>\nstream\n\nendstream"}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return b.Bytes()
}

package filetaskhttp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

// localReportClaims 构建真实网关DTO，保留明确的Portal本地来源标签。
// 参数：t为测试上下文，source为请求声明的来源类型，digest为合成内容摘要。
// 返回值：经过JSON解码的完整性声明。
func localReportClaims(t *testing.T, source string, digest []byte) portalReportSourceRequest {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"source_type": source, "request_id": 12,
		"expected_sha256": hex.EncodeToString(digest), "expected_size": 8,
		"expected_mime": "application/pdf", "expected_name": "local-report.pdf"})
	var input portalReportSourceRequest
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	return input
}

// TestPortalLocalReportPurposeIsSeparate 核对本地报告用途不借用项目应用命名空间。
// 参数：t为测试上下文。
// 返回值：无；错误应用可上传或报告策略缺失时失败。
func TestPortalLocalReportPurposeIsSeparate(t *testing.T) {
	value, exists := uploadPolicies["portal.local_report"]
	if !exists || value.Namespace != "portal" || value.Purpose != "local-report" ||
		value.MaxBytes != 20<<20 || value.Temporary {
		t.Fatal("Portal local report requires its own reviewed upload policy")
	}
	if len(value.Media) != 1 {
		t.Fatal("only PDF is supported")
	}
	if _, exists := value.Media["application/pdf"]; !exists {
		t.Fatal("PDF policy missing")
	}
	if !validPurposeApplication("customer_portal", value.Namespace) ||
		validPurposeApplication("project_management", value.Namespace) {
		t.Fatal("local report purpose crossed application boundary")
	}
}

// TestPortalLocalReportPinsSourceKindAndContent 验证本地文件不能按默认项目声明读取。
// 参数：t为测试上下文，所有记录均为合成数据且不连接数据库。
// 返回值：无；来源混用或内容绑定变化被接受时失败。
func TestPortalLocalReportPinsSourceKindAndContent(t *testing.T) {
	digest := sha256.Sum256([]byte("test-pdf"))
	input := localReportClaims(t, "PORTAL_LOCAL", digest[:])
	session := uploadV2Session{TenantID: "tenant", ApplicationID: "portal-app", ApplicationCode: "customer_portal",
		FileID: "file", VersionID: "version", Namespace: "portal", Purpose: "local-report",
		ExpectedSize: 8, ExpectedSHA256: digest[:]}
	stored := domain.StoredFile{File: domain.File{ID: "file", TenantID: "tenant", ApplicationID: "portal-app",
		Namespace: "portal", Purpose: "local-report", Status: domain.FileStatusReady},
		Version: domain.FileVersion{ID: "version", FileID: "file", SizeBytes: 8, SHA256: digest[:],
			MediaType: "application/pdf", OriginalName: "local-report.pdf", Status: domain.FileVersionStatusReady}}
	if !portalReportSourceMatches(stored, session, input, digest[:]) {
		t.Fatal("valid Portal local immutable source was rejected")
	}
	legacy := localReportClaims(t, "", digest[:])
	if portalReportSourceMatches(stored, session, legacy, digest[:]) {
		t.Fatal("local file was reinterpreted as a project report")
	}
	unknown := localReportClaims(t, "ANY_APPLICATION", digest[:])
	if _, valid := validPortalReportSource(unknown); valid {
		t.Fatal("unknown source kind was accepted")
	}
	session.ApplicationCode = "project_management"
	if portalReportSourceMatches(stored, session, input, digest[:]) {
		t.Fatal("project session was accepted as Portal local source")
	}
}

package filetaskhttp

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// 本文件回归 BIZ-XAPI-11：v1 上传此前只对两个合同 purpose 强制媒体/大小策略，
// 其余 purpose（包括已登记 policy 的 purpose）只受全局上限约束，空 purpose 更是
// 完全不查策略。现在登记 purpose 一律按策略表强制媒体与大小，空 purpose 与未登记
// purpose 都失败关闭，策略表仍是 v1/v2 共用的唯一事实源。

// v1PurposeAccountCodes 是各存储命名空间对应的代表应用编码，取值与
// validPurposeApplication 的别名表一致：v1 上传必须同时通过用途与命名空间绑定。
var v1PurposeAccountCodes = map[string]string{
	"reimbursement": "reimbursement",
	"platform":      "platform",
	"crm":           "customer_and_opportunity",
	"portal":        "customer_portal",
	"contract":      "contract_management",
	"project":       "project_management",
	"settlement":    "settlement",
}

// v1AllowedMedia 返回该策略白名单里字典序最小的合法媒体类型，供允许矩阵使用。
func v1AllowedMedia(item uploadPolicy) string {
	allowed := make([]string, 0, len(item.Media))
	for media := range item.Media {
		allowed = append(allowed, media)
	}
	sort.Strings(allowed)
	return allowed[0]
}

// v1DisallowedMedia 返回一个确定不在该策略白名单内的媒体类型。
func v1DisallowedMedia(item uploadPolicy) string {
	for _, candidate := range []string{"text/plain", "image/gif", "application/x-unsupported"} {
		if _, allowed := item.Media[candidate]; !allowed {
			return candidate
		}
	}
	panic("no disallowed media candidate for policy")
}

// v1ForeignAccountCode 返回一个不属于该策略命名空间的应用编码，用于跨命名空间拒绝。
func v1ForeignAccountCode(t *testing.T, namespace string) string {
	t.Helper()
	names := make([]string, 0, len(v1PurposeAccountCodes))
	for name := range v1PurposeAccountCodes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name != namespace {
			return v1PurposeAccountCodes[name]
		}
	}
	t.Fatalf("no foreign namespace available for %q", namespace)
	return ""
}

// v1PolicyReadyFile 是策略校验通过后服务层返回的 READY 文件。
func v1PolicyReadyFile() domain.File {
	return domain.File{ID: "file-1", OriginalName: "payload", MediaType: "application/pdf", Classification: "INTERNAL", Status: domain.FileStatusReady, CreatedAt: time.Now().UTC()}
}

// v1PolicyExpectedRetention 反映策略表 Temporary 标记到保留类的映射；v2 会话用同一
// 规则（Temporary ⇒ 24h 临时保留），故可作为 v1/v2 一致性口径。
func v1PolicyExpectedRetention(item uploadPolicy) string {
	if item.Temporary {
		return "TEMPORARY"
	}
	return "LONG_TERM"
}

func v1PolicyUploadRequest(t *testing.T, accountCode, purpose, name, contentType string, content []byte) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("application_id", "app-1"); err != nil {
		t.Fatal(err)
	}
	if purpose != "" {
		if err := writer.WriteField("purpose", purpose); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="` + name + `"`},
		"Content-Type":        {contentType},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Request-ID", "req-v1-policy")
	principal := v1Principal()
	principal.Account.Code = accountCode
	return request.WithContext(authctx.WithPrincipal(request.Context(), principal))
}

func newV1PolicyHandler(t *testing.T, service *stubV1FileService) *Handler {
	t.Helper()
	handler, err := NewHandler(service, &stubV1JobService{}, slog.New(&captureLogger{}), nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return handler
}

// 已登记 purpose 必须按其策略校验媒体类型：合同模板只允许 docx，不允许 text/plain。
func TestV1UploadRejectsMediaOutsidePurposePolicy(t *testing.T) {
	for _, tt := range []struct {
		name        string
		accountCode string
		purpose     string
		contentType string
	}{
		{name: "contract template", accountCode: "contract_management", purpose: "contract.template", contentType: "text/plain"},
		{name: "contract stamped pdf", accountCode: "contract_management", purpose: "contract.stamped-pdf", contentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{name: "contract external source over media", accountCode: "contract_management", purpose: "contract.external.source", contentType: "application/pdf"},
		{name: "crm attachment", accountCode: "customer_and_opportunity", purpose: "crm.opportunity.attachment", contentType: "text/plain"},
		{name: "settlement invoice", accountCode: "settlement", purpose: "settlement.invoice.document", contentType: "image/gif"},
		{name: "platform user import", accountCode: "platform", purpose: "platform.iam.user-import", contentType: "application/pdf"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := &stubV1FileService{}
			handler := newV1PolicyHandler(t, files)
			response := httptest.NewRecorder()
			handler.Upload(response, v1PolicyUploadRequest(t, tt.accountCode, tt.purpose, "payload.bin", tt.contentType, []byte("payload")))
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body = %s", response.Code, response.Body.String())
			}
		})
	}
}

// 媒体类型符合策略时仍必须进入服务层，并携带策略解析出的命名空间与用途。
func TestV1UploadAcceptsMediaInsidePurposePolicy(t *testing.T) {
	files := &stubV1FileService{file: domain.File{ID: "file-1", OriginalName: "template.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Classification: "INTERNAL", Status: domain.FileStatusReady, CreatedAt: time.Now().UTC()}}
	handler := newV1PolicyHandler(t, files)
	response := httptest.NewRecorder()
	handler.Upload(response, v1PolicyUploadRequest(t, "contract_management", "contract.template", "template.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", []byte("docx")))

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", response.Code, response.Body.String())
	}
	if files.uploaded.Namespace != "contract" || files.uploaded.Purpose != "template" {
		t.Fatalf("upload input namespace/purpose = %q/%q, want contract/template", files.uploaded.Namespace, files.uploaded.Purpose)
	}
	// 此前未强制策略的 purpose 现在同样带上策略版本与保留类。
	if files.uploaded.PolicyVersion != v2PolicyVersion || files.uploaded.RetentionClass != "LONG_TERM" {
		t.Fatalf("upload input policy = %q/%q, want %q/LONG_TERM", files.uploaded.PolicyVersion, files.uploaded.RetentionClass, v2PolicyVersion)
	}
}

// 未登记的 purpose 继续失败关闭（403），不会落回无策略上传。
func TestV1UploadRejectsUnregisteredPurpose(t *testing.T) {
	files := &stubV1FileService{}
	handler := newV1PolicyHandler(t, files)
	response := httptest.NewRecorder()
	handler.Upload(response, v1PolicyUploadRequest(t, "contract_management", "unknown.purpose", "payload.bin", "application/pdf", []byte("payload")))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", response.Code, response.Body.String())
	}
}

// 空 purpose 一律 422 且不进入服务层：v1 不再存在“无策略上传”通道，历史裸上传
// 必须显式改用策略表中的用途键（BIZ-XAPI-11）。
func TestV1UploadRejectsEmptyPurpose(t *testing.T) {
	for _, tt := range []struct {
		name        string
		accountCode string
		contentType string
	}{
		{name: "contract", accountCode: "contract_management", contentType: "application/pdf"},
		{name: "crm", accountCode: "customer_and_opportunity", contentType: "application/pdf"},
		{name: "settlement csv report", accountCode: "settlement", contentType: "text/csv; charset=utf-8"},
		{name: "platform", accountCode: "platform", contentType: "text/csv"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := &stubV1FileService{file: v1PolicyReadyFile()}
			handler := newV1PolicyHandler(t, files)
			response := httptest.NewRecorder()
			handler.Upload(response, v1PolicyUploadRequest(t, tt.accountCode, "", "payload.bin", tt.contentType, []byte("payload")))
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body = %s", response.Code, response.Body.String())
			}
			if files.uploaded.ApplicationID != "" {
				t.Fatalf("empty purpose must not reach the service layer: %+v", files.uploaded)
			}
		})
	}
}

// TestV1UploadAppliesEveryPurposePolicy 锁定旧用途上传策略及新本地用途的v1拒绝。
// 参数：t为测试上下文，遍历全部登记用途。
// 返回值：无；旧用途策略或本地用途隔离退化时失败。
func TestV1UploadAppliesEveryPurposePolicy(t *testing.T) {
	if len(uploadPolicies) < 16 {
		t.Fatalf("policy table shrank to %d entries; the v1 matrix expects at least the 16 registered purposes", len(uploadPolicies))
	}
	for key, item := range uploadPolicies {
		t.Run(key, func(t *testing.T) {
			if key == portalLocalReportPurpose {
				assertPortalLocalV1Rejected(t, v1AllowedMedia(item))
				return
			}
			accountCode, known := v1PurposeAccountCodes[item.Namespace]
			if !known {
				t.Fatalf("policy %q uses namespace %q without a mapped application code", key, item.Namespace)
			}
			files := &stubV1FileService{file: v1PolicyReadyFile()}
			handler := newV1PolicyHandler(t, files)
			response := httptest.NewRecorder()
			handler.Upload(response, v1PolicyUploadRequest(t, accountCode, key, "payload", v1AllowedMedia(item), []byte("payload")))
			if response.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body = %s", response.Code, response.Body.String())
			}
			if files.uploaded.Namespace != item.Namespace || files.uploaded.Purpose != item.Purpose {
				t.Fatalf("upload input namespace/purpose = %q/%q, want %q/%q", files.uploaded.Namespace, files.uploaded.Purpose, item.Namespace, item.Purpose)
			}
			if item.Classification != "" && files.uploaded.Classification != item.Classification {
				t.Fatal("purpose classification was downgraded")
			}
			if files.uploaded.PolicyVersion != v2PolicyVersion || files.uploaded.RetentionClass != v1PolicyExpectedRetention(item) {
				t.Fatalf("upload input policy = %q/%q, want %q/%q", files.uploaded.PolicyVersion, files.uploaded.RetentionClass, v2PolicyVersion, v1PolicyExpectedRetention(item))
			}
			if (files.uploaded.RetentionUntil != nil) != item.Temporary {
				t.Fatalf("temporary retention deadline = %v, want temporary=%v", files.uploaded.RetentionUntil, item.Temporary)
			}
		})
	}
}

// TestV1UploadRejectsMediaOutsideEveryPurposePolicy 锁定每个旧用途的媒体拒绝和新用途禁入。
// 参数：t为测试上下文，遍历全部登记用途。
// 返回值：无；非法媒体到达文件服务或新用途可降级时失败。
func TestV1UploadRejectsMediaOutsideEveryPurposePolicy(t *testing.T) {
	for key, item := range uploadPolicies {
		t.Run(key, func(t *testing.T) {
			if key == portalLocalReportPurpose {
				assertPortalLocalV1Rejected(t, v1DisallowedMedia(item))
				return
			}
			accountCode, known := v1PurposeAccountCodes[item.Namespace]
			if !known {
				t.Fatalf("policy %q uses namespace %q without a mapped application code", key, item.Namespace)
			}
			files := &stubV1FileService{file: v1PolicyReadyFile()}
			handler := newV1PolicyHandler(t, files)
			response := httptest.NewRecorder()
			handler.Upload(response, v1PolicyUploadRequest(t, accountCode, key, "payload", v1DisallowedMedia(item), []byte("payload")))
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body = %s", response.Code, response.Body.String())
			}
			if files.uploaded.ApplicationID != "" {
				t.Fatalf("media outside policy must not reach the service layer: %+v", files.uploaded)
			}
		})
	}
}

// TestV1UploadRejectsOversizeForEveryDistinctLimit 锁定每种旧v1文件上限。
// 参数：t为测试上下文；本地报告上限由专用v2矩阵验证。
// 返回值：无；任何既有大小限制放宽时失败。
func TestV1UploadRejectsOversizeForEveryDistinctLimit(t *testing.T) {
	limits := map[uint64]string{}
	for key, item := range uploadPolicies {
		if key == portalLocalReportPurpose {
			continue
		}
		limits[item.MaxBytes] = key
	}
	for maxBytes, key := range limits {
		key, item := key, uploadPolicies[key]
		t.Run(key, func(t *testing.T) {
			accountCode := v1PurposeAccountCodes[item.Namespace]
			files := &stubV1FileService{file: v1PolicyReadyFile()}
			handler := newV1PolicyHandler(t, files)
			response := httptest.NewRecorder()
			handler.Upload(response, v1PolicyUploadRequest(t, accountCode, key, "payload", v1AllowedMedia(item), make([]byte, maxBytes+1)))
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 for %d bytes over the %d limit; body = %s", response.Code, maxBytes+1, maxBytes, response.Body.String())
			}
		})
	}
}

// 跨命名空间 403：应用只能使用自己命名空间下的用途，逐条目锁定。
func TestV1UploadRejectsCrossNamespacePurpose(t *testing.T) {
	for key, item := range uploadPolicies {
		t.Run(key, func(t *testing.T) {
			files := &stubV1FileService{file: v1PolicyReadyFile()}
			handler := newV1PolicyHandler(t, files)
			response := httptest.NewRecorder()
			handler.Upload(response, v1PolicyUploadRequest(t, v1ForeignAccountCode(t, item.Namespace), key, "payload", v1AllowedMedia(item), []byte("payload")))
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", response.Code, response.Body.String())
			}
			if files.uploaded.ApplicationID != "" {
				t.Fatalf("cross-namespace purpose must not reach the service layer: %+v", files.uploaded)
			}
		})
	}
}

// TestV1AndV2UploadsResolveSamePurposePolicy 核对共享用途映射及本地报告v2专有规则。
// 参数：t为测试上下文，通过真实handler和落库SQL比较策略。
// 返回值：无；旧映射变化、专有用途降级或v2不能落库时失败。
func TestV1AndV2UploadsResolveSamePurposePolicy(t *testing.T) {
	v2Handler, script := newActorProbeHandler(t)
	for key, item := range uploadPolicies {
		t.Run(key, func(t *testing.T) {
			accountCode := v1PurposeAccountCodes[item.Namespace]
			sessionRequest := v2PolicySessionRequest(t, accountCode, key, item)
			response := httptest.NewRecorder()
			v2Handler.CreateSession(response, sessionRequest)
			if response.Code != http.StatusCreated {
				t.Fatalf("v2 session status = %d, want 201; body = %s", response.Code, response.Body.String())
			}
			sessions := script.insertInto("file_upload_v2_session")
			if len(sessions) == 0 {
				t.Fatal("v2 CreateSession did not insert an upload session")
			}
			stored := statementArgumentText(sessions[len(sessions)-1])
			if item.Classification != "" && !strings.Contains(stored, item.Classification) {
				t.Fatal("v2 purpose classification was downgraded")
			}
			for _, expected := range []string{item.Namespace, item.Purpose, v2PolicyVersion} {
				if !strings.Contains(stored, expected) {
					t.Fatalf("v2 session lost policy value %q: args = %s", expected, stored)
				}
			}
			if key == portalLocalReportPurpose {
				assertPortalLocalV1Rejected(t, v1AllowedMedia(item))
				for _, expected := range []string{"PORTAL_REPORT_REQUEST", "12", "REPORT"} {
					if !strings.Contains(stored, expected) {
						t.Fatalf("local report v2 binding missing %q: %s", expected, stored)
					}
				}
				return
			}

			files := &stubV1FileService{file: v1PolicyReadyFile()}
			handler := newV1PolicyHandler(t, files)
			v1Response := httptest.NewRecorder()
			handler.Upload(v1Response, v1PolicyUploadRequest(t, accountCode, key, "payload", v1AllowedMedia(item), []byte("payload")))
			if v1Response.Code != http.StatusCreated {
				t.Fatalf("v1 upload status = %d, want 201; body = %s", v1Response.Code, v1Response.Body.String())
			}
			if files.uploaded.Namespace != item.Namespace || files.uploaded.Purpose != item.Purpose || files.uploaded.PolicyVersion != v2PolicyVersion {
				t.Fatalf("v1 input %q/%q/%q disagrees with v2 session %q/%q/%q", files.uploaded.Namespace, files.uploaded.Purpose, files.uploaded.PolicyVersion, item.Namespace, item.Purpose, v2PolicyVersion)
			}
		})
	}
}

// v2PolicySessionRequest 构造该用途的最小合法v2会话声明。
// 参数：t为测试上下文，accountCode为应用，purpose/item为登记策略。
// 返回值：带可信主体的请求，本地报告独立使用机器及申请绑定。
func v2PolicySessionRequest(t *testing.T, accountCode, purpose string, item uploadPolicy) *http.Request {
	t.Helper()
	input := map[string]any{
		"purpose": purpose, "original_name": "payload",
		"media_type": v1AllowedMedia(item), "sha256": strings.Repeat("a", 64), "size_bytes": 1024,
		"resource_type": "CONTRACT", "resource_id": "contract-1", "binding_type": "PRIMARY",
		"idempotency_key": "idem-consistency-" + purpose,
	}
	if purpose == portalLocalReportPurpose {
		input["resource_type"], input["resource_id"], input["binding_type"] = "PORTAL_REPORT_REQUEST", "12", "REPORT"
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal v2 session request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/file-gateway/api/v2/upload-sessions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	principal := v1Principal()
	principal.Account.Code = accountCode
	if purpose == portalLocalReportPurpose {
		principal.User = authctx.ReferenceName{}
	}
	return request.WithContext(authctx.WithPrincipal(request.Context(), principal))
}

// enforceUploadPolicy 是媒体白名单与大小的唯一执行点，逐项锁定边界条件。
func TestEnforceUploadPolicyBoundaries(t *testing.T) {
	selected := policyWithMax("contract", "template", false, 10<<20, "application/pdf")
	header := func(contentType string, size int64) *multipart.FileHeader {
		return &multipart.FileHeader{Filename: "payload.pdf", Size: size, Header: textproto.MIMEHeader{"Content-Type": {contentType}}}
	}
	for _, tt := range []struct {
		name    string
		header  *multipart.FileHeader
		wantErr bool
	}{
		{name: "allowed", header: header("application/pdf", 1024)},
		{name: "media parameters are tolerated", header: header("application/pdf; charset=binary", 1024)},
		{name: "media case is normalized like v2", header: header("Application/PDF", 1024)},
		{name: "media outside whitelist", header: header("text/plain", 1024), wantErr: true},
		{name: "malformed media type", header: header("application", 1024), wantErr: true},
		{name: "empty file", header: header("application/pdf", 0), wantErr: true},
		{name: "exactly at limit", header: header("application/pdf", 10<<20)},
		{name: "one byte over limit", header: header("application/pdf", (10<<20)+1), wantErr: true},
		{name: "missing part", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := enforceUploadPolicy(tt.header, selected)
			if (err != nil) != tt.wantErr {
				t.Fatalf("enforceUploadPolicy error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// 策略表本身必须自洽：每个 purpose 都要有媒体白名单与非零上限，否则“全部 purpose
// 走策略”会退化成放行。
func TestUploadPoliciesDefineMediaAndLimitForEveryPurpose(t *testing.T) {
	if len(uploadPolicies) == 0 {
		t.Fatal("upload policy table must not be empty")
	}
	for key, item := range uploadPolicies {
		if item.Namespace == "" || item.Purpose == "" {
			t.Fatalf("policy %q must define namespace and purpose: %+v", key, item)
		}
		if len(item.Media) == 0 {
			t.Fatalf("policy %q must define a media whitelist", key)
		}
		if item.MaxBytes == 0 {
			t.Fatalf("policy %q must define a size limit", key)
		}
		for media := range item.Media {
			if strings.TrimSpace(media) == "" || media == "*/*" || strings.Contains(media, "*") {
				t.Fatalf("policy %q must not contain wildcard media %q", key, media)
			}
		}
	}
}

// 编译期确认测试替身仍满足处理器依赖契约（v1 服务接口未随本修复变化）。
var _ fileService = (*stubV1FileService)(nil)
var _ io.ReadSeekCloser = (*nopReadSeekCloser)(nil)

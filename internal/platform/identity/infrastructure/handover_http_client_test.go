package infrastructure_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
)

// handoverTestNow 是固定的请求时间戳，用于断言时间戳头格式稳定。
var handoverTestNow = time.Date(2026, 10, 30, 1, 2, 3, 456789000, time.UTC)

// handoverClientFor 构造使用按租户铸造令牌的客户端；tokenTenants 记录每次铸造请求的租户，
// 用于断言"每请求一令牌"和租户来源（请求体 vs 批次上下文）。
func handoverClientFor(t *testing.T, server *httptest.Server, tokenTenants *[]string) *infrastructure.HandoverHTTPClient {
	t.Helper()
	client, err := infrastructure.NewHandoverHTTPClient(infrastructure.HandoverHTTPClientConfig{
		System: "contract", BaseURL: server.URL,
		TokenSource: func(tenantID string) (string, error) {
			if tokenTenants != nil {
				*tokenTenants = append(*tokenTenants, tenantID)
			}
			return "application-jwt-for-" + tenantID, nil
		},
		ApplicationCode: "platform", Environment: "prod",
		Timeout: 5 * time.Second, RequestID: func() (string, error) { return "01REQUESTID0000000000000000", nil },
		Now: func() time.Time { return handoverTestNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestHandoverHTTPClientFreezeMintsPerTenantApplicationToken(t *testing.T) {
	var captured *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(context.Background())
		_ = json.NewEncoder(w).Encode(map[string]any{"system": "contract", "frozen": true, "responsibility_count": 2})
	}))
	defer server.Close()

	var tokenTenants []string
	response, err := handoverClientFor(t, server, &tokenTenants).Freeze(context.Background(), application.HandoverFreezeRequest{
		TenantID: "tenant-1", BatchID: "batch-1", RequestID: "request-1", UserID: "user-1", RequestedAt: handoverTestNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured.URL.Path != "/internal/v1/handover/freeze" || captured.Method != http.MethodPost {
		t.Fatalf("request = %s %s", captured.Method, captured.URL.Path)
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer application-jwt-for-tenant-1" {
		t.Fatalf("authorization header = %q", got)
	}
	if len(tokenTenants) != 1 || tokenTenants[0] != "tenant-1" {
		t.Fatalf("token tenants = %v, want the batch tenant", tokenTenants)
	}
	if captured.Header.Get("X-Tenant-Id") != "tenant-1" || captured.Header.Get("X-Application-Code") != "platform" || captured.Header.Get("X-Environment") != "prod" {
		t.Fatalf("machine scope headers missing: %v", captured.Header)
	}
	if captured.Header.Get("X-Integration-Timestamp") != handoverTestNow.Format(time.RFC3339Nano) {
		t.Fatalf("integration timestamp = %q", captured.Header.Get("X-Integration-Timestamp"))
	}
	nonce := captured.Header.Get("X-Integration-Nonce")
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(nonce)
	if decodeErr != nil || len(decoded) != 32 {
		t.Fatalf("integration nonce = %q (decoded %d bytes, err %v)", nonce, len(decoded), decodeErr)
	}
	if captured.Header.Get("X-Request-Id") != "request-1" {
		t.Fatalf("anti-replay request id = %q", captured.Header.Get("X-Request-Id"))
	}
	if !response.Frozen || response.System != "contract" || response.ResponsibilityCount != 2 {
		t.Fatalf("freeze response = %+v", response)
	}
}

func TestHandoverHTTPClientTransferCarriesIdempotencyAndVersion(t *testing.T) {
	var body map[string]any
	var idempotency, authorization string
	var tokenTenants []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idempotency = r.Header.Get("Idempotency-Key")
		authorization = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"item_id": "item-1", "idempotency_key": "handover:batch-1:item-1", "status": "APPLIED", "applied_version": 9})
	}))
	defer server.Close()

	receipt, err := handoverClientFor(t, server, &tokenTenants).Transfer(context.Background(), application.HandoverTransferRequest{
		TenantID: "tenant-9", BatchID: "batch-1", ItemID: "item-1", RequestID: "request-1", System: "contract",
		ResourceType: "contract", ResourceID: "c-1", FromUserID: "user-1", ToUserID: "user-2",
		ExpectedVersion: 7, IdempotencyKey: "handover:batch-1:item-1", LeaseID: "lease-1", Deadline: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if idempotency != "handover:batch-1:item-1" {
		t.Fatalf("idempotency header = %q", idempotency)
	}
	// 转移请求的租户来自请求体，客户端必须据此铸造令牌（而不是使用上一个请求的租户）。
	if authorization != "Bearer application-jwt-for-tenant-9" || len(tokenTenants) != 1 || tokenTenants[0] != "tenant-9" {
		t.Fatalf("authorization = %q, token tenants = %v", authorization, tokenTenants)
	}
	if body["expected_version"] != float64(7) || body["lease_id"] != "lease-1" || body["to_user_id"] != "user-2" {
		t.Fatalf("transfer body = %v", body)
	}
	if receipt.Status != application.HandoverReceiptApplied || receipt.AppliedVersion != 9 || receipt.System != "contract" {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestHandoverHTTPClientResponsibilitiesAndReceiptUseContextTenant(t *testing.T) {
	var queries, paths, authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		paths = append(paths, r.URL.Path)
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		if strings.HasPrefix(r.URL.Path, "/internal/v1/handover/transfers/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"item_id": "item-1", "status": "ALREADY_APPLIED", "applied_version": 4})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"system": "contract", "next_cursor": "c2", "items": []map[string]any{{"resource_type": "contract", "resource_id": "c-1", "owner_user_id": "user-1", "category": "OWNER", "version": 3}}})
	}))
	defer server.Close()
	client := handoverClientFor(t, server, nil)
	// 编排器在调用发现/回执前把批次租户写入上下文；客户端不接受其它租户来源。
	ctx := application.WithHandoverTenant(context.Background(), "tenant-ctx")

	page, err := client.Responsibilities(ctx, "request-1", "user-leaver", "c1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ResourceID != "c-1" || page.NextCursor != "c2" || page.Items[0].System != "contract" {
		t.Fatalf("page = %+v", page)
	}
	if paths[0] != "/internal/v1/handover/responsibilities" {
		t.Fatalf("responsibilities path = %q", paths[0])
	}
	// 四端 responsibilities 端点要求 user_id=<离职人>；request_id/cursor/limit 同时保留。
	if !strings.Contains(queries[0], "request_id=request-1") || !strings.Contains(queries[0], "cursor=c1") || !strings.Contains(queries[0], "limit=50") {
		t.Fatalf("responsibilities query = %q", queries[0])
	}
	if !strings.Contains(queries[0], "user_id=user-leaver") {
		t.Fatalf("responsibilities query must carry the leaver user id, query = %q", queries[0])
	}
	receipt, err := client.Receipt(ctx, "item-1", "handover:batch-1:item-1")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != application.HandoverReceiptAlreadyApplied || receipt.AppliedVersion != 4 {
		t.Fatalf("receipt = %+v", receipt)
	}
	if paths[1] != "/internal/v1/handover/transfers/item-1" || !strings.Contains(queries[1], "idempotency_key=") {
		t.Fatalf("receipt path/query = %s %s", paths[1], queries[1])
	}
	for _, authorization := range authorizations {
		if authorization != "Bearer application-jwt-for-tenant-ctx" {
			t.Fatalf("authorization = %q, want the batch tenant token", authorization)
		}
	}
}

// TestHandoverHTTPClientResponsibilitiesRequiresLeaverUserID 钉住四端契约：合同、CRM、项目、
// 结算的 responsibilities 端点都以 user_id 为必填查询参数，缺失即 422。空 userID 因此在本地
// 失败关闭：不铸造令牌、不发请求，避免整批 discovery 被标 BLOCKED。
func TestHandoverHTTPClientResponsibilitiesRequiresLeaverUserID(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	}))
	defer server.Close()
	var tokenTenants []string
	client := handoverClientFor(t, server, &tokenTenants)
	ctx := application.WithHandoverTenant(context.Background(), "tenant-ctx")

	for _, userID := range []string{"", "   "} {
		if _, err := client.Responsibilities(ctx, "request-1", userID, "", 100); !errors.Is(err, application.ErrValidation) {
			t.Fatalf("userID %q: err = %v, want a validation error", userID, err)
		}
	}
	if requests != 0 {
		t.Fatalf("responsibilities without a leaver user id must not reach the remote system, got %d requests", requests)
	}
	if len(tokenTenants) != 0 {
		t.Fatalf("responsibilities without a leaver user id must not mint a token, got %v", tokenTenants)
	}
}

func TestHandoverHTTPClientFinishRequiresContextTenantAndUsesV1Path(t *testing.T) {
	var path string
	var body map[string]any
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		authorization = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := handoverClientFor(t, server, nil)

	if err := client.Finish(context.Background(), "batch-1", "request-1", "COMPLETED"); err == nil {
		t.Fatal("finish without a batch tenant must fail closed")
	}
	if path != "" {
		t.Fatalf("finish without tenant must not reach the remote system, path = %q", path)
	}
	if err := client.Finish(application.WithHandoverTenant(context.Background(), "tenant-ctx"), "batch-1", "request-1", "COMPLETED"); err != nil {
		t.Fatal(err)
	}
	if path != "/internal/v1/handover/complete" || body["tenant_id"] != "tenant-ctx" || body["outcome"] != "COMPLETED" {
		t.Fatalf("finish path=%q body=%v", path, body)
	}
	if authorization != "Bearer application-jwt-for-tenant-ctx" {
		t.Fatalf("authorization = %q", authorization)
	}
}

func TestHandoverHTTPClientClassifiesFailures(t *testing.T) {
	cases := map[string]struct {
		status      int
		payload     string
		wantCode    string
		endpointErr bool
	}{
		"license-denied": {status: http.StatusForbidden, payload: `{"code":"LICENSE_DENIED"}`, wantCode: "LICENSE_DENIED"},
		"incompatible":   {status: http.StatusNotFound, payload: `{"code":"NOT_FOUND"}`, wantCode: "NOT_FOUND"},
		"server-error":   {status: http.StatusInternalServerError, payload: `{"code":"BOOM"}`, endpointErr: true},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.payload))
			}))
			defer server.Close()
			_, err := handoverClientFor(t, server, nil).Freeze(context.Background(), application.HandoverFreezeRequest{TenantID: "tenant-1", BatchID: "b", RequestID: "r", UserID: "u"})
			if err == nil {
				t.Fatal("failure must be reported")
			}
			var endpoint *infrastructure.HandoverEndpointError
			var protocol *infrastructure.HandoverProtocolError
			if testCase.endpointErr {
				if !errors.As(err, &endpoint) {
					t.Fatalf("5xx must be an endpoint error, got %v", err)
				}
			} else {
				if !errors.As(err, &protocol) || protocol.Code != testCase.wantCode {
					t.Fatalf("error = %v, want protocol code %s", err, testCase.wantCode)
				}
			}
			if strings.Contains(err.Error(), "application-jwt") {
				t.Fatalf("error leaked the application token: %v", err)
			}
		})
	}
}

// TestHandoverHTTPClientTokenFailureDoesNotSendRequest 钉住"铸造失败即不发送请求"：
// 凭据不可用不等于业务失败，更不能带着空/旧令牌打到对端。
func TestHandoverHTTPClientTokenFailureDoesNotSendRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{"frozen": true})
	}))
	defer server.Close()
	client, err := infrastructure.NewHandoverHTTPClient(infrastructure.HandoverHTTPClientConfig{
		System: "contract", BaseURL: server.URL,
		TokenSource: func(string) (string, error) { return "", errors.New("signing key unavailable") },
		Timeout:     5 * time.Second, RequestID: func() (string, error) { return "01REQUESTID0000000000000000", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Freeze(context.Background(), application.HandoverFreezeRequest{TenantID: "tenant-1", BatchID: "b", RequestID: "r", UserID: "u"})
	var endpoint *infrastructure.HandoverEndpointError
	if !errors.As(err, &endpoint) {
		t.Fatalf("err = %v, want an endpoint error", err)
	}
	if requests != 0 {
		t.Fatalf("token failure must not send a request, got %d", requests)
	}
	if strings.Contains(err.Error(), "signing key unavailable") {
		t.Fatalf("error leaked the token source detail: %v", err)
	}
}

func TestHandoverHTTPClientRequiresTokenSource(t *testing.T) {
	if _, err := infrastructure.NewHandoverHTTPClient(infrastructure.HandoverHTTPClientConfig{
		System: "contract", BaseURL: "https://contract.internal",
		RequestID: func() (string, error) { return "01REQUESTID0000000000000000", nil },
	}); err == nil {
		t.Fatal("client without a token source must be rejected")
	}
}

func TestHandoverHTTPClientTreatsUnparseableResponseAsIncompatible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer server.Close()
	_, err := handoverClientFor(t, server, nil).Freeze(context.Background(), application.HandoverFreezeRequest{TenantID: "tenant-1", BatchID: "b", RequestID: "r", UserID: "u"})
	var protocol *infrastructure.HandoverProtocolError
	if !errors.As(err, &protocol) || protocol.Code != "INCOMPATIBLE_RESPONSE" {
		t.Fatalf("err = %v, want INCOMPATIBLE_RESPONSE", err)
	}
}

func TestHandoverHTTPClientTransportFailureDoesNotLeakCredential(t *testing.T) {
	client, err := infrastructure.NewHandoverHTTPClient(infrastructure.HandoverHTTPClientConfig{
		System: "contract", BaseURL: "http://127.0.0.1:1",
		TokenSource: func(tenantID string) (string, error) { return "application-jwt-for-" + tenantID, nil },
		Timeout:     5 * time.Second, RequestID: func() (string, error) { return "01REQUESTID0000000000000000", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Freeze(context.Background(), application.HandoverFreezeRequest{TenantID: "tenant-1", BatchID: "b", RequestID: "r", UserID: "u"})
	var endpoint *infrastructure.HandoverEndpointError
	if !errors.As(err, &endpoint) {
		t.Fatalf("err = %v, want an endpoint error", err)
	}
	if strings.Contains(err.Error(), "application-jwt") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("transport error leaked details: %v", err)
	}
}

// TestHandoverHTTPClientDecodesEnvelopedAndBarePayloads 覆盖两种真实响应形态：
// 业务系统既可能返回 {code,message,data} 信封，也可能直接返回 DTO。
func TestHandoverHTTPClientDecodesEnvelopedAndBarePayloads(t *testing.T) {
	cases := map[string]string{
		"enveloped": `{"code":"OK","message":"冻结已确认","data":{"system":"contract","frozen":true,"responsibility_count":2}}`,
		"bare":      `{"system":"contract","frozen":true,"responsibility_count":2}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			response, err := handoverClientFor(t, server, nil).Freeze(context.Background(), application.HandoverFreezeRequest{
				TenantID: "tenant-1", BatchID: "batch-1", RequestID: "request-1", UserID: "user-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			if !response.Frozen || response.System != "contract" || response.ResponsibilityCount != 2 {
				t.Fatalf("decoded freeze response = %+v, want frozen with count 2", response)
			}
		})
	}
}

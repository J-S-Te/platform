package infrastructure_test

import (
	"context"
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

const handoverMachineToken = "machine-token-must-not-leak"

func handoverClientFor(t *testing.T, server *httptest.Server) *infrastructure.HandoverHTTPClient {
	t.Helper()
	client, err := infrastructure.NewHandoverHTTPClient(infrastructure.HandoverHTTPClientConfig{
		System: "contract", BaseURL: server.URL, MachineToken: handoverMachineToken,
		TenantID: "tenant-1", ApplicationCode: "contract_management", Environment: "prod",
		Timeout: 5 * time.Second, RequestID: func() (string, error) { return "01REQUESTID0000000000000000", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestHandoverHTTPClientFreezeSendsMachineProtocolHeaders(t *testing.T) {
	var captured *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(context.Background())
		_ = json.NewEncoder(w).Encode(map[string]any{"system": "contract", "frozen": true, "responsibility_count": 2})
	}))
	defer server.Close()

	response, err := handoverClientFor(t, server).Freeze(context.Background(), application.HandoverFreezeRequest{
		TenantID: "tenant-1", BatchID: "batch-1", RequestID: "request-1", UserID: "user-1", RequestedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured.URL.Path != "/internal/handover/freeze" || captured.Method != http.MethodPost {
		t.Fatalf("request = %s %s", captured.Method, captured.URL.Path)
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer "+handoverMachineToken {
		t.Fatalf("authorization header = %q", got)
	}
	if captured.Header.Get("X-Tenant-Id") != "tenant-1" || captured.Header.Get("X-Application-Code") != "contract_management" || captured.Header.Get("X-Environment") != "prod" {
		t.Fatalf("machine scope headers missing: %v", captured.Header)
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
	var idempotency string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idempotency = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"item_id": "item-1", "idempotency_key": "handover:batch-1:item-1", "status": "APPLIED", "applied_version": 9})
	}))
	defer server.Close()

	receipt, err := handoverClientFor(t, server).Transfer(context.Background(), application.HandoverTransferRequest{
		TenantID: "tenant-1", BatchID: "batch-1", ItemID: "item-1", RequestID: "request-1", System: "contract",
		ResourceType: "contract", ResourceID: "c-1", FromUserID: "user-1", ToUserID: "user-2",
		ExpectedVersion: 7, IdempotencyKey: "handover:batch-1:item-1", LeaseID: "lease-1", Deadline: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if idempotency != "handover:batch-1:item-1" {
		t.Fatalf("idempotency header = %q", idempotency)
	}
	if body["expected_version"] != float64(7) || body["lease_id"] != "lease-1" || body["to_user_id"] != "user-2" {
		t.Fatalf("transfer body = %v", body)
	}
	if receipt.Status != application.HandoverReceiptApplied || receipt.AppliedVersion != 9 || receipt.System != "contract" {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestHandoverHTTPClientResponsibilitiesAndReceiptPaths(t *testing.T) {
	var queries []string
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		paths = append(paths, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/internal/handover/transfers/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"item_id": "item-1", "status": "ALREADY_APPLIED", "applied_version": 4})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"system": "contract", "next_cursor": "c2", "items": []map[string]any{{"resource_type": "contract", "resource_id": "c-1", "owner_user_id": "user-1", "category": "OWNER", "version": 3}}})
	}))
	defer server.Close()
	client := handoverClientFor(t, server)

	page, err := client.Responsibilities(context.Background(), "request-1", "c1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ResourceID != "c-1" || page.NextCursor != "c2" || page.Items[0].System != "contract" {
		t.Fatalf("page = %+v", page)
	}
	if !strings.Contains(queries[0], "request_id=request-1") || !strings.Contains(queries[0], "cursor=c1") || !strings.Contains(queries[0], "limit=50") {
		t.Fatalf("responsibilities query = %q", queries[0])
	}
	receipt, err := client.Receipt(context.Background(), "item-1", "handover:batch-1:item-1")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != application.HandoverReceiptAlreadyApplied || receipt.AppliedVersion != 4 {
		t.Fatalf("receipt = %+v", receipt)
	}
	if paths[1] != "/internal/handover/transfers/item-1" || !strings.Contains(queries[1], "idempotency_key=") {
		t.Fatalf("receipt path/query = %s %s", paths[1], queries[1])
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
			_, err := handoverClientFor(t, server).Freeze(context.Background(), application.HandoverFreezeRequest{TenantID: "tenant-1", BatchID: "b", RequestID: "r", UserID: "u"})
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
			if strings.Contains(err.Error(), handoverMachineToken) {
				t.Fatalf("error leaked the machine credential: %v", err)
			}
		})
	}
}

func TestHandoverHTTPClientTreatsUnparseableResponseAsIncompatible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer server.Close()
	_, err := handoverClientFor(t, server).Freeze(context.Background(), application.HandoverFreezeRequest{TenantID: "tenant-1", BatchID: "b", RequestID: "r", UserID: "u"})
	var protocol *infrastructure.HandoverProtocolError
	if !errors.As(err, &protocol) || protocol.Code != "INCOMPATIBLE_RESPONSE" {
		t.Fatalf("err = %v, want INCOMPATIBLE_RESPONSE", err)
	}
}

func TestHandoverHTTPClientTransportFailureDoesNotLeakCredential(t *testing.T) {
	client, err := infrastructure.NewHandoverHTTPClient(infrastructure.HandoverHTTPClientConfig{
		System: "contract", BaseURL: "http://127.0.0.1:1", MachineToken: handoverMachineToken,
		TenantID: "tenant-1", RequestID: func() (string, error) { return "01REQUESTID0000000000000000", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Freeze(context.Background(), application.HandoverFreezeRequest{TenantID: "tenant-1", BatchID: "b", RequestID: "r", UserID: "u"})
	var endpoint *infrastructure.HandoverEndpointError
	if !errors.As(err, &endpoint) {
		t.Fatalf("err = %v, want an endpoint error", err)
	}
	if strings.Contains(err.Error(), handoverMachineToken) || strings.Contains(err.Error(), "127.0.0.1") {
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
			response, err := handoverClientFor(t, server).Freeze(context.Background(), application.HandoverFreezeRequest{
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

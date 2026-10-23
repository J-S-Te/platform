package dictionaryhttp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dictionaryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/application"
	dictionarydomain "github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// dictionaryServiceStub 记录调用参数并返回预设结果；未覆写的行为保持零值，避免用例走过未桩接的路径。
type dictionaryServiceStub struct {
	createErr   error
	activeErr   error
	tenantSeen  string
	codeSeen    string
	createdSeen dictionaryapplication.DictionaryCreateInput
	activeItems dictionaryapplication.PageResult[dictionarydomain.Item]
}

func (stub *dictionaryServiceStub) ListDictionaries(context.Context, string, dictionaryapplication.PageRequest) (dictionaryapplication.PageResult[dictionarydomain.Dictionary], error) {
	return dictionaryapplication.PageResult[dictionarydomain.Dictionary]{}, nil
}

func (stub *dictionaryServiceStub) CreateDictionary(_ context.Context, input dictionaryapplication.DictionaryCreateInput) (dictionarydomain.Dictionary, error) {
	stub.createdSeen = input
	if stub.createErr != nil {
		return dictionarydomain.Dictionary{}, stub.createErr
	}
	return dictionarydomain.Dictionary{ID: "dict-1", TenantID: input.TenantID, Code: input.Code, Name: input.Name, Version: 1}, nil
}

func (stub *dictionaryServiceStub) GetDictionary(context.Context, string, string) (dictionarydomain.Dictionary, error) {
	return dictionarydomain.Dictionary{}, nil
}

func (stub *dictionaryServiceStub) UpdateDictionary(context.Context, dictionaryapplication.DictionaryUpdateInput) (dictionarydomain.Dictionary, error) {
	return dictionarydomain.Dictionary{}, nil
}

func (stub *dictionaryServiceStub) ListItems(context.Context, string, string, dictionaryapplication.PageRequest) (dictionaryapplication.PageResult[dictionarydomain.Item], error) {
	return dictionaryapplication.PageResult[dictionarydomain.Item]{}, nil
}

func (stub *dictionaryServiceStub) CreateItem(context.Context, dictionaryapplication.ItemCreateInput) (dictionarydomain.Item, error) {
	return dictionarydomain.Item{}, nil
}

func (stub *dictionaryServiceStub) UpdateItem(context.Context, dictionaryapplication.ItemUpdateInput) (dictionarydomain.Item, error) {
	return dictionarydomain.Item{}, nil
}

func (stub *dictionaryServiceStub) DeleteDictionary(context.Context, dictionaryapplication.DictionaryDeleteInput) (int64, error) {
	return 0, nil
}

func (stub *dictionaryServiceStub) DeleteItem(context.Context, dictionaryapplication.ItemDeleteInput) error {
	return nil
}

func (stub *dictionaryServiceStub) ListActiveItemsByCode(_ context.Context, tenantID, code string, _ dictionaryapplication.PageRequest) (dictionaryapplication.PageResult[dictionarydomain.Item], error) {
	stub.tenantSeen, stub.codeSeen = tenantID, code
	if stub.activeErr != nil {
		return dictionaryapplication.PageResult[dictionarydomain.Item]{}, stub.activeErr
	}
	return stub.activeItems, nil
}

func dictionaryHandlerFor(t *testing.T, stub *dictionaryServiceStub) *Handler {
	t.Helper()
	handler, err := NewHandler(stub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func dictionaryRequest(t *testing.T, method, target, body string, principal *authctx.Principal) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if principal != nil {
		request = request.WithContext(authctx.WithPrincipal(request.Context(), *principal))
	}
	return request
}

func dictionaryPrincipal() authctx.Principal {
	return authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "admin-1"}}
}

// 未认证请求必须 401，且绝不触达业务服务。
func TestDictionaryEndpointsRequirePrincipal(t *testing.T) {
	stub := &dictionaryServiceStub{}
	handler := dictionaryHandlerFor(t, stub)
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"list":        handler.ListDictionaries,
		"create":      handler.CreateDictionary,
		"get":         handler.GetDictionary,
		"activeItems": handler.ListActiveItemsByCode,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			call(response, dictionaryRequest(t, http.MethodGet, "/dictionaries", `{}`, nil))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
		})
	}
	if stub.tenantSeen != "" || stub.createdSeen.Code != "" {
		t.Fatal("unauthenticated request must not reach the service")
	}
}

// 领域错误必须映射为确定性状态码（基础管理验收要求可预期的失败语义）。
func TestDictionaryCreateMapsDomainErrors(t *testing.T) {
	cases := map[string]struct {
		err  error
		want int
	}{
		"validation":       {dictionaryapplication.ErrValidation, http.StatusUnprocessableEntity},
		"conflict":         {dictionaryapplication.ErrConflict, http.StatusConflict},
		"version conflict": {dictionaryapplication.ErrVersionConflict, http.StatusConflict},
		"internal":         {errors.New("database down"), http.StatusInternalServerError},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &dictionaryServiceStub{createErr: testCase.err}
			handler := dictionaryHandlerFor(t, stub)
			principal := dictionaryPrincipal()
			response := httptest.NewRecorder()
			handler.CreateDictionary(response, dictionaryRequest(t, http.MethodPost, "/dictionaries",
				`{"code":"ORDER_STATUS","name":"订单状态","status":"ACTIVE"}`, &principal))
			if response.Code != testCase.want {
				t.Fatalf("status = %d, want %d (%s)", response.Code, testCase.want, response.Body.String())
			}
		})
	}
}

// 成功路径必须把租户与操作人取自认证主体。
func TestDictionaryCreateBindsTenantFromPrincipal(t *testing.T) {
	stub := &dictionaryServiceStub{}
	handler := dictionaryHandlerFor(t, stub)
	principal := dictionaryPrincipal()
	response := httptest.NewRecorder()
	handler.CreateDictionary(response, dictionaryRequest(t, http.MethodPost, "/dictionaries",
		`{"code":"ORDER_STATUS","name":"订单状态","status":"ACTIVE"}`, &principal))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", response.Code, response.Body.String())
	}
	if stub.createdSeen.TenantID != "tenant-1" || stub.createdSeen.OperatorID != "admin-1" {
		t.Fatalf("service input = %+v, want tenant/operator from the principal", stub.createdSeen)
	}
}

// 请求体自带 tenant_id 必须被拒绝（未知字段），不得被静默忽略后按请求体执行。
func TestDictionaryCreateRejectsBodySuppliedTenant(t *testing.T) {
	stub := &dictionaryServiceStub{}
	handler := dictionaryHandlerFor(t, stub)
	principal := dictionaryPrincipal()
	response := httptest.NewRecorder()
	handler.CreateDictionary(response, dictionaryRequest(t, http.MethodPost, "/dictionaries",
		`{"code":"ORDER_STATUS","name":"订单状态","status":"ACTIVE","tenant_id":"tenant-attacker"}`, &principal))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for a body-supplied tenant (%s)", response.Code, response.Body.String())
	}
	if stub.createdSeen.Code != "" {
		t.Fatal("rejected request must not reach the service")
	}
}

// 路径参数与租户必须正确传给服务，并按启用状态查询字典项。
func TestDictionaryActiveItemsUsePathCodeAndTenant(t *testing.T) {
	stub := &dictionaryServiceStub{}
	handler := dictionaryHandlerFor(t, stub)
	principal := dictionaryPrincipal()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dictionaries/code/{dictionary_code}/items", handler.ListActiveItemsByCode)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, dictionaryRequest(t, http.MethodGet, "/dictionaries/code/ORDER_STATUS/items", "", &principal))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", response.Code, response.Body.String())
	}
	if stub.tenantSeen != "tenant-1" || stub.codeSeen != "ORDER_STATUS" {
		t.Fatalf("service saw tenant=%q code=%q", stub.tenantSeen, stub.codeSeen)
	}
}

// 超大请求体必须被拒绝（处理器有 32KiB 上限），不得进入服务层。
func TestDictionaryCreateRejectsOversizedBody(t *testing.T) {
	stub := &dictionaryServiceStub{}
	handler := dictionaryHandlerFor(t, stub)
	principal := dictionaryPrincipal()
	oversized := `{"code":"BIG","name":"` + strings.Repeat("A", 40*1024) + `","status":"ACTIVE"}`
	response := httptest.NewRecorder()
	handler.CreateDictionary(response, dictionaryRequest(t, http.MethodPost, "/dictionaries", oversized, &principal))
	if response.Code < 400 {
		t.Fatalf("oversized body accepted: status = %d", response.Code)
	}
	if stub.createdSeen.Code != "" {
		t.Fatal("oversized request must not reach the service")
	}
}

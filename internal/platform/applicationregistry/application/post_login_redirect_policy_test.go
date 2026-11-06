package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	policyTestTenantID      = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	policyTestApplicationID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	policyTestEnvironmentID = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	policyTestTargetID      = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	policyTestOperatorID    = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
)

func TestPostLoginRedirectPolicyRequiresHTTPSAndAllowlistedHost(t *testing.T) {
	policy := NewPostLoginRedirectPolicy("https://portal.example.com")
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "allowlisted https", value: "https://portal.example.com/portal", want: true},
		{name: "allowlisted https with port", value: "https://portal.example.com:8443/portal", want: true},
		{name: "host comparison ignores case", value: "https://PORTAL.Example.COM/portal", want: true},
		{name: "unlisted https host", value: "https://evil.example.net/portal", want: false},
		{name: "allowlisted host over plaintext http", value: "http://portal.example.com/portal", want: false},
		{name: "loopback ipv4 http", value: "http://127.0.0.1:8081/portal", want: true},
		{name: "loopback localhost http", value: "http://localhost:8081/portal", want: true},
		{name: "loopback ipv6 http", value: "http://[::1]:8081/portal", want: true},
		{name: "private network http", value: "http://192.168.1.20:8081/portal", want: false},
		{name: "userinfo rejected", value: "https://portal.example.com@evil.example.net/", want: false},
		{name: "relative path is not absolute", value: "/portal", want: false},
		{name: "scheme relative rejected", value: "//portal.example.com/portal", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := policy.ValidAbsoluteRedirect(test.value); got != test.want {
				t.Fatalf("ValidAbsoluteRedirect(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

// 零值策略未接线主机集合时保留既有结构校验口径：装配层始终注入真实集合，零值
// 只服务于未接线的嵌入调用与单测，不能在此处静默收紧成"全部拒绝"。
func TestPostLoginRedirectPolicyWithoutAllowlistKeepsStructuralRules(t *testing.T) {
	policy := PostLoginRedirectPolicy{}
	if policy.Enabled() {
		t.Fatal("zero value policy must report itself as not configured")
	}
	if !policy.ValidAbsoluteRedirect("https://portal.example.com/portal") {
		t.Fatal("https must stay acceptable without a configured host set")
	}
	if !policy.ValidAbsoluteRedirect("http://portal.example.com/portal") {
		t.Fatal("plaintext http keeps the legacy structural rules without a configured host set")
	}
	if policy.ValidAbsoluteRedirect("//portal.example.com/portal") || policy.ValidAbsoluteRedirect("/portal") {
		t.Fatal("relative and scheme-relative values must never be accepted as absolute redirects")
	}
	if policy.ValidAbsoluteRedirect("ftp://portal.example.com/portal") {
		t.Fatal("non http(s) schemes must be rejected")
	}
}

// 明文本地/局域网部署必须由运维把该 origin 显式写成 http:// 才放行，裸主机名或
// https 条目不会顺带批准明文。
func TestPostLoginRedirectPolicyRequiresExplicitPlaintextOrigin(t *testing.T) {
	policy := NewPostLoginRedirectPolicy("http://192.168.1.20:8081")
	if !policy.ValidAbsoluteRedirect("http://192.168.1.20:8081/portal") {
		t.Fatal("explicitly configured plaintext origin must be allowed")
	}
	if policy.ValidAbsoluteRedirect("http://192.168.1.21:8081/portal") {
		t.Fatal("plaintext origin must not authorize other hosts")
	}
	if !policy.ValidAbsoluteRedirect("https://192.168.1.20/portal") {
		t.Fatal("host allowlist is scheme independent for https")
	}

	hostOnly := NewPostLoginRedirectPolicy("192.168.1.20")
	if hostOnly.ValidAbsoluteRedirect("http://192.168.1.20:8081/portal") {
		t.Fatal("bare host entries must not approve plaintext by implication")
	}
}

func TestNewPostLoginRedirectPolicyIgnoresMalformedEntries(t *testing.T) {
	policy := NewPostLoginRedirectPolicy("https://portal.example.com", "portal.example.com:8443", "not a host", "", "/relative")
	if !policy.AllowsHost("portal.example.com") {
		t.Fatal("origin entry host must be collected")
	}
	if !policy.AllowsHost("PORTAL.EXAMPLE.COM") {
		t.Fatal("host comparison must ignore case")
	}
	if policy.AllowsHost("not a host") {
		t.Fatal("malformed entries must not become allowed hosts")
	}
}

func TestResolveActiveTargetURIEnforcesRedirectAllowlist(t *testing.T) {
	policy := NewPostLoginRedirectPolicy("https://portal.example.com")
	prefix := "/contract"
	tests := []struct {
		name       string
		targetURI  string
		baseURL    string
		pathPrefix *string
		want       string
		wantErr    error
	}{
		{
			name:      "absolute target on allowlisted host",
			targetURI: "https://portal.example.com/dashboard",
			want:      "https://portal.example.com/dashboard",
		},
		{
			name:      "absolute target on unlisted https host",
			targetURI: "https://evil.example.net/dashboard",
			wantErr:   ErrNotFound,
		},
		{
			name:      "absolute plaintext target on unlisted host",
			targetURI: "http://contracts.example.com/dashboard",
			wantErr:   ErrNotFound,
		},
		{
			name:       "relative target joined with allowlisted base",
			targetURI:  "/dashboard",
			baseURL:    "https://portal.example.com",
			pathPrefix: &prefix,
			want:       "https://portal.example.com/contract/dashboard",
		},
		{
			name:       "relative target joined with unlisted https base",
			targetURI:  "/dashboard",
			baseURL:    "https://evil.example.net",
			pathPrefix: &prefix,
			wantErr:    ErrNotFound,
		},
		{
			name:       "relative target joined with non loopback http base",
			targetURI:  "/dashboard",
			baseURL:    "http://portal.internal",
			pathPrefix: &prefix,
			wantErr:    ErrNotFound,
		},
		{
			name:      "relative target joined with loopback http base",
			targetURI: "/dashboard",
			baseURL:   "http://localhost:8081",
			want:      "http://localhost:8081/dashboard",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseURL := test.baseURL
			service, err := NewLoginTargetService(
				loginTargetRepositoryStub{
					target: LoginTarget{
						ID:            policyTestTargetID,
						TenantID:      policyTestTenantID,
						ApplicationID: policyTestApplicationID,
						EnvironmentID: policyTestEnvironmentID,
						TargetCode:    "dashboard",
						TargetURI:     test.targetURI,
						Status:        loginTargetStatusActive,
					},
					environment: Environment{
						ID:            policyTestEnvironmentID,
						TenantID:      policyTestTenantID,
						ApplicationID: policyTestApplicationID,
						Environment:   "prod",
						BaseURL:       &baseURL,
						PathPrefix:    test.pathPrefix,
						Status:        "ACTIVE",
					},
				},
				WithLoginTargetRedirectPolicy(policy),
			)
			if err != nil {
				t.Fatalf("NewLoginTargetService() error = %v", err)
			}

			got, err := service.ResolveActiveTargetURI(context.Background(), LoginTargetResolveInput{
				TenantID:      policyTestTenantID,
				ApplicationID: policyTestApplicationID,
				EnvironmentID: policyTestEnvironmentID,
				TargetCode:    "dashboard",
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("ResolveActiveTargetURI() error = %v, want %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("ResolveActiveTargetURI() = %q, want %q", got, test.want)
			}
		})
	}
}

// 隔离网络明文本地部署：公开基址必须以 http:// 显式列入来源集合才可用于跳转。
func TestResolveActiveTargetURIAllowsExplicitPlaintextOrigin(t *testing.T) {
	policy := NewPostLoginRedirectPolicy("http://192.168.1.20:8081")
	prefix := "/contract"
	baseURL := "http://192.168.1.20:8081"
	service, err := NewLoginTargetService(
		loginTargetRepositoryStub{
			target: LoginTarget{
				ID:            policyTestTargetID,
				TenantID:      policyTestTenantID,
				ApplicationID: policyTestApplicationID,
				EnvironmentID: policyTestEnvironmentID,
				TargetCode:    "dashboard",
				TargetURI:     "/dashboard",
				Status:        loginTargetStatusActive,
			},
			environment: Environment{
				ID:            policyTestEnvironmentID,
				TenantID:      policyTestTenantID,
				ApplicationID: policyTestApplicationID,
				Environment:   "prod",
				BaseURL:       &baseURL,
				PathPrefix:    &prefix,
				Status:        "ACTIVE",
			},
		},
		WithLoginTargetRedirectPolicy(policy),
	)
	if err != nil {
		t.Fatalf("NewLoginTargetService() error = %v", err)
	}
	got, err := service.ResolveActiveTargetURI(context.Background(), LoginTargetResolveInput{
		TenantID:      policyTestTenantID,
		ApplicationID: policyTestApplicationID,
		EnvironmentID: policyTestEnvironmentID,
		TargetCode:    "dashboard",
	})
	if err != nil {
		t.Fatalf("ResolveActiveTargetURI() error = %v", err)
	}
	if want := "http://192.168.1.20:8081/contract/dashboard"; got != want {
		t.Fatalf("resolved URI = %q, want %q", got, want)
	}
}

type loginTargetManagementRepositoryStub struct {
	created []LoginTargetCreateInput
	updated []LoginTargetUpdateInput
}

func (stub *loginTargetManagementRepositoryStub) EnsureLoginTargetBoundary(context.Context, string, string, string) error {
	return nil
}

func (stub *loginTargetManagementRepositoryStub) ListLoginTargets(context.Context, string, string, string, PageRequest) (PageResult[LoginTargetManagementItem], error) {
	return PageResult[LoginTargetManagementItem]{}, nil
}

func (stub *loginTargetManagementRepositoryStub) CreateLoginTarget(_ context.Context, input LoginTargetCreateInput, id string, now time.Time) (LoginTargetManagementItem, error) {
	stub.created = append(stub.created, input)
	return LoginTargetManagementItem{
		ID: id, TenantID: input.TenantID, ApplicationID: input.ApplicationID, EnvironmentID: input.EnvironmentID,
		TargetCode: input.TargetCode, Name: input.Name, TargetURI: input.TargetURI, Status: input.Status,
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (stub *loginTargetManagementRepositoryStub) GetLoginTarget(context.Context, string, string, string, string) (LoginTargetManagementItem, error) {
	return LoginTargetManagementItem{}, ErrNotFound
}

func (stub *loginTargetManagementRepositoryStub) UpdateLoginTarget(_ context.Context, input LoginTargetUpdateInput, now time.Time) (LoginTargetManagementItem, error) {
	stub.updated = append(stub.updated, input)
	return LoginTargetManagementItem{
		ID: input.LoginTargetID, TenantID: input.TenantID, ApplicationID: input.ApplicationID, EnvironmentID: input.EnvironmentID,
		Name: input.Name, TargetURI: input.TargetURI, Status: input.Status, Version: input.Version + 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func TestLoginTargetRegistrationRejectsUnapprovedAbsoluteTargets(t *testing.T) {
	repository := &loginTargetManagementRepositoryStub{}
	service, err := NewLoginTargetManagementService(
		repository, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)},
		WithLoginTargetManagementRedirectPolicy(NewPostLoginRedirectPolicy("https://portal.example.com")),
	)
	if err != nil {
		t.Fatalf("NewLoginTargetManagementService() error = %v", err)
	}
	base := LoginTargetCreateInput{
		TenantID: policyTestTenantID, OperatorID: policyTestOperatorID,
		ApplicationID: policyTestApplicationID, EnvironmentID: policyTestEnvironmentID,
		TargetCode: "dashboard", Name: "业务首页", Status: "ACTIVE",
	}

	allowed := base
	allowed.TargetURI = "https://portal.example.com/dashboard"
	if _, err := service.CreateLoginTarget(context.Background(), allowed); err != nil {
		t.Fatalf("allowlisted absolute target rejected: %v", err)
	}

	relative := base
	relative.TargetURI = "/dashboard"
	if _, err := service.CreateLoginTarget(context.Background(), relative); err != nil {
		t.Fatalf("relative target must stay registerable: %v", err)
	}

	rejected := []struct {
		name      string
		targetURI string
	}{
		{name: "unlisted https host", targetURI: "https://evil.example.net/dashboard"},
		{name: "non loopback plaintext", targetURI: "http://portal.example.com/dashboard"},
		{name: "private network plaintext", targetURI: "http://192.168.1.20:8081/dashboard"},
	}
	for _, test := range rejected {
		t.Run(test.name, func(t *testing.T) {
			input := base
			input.TargetURI = test.targetURI
			if _, err := service.CreateLoginTarget(context.Background(), input); !errors.Is(err, ErrValidation) {
				t.Fatalf("CreateLoginTarget(%q) error = %v, want ErrValidation", test.targetURI, err)
			}
			update := LoginTargetUpdateInput{
				TenantID: policyTestTenantID, OperatorID: policyTestOperatorID,
				ApplicationID: policyTestApplicationID, EnvironmentID: policyTestEnvironmentID,
				LoginTargetID: policyTestTargetID, Name: "业务首页", TargetURI: test.targetURI,
				Status: "ACTIVE", Version: 1,
			}
			if _, err := service.UpdateLoginTarget(context.Background(), update); !errors.Is(err, ErrValidation) {
				t.Fatalf("UpdateLoginTarget(%q) error = %v, want ErrValidation", test.targetURI, err)
			}
		})
	}
	if len(repository.created) != 2 || len(repository.updated) != 0 {
		t.Fatalf("rejected targets reached the repository: created=%d updated=%d", len(repository.created), len(repository.updated))
	}
}

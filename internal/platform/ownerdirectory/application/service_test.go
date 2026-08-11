package application

import (
	"context"
	"errors"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
)

type repositoryStub struct {
	tenantID, applicationID, environmentID string
	query                                  Query
	page                                   domain.Page
	err                                    error
}

func (repository *repositoryStub) List(_ context.Context, tenantID, applicationID, environmentID string, query Query) (domain.Page, error) {
	repository.tenantID, repository.applicationID, repository.environmentID, repository.query = tenantID, applicationID, environmentID, query
	return repository.page, repository.err
}

func validPrincipal() appctx.Principal {
	return appctx.Principal{
		OAuthClientID: "oauth-1", ClientID: "crm-owner-directory", TenantID: "tenant-1",
		ApplicationID: "crm-app", ApplicationCode: "customer_and_opportunity",
		EnvironmentID: "env-1", EnvironmentCode: "prod", Scopes: map[string]struct{}{"owner_directory.read": {}},
	}
}

func TestListDerivesTenantAndApplicationFromMachinePrincipal(t *testing.T) {
	repository := &repositoryStub{page: domain.Page{Page: 1, PageSize: 20}}
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.List(context.Background(), validPrincipal(), Query{Keyword: " 张三 "})
	if err != nil || page.Page != 1 {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	if repository.tenantID != "tenant-1" || repository.applicationID != "crm-app" || repository.environmentID != "env-1" {
		t.Fatalf("scope tenant=%q application=%q environment=%q", repository.tenantID, repository.applicationID, repository.environmentID)
	}
	if repository.query.Keyword != "张三" || repository.query.Page != 1 || repository.query.PageSize != 20 {
		t.Fatalf("query=%#v", repository.query)
	}
}

func TestListRejectsMixedFiltersAndNormalizesRepositoryFailures(t *testing.T) {
	repository := &repositoryStub{}
	service, _ := NewService(repository)
	if _, err := service.List(context.Background(), validPrincipal(), Query{Keyword: "张三", UserID: "sub-1"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("mixed filters error=%v", err)
	}
	repository.err = errors.New("private database detail")
	if _, err := service.List(context.Background(), validPrincipal(), Query{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("repository error=%v", err)
	}
}

package http

import (
	"net/http/httptest"
	"testing"
)

func TestPageQueryAcceptsDocumentedAndLegacyFilterSyntax(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/v1/audit/events?page=2&page_size=50&filter[action_category]=LOGIN&result=SUCCESS&filter[occurred_from]=2026-08-20T00:00:00Z", nil)
	query := pageQuery(request)
	if query.Page != 2 || query.PageSize != 50 {
		t.Fatalf("pagination = %#v, want page 2/page_size 50", query)
	}
	if query.ActionCategory != "LOGIN" || query.Result != "SUCCESS" {
		t.Fatalf("mixed filter syntax was not read: %#v", query)
	}
	if query.OccurredFrom == nil {
		t.Fatal("documented bracketed time filter was not parsed")
	}
}

func TestPageQueryPrefersDocumentedFilterSyntax(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/v1/audit/events?filter[result]=DENIED&result=SUCCESS", nil)
	query := pageQuery(request)
	if query.Result != "DENIED" {
		t.Fatalf("result = %q, want bracketed filter value", query.Result)
	}
}

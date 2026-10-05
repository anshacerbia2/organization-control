package httpapi

// The tenant administration routes, at the transport boundary (ADR-ORG-003, TDD-organization-control-001
// §The Tenant Administration Grant). The fixture's transactor refuses every acquisition, so each
// refusal here lands before the database is touched; a request that reached a pool would answer 500.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A Tenant administrator does not make another: no Tenant administrator grants it, for now
// (ADR-ORG-003 §5.2). A tenant caller is refused on all three routes, its own Tenant included.
func TestTheTenantAdministrationRoutesAreProviderOnly(t *testing.T) {
	t.Parallel()

	tenant := tenantCaller(t)
	handler := mounted(t, &tenant)
	own := "/v1/tenants/" + tenant.Tenant.String() + "/administrators"
	for _, path := range []string{own, own + "/" + mustID(t).String() + "/revoke"} {
		body := `{"principal_id":"` + mustID(t).String() + `"}`
		if recorder := post(t, handler, path, body, map[string]string{ReasonHeader: "an audit"}); recorder.Code != http.StatusForbidden {
			t.Errorf("a tenant caller on POST %s answered %d, want 403", path, recorder.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, own, nil)
	request.Header.Set(ReasonHeader, "an audit")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("a tenant caller on GET %s answered %d, want 403", own, recorder.Code)
	}
}

func TestATenantAdministrationGrantNeedsAReasonAndAPrincipal(t *testing.T) {
	t.Parallel()

	provider := providerCaller(t)
	handler := mounted(t, &provider)
	base := "/v1/tenants/" + mustID(t).String() + "/administrators"
	audit := map[string]string{ReasonHeader: "an audit"}
	cases := []struct {
		name, path, body string
		headers          map[string]string
	}{
		{"a grant without a reason", base, `{"principal_id":"` + mustID(t).String() + `"}`, nil},
		{"a revocation without a reason", base + "/" + mustID(t).String() + "/revoke", `{}`, nil},
		{"a grant naming no principal", base, `{}`, audit},
		{"a grant naming a malformed principal", base, `{"principal_id":"kc-user"}`, audit},
		{"a grant in a malformed Tenant", "/v1/tenants/acme/administrators",
			`{"principal_id":"` + mustID(t).String() + `"}`, audit},
		{"a revocation naming a malformed grant", base + "/not-a-uuid/revoke", `{}`, audit},
	}
	for _, c := range cases {
		if recorder := post(t, handler, c.path, c.body, c.headers); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400: %s", c.name, recorder.Code, recorder.Body.String())
		}
	}
}

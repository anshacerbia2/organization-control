package httpapi

// The provider grant routes, at the transport boundary (TDD-organization-control-001 §Caller
// Authority). The fixture's transactor refuses every acquisition, so each refusal here is one that
// lands before the database is touched; a request that reached a pool would answer 500.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTheProviderGrantRoutesAreProviderOnly(t *testing.T) {
	t.Parallel()

	tenant := tenantCaller(t)
	handler := mounted(t, &tenant)
	for _, path := range []string{"/v1/provider-grants", "/v1/provider-grants/" + mustID(t).String() + "/revoke"} {
		body := `{"principal_id":"` + mustID(t).String() + `"}`
		if path != "/v1/provider-grants" {
			body = `{}`
		}
		if recorder := post(t, handler, path, body, map[string]string{ReasonHeader: "an audit"}); recorder.Code != http.StatusForbidden {
			t.Errorf("a tenant caller on POST %s answered %d, want 403", path, recorder.Code)
		}
	}
}

func TestAProviderGrantNeedsAReasonAndAPrincipal(t *testing.T) {
	t.Parallel()

	provider := providerCaller(t)
	handler := mounted(t, &provider)
	cases := []struct {
		name, path, body string
		headers          map[string]string
	}{
		{"a grant without a reason", "/v1/provider-grants", `{"principal_id":"` + mustID(t).String() + `"}`, nil},
		{"a revocation without a reason", "/v1/provider-grants/" + mustID(t).String() + "/revoke", `{}`, nil},
		{"a grant naming no principal", "/v1/provider-grants", `{}`, map[string]string{ReasonHeader: "an audit"}},
		{"a grant naming a malformed principal", "/v1/provider-grants", `{"principal_id":"kc-user"}`,
			map[string]string{ReasonHeader: "an audit"}},
		{"a revocation naming a malformed grant", "/v1/provider-grants/not-a-uuid/revoke", `{}`,
			map[string]string{ReasonHeader: "an audit"}},
	}
	for _, c := range cases {
		if recorder := post(t, handler, c.path, c.body, c.headers); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400: %s", c.name, recorder.Code, recorder.Body.String())
		}
	}
}

// The validation report is a provider's (ADR-ORG-002 §5.2).
func TestTheEmergencyValidationReportIsProviderOnly(t *testing.T) {
	t.Parallel()

	tenant := tenantCaller(t)
	handler := mounted(t, &tenant)
	request := httptest.NewRequest(http.MethodGet, "/v1/provider-grants:emergency-validation", nil)
	request.Header.Set(ReasonHeader, "an audit")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("a tenant caller answered %d, want 403", recorder.Code)
	}
}

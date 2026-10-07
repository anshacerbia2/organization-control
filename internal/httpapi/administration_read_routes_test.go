package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, handler http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestAListRefusesWhatItCannotHonour is STD-GLB-001 1.3.0 §Pagination at the transport: a `limit`
// outside 1 to 100 is refused rather than coerced, a cursor or identifier filter that is not a UUID
// is refused, an unknown filter value is refused, and so is a parameter the list does not take. Every
// case is answered before the database, which the refusing transactor turns into a failure.
func TestAListRefusesWhatItCannotHonour(t *testing.T) {
	t.Parallel()

	tenant := tenantCaller(t)
	provider := providerCaller(t)
	audit := map[string]string{ReasonHeader: "an audit"}

	cases := []struct {
		name    string
		caller  Caller
		path    string
		headers map[string]string
		want    string
	}{
		{"limit zero", tenant, "/v1/workspaces?limit=0", nil, "limit"},
		{"limit above the bound", tenant, "/v1/memberships?limit=101", nil, "limit"},
		{"limit negative", tenant, "/v1/invitations?limit=-1", nil, "limit"},
		{"limit not a number", tenant, "/v1/workspaces?limit=ten", nil, "limit"},
		{"limit empty", tenant, "/v1/workspaces?limit=", nil, "limit"},
		{"after not a UUID", tenant, "/v1/memberships?after=page-2", nil, "after"},
		{"workspace_id not a UUID", tenant, "/v1/memberships?workspace_id=nope", nil, "workspace_id"},
		{"principal_id not a UUID", tenant, "/v1/memberships?principal_id=nope", nil, "principal_id"},
		{"an unknown workspace status", tenant, "/v1/workspaces?status=deleted", nil, "status"},
		{"an unknown membership status", tenant, "/v1/memberships?status=lapsed", nil, "status"},
		{"an unknown invitation state", tenant, "/v1/invitations?state=sent", nil, "state"},
		{"a parameter the list does not take", tenant, "/v1/workspaces?offset=100", nil, "offset"},
		{"a misspelled filter", tenant, "/v1/memberships?stauts=active", nil, "stauts"},
		{"a filter given twice", tenant, "/v1/workspaces?status=active&status=archived", nil, "more than once"},
		{"an unknown organization status", provider, "/v1/organizations?status=closed", audit, "status"},
		{"an unknown classification", provider, "/v1/organizations?classification=vendor", audit, "classification"},
		{"an unknown tenant status", provider, "/v1/tenants?status=paused", audit, "status"},
		{"organization_id not a UUID", provider, "/v1/tenants?organization_id=acme", audit, "organization_id"},
		{"a provider limit above the bound", provider, "/v1/tenants?limit=1000", audit, "limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			caller := tc.caller
			recorder := get(t, mounted(t, &caller), tc.path, tc.headers)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("%s answered %d, want 400:\n%s", tc.path, recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.want) {
				t.Errorf("the refusal did not name %q:\n%s", tc.want, recorder.Body.String())
			}
		})
	}
}

// TestTheListsKeepTheirScopes holds each list to the authority its records need: a provider caller
// on a Tenant's list and a tenant caller on the estate's are refused, and a provider list without a
// reason is refused before any access is recorded.
func TestTheListsKeepTheirScopes(t *testing.T) {
	t.Parallel()

	tenant := tenantCaller(t)
	provider := providerCaller(t)
	audit := map[string]string{ReasonHeader: "an audit"}

	cases := []struct {
		name    string
		caller  Caller
		path    string
		headers map[string]string
		status  int
	}{
		{"a provider on the Workspace list", provider, "/v1/workspaces", audit, http.StatusForbidden},
		{"a provider on the Membership list", provider, "/v1/memberships", audit, http.StatusForbidden},
		{"a provider on a Membership", provider, "/v1/memberships/" + mustID(t).String(), audit, http.StatusForbidden},
		{"a provider on the invitation list", provider, "/v1/invitations", audit, http.StatusForbidden},
		{"a tenant caller on the Organization list", tenant, "/v1/organizations", audit, http.StatusForbidden},
		{"a tenant caller on the Tenant list", tenant, "/v1/tenants", audit, http.StatusForbidden},
		{"a provider list without a reason", provider, "/v1/organizations", nil, http.StatusBadRequest},
		{"the Tenant list without a reason", provider, "/v1/tenants", nil, http.StatusBadRequest},
		{"a Membership named by something other than a UUID", tenant, "/v1/memberships/not-a-uuid", nil, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			caller := tc.caller
			recorder := get(t, mounted(t, &caller), tc.path, tc.headers)
			if recorder.Code != tc.status {
				t.Fatalf("%s answered %d, want %d:\n%s", tc.path, recorder.Code, tc.status, recorder.Body.String())
			}
		})
	}
}

// TestAMembershipTransitionNamesItsVersion is TDD-organization-control-002 §API: every transition
// carries the version the caller was shown, and a revocation, being irreversible, carries a reason
// from a tenant caller too. Each omission is refused before the database.
func TestAMembershipTransitionNamesItsVersion(t *testing.T) {
	t.Parallel()

	caller := tenantCaller(t)
	membershipID := mustID(t).String()
	reasoned := map[string]string{ReasonHeader: "left the organisation"}

	cases := []struct {
		name    string
		path    string
		body    string
		headers map[string]string
		want    string
	}{
		{"a suspension with no body", "/suspend", ``, nil, "empty"},
		{"a suspension with no version", "/suspend", `{}`, nil, "expected version"},
		{"a restoration with a zero version", "/restore", `{"expected_version":0}`, nil, "expected version"},
		{"a misspelled version", "/restore", `{"expected_versoin":3}`, nil, "expected_versoin"},
		{"a revocation with no reason", "/revoke", `{"expected_version":1}`, nil, ReasonHeader},
		{"a revocation with a blank reason", "/revoke", `{"expected_version":1}`, map[string]string{ReasonHeader: "   "}, ReasonHeader},
		{"a revocation with a reason and no version", "/revoke", `{}`, reasoned, "expected version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder := post(t, mounted(t, &caller), "/v1/memberships/"+membershipID+tc.path, tc.body, tc.headers)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("answered %d, want 400:\n%s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.want) {
				t.Errorf("the refusal did not name %q:\n%s", tc.want, recorder.Body.String())
			}
		})
	}
}

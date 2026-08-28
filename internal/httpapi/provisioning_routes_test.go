package httpapi

// The provisioning correlation routes, at the transport boundary.
//
// Every assertion here is about a refusal that lands before the database is touched — the fixture's
// transactor refuses every acquisition, so a request that reached a pool would answer 500 and these
// tests would fail rather than pass quietly.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheProvisioningRoutesAreProviderScoped is the posture, asserted route by route.
//
// Two of these are driven by an external system rather than by a person, which is the argument
// somebody will eventually make for exempting them from authentication. It is the wrong argument: a
// caller who learned a correlation identifier could otherwise declare a Tenant's boundary built, and
// `Service.Activate` reads exactly that statement before letting Memberships in.
func TestTheProvisioningRoutesAreProviderScoped(t *testing.T) {
	t.Parallel()

	caller := tenantCaller(t)
	handler := mounted(t, &caller)

	for _, path := range []string{
		"/v1/tenants",
		"/v1/tenants/" + mustID(t).String() + "/provisioning",
		"/v1/provisioning/realized",
		"/v1/provisioning/failed",
		"/v1/provisioning/sweep-unresolved",
	} {
		recorder := post(t, handler, path, `{}`, map[string]string{ReasonHeader: "an audit"})
		if recorder.Code != http.StatusForbidden {
			t.Errorf("a tenant caller on %s answered %d, want 403", path, recorder.Code)
		}
	}
}

// TestTheProvisioningRoutesRefuseAMalformedBody covers the validation that happens before any
// authority is exercised.
//
// Each of these is a 400 rather than a 500, which is the distinction the per-package `ErrInvalid`
// exists for: before it, a caller who omitted a field was told the service was broken.
func TestTheProvisioningRoutesRefuseAMalformedBody(t *testing.T) {
	t.Parallel()

	caller := providerCaller(t)
	handler := mounted(t, &caller)
	headers := map[string]string{ReasonHeader: "an audit"}

	cases := []struct {
		name string
		path string
		body string
	}{
		{
			name: "an undeclared isolation profile",
			path: "/v1/tenants",
			body: `{"organization_id":"` + mustID(t).String() +
				`","display_name":"a tenant","isolation_profile":"hyperscale"}`,
		},
		{
			// DisallowUnknownFields is what makes an absent field enforced rather than ignored. A
			// body naming a status would otherwise look honoured, and intake would silently create
			// the Tenant in `requested` regardless.
			name: "a field the intake body does not have",
			path: "/v1/tenants",
			body: `{"organization_id":"` + mustID(t).String() +
				`","display_name":"a tenant","isolation_profile":"pooled","status":"active"}`,
		},
		{
			name: "an outcome with no correlation identifier",
			path: "/v1/provisioning/realized",
			body: `{"detail":"boundary built"}`,
		},
		{
			// A refusal that says only "failed" leaves the retry decision with nothing to go on, so
			// the detail is required on this route and optional on the realized one.
			name: "a failure with no detail",
			path: "/v1/provisioning/failed",
			body: `{"correlation_id":"` + mustID(t).String() + `"}`,
		},
		{
			name: "a sweep with no batch size",
			path: "/v1/provisioning/sweep-unresolved",
			body: `{}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			recorder := post(t, handler, testCase.path, testCase.body, headers)
			if recorder.Code != http.StatusBadRequest {
				t.Errorf("%s answered %d, want 400:\n%s",
					testCase.name, recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestAMalformedTenantIdentifierIsRefusedBeforeTheDatabase keeps a path parameter from reaching a
// statement.
func TestAMalformedTenantIdentifierIsRefusedBeforeTheDatabase(t *testing.T) {
	t.Parallel()

	caller := providerCaller(t)
	handler := mounted(t, &caller)

	request := httptest.NewRequest(http.MethodGet, "/v1/tenants/not-a-uuid", nil)
	request.Header.Set(ReasonHeader, "an audit")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("a malformed tenant identifier answered %d, want 400:\n%s",
			recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "tenant_id") {
		t.Errorf("the refusal did not name the parameter:\n%s", recorder.Body.String())
	}
}

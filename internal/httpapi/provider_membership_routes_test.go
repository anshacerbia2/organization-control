package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The provider's read of one Membership (ADR-ORG-003 §5.6, amended 2026-10-21;
// TDD-organization-control-002 1.19.0). Every case here is refused before the database, which the test
// surface's transactor enforces; what the read returns and records is asserted against the engine in
// internal/membership.
func TestTheProviderMembershipReadIsAProvidersInForceWithAReason(t *testing.T) {
	t.Parallel()

	path := "/v1/tenants/" + mustID(t).String() + "/memberships/" + mustID(t).String()
	reason := map[string]string{ReasonHeader: "INC-3 a compromised member"}

	tenant, eligible := tenantCaller(t), eligibleCaller(t)
	for name, caller := range map[string]*Caller{"a tenant caller": &tenant, "an eligible holder": &eligible} {
		if recorder := getWith(t, mounted(t, caller), path, reason); recorder.Code != http.StatusForbidden {
			t.Errorf("%s answered %d, want 403", name, recorder.Code)
		}
	}

	provider := providerCaller(t)
	handler := mounted(t, &provider)
	if recorder := getWith(t, handler, path, nil); recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), ReasonHeader) {
		t.Errorf("a read without a reason answered %d, want 400 naming %s:\n%s",
			recorder.Code, ReasonHeader, recorder.Body.String())
	}
	for name, malformed := range map[string]string{
		"a malformed Tenant":     "/v1/tenants/kc-tenant/memberships/" + mustID(t).String(),
		"a malformed Membership": "/v1/tenants/" + mustID(t).String() + "/memberships/m-1",
	} {
		if recorder := getWith(t, handler, malformed, reason); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", name, recorder.Code)
		}
	}
}

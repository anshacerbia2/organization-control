package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// The privileged-access routes (ADR-ORG-002 §5.6, TDD-organization-control-001 §Privileged Access
// Review). Every case here is refused before the database, which the test surface's transactor
// enforces; what the routes read and record is asserted against the engine in internal/authority.

const privilegedAccessPath = "/v1/privileged-access"

func TestThePrivilegedAccessRoutesAreAProvidersInForce(t *testing.T) {
	t.Parallel()

	tenant, eligible := tenantCaller(t), eligibleCaller(t)
	reason := map[string]string{ReasonHeader: "weekly review"}
	for _, path := range []string{
		privilegedAccessPath, privilegedAccessPath + ":unreviewed", privilegedAccessPath + "/reviews",
	} {
		for name, caller := range map[string]*Caller{"a tenant caller": &tenant, "an eligible holder": &eligible} {
			// An eligible caller is refused by authentication in production; mounted here it reaches
			// the route, and requireProvider refuses it on any route regardless.
			if recorder := getWith(mounted(t, caller), path, reason); recorder.Code != http.StatusForbidden {
				t.Errorf("%s on %s answered %d, want 403", name, path, recorder.Code)
			}
		}
	}
	if recorder := post(t, mounted(t, &tenant), privilegedAccessPath+"/reviews", `{}`, reason); recorder.Code != http.StatusForbidden {
		t.Errorf("a tenant caller recording a review answered %d, want 403", recorder.Code)
	}
}

func TestTheProviderListRefusesWhatItCannotHonour(t *testing.T) {
	t.Parallel()

	provider := providerCaller(t)
	handler := mounted(t, &provider)
	reason := map[string]string{ReasonHeader: "weekly review"}

	if recorder := getWith(handler, privilegedAccessPath, nil); recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), ReasonHeader) {
		t.Errorf("a read without a reason answered %d, want 400 naming %s", recorder.Code, ReasonHeader)
	}
	for name, query := range map[string]string{
		"an unknown filter":            "?principal_id=" + mustID(t).String(),
		"a malformed actor":            "?actor_id=kc-user",
		"a malformed instant":          "?from=yesterday",
		"an instant without offset":    "?from=2026-10-01T00:00:00",
		"a window ending at its start": "?from=2026-10-01T00:00:00Z&to=2026-10-01T00:00:00Z",
		"a window ending before it":    "?from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z",
		"an unknown authority":         "?authority=root",
		"a limit above the bound":      "?limit=101",
		"a filter given twice":         "?authority=emergency&authority=activation",
	} {
		t.Run(name, func(t *testing.T) {
			if recorder := getWith(handler, privilegedAccessPath+query, reason); recorder.Code != http.StatusBadRequest {
				t.Errorf("answered %d, want 400:\n%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestTheTenantsListIsATenantAdministratorsAndNamesNoTenant(t *testing.T) {
	t.Parallel()

	provider, tenant := providerCaller(t), tenantCaller(t)
	if recorder := getWith(mounted(t, &provider), "/v1/provider-access", map[string]string{ReasonHeader: "r"}); recorder.Code != http.StatusForbidden {
		t.Errorf("a provider on the Tenant's list answered %d, want 403", recorder.Code)
	}
	handler := mounted(t, &tenant)
	for name, query := range map[string]string{
		"a Tenant":            "?tenant_id=" + mustID(t).String(),
		"an actor":            "?actor_id=" + mustID(t).String(),
		"the consumer rows":   "?authority=consumer",
		"a malformed instant": "?to=now",
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if recorder := getWith(handler, "/v1/provider-access"+query, nil); recorder.Code != http.StatusBadRequest {
				t.Errorf("answered %d, want 400:\n%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestAReviewNamesAValidActor(t *testing.T) {
	t.Parallel()

	provider := providerCaller(t)
	handler := mounted(t, &provider)
	reason := map[string]string{ReasonHeader: "weekly review"}
	for name, body := range map[string]string{
		"no actor":          `{"from":"2026-10-01T00:00:00Z","to":"2026-10-08T00:00:00Z","outcome":"appropriate"}`,
		"a malformed actor": `{"actor_id":"kc-user","from":"2026-10-01T00:00:00Z","to":"2026-10-08T00:00:00Z","outcome":"appropriate"}`,
		"an unknown field":  `{"actor_id":"` + mustID(t).String() + `","period":"week"}`,
		"a malformed instant": `{"actor_id":"` + mustID(t).String() +
			`","from":"last week","to":"2026-10-08T00:00:00Z","outcome":"appropriate"}`,
		"an unknown outcome": `{"actor_id":"` + mustID(t).String() +
			`","from":"2026-10-01T00:00:00Z","to":"2026-10-08T00:00:00Z","outcome":"fine"}`,
		"a period ending where it starts": `{"actor_id":"` + mustID(t).String() +
			`","from":"2026-10-01T00:00:00Z","to":"2026-10-01T00:00:00Z","outcome":"appropriate"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if recorder := post(t, handler, privilegedAccessPath+"/reviews", body, reason); recorder.Code != http.StatusBadRequest {
				t.Errorf("answered %d, want 400:\n%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestEveryRouteRecordsItsPatternAndPathTenant is the evidence's operation and Tenant: the mux puts
// both in the request context, and a scope's evidence reads them.
func TestEveryRouteRecordsItsPatternAndPathTenant(t *testing.T) {
	t.Parallel()

	scope, err := db.ProviderScope(mustID(t), mustID(t), db.EmergencyAuthority())
	if err != nil {
		t.Fatal(err)
	}
	var seen db.ProviderAccess
	mux := newRouteMux()
	capture := func(_ http.ResponseWriter, r *http.Request) {
		seen = scope.Evidence(r.Context(), "a reason")
	}
	mux.HandleFunc("POST /v1/tenants/{tenant_id}/suspend", capture)
	mux.HandleFunc("GET /v1/tenants", capture)

	tenant := mustID(t)
	cases := []struct {
		method, path, operation string
		tenant                  id.UUID
	}{
		{http.MethodPost, "/v1/tenants/" + tenant.String() + "/suspend", "POST /v1/tenants/{tenant_id}/suspend", tenant},
		{http.MethodPost, "/v1/tenants/kc-tenant/suspend", "POST /v1/tenants/{tenant_id}/suspend", id.UUID{}},
		{http.MethodGet, "/v1/tenants", "GET /v1/tenants", id.UUID{}},
	}
	for _, c := range cases {
		seen = db.ProviderAccess{}
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		if seen.Operation != c.operation || seen.Tenant != c.tenant || seen.Authority != db.AuthorityEmergency {
			t.Errorf("%s %s recorded %+v; want operation %q and Tenant %v", c.method, c.path, seen, c.operation, c.tenant)
		}
	}
}

// TestTheScopeCarriesTheAuthorityTheCallerActedOn: emergency, the activation by name, or an eligible
// holder's nothing; and a provider in force by neither is refused rather than recorded as acting on
// nothing.
func TestTheScopeCarriesTheAuthorityTheCallerActedOn(t *testing.T) {
	t.Parallel()

	correlation, activation := mustID(t), mustID(t)
	cases := []struct {
		name   string
		caller Caller
		kind   string
	}{
		{"an emergency grant", Caller{Subject: mustID(t), Provider: true, Emergency: true}, db.AuthorityEmergency},
		{"an activation", Caller{Subject: mustID(t), Provider: true, Activation: activation}, db.AuthorityActivation},
		{"an eligible holder", Caller{Subject: mustID(t), Eligible: true}, db.AuthorityEligible},
	}
	for _, c := range cases {
		scope, err := resolve(c.caller, correlation)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := scope.Authority(); got.Kind() != c.kind {
			t.Errorf("%s resolved to authority %q, want %q", c.name, got.Kind(), c.kind)
		}
		if c.kind == db.AuthorityActivation && scope.Authority().Activation() != activation {
			t.Errorf("the activation scope names %v, want %v", scope.Authority().Activation(), activation)
		}
	}
	if _, err := resolve(Caller{Subject: mustID(t), Provider: true}, correlation); err == nil {
		t.Error("a provider with neither an emergency grant nor an activation resolved to a scope")
	}
	consumer, err := resolve(Caller{Subject: mustID(t), Consumer: "foundation-reference"}, correlation)
	if err != nil || consumer.Authority().Kind() != db.AuthorityConsumer {
		t.Errorf("a consumer resolved to %q, %v; want the consumer authority", consumer.Authority().Kind(), err)
	}
}

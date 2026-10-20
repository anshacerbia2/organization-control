package httpapi

// A reporter is admitted from its record, on its own report routes alone (ADR-ORG-001 §5.11,
// amended 2026-10-20; TDD-organization-control-001 §Reporters).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/authority"
)

func reporterRecords(t *testing.T) (*fakeRecords, id.UUID, id.UUID) {
	t.Helper()
	records := testRecords()
	provisioner, billing := mustID(t), mustID(t)
	records.reporters = map[id.UUID]heldReporter{
		provisioner: {id: mustID(t), reports: authority.ReportsProvisioning},
		billing:     {id: mustID(t), reports: "obligation:billing"},
	}
	return records, provisioner, billing
}

func TestTheReportRoutesAreExactlyThese(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"POST /v1/provisioning/realized":                    reportProvisioning,
		"POST /v1/provisioning/failed":                      reportProvisioning,
		"POST /v1/offboardings/x/deprovisioning":            reportProvisioning,
		"POST /v1/obligations/x/resolve":                    reportObligation,
		"POST /v1/offboardings/x/deprovisioning/resend":     "",
		"POST /v1/provisioning/sweep-unresolved":            "",
		"GET /v1/provisioning/realized":                     "",
		"POST /v1/offboardings//deprovisioning":             "",
		"POST /v1/obligations/x/y/resolve":                  "",
		"POST /v1/offboardings/x/obligations":               "",
		"POST /v1/tenants/x/provisioning":                   "",
		"POST /v1/projections/reconcile":                    "",
		"POST /v1/obligations/x/resolve/again":              "",
		"POST /v1/offboardings/x/deprovisioning?state=same": reportProvisioning,
	}
	for route, want := range cases {
		method, target, _ := strings.Cut(route, " ")
		got, _ := reportRoute(httptest.NewRequest(method, target, nil))
		if got != want {
			t.Errorf("%s is report %q, want %q", route, got, want)
		}
	}
}

func TestAReporterIsAdmittedOnItsOwnReportsAlone(t *testing.T) {
	t.Parallel()
	s := newSigner(t)
	records, provisioner, billing := reporterRecords(t)
	cfg := AuthenticationConfig{Records: records, Consumers: true}

	admitted := []struct {
		principal id.UUID
		path      string
		reports   authority.Reports
	}{
		{provisioner, "/v1/provisioning/realized", authority.ReportsProvisioning},
		{provisioner, "/v1/provisioning/failed", authority.ReportsProvisioning},
		{provisioner, "/v1/offboardings/" + mustID(t).String() + "/deprovisioning", authority.ReportsProvisioning},
		{billing, "/v1/obligations/" + mustID(t).String() + "/resolve", "obligation:billing"},
	}
	for _, tc := range admitted {
		caller, called, recorder := authenticatedAt(t, s, cfg, s.sign(t, consumerClaims(tc.principal)), tc.path)
		if !called {
			t.Errorf("%s: answered %d: %s", tc.path, recorder.Code, recorder.Body.String())
			continue
		}
		if caller.Reporter.IsNil() || caller.Reports != tc.reports || caller.Consumer != "" || caller.Provider {
			t.Errorf("%s: produced %+v", tc.path, caller)
		}
		scope, err := resolve(caller, mustID(t))
		if err != nil || !scope.IsProvider() || scope.Authority().Kind() != "reporter" {
			t.Errorf("%s: resolved to %+v, %v; want the provider scope under the reporter authority", tc.path, scope, err)
		}
	}

	refused := []struct {
		name      string
		principal id.UUID
		path      string
	}{
		{"the provisioning reporter on an obligation", provisioner, "/v1/obligations/" + mustID(t).String() + "/resolve"},
		{"an obligation reporter on provisioning", billing, "/v1/provisioning/realized"},
		{"a reporter on a provider route", provisioner, "/v1/tenants/" + mustID(t).String() + "/activate"},
		{"a reporter on the operator's resend", provisioner, "/v1/offboardings/" + mustID(t).String() + "/deprovisioning/resend"},
		{"a reporter on reconcile", provisioner, "/v1/projections/reconcile"},
		{"an unregistered workload on a report", mustID(t), "/v1/provisioning/realized"},
		{"a consumer on a report", testConsumerWorkload, "/v1/provisioning/realized"},
	}
	for _, tc := range refused {
		_, called, recorder := authenticatedAt(t, s, cfg, s.sign(t, consumerClaims(tc.principal)), tc.path)
		if called || recorder.Code != http.StatusForbidden {
			t.Errorf("%s: answered %d, called=%t, want 403", tc.name, recorder.Code, called)
		}
	}
}

// TestAPersonOnAReportRouteIsAProviderAsBefore: the provider's fallback for the provisioning system
// stays a person's act, authorized as a provider (ADR-ORG-001 §5.11).
func TestAPersonOnAReportRouteIsAProviderAsBefore(t *testing.T) {
	t.Parallel()
	s := newSigner(t)
	records, _, _ := reporterRecords(t)
	caller, called, recorder := authenticatedAt(t, s, AuthenticationConfig{Records: records, Consumers: true},
		s.sign(t, providerClaims(testProvider)), "/v1/provisioning/realized")
	if !called || !caller.Provider || !caller.Reporter.IsNil() {
		t.Errorf("a provider on a report route answered %d and produced %+v", recorder.Code, caller)
	}
}

func TestAReporterCarryingAnotherAuthorityIsRefusedAtScope(t *testing.T) {
	t.Parallel()
	reporter := mustID(t)
	for name, caller := range map[string]Caller{
		"and a provider": {Subject: mustID(t), Reporter: reporter, Provider: true},
		"and a consumer": {Subject: mustID(t), Reporter: reporter, Consumer: "c"},
		"and a Tenant":   {Subject: mustID(t), Reporter: reporter, Tenant: mustID(t)},
		"and a self":     {Subject: mustID(t), Reporter: reporter, Self: true},
	} {
		if _, err := resolve(caller, mustID(t)); err == nil {
			t.Errorf("a reporter %s resolved to a scope", name)
		}
	}
}

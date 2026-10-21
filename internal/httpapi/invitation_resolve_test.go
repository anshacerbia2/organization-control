package httpapi

// POST /v1/invitations/resolve admits identity-control's invitation:resolve entry alone, and that
// entry reaches nothing else (ADR-IAM-011 §5.7 (a), ADR-ORG-001 §5.11 as amended 2026-10-21).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/authority"
)

const resolvePath = "/v1/invitations/resolve"

func TestTheResolveRouteIsTheInvitationReportAlone(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"POST /v1/invitations/resolve":         reportInvitation,
		"POST /v1/invitations/resolve?x=y":     reportInvitation,
		"GET /v1/invitations/resolve":          "",
		"POST /v1/invitations/resolve/again":   "",
		"POST /v1/invitations/accept":          "",
		"POST /v1/invitations/lookup":          "",
		"POST /v1/invitations/x/revoke":        "",
		"POST /v1/obligations/resolve/resolve": reportObligation,
	}
	for route, want := range cases {
		method, target, _ := strings.Cut(route, " ")
		got, _ := reportRoute(httptest.NewRequest(method, target, nil))
		if got != want {
			t.Errorf("%s is report %q, want %q", route, got, want)
		}
	}
}

func TestTheInvitationResolverIsAdmittedOnTheResolveRouteAlone(t *testing.T) {
	t.Parallel()
	s := newSigner(t)
	records, provisioner, billing := reporterRecords(t)
	resolver := mustID(t)
	records.reporters[resolver] = heldReporter{id: mustID(t), reports: authority.ReportsInvitationResolve}
	cfg := AuthenticationConfig{Records: records, Consumers: true}

	caller, called, recorder := authenticatedAt(t, s, cfg, s.sign(t, consumerClaims(resolver)), resolvePath)
	if !called {
		t.Fatalf("the resolver answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if caller.Reporter.IsNil() || !caller.Reports.InvitationResolve() || caller.Consumer != "" || caller.Provider {
		t.Errorf("the resolver produced %+v", caller)
	}
	scope, err := resolve(caller, mustID(t))
	switch {
	case err != nil:
		t.Fatalf("resolve: %v", err)
	case !scope.IsInvitationResolver() || scope.IsProvider() || scope.IsConsumer() || scope.IsSelf() || !scope.TenantID().IsNil():
		t.Errorf("the resolver resolved to %+v; want the invitation resolver scope alone", scope)
	case scope.Authority().Kind() != "reporter":
		t.Errorf("the resolver's authority is %q, want reporter", scope.Authority().Kind())
	}

	refused := []struct {
		name      string
		principal id.UUID
		path      string
	}{
		{"the resolver on a provisioning report", resolver, "/v1/provisioning/realized"},
		{"the resolver on a deprovisioning report", resolver, "/v1/offboardings/" + mustID(t).String() + "/deprovisioning"},
		{"the resolver on an obligation", resolver, "/v1/obligations/" + mustID(t).String() + "/resolve"},
		{"the resolver on a provider route", resolver, "/v1/tenants/" + mustID(t).String() + "/activate"},
		{"the resolver on acceptance", resolver, "/v1/invitations/accept"},
		{"the provisioning reporter on the resolve", provisioner, resolvePath},
		{"an obligation reporter on the resolve", billing, resolvePath},
		{"an unregistered workload on the resolve", mustID(t), resolvePath},
		{"a registered consumer on the resolve", testConsumerWorkload, resolvePath},
	}
	for _, tc := range refused {
		_, called, recorder := authenticatedAt(t, s, cfg, s.sign(t, consumerClaims(tc.principal)), tc.path)
		if called || recorder.Code != http.StatusForbidden {
			t.Errorf("%s: answered %d, called=%t, want 403", tc.name, recorder.Code, called)
		}
	}
}

// A person on the resolve is authorized as any person's token is, and the handler refuses them before
// the body is read or the database is reached: a provider grant does not resolve invitations.
func TestTheResolveRefusesEveryCallerButTheResolver(t *testing.T) {
	t.Parallel()
	body := `{"token":"` + strings.Repeat("A", 43) + `"}`
	for name, caller := range map[string]Caller{
		"a provider":              providerCaller(t),
		"a Tenant administrator":  tenantCaller(t),
		"the provisioning report": {Subject: mustID(t), Reporter: mustID(t), Reports: authority.ReportsProvisioning},
		"an obligation report":    {Subject: mustID(t), Reporter: mustID(t), Reports: "obligation:billing"},
	} {
		recorder := post(t, mounted(t, &caller), resolvePath, body, map[string]string{ReasonHeader: "resolving"})
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s answered %d: %s", name, recorder.Code, recorder.Body.String())
		}
	}
}

// A malformed body or token is 400 and reaches nothing: mounted's transactor fails the test if the
// database is reached, and the refusal names no invitation (ADR-IAM-011 §5.7 (b)).
func TestTheResolveRefusesAMalformedRequestBeforeTheDatabase(t *testing.T) {
	t.Parallel()
	caller := Caller{Subject: mustID(t), Reporter: mustID(t), Reports: authority.ReportsInvitationResolve}
	for name, body := range map[string]string{
		"no body":          ``,
		"not JSON":         `token`,
		"an unknown field": `{"token":"` + strings.Repeat("A", 43) + `","tenant_id":"x"}`,
		"a short token":    `{"token":"abc"}`,
		"an empty token":   `{"token":""}`,
	} {
		recorder := post(t, mounted(t, &caller), resolvePath, body, map[string]string{IdempotencyHeader: ""})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d: %s", name, recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "invitation-not-acceptable") {
			t.Errorf("%s was answered as a token that names nothing: %s", name, recorder.Body.String())
		}
	}
}

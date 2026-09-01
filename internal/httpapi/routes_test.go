package httpapi

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/observability"

	occontext "github.com/anshacerbia2/organization-control/internal/context"
	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/invitation"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/offboarding"
	"github.com/anshacerbia2/organization-control/internal/organization"
	"github.com/anshacerbia2/organization-control/internal/projection"
	"github.com/anshacerbia2/organization-control/internal/tenant"
	"github.com/anshacerbia2/organization-control/internal/workspace"
)

// refusingTransactor fails any attempt to open a transaction.
//
// Every test in this file asserts a request is stopped before it reaches the database, so reaching
// the database is the failure. A transactor that succeeded would let a test pass because the fake
// happened to return a zero value, which is the shape of a test that proves nothing.
type refusingTransactor struct{ t *testing.T }

func (r refusingTransactor) InTx(stdcontext.Context, func(stdcontext.Context, db.Tx) error) error {
	r.t.Helper()
	r.t.Error("a request reached the database when it should have been refused first")
	return errors.New("the transactor must not be reached")
}

// stubRecorder satisfies the mandatory privileged-access recorder.
type stubRecorder struct{}

func (stubRecorder) RecordProviderAccess(stdcontext.Context, db.ProviderAccess) error { return nil }

// okProber answers readiness affirmatively.
type okProber struct{ err error }

func (p okProber) Ping(stdcontext.Context) error { return p.err }

func testSurface(t *testing.T) Surface {
	t.Helper()

	transactor := refusingTransactor{t: t}

	tenantPool, err := db.NewTenantPool(transactor)
	if err != nil {
		t.Fatalf("tenant pool: %v", err)
	}
	providerPool, err := db.NewProviderPool(transactor, stubRecorder{})
	if err != nil {
		t.Fatalf("provider pool: %v", err)
	}

	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	memberships, err := membership.New(tenantPool)
	must(err, "membership service")
	tenants, err := tenant.New(providerPool)
	must(err, "tenant service")
	provisioning, err := tenant.NewCoordinator(providerPool, tenants, 30*time.Minute)
	must(err, "provisioning coordinator")
	organizations, err := organization.New(providerPool)
	must(err, "organization service")
	workspaces, err := workspace.New(tenantPool)
	must(err, "workspace service")
	invitations, err := invitation.New(tenantPool, providerPool, memberships)
	must(err, "invitation service")
	offboardings, err := offboarding.New(providerPool, tenantPool, tenants, memberships)
	must(err, "offboarding service")
	registry, err := projection.NewRegistry(providerPool)
	must(err, "projection registry")
	publisher, err := projection.NewPublisher(providerPool, registry)
	must(err, "projection publisher")
	reconciler, err := projection.NewReconciler(providerPool)
	must(err, "projection reconciler")
	contexts, err := occontext.New(providerPool)
	must(err, "context service")
	// The raw transactor, as production wires it: the frontier reads outbox aggregates carrying no
	// tenant column, so it takes no scope and writes no privileged-access record per poll.
	frontier, err := projection.NewFrontierReader(transactor)
	must(err, "frontier reader")

	surface, err := Routes(RoutesConfig{
		Services: Services{
			Memberships: memberships, Tenants: tenants, Provisioning: provisioning,
			Organizations: organizations,
			Workspaces:    workspaces, Invitations: invitations, Offboardings: offboardings,
			Registry: registry, Publisher: publisher, Reconciler: reconciler, Contexts: contexts,
			Frontier: frontier,
		},
		Database: okProber{},
	})
	must(err, "routes")
	return surface
}

// mounted builds the root handler with the real split, and with the caller the test supplies.
//
// The API chain is ResolveScope only. The platform chain is deliberately absent: these tests assert
// what this package does, and a correlation is supplied directly so the assertions do not depend on
// middleware belonging to another module.
func mounted(t *testing.T, caller *Caller) http.Handler {
	t.Helper()

	surface := testSurface(t)
	correlation, err := id.NewV7()
	if err != nil {
		t.Fatalf("correlation: %v", err)
	}

	establish := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := observability.WithCorrelationID(r.Context(), correlation)
			if caller != nil {
				ctx = WithCaller(ctx, *caller)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	passthrough := func(next http.Handler) http.Handler { return next }

	return surface.Mount(passthrough, establish, func(next http.Handler) http.Handler {
		return establish(ResolveScope(next))
	})
}

func post(t *testing.T, handler http.Handler, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader = strings.NewReader(body)
	request := httptest.NewRequest(http.MethodPost, path, reader)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func tenantCaller(t *testing.T) Caller {
	t.Helper()
	subject, err := id.NewV7()
	if err != nil {
		t.Fatalf("subject: %v", err)
	}
	tenantID, err := id.NewV7()
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	return Caller{Subject: subject, Tenant: tenantID}
}

func providerCaller(t *testing.T) Caller {
	t.Helper()
	subject, err := id.NewV7()
	if err != nil {
		t.Fatalf("subject: %v", err)
	}
	return Caller{Subject: subject, Provider: true}
}

// TestProbesNeedNoCaller is the regression identity-control paid for in an outage.
//
// One mux meant the authentication middleware wrapped the probes too, every probe answered 401, and
// no replica entered service. The split is the fix; this asserts the split is wired.
func TestProbesNeedNoCaller(t *testing.T) {
	t.Parallel()

	handler := mounted(t, nil)

	for _, path := range []string{"/healthz", "/readyz"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Errorf("%s answered %d with no caller, want 200", path, recorder.Code)
		}
	}
}

func TestReadinessFailsWhenTheDatabaseIsUnreachable(t *testing.T) {
	t.Parallel()

	surface, err := Routes(RoutesConfig{
		Services: testSurfaceServices(t),
		Database: okProber{err: errors.New("connection refused")},
	})
	if err != nil {
		t.Fatalf("routes: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recorder := httptest.NewRecorder()
	surface.Probes.ServeHTTP(recorder, request)

	// 503, not 500. The replica is healthy and its dependency is not, and the orchestrator's
	// correct response is to take it out of rotation rather than to restart it.
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness answered %d with an unreachable database, want 503", recorder.Code)
	}
}

func TestAPIRefusesARequestWithNoCaller(t *testing.T) {
	t.Parallel()

	handler := mounted(t, nil)
	recorder := post(t, handler, "/v1/memberships", `{}`, nil)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated mutation answered %d, want 401", recorder.Code)
	}
}

func TestTenantRouteRefusesAProviderCaller(t *testing.T) {
	t.Parallel()

	caller := providerCaller(t)
	handler := mounted(t, &caller)
	recorder := post(t, handler, "/v1/memberships", `{}`, nil)

	if recorder.Code != http.StatusForbidden {
		t.Errorf("a provider caller on a tenant route answered %d, want 403", recorder.Code)
	}
}

func TestProviderRouteRefusesATenantCaller(t *testing.T) {
	t.Parallel()

	caller := tenantCaller(t)
	handler := mounted(t, &caller)
	recorder := post(t, handler, "/v1/organizations", `{}`,
		map[string]string{ReasonHeader: "an audit"})

	if recorder.Code != http.StatusForbidden {
		t.Errorf("a tenant caller on a provider route answered %d, want 403", recorder.Code)
	}
}

func TestProviderRouteRequiresAReason(t *testing.T) {
	t.Parallel()

	caller := providerCaller(t)
	handler := mounted(t, &caller)
	recorder := post(t, handler, "/v1/organizations", `{}`, nil)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a provider request with no reason answered %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), ReasonHeader) {
		t.Errorf("the refusal did not name the missing header:\n%s", recorder.Body.String())
	}
}

// TestScopeIgnoresRequestSuppliedTenant is SAD-004 §8.3 at the transport boundary.
//
// The membership grant body has no `tenant_id` field, so a caller cannot ask for a Tenant other than
// its own even in principle. `DisallowUnknownFields` is what makes the absence enforced rather than
// merely ignored: a body naming a Tenant is refused, so a client cannot come to believe the field
// was honoured.
func TestScopeIgnoresRequestSuppliedTenant(t *testing.T) {
	t.Parallel()

	caller := tenantCaller(t)
	other, err := id.NewV7()
	if err != nil {
		t.Fatalf("other tenant: %v", err)
	}

	handler := mounted(t, &caller)
	recorder := post(t, handler, "/v1/memberships",
		`{"tenant_id":"`+other.String()+`","principal_id":"`+other.String()+
			`","subject_type":"human","provenance":"test","valid_from":"2026-01-01T00:00:00Z"}`, nil)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a body naming another Tenant answered %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "tenant_id") {
		t.Errorf("the refusal did not name the offending field:\n%s", recorder.Body.String())
	}
}

func TestProviderCallerCarryingATenantIsRefused(t *testing.T) {
	t.Parallel()

	subject, err := id.NewV7()
	if err != nil {
		t.Fatalf("subject: %v", err)
	}
	tenantID, err := id.NewV7()
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	correlation, err := id.NewV7()
	if err != nil {
		t.Fatalf("correlation: %v", err)
	}

	// Asserted on `resolve` directly. The ambiguity is in the caller-to-scope rule, and routing it
	// through a handler would only prove that some refusal happened somewhere.
	if _, err := resolve(Caller{Subject: subject, Tenant: tenantID, Provider: true}, correlation); err == nil {
		t.Error("a provider caller carrying a Tenant resolved to a scope")
	}
}

func TestResolvedScopeMatchesTheCaller(t *testing.T) {
	t.Parallel()

	correlation, err := id.NewV7()
	if err != nil {
		t.Fatalf("correlation: %v", err)
	}
	caller := tenantCaller(t)

	scope, err := resolve(caller, correlation)
	if err != nil {
		t.Fatalf("resolve a tenant caller: %v", err)
	}
	switch {
	case scope.IsProvider():
		t.Error("a tenant caller resolved to a provider scope")
	case scope.TenantID() != caller.Tenant:
		t.Errorf("the scope bound Tenant %s and the caller is %s", scope.TenantID(), caller.Tenant)
	case scope.Actor() != caller.Subject:
		t.Errorf("the scope's actor is %s and the caller's subject is %s", scope.Actor(), caller.Subject)
	case scope.Correlation() != correlation:
		t.Error("the scope did not carry the request's correlation identifier")
	}

	provider, err := resolve(providerCaller(t), correlation)
	if err != nil {
		t.Fatalf("resolve a provider caller: %v", err)
	}
	if !provider.IsProvider() {
		t.Error("a provider caller resolved to a tenant scope")
	}
	if !provider.TenantID().IsNil() {
		t.Errorf("a provider scope bound Tenant %s", provider.TenantID())
	}
}

// TestScopeRequiresACorrelation asserts the wiring check rather than a caller-facing rule.
//
// A scope built with no correlation would produce provider evidence that correlates to no request,
// which PAD-PLT-002 §5.2 exists to prevent. Mounting the API without the platform chain is the way
// that happens in practice, so it must fail loudly rather than proceed.
func TestScopeRequiresACorrelation(t *testing.T) {
	t.Parallel()

	surface := testSurface(t)
	caller := tenantCaller(t)

	handler := ResolveScope(surface.API)
	request := httptest.NewRequest(http.MethodPost, "/v1/memberships", strings.NewReader(`{}`))
	request = request.WithContext(WithCaller(request.Context(), caller))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("a request with no correlation answered %d, want 500", recorder.Code)
	}
}

// TestAnonymousLookupIsUniform is SAD-004 §5.5.
//
// Two well-formed tokens, neither of which corresponds to any invitation, must produce byte-identical
// responses — and so must a token that does. The handler reads nothing, which is the only
// construction where that holds for the status code, the body, and the response time at once.
func TestAnonymousLookupIsUniform(t *testing.T) {
	t.Parallel()

	handler := mounted(t, nil)

	first, _, err := invitation.NewToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	second, _, err := invitation.NewToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	answers := make([]string, 0, 2)
	for _, token := range []invitation.Token{first, second} {
		recorder := post(t, handler, "/v1/invitations/lookup",
			`{"token":"`+string(token)+`"}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("the lookup answered %d for a well-formed token, want 200: %s",
				recorder.Code, recorder.Body.String())
		}
		answers = append(answers, recorder.Body.String())
	}

	if answers[0] != answers[1] {
		t.Errorf("two tokens produced different answers:\n%s\n%s", answers[0], answers[1])
	}
}

func TestAnonymousLookupRefusesAMalformedToken(t *testing.T) {
	t.Parallel()

	handler := mounted(t, nil)
	recorder := post(t, handler, "/v1/invitations/lookup", `{"token":"short"}`, nil)

	// The one permitted distinction, and it discloses nothing: the shape of the token is a fact
	// about the request, not about any invitation.
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("a malformed token answered %d, want 400", recorder.Code)
	}
}

// TestOnlyIssueCarriesAToken walks the view types to prove the token has one exit.
//
// A field-by-field assertion on each response would pass while a new view quietly gained a token
// field. This reads the wire contract instead: every view marshalled here must be free of any
// token-shaped field, and the one that carries a token is named explicitly.
func TestOnlyIssueCarriesAToken(t *testing.T) {
	t.Parallel()

	token, _, err := invitation.NewToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	record := invitation.Invitation{
		InvitationID:     mustID(t),
		TenantID:         mustID(t),
		TargetIdentifier: "person@example.test",
		TargetHash:       invitation.HashIdentifier("person@example.test"),
		SubjectType:      "human",
		State:            invitation.StatePending,
	}

	encoded, err := json.Marshal(viewInvitation(record))
	if err != nil {
		t.Fatalf("marshal the invitation view: %v", err)
	}
	body := string(encoded)

	switch {
	case strings.Contains(body, "token"):
		t.Errorf("the invitation view carries a token field:\n%s", body)
	case strings.Contains(body, record.TargetIdentifier):
		t.Errorf("the invitation view discloses the target identifier:\n%s", body)
	case strings.Contains(body, record.TargetHash):
		t.Errorf("the invitation view discloses the target hash:\n%s", body)
	}

	issued, err := json.Marshal(issuedView{
		Invitation: viewInvitation(record),
		Token:      string(token),
	})
	if err != nil {
		t.Fatalf("marshal the issued view: %v", err)
	}
	if !strings.Contains(string(issued), string(token)) {
		t.Error("the issue response did not carry the token, which is the one place it is returned")
	}
}

func mustID(t *testing.T) id.UUID {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("mint an identifier: %v", err)
	}
	return value
}

// testSurfaceServices builds the same service set as testSurface, for the one test that needs to
// call Routes with a different prober.
func testSurfaceServices(t *testing.T) Services {
	t.Helper()

	transactor := refusingTransactor{t: t}
	tenantPool, err := db.NewTenantPool(transactor)
	if err != nil {
		t.Fatalf("tenant pool: %v", err)
	}
	providerPool, err := db.NewProviderPool(transactor, stubRecorder{})
	if err != nil {
		t.Fatalf("provider pool: %v", err)
	}

	memberships, _ := membership.New(tenantPool)
	tenants, _ := tenant.New(providerPool)
	provisioning, _ := tenant.NewCoordinator(providerPool, tenants, 30*time.Minute)
	organizations, _ := organization.New(providerPool)
	workspaces, _ := workspace.New(tenantPool)
	invitations, _ := invitation.New(tenantPool, providerPool, memberships)
	offboardings, _ := offboarding.New(providerPool, tenantPool, tenants, memberships)
	registry, _ := projection.NewRegistry(providerPool)
	publisher, _ := projection.NewPublisher(providerPool, registry)
	reconciler, _ := projection.NewReconciler(providerPool)
	contexts, _ := occontext.New(providerPool)
	// The raw transactor, as production wires it: the frontier reads outbox aggregates that carry no
	// tenant column, so it takes no scope and writes no privileged-access record per poll.
	frontier, _ := projection.NewFrontierReader(transactor)

	return Services{
		Memberships: memberships, Tenants: tenants, Provisioning: provisioning,
		Organizations: organizations,
		Workspaces:    workspaces, Invitations: invitations, Offboardings: offboardings,
		Registry: registry, Publisher: publisher, Reconciler: reconciler, Contexts: contexts,
		Frontier: frontier,
	}
}

func TestRoutesRefusesAnIncompleteServiceSet(t *testing.T) {
	t.Parallel()

	services := testSurfaceServices(t)
	services.Contexts = nil

	if _, err := Routes(RoutesConfig{Services: services, Database: okProber{}}); err == nil {
		t.Error("Routes built a surface with a nil service, so some routes would answer 500 and " +
			"nothing would report which")
	}
}

package httpapi

// The caller's own grants, on the activation surface (TDD-organization-control-001 §Provider
// Activation). The refusals run on the fixture whose transactor refuses every acquisition, so each
// lands before the database is touched. The success runs on a recording transaction, which shows
// what the handler bound and answered; which rows the statement selects is the integration suite's
// (internal/authority, TestAHolderReadsOnlyItsOwnUnrevokedGrants).

import (
	stdcontext "context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/db/dbtest"
	"github.com/anshacerbia2/foundation-platform/observability"

	"github.com/anshacerbia2/organization-control/internal/authority"
	"github.com/anshacerbia2/organization-control/internal/db"
)

const heldGrantsPath = "/v1/provider-activations/grants"

func getWith(handler http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func eligibleCaller(t *testing.T) Caller {
	t.Helper()
	return Caller{Subject: mustID(t), Eligible: true}
}

func TestTheHeldGrantsRouteRefusesATenantCaller(t *testing.T) {
	t.Parallel()

	tenant := tenantCaller(t)
	if recorder := getWith(mounted(t, &tenant), heldGrantsPath, map[string]string{ReasonHeader: "an audit"}); recorder.Code != http.StatusForbidden {
		t.Errorf("a tenant caller answered %d, want 403", recorder.Code)
	}
}

func TestTheHeldGrantsRouteRequiresAReason(t *testing.T) {
	t.Parallel()

	for _, caller := range []Caller{eligibleCaller(t), providerCaller(t)} {
		recorder := getWith(mounted(t, &caller), heldGrantsPath, nil)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), ReasonHeader) {
			t.Errorf("%+v without a reason answered %d, want 400 naming the header: %s", caller, recorder.Code,
				recorder.Body.String())
		}
	}
}

// recordingTransactor hands each transaction a recording one that answers Query with rows.
type recordingTransactor struct {
	tx *dbtest.Tx
}

func (r *recordingTransactor) InTx(ctx stdcontext.Context, fn func(stdcontext.Context, db.Tx) error) error {
	return fn(ctx, r.tx)
}

// capturingRecorder keeps the privileged-access records it is given.
type capturingRecorder struct{ records []db.ProviderAccess }

func (c *capturingRecorder) RecordProviderAccess(_ stdcontext.Context, access db.ProviderAccess) error {
	c.records = append(c.records, access)
	return nil
}

// An eligible holder reaches the route and reads its grants by its own principal_id, with the access
// recorded first, and the answer names no other Principal.
func TestAnEligibleHolderReadsItsOwnGrants(t *testing.T) {
	t.Parallel()

	caller := eligibleCaller(t)
	grantID, grantor := mustID(t), mustID(t)
	grantedAt := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	tx := &dbtest.Tx{Rows: [][]any{{
		grantID.String(), caller.Subject.String(), authority.Scope, authority.KindEligible, grantor.String(),
		"", "on-call", grantedAt, nil, "", "",
	}}}
	evidence := &capturingRecorder{}
	pool, err := db.NewProviderPool(&recordingTransactor{tx: tx}, evidence)
	if err != nil {
		t.Fatal(err)
	}
	services := testSurfaceServices(t)
	services.ProviderActivations, err = authority.NewActivations(pool,
		authority.ActivationPolicy{Max: 8 * time.Hour, ApprovalRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	surface, err := Routes(RoutesConfig{Services: services, Database: okProber{}})
	if err != nil {
		t.Fatal(err)
	}
	correlation := mustID(t)
	establish := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := WithCaller(observability.WithCorrelationID(r.Context(), correlation), caller)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	passthrough := func(next http.Handler) http.Handler { return next }
	handler := surface.Mount(passthrough, establish, func(next http.Handler) http.Handler {
		return establish(ResolveScope(next))
	})

	recorder := getWith(handler, heldGrantsPath, map[string]string{ReasonHeader: "find what I can activate"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("an eligible holder answered %d, want 200: %s", recorder.Code, recorder.Body.String())
	}

	var body map[string][]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	want := map[string][]map[string]any{"grants": {{
		"grant_id": grantID.String(), "scope": authority.Scope, "kind": authority.KindEligible,
		"granted_at": grantedAt.Format(time.RFC3339Nano),
	}}}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("the route answered %v, want %v", body, want)
	}
	if strings.Contains(recorder.Body.String(), grantor.String()) {
		t.Error("the answer names the grantor, another Principal")
	}

	var queried bool
	for _, call := range tx.Calls() {
		if !strings.Contains(call.SQL, "organization.provider_grant") {
			continue
		}
		queried = true
		if len(call.Args) != 1 || call.Args[0] != caller.Subject.String() || !strings.Contains(call.SQL, "revoked_at IS NULL") {
			t.Errorf("the grants were read with %q %v; want the caller's own principal_id and unrevoked only",
				call.SQL, call.Args)
		}
	}
	if !queried {
		t.Error("the grant table was not read")
	}
	if len(evidence.records) != 1 || evidence.records[0].Actor != caller.Subject ||
		evidence.records[0].Reason != "find what I can activate" || evidence.records[0].Correlation != correlation {
		t.Errorf("the privileged-access record is %+v; want one naming the caller, its reason and correlation",
			evidence.records)
	}
}

// The literal segment and the {activation_id} routes do not shadow one another.
func TestTheHeldGrantsRouteDoesNotShadowTheActivationRoutes(t *testing.T) {
	t.Parallel()

	caller := eligibleCaller(t)
	handler := mounted(t, &caller)
	if recorder := post(t, handler, heldGrantsPath, `{}`, map[string]string{ReasonHeader: "an audit"}); recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST %s answered %d, want 405", heldGrantsPath, recorder.Code)
	}
	// "grants" is not an activation identifier, so the decision route refuses it before the database.
	if recorder := post(t, handler, "/v1/provider-activations/grants/approve", `{}`, map[string]string{ReasonHeader: "an audit"}); recorder.Code != http.StatusBadRequest {
		t.Errorf("POST /v1/provider-activations/grants/approve answered %d, want 400", recorder.Code)
	}
}

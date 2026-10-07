package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anshacerbia2/organization-control/internal/offboarding"
	"github.com/anshacerbia2/organization-control/internal/projection"
	"github.com/anshacerbia2/organization-control/internal/tenant"
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
		{"an unknown offboarding stage", provider, "/v1/offboardings?stage=paused", audit, "stage"},
		{"tenant_id not a UUID", provider, "/v1/offboardings?tenant_id=acme", audit, "tenant_id"},
		{"an offboarding limit of zero", provider, "/v1/offboardings?limit=0", audit, "limit"},
		{"a parameter the offboarding list does not take", provider, "/v1/offboardings?status=freeze", audit, "status"},
		{"an offboarding filter given twice", provider, "/v1/offboardings?stage=freeze&stage=release", audit, "more than once"},
		{"an unknown consumer state", provider, "/v1/projections/consumers?state=stale", audit, "state"},
		{"a consumer limit of zero", provider, "/v1/projections/consumers?limit=0", audit, "limit"},
		{"a consumer limit above the bound", provider, "/v1/projections/consumers?limit=101", audit, "limit"},
		{"a parameter the consumer list does not take", provider, "/v1/projections/consumers?status=active", audit, "status"},
		{"a consumer cursor given twice", provider, "/v1/projections/consumers?after=a&after=b", audit, "more than once"},
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
		{"a tenant caller on the offboarding list", tenant, "/v1/offboardings", audit, http.StatusForbidden},
		{"the offboarding list without a reason", provider, "/v1/offboardings", nil, http.StatusBadRequest},
		{"a tenant caller on the consumer list", tenant, "/v1/projections/consumers", audit, http.StatusForbidden},
		{"the consumer list without a reason", provider, "/v1/projections/consumers", nil, http.StatusBadRequest},
		{"a tenant caller on an obligation board", tenant, "/v1/offboardings/" + mustID(t).String() + "/obligations", audit, http.StatusForbidden},
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

// TestTheReadSideShapesNameWhatIsNotYetReached holds the 1.7.0 additions to their contract: the new
// offboarding fields are present and null until reached, the obligation's resolver is omitted while
// open, and the single Tenant read carries the record flat beside its two computed fields.
func TestTheReadSideShapesNameWhatIsNotYetReached(t *testing.T) {
	t.Parallel()

	marshal := func(value any) string {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(raw)
	}

	begun := marshal(viewOffboarding(offboarding.Offboarding{Stage: offboarding.StageFreeze, ActiveMemberships: 4}))
	for _, want := range []string{`"obligations_at":null`, `"released_at":null`, `"deprovisioning":null`, `"active_memberships":4`} {
		if !strings.Contains(begun, want) {
			t.Errorf("a begun offboarding lacks %s:\n%s", want, begun)
		}
	}
	if strings.Contains(begun, `"frozen_at"`) || strings.Contains(begun, `"retired_at"`) {
		t.Errorf("the stamps that were omitted before 1.7.0 are now present:\n%s", begun)
	}

	frozenAt := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	detail := "no status"
	released := marshal(viewOffboarding(offboarding.Offboarding{
		Stage: offboarding.StageRelease, FrozenAt: &frozenAt,
		Deprovisioning: &offboarding.Deprovisioning{State: "unresolved", Detail: &detail, RequestedAt: frozenAt},
	}))
	for _, want := range []string{`"obligations_at":"2026-10-07T12:00:00Z"`, `"frozen_at":"2026-10-07T12:00:00Z"`,
		`"deprovisioning":{"state":"unresolved","detail":"no status","requested_at":"2026-10-07T12:00:00Z","resolved_at":null}`} {
		if !strings.Contains(released, want) {
			t.Errorf("a released offboarding lacks %s:\n%s", want, released)
		}
	}

	open := marshal(viewObligation(offboarding.Obligation{State: offboarding.ObligationOpen}))
	if strings.Contains(open, "resolved_") {
		t.Errorf("an open obligation names a resolution:\n%s", open)
	}

	tenantID := mustID(t)
	read := marshal(viewTenantDetail(tenant.Detail{Record: tenant.Record{TenantID: tenantID}, ActiveMemberships: 847}))
	for _, want := range []string{`"tenant_id":"` + tenantID.String() + `"`, `"offboarding_id":null`, `"active_memberships":847`,
		`"provisioning":null`} {
		if !strings.Contains(read, want) {
			t.Errorf("the Tenant read lacks %s:\n%s", want, read)
		}
	}

	requestID, correlationID := mustID(t), mustID(t)
	pending := marshal(viewTenantDetail(tenant.Detail{Record: tenant.Record{TenantID: tenantID},
		Provisioning: &tenant.ProvisioningRequest{RequestID: requestID, CorrelationID: correlationID,
			State: tenant.RequestUnresolved, Detail: &detail, RequestedAt: frozenAt, ResolvedAt: &frozenAt}}))
	want := `"provisioning":{"request_id":"` + requestID.String() + `","correlation_id":"` + correlationID.String() +
		`","state":"unresolved","detail":"no status","requested_at":"2026-10-07T12:00:00Z","resolved_at":"2026-10-07T12:00:00Z"}`
	if !strings.Contains(pending, want) {
		t.Errorf("the Tenant read lacks %s:\n%s", want, pending)
	}
	waiting := marshal(provisioningRequestView{State: "requested"})
	if !strings.Contains(waiting, `"detail":null`) || !strings.Contains(waiting, `"resolved_at":null`) {
		t.Errorf("an unresolved field is omitted rather than null:\n%s", waiting)
	}

	listed := marshal(viewListedConsumer(projection.ListedConsumer{State: projection.ConsumerActive, Stale: true}, time.Now()))
	for _, want := range []string{`"state":"active"`, `"stale":true`, `"event_types":[]`, `"max_accepted_age_seconds":0`} {
		if !strings.Contains(listed, want) {
			t.Errorf("a listed consumer lacks %s:\n%s", want, listed)
		}
	}
	if strings.Contains(listed, `"retired_at"`) {
		t.Errorf("an active consumer names a retirement:\n%s", listed)
	}
}

// TestTheContextListKeepsItsCallers is ADR-ORG-005 §5.1 at the routes: a self caller is answered
// without a reason, a provider needs one, and a tenant caller, an eligible provider and a consumer
// reading another person's contexts are refused before the database. The parameters are the list's.
func TestTheContextListKeepsItsCallers(t *testing.T) {
	t.Parallel()

	principal := mustID(t)
	path := "/v1/principals/" + principal.String() + "/contexts"
	audit := map[string]string{ReasonHeader: "an audit"}
	self := Caller{Subject: principal, Self: true}
	eligible := providerCaller(t)
	eligible.Provider, eligible.Eligible = false, true

	for _, tc := range []struct {
		name    string
		caller  Caller
		path    string
		headers map[string]string
		status  int
	}{
		{"a tenant caller on another person's", tenantCaller(t), path, audit, http.StatusForbidden},
		{"an eligible provider on another person's", eligible, path, audit, http.StatusForbidden},
		{"a consumer on another person's", consumerCallerFixture(t, "foundation-reference"), path, audit, http.StatusForbidden},
		{"a provider without a reason", providerCaller(t), path, nil, http.StatusBadRequest},
		{"a provider naming no UUID", providerCaller(t), "/v1/principals/nobody/contexts", audit, http.StatusBadRequest},
		{"a self limit of zero", self, path + "?limit=0", nil, http.StatusBadRequest},
		{"a self cursor that is not a UUID", self, path + "?after=x", nil, http.StatusBadRequest},
		{"a filter the list does not take", self, path + "?status=active", nil, http.StatusBadRequest},
		{"a self caller on someone else's", Caller{Subject: mustID(t), Self: true}, path, nil, http.StatusForbidden},
	} {
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

// TestTheCancelRouteNamesAVersionAndAReason is ADR-ORG-006 §5.1 at the route: a provider command
// with the Tenant version and a reason, refused before the database without either, and refused to
// a tenant caller. The view carries the cancellation, present and null until it happens.
func TestTheCancelRouteNamesAVersionAndAReason(t *testing.T) {
	t.Parallel()

	path := "/v1/offboardings/" + mustID(t).String() + "/cancel"
	audit := map[string]string{ReasonHeader: "begun by mistake"}
	for _, tc := range []struct {
		name    string
		caller  Caller
		body    string
		headers map[string]string
		status  int
	}{
		{"a tenant caller", tenantCaller(t), `{"expected_version":3}`, audit, http.StatusForbidden},
		{"no reason", providerCaller(t), `{"expected_version":3}`, nil, http.StatusBadRequest},
		{"no body", providerCaller(t), ``, audit, http.StatusBadRequest},
		{"a misspelled version", providerCaller(t), `{"expected_versoin":3}`, audit, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			caller := tc.caller
			recorder := post(t, mounted(t, &caller), path, tc.body, tc.headers)
			if recorder.Code != tc.status {
				t.Fatalf("answered %d, want %d:\n%s", recorder.Code, tc.status, recorder.Body.String())
			}
		})
	}

	raw, err := json.Marshal(viewOffboarding(offboarding.Offboarding{Stage: offboarding.StageFreeze}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"prior_status":null`, `"cancelled_by":null`, `"cancel_reason":null`,
		`"cancelled_at":null`, `"frozen_memberships":0`, `"restore_pending":0`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("a begun offboarding lacks %s:\n%s", want, raw)
		}
	}
	raw, _ = json.Marshal(viewOffboarding(offboarding.Offboarding{Stage: offboarding.StageCancelled, PriorStatus: "suspended"}))
	if !strings.Contains(string(raw), `"prior_status":"suspended"`) || !strings.Contains(string(raw), `"stage":"cancelled"`) {
		t.Errorf("a cancelled offboarding reads:\n%s", raw)
	}
}

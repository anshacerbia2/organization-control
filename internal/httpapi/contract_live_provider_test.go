package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"
)

// version reads a decoded body's version as the expected_version of the next command.
func version(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	value, ok := body["version"].(float64)
	for _, field := range body {
		if nested, isObject := field.(map[string]any); !ok && isObject {
			value, ok = nested["version"].(float64)
		}
	}
	if !ok {
		t.Fatalf("the body has no version: %v", body)
	}
	return map[string]any{"expected_version": int64(value)}
}

// An Organization and a Tenant through their whole provider lifecycle.
func TestLiveOrganizationAndTenantLifecycle(t *testing.T) {
	l := liveSurface(t)
	provider := l.emergencyProvider()

	org := l.call(provider, http.MethodPost, "/v1/organizations",
		map[string]any{"display_name": "Contract Org " + mustID(t).String(), "classification": "customer"}, http.StatusCreated)
	orgPath := "/v1/organizations/" + str(t, org, "organization_id")
	l.call(provider, http.MethodGet, "/v1/organizations?limit=2", nil, http.StatusOK)
	org = l.call(provider, http.MethodGet, orgPath, nil, http.StatusOK)
	org = l.call(provider, http.MethodPost, orgPath+"/suspend", version(t, org), http.StatusOK)
	org = l.call(provider, http.MethodPost, orgPath+"/restore", version(t, org), http.StatusOK)

	requested := l.call(provider, http.MethodPost, "/v1/tenants", map[string]any{
		"organization_id": str(t, org, "organization_id"), "display_name": "Contract Tenant", "isolation_profile": "pooled",
	}, http.StatusCreated)
	tenantPath := "/v1/tenants/" + str(t, obj(t, requested, "tenant"), "tenant_id")
	l.call(provider, http.MethodGet, "/v1/tenants?limit=2", nil, http.StatusOK)
	l.call(provider, http.MethodPost, "/v1/provisioning/failed",
		map[string]any{"correlation_id": str(t, requested, "correlation_id"), "detail": "a contract test"}, http.StatusOK)
	ten := l.call(provider, http.MethodGet, tenantPath, nil, http.StatusOK)
	l.call(provider, http.MethodPost, tenantPath+"/provisioning", version(t, ten), http.StatusOK)
	ten = l.call(provider, http.MethodGet, tenantPath, nil, http.StatusOK)
	l.call(provider, http.MethodPost, "/v1/provisioning/realized",
		map[string]any{"correlation_id": str(t, obj(t, ten, "provisioning"), "correlation_id")}, http.StatusOK)
	ten = l.call(provider, http.MethodGet, tenantPath, nil, http.StatusOK)
	ten = l.call(provider, http.MethodPost, tenantPath+"/activate", version(t, ten), http.StatusOK)
	ten = l.call(provider, http.MethodPost, tenantPath+"/suspend", version(t, ten), http.StatusOK)
	l.call(provider, http.MethodPost, tenantPath+"/restore", version(t, ten), http.StatusOK)
	l.call(provider, http.MethodPost, "/v1/provisioning/sweep-unresolved", map[string]any{"size": 1}, http.StatusOK)

	retired := l.call(provider, http.MethodPost, "/v1/organizations",
		map[string]any{"display_name": "Contract Org " + mustID(t).String(), "classification": "partner"}, http.StatusCreated)
	l.call(provider, http.MethodPost, "/v1/organizations/"+str(t, retired, "organization_id")+"/retire", version(t, retired), http.StatusOK)
}

// A provider acting inside one Tenant: its administrators and one Membership.
func TestLiveProviderInsideATenant(t *testing.T) {
	l := liveSurface(t)
	provider := l.emergencyProvider()
	tenantID := l.seedTenant()
	tenantPath := "/v1/tenants/" + tenantID
	administrator := Caller{Subject: mustID(t), Tenant: id.MustParse(tenantID)}

	member := obj(t, l.call(administrator, http.MethodPost, "/v1/memberships", map[string]any{
		"principal_id": mustID(t).String(), "subject_type": "human", "provenance": "contract test",
		"valid_from": time.Now().UTC().Format(time.RFC3339),
	}, http.StatusCreated), "membership")
	memberPath := tenantPath + "/memberships/" + str(t, member, "membership_id")
	member = l.call(provider, http.MethodGet, memberPath, nil, http.StatusOK)
	member = l.call(provider, http.MethodPost, memberPath+"/suspend", version(t, member), http.StatusOK)
	l.call(provider, http.MethodGet, memberPath+"/enforcement", nil, http.StatusOK)
	l.call(provider, http.MethodPost, memberPath+"/revoke", version(t, member), http.StatusOK)

	granted := l.call(provider, http.MethodPost, tenantPath+"/administrators",
		map[string]any{"principal_id": mustID(t).String()}, http.StatusCreated)
	l.call(provider, http.MethodGet, tenantPath+"/administrators", nil, http.StatusOK)
	l.call(provider, http.MethodPost, tenantPath+"/administrators/"+str(t, granted, "grant_id")+"/revoke", nil, http.StatusOK)
}

// Provider grants, an activation through approval to its end and another denied, the
// privileged-access record and its review, and the Tenant administration pause.
func TestLiveProviderAuthority(t *testing.T) {
	l := liveSurface(t)
	provider := l.emergencyProvider()

	holder := mustID(t)
	grant := l.call(provider, http.MethodPost, "/v1/provider-grants",
		map[string]any{"principal_id": holder.String(), "scope": "provider:organization-control"}, http.StatusCreated)
	l.call(provider, http.MethodGet, "/v1/provider-grants", nil, http.StatusOK)
	l.call(provider, http.MethodGet, "/v1/provider-grants:emergency-validation", nil, http.StatusOK)

	eligible := Caller{Subject: holder, Eligible: true}
	l.call(eligible, http.MethodGet, "/v1/provider-activations/grants", nil, http.StatusOK)
	request := func() string {
		return str(t, l.call(eligible, http.MethodPost, "/v1/provider-activations",
			map[string]any{"grant_id": str(t, grant, "grant_id"), "duration_seconds": 600}, http.StatusCreated), "activation_id")
	}
	denied := request()
	l.call(provider, http.MethodGet, "/v1/provider-activations", nil, http.StatusOK)
	l.call(provider, http.MethodPost, "/v1/provider-activations/"+denied+"/deny", nil, http.StatusOK)
	approved := request()
	l.call(provider, http.MethodPost, "/v1/provider-activations/"+approved+"/approve", nil, http.StatusOK)
	l.call(eligible, http.MethodPost, "/v1/provider-activations/"+approved+"/end", nil, http.StatusOK)
	l.call(provider, http.MethodPost, "/v1/provider-grants/"+str(t, grant, "grant_id")+"/revoke", nil, http.StatusOK)

	l.call(provider, http.MethodGet, "/v1/privileged-access?limit=2", nil, http.StatusOK)
	l.call(provider, http.MethodGet, "/v1/privileged-access:unreviewed", nil, http.StatusOK)
	now := time.Now().UTC()
	l.call(provider, http.MethodPost, "/v1/privileged-access/reviews", map[string]any{
		"actor_id": holder.String(), "from": now.Add(-time.Hour).Format(time.RFC3339), "to": now.Format(time.RFC3339),
		"outcome": "appropriate",
	}, http.StatusCreated)
	l.call(provider, http.MethodGet, "/v1/privileged-access/reviews?limit=2", nil, http.StatusOK)

	l.call(provider, http.MethodGet, "/v1/tenant-administration-pause", nil, http.StatusOK)
	l.call(provider, http.MethodPost, "/v1/tenant-administration-pause", map[string]any{"paused": true}, http.StatusOK)
	l.call(provider, http.MethodPost, "/v1/tenant-administration-pause", map[string]any{"paused": false}, http.StatusOK)
}

// A Tenant offboarded from the request through the freeze, its obligations and deprovisioning, to
// release and retirement; another cancelled.
func TestLiveOffboarding(t *testing.T) {
	l := liveSurface(t)
	provider := l.emergencyProvider()

	// The commands name the Tenant's version.
	tenantVersion := func(tenantID string) map[string]any {
		return version(t, l.call(provider, http.MethodGet, "/v1/tenants/"+tenantID, nil, http.StatusOK))
	}
	begin := func() (string, string) {
		tenantID := l.seedTenant()
		started := l.call(provider, http.MethodPost, "/v1/offboardings",
			map[string]any{"tenant_id": tenantID, "expected_version": 1}, http.StatusCreated)
		return tenantID, "/v1/offboardings/" + str(t, started, "offboarding_id")
	}
	cancelledTenant, cancelledPath := begin()
	l.call(provider, http.MethodPost, cancelledPath+"/cancel", tenantVersion(cancelledTenant), http.StatusOK)

	tenantID, path := begin()
	l.call(provider, http.MethodGet, "/v1/offboardings?limit=2", nil, http.StatusOK)
	l.call(provider, http.MethodGet, path, nil, http.StatusOK)
	l.call(provider, http.MethodPost, path+"/freeze", map[string]any{"size": 10}, http.StatusOK)
	l.call(provider, http.MethodPost, path+"/complete-freeze", nil, http.StatusOK)
	obligation := l.call(provider, http.MethodPost, path+"/obligations",
		map[string]any{"domain": "billing", "type": "final-invoice"}, http.StatusCreated)
	l.call(provider, http.MethodGet, path+"/obligations", nil, http.StatusOK)
	billing := mustID(t)
	reporter := l.call(provider, http.MethodPost, "/v1/reporters", map[string]any{
		"principal_id": billing.String(), "reports": "obligation:billing", "purpose": "contract test",
	}, http.StatusCreated)
	domain := Caller{Subject: billing, Reporter: id.MustParse(str(t, reporter, "reporter_id")), Reports: "obligation:billing"}
	l.call(domain, http.MethodPost, "/v1/obligations/"+str(t, obligation, "obligation_id")+"/resolve",
		map[string]any{"domain": "billing", "state": "completed"}, http.StatusOK)
	l.call(provider, http.MethodPost, path+"/legal-hold", map[string]any{"hold": true}, http.StatusOK)
	l.call(provider, http.MethodPost, path+"/legal-hold", map[string]any{"hold": false}, http.StatusOK)
	l.call(provider, http.MethodPost, path+"/release", nil, http.StatusOK)
	l.call(provider, http.MethodPost, path+"/deprovisioning", map[string]any{"state": "failed", "detail": "a contract test"}, http.StatusNoContent)
	l.call(provider, http.MethodPost, path+"/deprovisioning/resend", nil, http.StatusOK)
	l.call(provider, http.MethodPost, path+"/deprovisioning", map[string]any{"state": "realized"}, http.StatusNoContent)
	l.call(provider, http.MethodPost, path+"/retire", tenantVersion(tenantID), http.StatusOK)
}

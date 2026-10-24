package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"
)

// A Tenant administrator's Workspaces, Memberships and batches.
func TestLiveTenantAdministration(t *testing.T) {
	l := liveSurface(t)
	caller := Caller{Subject: mustID(t), Tenant: id.MustParse(l.seedTenant())}

	space := l.call(caller, http.MethodPost, "/v1/workspaces", map[string]any{"display_name": "Contract", "type": "project"}, http.StatusCreated)
	spacePath := "/v1/workspaces/" + str(t, space, "workspace_id")
	l.call(caller, http.MethodGet, "/v1/workspaces?limit=2", nil, http.StatusOK)
	space = l.call(caller, http.MethodGet, spacePath, nil, http.StatusOK)

	grant := func(workspace bool) map[string]any {
		body := map[string]any{"principal_id": mustID(t).String(), "subject_type": "human",
			"provenance": "contract test", "valid_from": time.Now().UTC().Format(time.RFC3339)}
		if workspace {
			body["workspace_id"] = str(t, space, "workspace_id")
			body["valid_until"] = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
		}
		return l.call(caller, http.MethodPost, "/v1/memberships", body, http.StatusCreated)
	}
	member := grant(true)
	memberPath := "/v1/memberships/" + str(t, obj(t, member, "membership"), "membership_id")
	l.call(caller, http.MethodGet, "/v1/memberships?limit=2", nil, http.StatusOK)
	member = l.call(caller, http.MethodGet, memberPath, nil, http.StatusOK)
	str(t, member, "workspace_id")
	str(t, member, "valid_until")
	l.call(caller, http.MethodGet, memberPath+"/enforcement", nil, http.StatusOK)
	member = l.call(caller, http.MethodPost, memberPath+"/suspend", version(t, member), http.StatusOK)
	member = l.call(caller, http.MethodPost, memberPath+"/restore", version(t, member), http.StatusOK)
	l.call(caller, http.MethodPost, memberPath+"/revoke", version(t, member), http.StatusOK)

	// A Tenant-wide Membership with no end: both members are null, never the nil UUID or the zero
	// time, in the command's answer and in the read (STD-GLB-001 2.0.0, ROADMAP item 67).
	bare := obj(t, grant(false), "membership")
	requireNull(t, bare, "workspace_id")
	requireNull(t, bare, "valid_until")
	barePath := "/v1/memberships/" + str(t, bare, "membership_id")
	read := l.call(caller, http.MethodGet, barePath, nil, http.StatusOK)
	requireNull(t, read, "workspace_id")
	requireNull(t, read, "valid_until")

	batch := l.call(caller, http.MethodPost, "/v1/membership-batches", map[string]any{
		"action": "suspend", "membership_ids": []string{str(t, bare, "membership_id")},
	}, http.StatusCreated)
	batchPath := "/v1/membership-batches/" + str(t, batch, "batch_id")
	l.call(caller, http.MethodGet, batchPath, nil, http.StatusOK)
	l.call(caller, http.MethodPost, batchPath+"/execute", map[string]any{}, http.StatusOK)

	member = l.call(caller, http.MethodGet, memberPath, nil, http.StatusOK)
	space = l.call(caller, http.MethodPost, spacePath+"/archive", version(t, space), http.StatusOK)
	space = l.call(caller, http.MethodPost, spacePath+"/restore", version(t, space), http.StatusOK)
	space = l.call(caller, http.MethodPost, spacePath+"/archive", version(t, space), http.StatusOK)
	l.call(caller, http.MethodPost, spacePath+"/retire", version(t, space), http.StatusOK)

	l.call(caller, http.MethodGet, "/v1/provider-access?limit=2", nil, http.StatusOK)
}

// An invitation issued, read, resolved for an enrolment, accepted, and another revoked; the reporter
// that resolves it registered and retired.
func TestLiveInvitationsAndReporters(t *testing.T) {
	l := liveSurface(t)
	provider := l.emergencyProvider()
	caller := Caller{Subject: mustID(t), Tenant: id.MustParse(l.seedTenant())}

	issue := func() map[string]any {
		return l.call(caller, http.MethodPost, "/v1/invitations", map[string]any{
			"target_identifier": "invitee-" + mustID(t).String() + "@example.test", "subject_type": "human",
		}, http.StatusCreated)
	}
	issued := issue()
	l.call(caller, http.MethodGet, "/v1/invitations?limit=2", nil, http.StatusOK)
	l.call(caller, http.MethodGet, "/v1/invitations/"+str(t, obj(t, issued, "invitation"), "invitation_id"), nil, http.StatusOK)
	revoked := issue()
	l.call(caller, http.MethodPost, "/v1/invitations/"+str(t, obj(t, revoked, "invitation"), "invitation_id")+"/revoke", nil, http.StatusOK)

	principal := mustID(t)
	reporter := l.call(provider, http.MethodPost, "/v1/reporters", map[string]any{
		"principal_id": principal.String(), "reports": "invitation:resolve", "purpose": "contract test",
	}, http.StatusCreated)
	reporterPath := "/v1/reporters/" + str(t, reporter, "reporter_id")
	l.call(provider, http.MethodGet, "/v1/reporters?limit=2", nil, http.StatusOK)
	reporter = l.call(provider, http.MethodGet, reporterPath, nil, http.StatusOK)
	resolver := Caller{Subject: principal, Reporter: id.MustParse(str(t, reporter, "reporter_id")), Reports: "invitation:resolve"}
	l.call(resolver, http.MethodPost, "/v1/invitations/resolve", map[string]any{"token": str(t, issued, "token")}, http.StatusOK)
	l.call(provider, http.MethodPost, reporterPath+"/retire", version(t, reporter), http.StatusOK)

	invitee := Caller{Subject: mustID(t), Self: true}
	l.call(invitee, http.MethodPost, "/v1/invitations/accept", map[string]any{"token": str(t, issued, "token")}, http.StatusOK)
	l.call(provider, http.MethodPost, "/v1/invitations/expire-lapsed", map[string]any{"size": 1}, http.StatusOK)
}

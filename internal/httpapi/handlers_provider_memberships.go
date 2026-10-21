package httpapi

import (
	stdcontext "context"
	"net/http"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/membership"
)

// A provider suspends or revokes one Membership inside the Tenant the path names (ADR-ORG-003 §5.6,
// decided 2026-10-20; TDD-organization-control-002 §A Provider Withdraws One Membership). It
// contains a withdrawal a restore to an older point lost, or a compromised member in a cross-tenant
// exposure. There is no provider route that restores or grants one.
//
// requireProvider admits a provider in force alone, by an approved activation or an emergency grant,
// and demands X-Administrative-Reason and an Idempotency-Key; the body names the Membership's
// expected_version. The privileged-access row names the Tenant, so its administrators see the act in
// GET /v1/provider-access. A Tenant administration pause does not hold it: the pause holds Tenant
// administrators, and this is how a provider re-applies a lost withdrawal during one.
func (h *handlers) providerMembershipTransition(w http.ResponseWriter, r *http.Request,
	apply func(stdcontext.Context, id.UUID, membership.Command) (membership.Result, error)) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	tenantID, ok := pathUUID(w, r, "tenant_id")
	if !ok {
		return
	}
	membershipID, ok := pathUUID(w, r, "membership_id")
	if !ok {
		return
	}
	body, ok := decode[membershipCommand](w, r)
	if !ok {
		return
	}
	cmd := membership.Command{MembershipID: membershipID, ExpectedVersion: body.ExpectedVersion, Reason: reason(r)}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (membership.Result, error) {
		return apply(ctx, tenantID, cmd)
	}, viewMembershipResult)
}

func (h *handlers) providerSuspendMembership(w http.ResponseWriter, r *http.Request) {
	h.providerMembershipTransition(w, r, h.services.Memberships.ProviderSuspend)
}

func (h *handlers) providerRevokeMembership(w http.ResponseWriter, r *http.Request) {
	h.providerMembershipTransition(w, r, h.services.Memberships.ProviderRevoke)
}

// providerGetMembership serves `GET /v1/tenants/{tenant_id}/memberships/{membership_id}`: the read the
// two commands above need, with their authority and no more (ADR-ORG-003 §5.6, amended 2026-10-21;
// TDD-organization-control-002 1.19.0). A provider in force and X-Administrative-Reason, recorded
// naming the Tenant; a safe GET, so requireProvider asks no Idempotency-Key of it. A Membership that is
// not in the path's Tenant answers 404, as an absent one does. It answers what the Tenant
// administrator's GET /v1/memberships/{membership_id} answers.
func (h *handlers) providerGetMembership(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	tenantID, ok := pathUUID(w, r, "tenant_id")
	if !ok {
		return
	}
	membershipID, ok := pathUUID(w, r, "membership_id")
	if !ok {
		return
	}
	record, err := h.services.Memberships.GetInTenant(r.Context(), tenantID, membershipID, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewMembership(record))
}

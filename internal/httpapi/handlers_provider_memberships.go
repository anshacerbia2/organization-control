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

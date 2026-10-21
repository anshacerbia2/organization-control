package httpapi

import (
	"errors"
	"net/http"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"

	"github.com/anshacerbia2/organization-control/internal/invitation"
)

// POST /v1/invitations/resolve: identity-control asks, as its workload, which invitation a token
// names, for an enrolment (ADR-IAM-011 §5.1, §5.7; TDD-organization-control-004 1.18.0).

type resolveInvitationRequest struct {
	Token string `json:"token"`
}

// resolvedInvitationView is the whole answer: the invitation's identifier and its target address. It
// names no Tenant, no Workspace and no state (ADR-IAM-011 §5.1).
type resolvedInvitationView struct {
	InvitationID     string `json:"invitation_id"`
	TargetIdentifier string `json:"target_identifier"`
}

// unresolvedDetail is the one detail of the one refusal: the type, status and title are the
// registry's, the instance is the route, and nothing comes from a row (ADR-IAM-011 §5.7 (b)).
const unresolvedDetail = "The invitation cannot be accepted"

func (h *handlers) resolveInvitation(w http.ResponseWriter, r *http.Request) {
	// identity-control's registered workload alone, from its invitation:resolve entry (ADR-IAM-011
	// §5.7 (a)). Authentication admits no other reporter here and resolves this one to the resolver
	// scope; a provider or anyone else who reached the handler is refused before the body is read.
	caller, _ := CallerFrom(r.Context())
	if caller.Reporter.IsNil() || !caller.Reports.InvitationResolve() {
		platform.Problem(w, r, platform.Forbidden,
			"An invitation token is resolved by identity-control's registered workload alone")
		return
	}
	body, ok := decode[resolveInvitationRequest](w, r)
	if !ok {
		return
	}

	resolved, err := h.services.InvitationResolver.Resolve(r.Context(), invitation.Token(body.Token))
	switch {
	case errors.Is(err, invitation.ErrNotAcceptable):
		// Unknown, expired, revoked or accepted: one answer (ADR-IAM-011 §5.7 (b)).
		platform.Problem(w, r, platform.InvitationNotAcceptable, unresolvedDetail)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	// The address is personal data, so no cache keeps it (RFC 9111 §5.2.2.5).
	w.Header().Set("Cache-Control", "no-store")
	respond(w, http.StatusOK, resolvedInvitationView{
		InvitationID: resolved.InvitationID.String(), TargetIdentifier: resolved.TargetIdentifier})
}

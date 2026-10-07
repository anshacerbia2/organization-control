package httpapi

import (
	"net/http"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"

	occontext "github.com/anshacerbia2/organization-control/internal/context"
)

// contextView is one item of `GET /v1/principals/{principal_id}/contexts` (TDD-organization-control-002
// 1.12.0 §The Context List). Nothing else is returned: no version, no grant and no other person
// (ADR-ORG-005 §5.2).
type contextView struct {
	MembershipID      id.UUID  `json:"membership_id"`
	TenantID          id.UUID  `json:"tenant_id"`
	TenantDisplayName string   `json:"tenant_display_name"`
	TenantStatus      string   `json:"tenant_status"`
	WorkspaceID       *id.UUID `json:"workspace_id"`
	Administers       bool     `json:"administers"`
}

type contextPageView struct {
	Contexts []contextView `json:"contexts"`
	Next     *string       `json:"next"`
}

// listContexts serves a person's contexts to themselves, and anyone's to a provider.
//
// A self caller was admitted by authentication for this route and its own principal_id only, and
// reads as organization_self_rt with no privileged-access record. Every other caller must be a
// provider with authority in force and a reason, and the page records the access: a tenant caller
// reading another person's contexts, an eligible provider and a consumer are refused 403
// (ADR-ORG-005 §5.1).
func (h *handlers) listContexts(w http.ResponseWriter, r *http.Request) {
	principal, ok := pathUUID(w, r, "principal_id")
	if !ok {
		return
	}
	caller, _ := CallerFrom(r.Context())
	if caller.Self && caller.Subject != principal {
		// Unreachable while authentication admits a self caller only for its own principal_id; kept
		// so the handler does not rest its one rule on another file.
		platform.Problem(w, r, platform.Forbidden, "A person reads only their own contexts")
		return
	}
	if !caller.Self {
		if _, ok := requireProvider(w, r); !ok {
			return
		}
	}
	params, ok := readList(w, r, nil, nil)
	if !ok {
		return
	}
	query := occontext.ContextQuery{After: params.After, Limit: params.Limit}

	var (
		page occontext.ContextPage
		err  error
	)
	if caller.Self {
		page, err = h.services.ContextList.Own(r.Context(), query)
	} else {
		page, err = h.services.ContextList.Of(r.Context(), principal, query, reason(r))
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := contextPageView{Contexts: make([]contextView, 0, len(page.Contexts)), Next: nextCursor(page.Next)}
	for _, c := range page.Contexts {
		view.Contexts = append(view.Contexts, contextView{
			MembershipID: c.MembershipID, TenantID: c.TenantID, TenantDisplayName: c.TenantDisplayName,
			TenantStatus: c.TenantStatus, WorkspaceID: c.WorkspaceID, Administers: c.Administers,
		})
	}
	respond(w, http.StatusOK, view)
}

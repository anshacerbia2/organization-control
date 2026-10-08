package httpapi

import (
	stdcontext "context"
	"net/http"
	"strings"
	"time"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/authority"
)

// Tenant administration grants (ADR-ORG-003, TDD-organization-control-001 §The Tenant Administration
// Grant). Three provider routes, each requiring X-Administrative-Reason: the reason is the access
// record's and the grant's or the revocation's own. The Tenant in the path is the one a provider acts
// in, and it reaches a binding only through db.WithProviderInTenant, which refuses any other scope.

type tenantAdminGrantView struct {
	GrantID      string     `json:"grant_id"`
	TenantID     string     `json:"tenant_id"`
	PrincipalID  string     `json:"principal_id"`
	GrantedBy    string     `json:"granted_by"`
	Reason       string     `json:"reason"`
	GrantedAt    time.Time  `json:"granted_at"`
	Active       bool       `json:"active"`
	RevokedAt    *time.Time `json:"revoked_at"`
	RevokedBy    *string    `json:"revoked_by"`
	RevokeReason string     `json:"revoke_reason,omitempty"`
}

func viewTenantAdminGrant(g authority.TenantAdminGrant) tenantAdminGrantView {
	view := tenantAdminGrantView{
		GrantID: g.ID.String(), TenantID: g.TenantID.String(), PrincipalID: g.Principal.String(),
		GrantedBy: g.GrantedBy.String(), Reason: g.Reason, GrantedAt: g.GrantedAt, Active: g.RevokedAt == nil,
		RevokedAt: g.RevokedAt, RevokeReason: g.RevokeReason,
	}
	if g.RevokedBy != nil {
		text := g.RevokedBy.String()
		view.RevokedBy = &text
	}
	return view
}

// administratorView is a grant and the Membership it stands on.
type administratorView struct {
	tenantAdminGrantView
	MembershipID      string `json:"membership_id"`
	MembershipCreated bool   `json:"membership_created"`
}

func (h *handlers) listTenantAdministrators(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	tenantID, ok := pathUUID(w, r, "tenant_id")
	if !ok {
		return
	}
	grants, err := h.services.TenantAdministrators.List(r.Context(), tenantID, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	views := make([]tenantAdminGrantView, 0, len(grants))
	for _, grant := range grants {
		views = append(views, viewTenantAdminGrant(grant))
	}
	respond(w, http.StatusOK, map[string]any{"grants": views})
}

type grantTenantAdministratorRequest struct {
	PrincipalID string `json:"principal_id"`
}

func (h *handlers) grantTenantAdministrator(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	tenantID, ok := pathUUID(w, r, "tenant_id")
	if !ok {
		return
	}
	body, ok := decode[grantTenantAdministratorRequest](w, r)
	if !ok {
		return
	}
	principal, err := id.Parse(strings.TrimSpace(body.PrincipalID))
	if err != nil || principal.IsNil() {
		platform.Problem(w, r, platform.ValidationFailed, "principal_id is not a valid identifier")
		return
	}
	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (authority.Administrator, error) {
		return h.services.TenantAdministrators.Grant(ctx, tenantID, principal, reason(r))
	}, func(administrator authority.Administrator) administratorView {
		return administratorView{
			tenantAdminGrantView: viewTenantAdminGrant(administrator.Grant),
			MembershipID:         administrator.MembershipID.String(),
			MembershipCreated:    administrator.MembershipCreated,
		}
	})
}

func (h *handlers) revokeTenantAdministrator(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	tenantID, ok := pathUUID(w, r, "tenant_id")
	if !ok {
		return
	}
	grantID, ok := pathUUID(w, r, "grant_id")
	if !ok {
		return
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (authority.TenantAdminGrant, error) {
		return h.services.TenantAdministrators.Revoke(ctx, tenantID, grantID, reason(r))
	}, viewTenantAdminGrant)
}

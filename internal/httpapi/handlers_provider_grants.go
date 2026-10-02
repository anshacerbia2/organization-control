package httpapi

import (
	"net/http"
	"strings"
	"time"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/authority"
)

// Provider grants (ADR-ORG-001 §5.11, TDD-organization-control-001 §Caller Authority). Three
// provider routes, each requiring X-Administrative-Reason: the reason is the access record's and the
// grant's or the revocation's own.

type providerGrantView struct {
	GrantID           string     `json:"grant_id"`
	PrincipalID       string     `json:"principal_id"`
	Scope             string     `json:"scope"`
	Kind              string     `json:"kind"`
	GrantedBy         *string    `json:"granted_by"`
	BootstrapOperator string     `json:"bootstrap_operator,omitempty"`
	Reason            string     `json:"reason"`
	GrantedAt         time.Time  `json:"granted_at"`
	Active            bool       `json:"active"`
	RevokedAt         *time.Time `json:"revoked_at"`
	RevokedBy         *string    `json:"revoked_by"`
	RevokeReason      string     `json:"revoke_reason,omitempty"`
}

func viewProviderGrant(r authority.Record) providerGrantView {
	optional := func(value *id.UUID) *string {
		if value == nil {
			return nil
		}
		text := value.String()
		return &text
	}
	return providerGrantView{
		GrantID: r.ID.String(), PrincipalID: r.Principal.String(), Scope: r.Scope, Kind: r.Kind,
		GrantedBy: optional(r.GrantedBy), BootstrapOperator: r.BootstrapOperator, Reason: r.Reason,
		GrantedAt: r.GrantedAt, Active: r.RevokedAt == nil, RevokedAt: r.RevokedAt,
		RevokedBy: optional(r.RevokedBy), RevokeReason: r.RevokeReason,
	}
}

func (h *handlers) listProviderGrants(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	records, err := h.services.ProviderGrants.List(r.Context(), reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	views := make([]providerGrantView, 0, len(records))
	for _, record := range records {
		views = append(views, viewProviderGrant(record))
	}
	respond(w, http.StatusOK, map[string]any{"grants": views})
}

type grantProviderRequest struct {
	PrincipalID string `json:"principal_id"`

	// Kind is eligible, the default, or emergency (ADR-ORG-002 §5.2).
	Kind string `json:"kind"`
}

func (h *handlers) grantProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[grantProviderRequest](w, r)
	if !ok {
		return
	}
	principal, err := id.Parse(strings.TrimSpace(body.PrincipalID))
	if err != nil {
		platform.Problem(w, r, platform.ValidationFailed, "principal_id is not a valid identifier")
		return
	}
	kind := strings.TrimSpace(body.Kind)
	if kind == "" {
		kind = authority.KindEligible
	}
	record, err := h.services.ProviderGrants.Grant(r.Context(), principal, kind, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusCreated, viewProviderGrant(record))
}

func (h *handlers) revokeProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	grantID, ok := pathUUID(w, r, "grant_id")
	if !ok {
		return
	}
	record, err := h.services.ProviderGrants.Revoke(r.Context(), grantID, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewProviderGrant(record))
}

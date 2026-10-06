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

	// Scope is provider:organization-control or provider:identity-control. Required, with no
	// default: a grant that does not say what it grants is refused (TDD-organization-control-001
	// §Provider Authority Projection).
	Scope string `json:"scope"`

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
	record, err := h.services.ProviderGrants.Grant(r.Context(), principal, strings.TrimSpace(body.Scope), kind, reason(r))
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

// emergencyValidationView is one emergency grant of this service's scope and when it was last used
// (ADR-ORG-002 §5.2).
type emergencyValidationView struct {
	GrantID     string     `json:"grant_id"`
	PrincipalID string     `json:"principal_id"`
	GrantedAt   time.Time  `json:"granted_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	Uses        int64      `json:"uses"`
	DueAt       time.Time  `json:"due_at"`
	Overdue     bool       `json:"overdue"`
}

// emergencyValidation lists every unrevoked emergency grant of provider:organization-control with
// its last use, the oldest due first. A grant is validated by using it, and one unused for 90 days
// is overdue.
func (h *handlers) emergencyValidation(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	report, err := h.services.ProviderGrants.EmergencyValidation(r.Context(), reason(r), time.Now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	views := make([]emergencyValidationView, 0, len(report))
	for _, v := range report {
		views = append(views, emergencyValidationView{
			GrantID: v.GrantID.String(), PrincipalID: v.Principal.String(), GrantedAt: v.GrantedAt,
			LastUsedAt: v.LastUsedAt, Uses: v.Uses, DueAt: v.DueAt, Overdue: v.Overdue,
		})
	}
	respond(w, http.StatusOK, map[string]any{
		"scope":                  authority.Scope,
		"validation_period_days": int(authority.ValidationPeriod.Hours() / 24),
		"grants":                 views,
	})
}

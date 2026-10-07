package httpapi

import (
	"net/http"
	"strings"
	"time"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/authority"
)

// Provider activations (ADR-ORG-002, TDD-organization-control-001 §Provider Activation). The routes a
// grant holder reaches whether or not its authority is in force; each requires
// X-Administrative-Reason, which is the access record's and the activation's own.

type providerActivationView struct {
	ActivationID     string     `json:"activation_id"`
	GrantID          string     `json:"grant_id"`
	PrincipalID      string     `json:"principal_id"`
	Scope            string     `json:"scope"`
	Reason           string     `json:"reason"`
	DurationSeconds  int        `json:"duration_seconds"`
	ApprovalRequired bool       `json:"approval_required"`
	RequestedAt      time.Time  `json:"requested_at"`
	Decision         string     `json:"decision,omitempty"`
	DecidedBy        *string    `json:"decided_by"`
	DecisionReason   string     `json:"decision_reason,omitempty"`
	DecidedAt        *time.Time `json:"decided_at"`
	EndsAt           *time.Time `json:"ends_at"`
	InForce          bool       `json:"in_force"`
	EndedBy          *string    `json:"ended_by"`
	EndReason        string     `json:"end_reason,omitempty"`
	EndedAt          *time.Time `json:"ended_at"`
}

func viewProviderActivation(a authority.Activation) providerActivationView {
	optional := func(value *id.UUID) *string {
		if value == nil {
			return nil
		}
		text := value.String()
		return &text
	}
	return providerActivationView{
		ActivationID: a.ID.String(), GrantID: a.Grant.String(), PrincipalID: a.Principal.String(), Scope: a.Scope,
		Reason: a.Reason, DurationSeconds: a.DurationSeconds, ApprovalRequired: a.ApprovalRequired,
		RequestedAt: a.RequestedAt, Decision: a.Decision, DecidedBy: optional(a.DecidedBy),
		DecisionReason: a.DecisionReason, DecidedAt: a.DecidedAt, EndsAt: a.EndsAt, InForce: a.InForce(time.Now()),
		EndedBy: optional(a.EndedBy), EndReason: a.EndReason, EndedAt: a.EndedAt,
	}
}

// requireGrantHolder admits a provider in force or an eligible holder, with a reason. Authentication
// has already refused anyone else who is not a provider.
func requireGrantHolder(w http.ResponseWriter, r *http.Request) (Caller, bool) {
	caller, ok := CallerFrom(r.Context())
	if !ok || (!caller.Provider && !caller.Eligible) {
		platform.Problem(w, r, platform.Forbidden, "This route requires a provider grant")
		return Caller{}, false
	}
	if strings.TrimSpace(r.Header.Get(ReasonHeader)) == "" {
		platform.Problem(w, r, platform.ValidationFailed, "This request must carry the "+ReasonHeader+" header")
		return Caller{}, false
	}
	if !requireKey(w, r) {
		return Caller{}, false
	}
	return caller, true
}

func (h *handlers) listProviderActivations(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireGrantHolder(w, r); !ok {
		return
	}
	activations, err := h.services.ProviderActivations.List(r.Context(), reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	views := make([]providerActivationView, 0, len(activations))
	for _, activation := range activations {
		views = append(views, viewProviderActivation(activation))
	}
	respond(w, http.StatusOK, map[string]any{"activations": views})
}

// heldGrantView is one of the caller's own grants. Narrower than providerGrantView on purpose: it
// names no other Principal (granted_by, revoked_by), only what the holder needs to ask for an
// activation.
type heldGrantView struct {
	GrantID   string    `json:"grant_id"`
	Scope     string    `json:"scope"`
	Kind      string    `json:"kind"`
	GrantedAt time.Time `json:"granted_at"`
}

// listHeldProviderGrants is the caller's own unrevoked grants, eligible and emergency. An eligible
// holder reaches no grant route, and POST /v1/provider-activations names a grant_id, so this is how
// it learns what it can activate (TDD-organization-control-001 §Provider Activation).
func (h *handlers) listHeldProviderGrants(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireGrantHolder(w, r); !ok {
		return
	}
	records, err := h.services.ProviderActivations.Grants(r.Context(), reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	views := make([]heldGrantView, 0, len(records))
	for _, record := range records {
		views = append(views, heldGrantView{GrantID: record.ID.String(), Scope: record.Scope, Kind: record.Kind,
			GrantedAt: record.GrantedAt})
	}
	respond(w, http.StatusOK, map[string]any{"grants": views})
}

type requestActivationRequest struct {
	GrantID         string `json:"grant_id"`
	DurationSeconds int    `json:"duration_seconds"`
}

func (h *handlers) requestProviderActivation(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireGrantHolder(w, r); !ok {
		return
	}
	body, ok := decode[requestActivationRequest](w, r)
	if !ok {
		return
	}
	grantID, err := id.Parse(strings.TrimSpace(body.GrantID))
	if err != nil {
		platform.Problem(w, r, platform.ValidationFailed, "grant_id is not a valid identifier")
		return
	}
	activation, err := h.services.ProviderActivations.Request(r.Context(), grantID,
		time.Duration(body.DurationSeconds)*time.Second, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusCreated, viewProviderActivation(activation))
}

func (h *handlers) approveProviderActivation(w http.ResponseWriter, r *http.Request) {
	h.decideProviderActivation(w, r, authority.DecisionApproved)
}

func (h *handlers) denyProviderActivation(w http.ResponseWriter, r *http.Request) {
	h.decideProviderActivation(w, r, authority.DecisionDenied)
}

func (h *handlers) decideProviderActivation(w http.ResponseWriter, r *http.Request, decision string) {
	if _, ok := requireGrantHolder(w, r); !ok {
		return
	}
	activationID, ok := pathUUID(w, r, "activation_id")
	if !ok {
		return
	}
	activation, err := h.services.ProviderActivations.Decide(r.Context(), activationID, decision, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewProviderActivation(activation))
}

func (h *handlers) endProviderActivation(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireGrantHolder(w, r)
	if !ok {
		return
	}
	activationID, ok := pathUUID(w, r, "activation_id")
	if !ok {
		return
	}
	activation, err := h.services.ProviderActivations.End(r.Context(), activationID, caller.Provider, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewProviderActivation(activation))
}

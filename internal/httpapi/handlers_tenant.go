package httpapi

// The tenant-scoped routes. Every handler here runs under a scope resolved from the caller, and
// none of them reads a Tenant identifier from the request — the paths do not carry one, and where a
// domain request struct has a `TenantID` field the handler fills it from the scope. That is why
// there is no validation in this file rejecting a mismatched Tenant: the mismatch cannot be
// expressed. The services still refuse one, for callers that are not this surface.

import (
	"net/http"
	"time"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/invitation"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/workspace"
)

type grantMembershipRequest struct {
	PrincipalID id.UUID   `json:"principal_id"`
	WorkspaceID *id.UUID  `json:"workspace_id,omitempty"`
	SubjectType string    `json:"subject_type"`
	Provenance  string    `json:"provenance"`
	ValidFrom   time.Time `json:"valid_from"`
	ValidUntil  time.Time `json:"valid_until,omitempty"`
}

func (h *handlers) grantMembership(w http.ResponseWriter, r *http.Request) {
	scope, ok := requireTenant(w, r)
	if !ok {
		return
	}
	body, ok := decode[grantMembershipRequest](w, r)
	if !ok {
		return
	}

	var workspaceID id.UUID
	if body.WorkspaceID != nil {
		workspaceID = *body.WorkspaceID
	}

	result, err := h.services.Memberships.Grant(r.Context(), membership.GrantRequest{
		PrincipalID: body.PrincipalID,
		TenantID:    scope.TenantID(),
		WorkspaceID: workspaceID,
		SubjectType: body.SubjectType,
		Provenance:  body.Provenance,
		ValidFrom:   body.ValidFrom,
		ValidUntil:  body.ValidUntil,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusCreated, viewMembershipResult(result))
}

// membershipTransition is the shape the three lifecycle routes share.
//
// One helper rather than three near-identical handlers, because three copies is where the fourth
// transition gets added to two of them.
func (h *handlers) membershipTransition(w http.ResponseWriter, r *http.Request,
	apply func(*http.Request, id.UUID) (membership.Result, error)) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	membershipID, ok := pathUUID(w, r, "membership_id")
	if !ok {
		return
	}
	result, err := apply(r, membershipID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewMembershipResult(result))
}

func (h *handlers) suspendMembership(w http.ResponseWriter, r *http.Request) {
	h.membershipTransition(w, r, func(r *http.Request, membershipID id.UUID) (membership.Result, error) {
		return h.services.Memberships.Suspend(r.Context(), membershipID)
	})
}

func (h *handlers) restoreMembership(w http.ResponseWriter, r *http.Request) {
	h.membershipTransition(w, r, func(r *http.Request, membershipID id.UUID) (membership.Result, error) {
		return h.services.Memberships.Restore(r.Context(), membershipID)
	})
}

func (h *handlers) revokeMembership(w http.ResponseWriter, r *http.Request) {
	h.membershipTransition(w, r, func(r *http.Request, membershipID id.UUID) (membership.Result, error) {
		return h.services.Memberships.Revoke(r.Context(), membershipID)
	})
}

type createWorkspaceRequest struct {
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
}

func (h *handlers) createWorkspace(w http.ResponseWriter, r *http.Request) {
	scope, ok := requireTenant(w, r)
	if !ok {
		return
	}
	body, ok := decode[createWorkspaceRequest](w, r)
	if !ok {
		return
	}

	record, err := h.services.Workspaces.Create(r.Context(), workspace.CreateRequest{
		DisplayName: body.DisplayName,
		Type:        body.Type,
		TenantID:    scope.TenantID(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusCreated, viewWorkspace(record))
}

func (h *handlers) getWorkspace(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	workspaceID, ok := pathUUID(w, r, "workspace_id")
	if !ok {
		return
	}
	record, err := h.services.Workspaces.Get(r.Context(), workspaceID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewWorkspace(record))
}

// workspaceCommand is the body the three workspace transitions share.
//
// `expected_version` is required by the domain for retire and optional elsewhere; it is decoded
// uniformly and passed through, so the rule stays in the lifecycle rather than being restated here
// in a form that can drift from it.
type workspaceCommand struct {
	ExpectedVersion int64 `json:"expected_version,omitempty"`
}

func (h *handlers) workspaceTransition(w http.ResponseWriter, r *http.Request,
	apply func(*http.Request, workspace.Command) (workspace.Workspace, error)) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	workspaceID, ok := pathUUID(w, r, "workspace_id")
	if !ok {
		return
	}
	body, ok := decode[workspaceCommand](w, r)
	if !ok {
		return
	}
	record, err := apply(r, workspace.Command{
		WorkspaceID:     workspaceID,
		ExpectedVersion: body.ExpectedVersion,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewWorkspace(record))
}

func (h *handlers) archiveWorkspace(w http.ResponseWriter, r *http.Request) {
	h.workspaceTransition(w, r, func(r *http.Request, cmd workspace.Command) (workspace.Workspace, error) {
		return h.services.Workspaces.Archive(r.Context(), cmd)
	})
}

func (h *handlers) restoreWorkspace(w http.ResponseWriter, r *http.Request) {
	h.workspaceTransition(w, r, func(r *http.Request, cmd workspace.Command) (workspace.Workspace, error) {
		return h.services.Workspaces.Restore(r.Context(), cmd)
	})
}

func (h *handlers) retireWorkspace(w http.ResponseWriter, r *http.Request) {
	h.workspaceTransition(w, r, func(r *http.Request, cmd workspace.Command) (workspace.Workspace, error) {
		return h.services.Workspaces.Retire(r.Context(), cmd)
	})
}

type issueInvitationRequest struct {
	TargetIdentifier string   `json:"target_identifier"`
	WorkspaceID      *id.UUID `json:"workspace_id,omitempty"`
	SubjectType      string   `json:"subject_type"`
	Reason           string   `json:"reason,omitempty"`
	TTLSeconds       seconds  `json:"ttl_seconds,omitempty"`
}

func (h *handlers) issueInvitation(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	body, ok := decode[issueInvitationRequest](w, r)
	if !ok {
		return
	}

	issued, err := h.services.Invitations.Issue(r.Context(), invitation.IssueRequest{
		TargetIdentifier: body.TargetIdentifier,
		WorkspaceID:      body.WorkspaceID,
		SubjectType:      body.SubjectType,
		Reason:           body.Reason,
		TTL:              body.TTLSeconds.Duration(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	// The token leaves here and nowhere else. `issuedView` is the only view that carries one, and
	// `TestTokenAppearsOnlyOnIssue` asserts no other response can.
	respond(w, http.StatusCreated, issuedView{
		Invitation: viewInvitation(issued.Invitation),
		Token:      string(issued.Token),
	})
}

func (h *handlers) getInvitation(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	invitationID, ok := pathUUID(w, r, "invitation_id")
	if !ok {
		return
	}
	record, err := h.services.Invitations.Get(r.Context(), invitationID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewInvitation(record))
}

func (h *handlers) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	invitationID, ok := pathUUID(w, r, "invitation_id")
	if !ok {
		return
	}
	record, err := h.services.Invitations.Revoke(r.Context(), invitationID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewInvitation(record))
}

type acceptInvitationRequest struct {
	Token string `json:"token"`
}

type acceptInvitationResponse struct {
	Invitation invitationView       `json:"invitation"`
	Membership membershipResultView `json:"membership"`
}

func (h *handlers) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	body, ok := decode[acceptInvitationRequest](w, r)
	if !ok {
		return
	}

	record, result, err := h.services.Invitations.Accept(r.Context(), invitation.Token(body.Token))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, acceptInvitationResponse{
		Invitation: viewInvitation(record),
		Membership: viewMembershipResult(result),
	})
}

// lookupResponse is deliberately the same value for every token.
//
// It carries no field derived from a record, because the handler reads no record. SAD-004 §5.5
// requires an absent, expired, revoked, accepted, and valid token to be indistinguishable, and the
// only construction that cannot leak through a field, a status code, or a response time is one that
// looks nothing up.
type lookupResponse struct {
	// Accepted reports that the token is well formed and, if it corresponds to a live invitation,
	// that the identity flow may proceed. It is not a statement that the invitation exists.
	Accepted bool `json:"accepted"`

	// Next tells the caller where to go. Constant, so it cannot become a channel for the answer.
	Next string `json:"next"`
}

func (h *handlers) lookupInvitation(w http.ResponseWriter, r *http.Request) {
	body, ok := decode[acceptInvitationRequest](w, r)
	if !ok {
		return
	}

	// `Lookup` reads nothing; it checks the token's shape. A malformed token is refused because
	// that is a fact about the request rather than about any invitation, and it is the one
	// distinction that discloses nothing.
	if !h.services.Invitations.Lookup(invitation.Token(body.Token)) {
		platform.Problem(w, r, platform.ValidationFailed, "The token is not well formed")
		return
	}

	respond(w, http.StatusOK, lookupResponse{
		Accepted: true,
		Next:     "verify-identity",
	})
}

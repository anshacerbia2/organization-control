package httpapi

// The tenant-scoped routes. Every handler here runs under a scope resolved from the caller, and
// none of them reads a Tenant identifier from the request — the paths do not carry one, and where a
// domain request struct has a `TenantID` field the handler fills it from the scope. That is why
// there is no validation in this file rejecting a mismatched Tenant: the mismatch cannot be
// expressed. The services still refuse one, for callers that are not this surface.

import (
	stdcontext "context"
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

	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (membership.Result, error) {
		return h.services.Memberships.Grant(ctx, membership.GrantRequest{
			PrincipalID: body.PrincipalID,
			TenantID:    scope.TenantID(),
			WorkspaceID: workspaceID,
			SubjectType: body.SubjectType,
			Provenance:  body.Provenance,
			ValidFrom:   body.ValidFrom,
			ValidUntil:  body.ValidUntil,
		})
	}, viewMembershipResult)
}

// membershipCommand is the body the three Membership transitions take.
//
// `expected_version` is required: TDD-organization-control-002 §API has every mutation name the
// version of the record the caller was shown, and a mismatch answers 409 rather than landing on a
// state the caller never saw. The service refuses a missing one, so the rule lives in one place.
type membershipCommand struct {
	ExpectedVersion int64 `json:"expected_version"`
}

// membershipTransition is the shape the three lifecycle routes share.
//
// One helper rather than three near-identical handlers, because three copies is where the fourth
// transition gets added to two of them.
//
// The reason is the X-Administrative-Reason header, as on every provider route. A revocation is
// irreversible, so it requires one from a tenant caller too and is refused here, before anything is
// read, when the header is absent; a suspension or restoration records one when it is sent.
func (h *handlers) membershipTransition(w http.ResponseWriter, r *http.Request, action membership.Action,
	apply func(stdcontext.Context, membership.Command) (membership.Result, error)) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	membershipID, ok := pathUUID(w, r, "membership_id")
	if !ok {
		return
	}
	if action == membership.ActionRevoke && reason(r) == "" {
		platform.Problem(w, r, platform.ValidationFailed,
			"A revocation is irreversible and must carry the "+ReasonHeader+" header")
		return
	}
	body, ok := decode[membershipCommand](w, r)
	if !ok {
		return
	}
	cmd := membership.Command{
		MembershipID:    membershipID,
		ExpectedVersion: body.ExpectedVersion,
		Reason:          reason(r),
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (membership.Result, error) {
		return apply(ctx, cmd)
	}, viewMembershipResult)
}

func (h *handlers) suspendMembership(w http.ResponseWriter, r *http.Request) {
	h.membershipTransition(w, r, membership.ActionSuspend,
		func(ctx stdcontext.Context, cmd membership.Command) (membership.Result, error) {
			return h.services.Memberships.Suspend(ctx, cmd)
		})
}

func (h *handlers) restoreMembership(w http.ResponseWriter, r *http.Request) {
	h.membershipTransition(w, r, membership.ActionRestore,
		func(ctx stdcontext.Context, cmd membership.Command) (membership.Result, error) {
			return h.services.Memberships.Restore(ctx, cmd)
		})
}

func (h *handlers) revokeMembership(w http.ResponseWriter, r *http.Request) {
	h.membershipTransition(w, r, membership.ActionRevoke,
		func(ctx stdcontext.Context, cmd membership.Command) (membership.Result, error) {
			return h.services.Memberships.Revoke(ctx, cmd)
		})
}

func (h *handlers) getMembership(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	membershipID, ok := pathUUID(w, r, "membership_id")
	if !ok {
		return
	}
	record, err := h.services.Memberships.Get(r.Context(), membershipID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewMembership(record))
}

type membershipPageView struct {
	Memberships []membershipView `json:"memberships"`
	Next        *string          `json:"next"`
}

// listMemberships serves `GET /v1/memberships?after=&limit=&status=&workspace_id=&principal_id=`.
func (h *handlers) listMemberships(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	params, ok := readList(w, r, []string{"status"}, []string{"workspace_id", "principal_id"})
	if !ok {
		return
	}
	page, err := h.services.Memberships.List(r.Context(), membership.ListQuery{
		After:       params.After,
		Limit:       params.Limit,
		Status:      membership.State(params.Filters["status"]),
		WorkspaceID: params.IDs["workspace_id"],
		PrincipalID: params.IDs["principal_id"],
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := membershipPageView{Memberships: make([]membershipView, 0, len(page.Memberships)), Next: nextCursor(page.Next)}
	for _, record := range page.Memberships {
		view.Memberships = append(view.Memberships, viewMembership(record))
	}
	respond(w, http.StatusOK, view)
}

type workspacePageView struct {
	Workspaces []workspaceView `json:"workspaces"`
	Next       *string         `json:"next"`
}

// listWorkspaces serves `GET /v1/workspaces?after=&limit=&status=`.
func (h *handlers) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	params, ok := readList(w, r, []string{"status"}, nil)
	if !ok {
		return
	}
	page, err := h.services.Workspaces.List(r.Context(), workspace.ListQuery{
		After:  params.After,
		Limit:  params.Limit,
		Status: workspace.State(params.Filters["status"]),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := workspacePageView{Workspaces: make([]workspaceView, 0, len(page.Workspaces)), Next: nextCursor(page.Next)}
	for _, record := range page.Workspaces {
		view.Workspaces = append(view.Workspaces, viewWorkspace(record))
	}
	respond(w, http.StatusOK, view)
}

type invitationPageView struct {
	Invitations []invitationView `json:"invitations"`
	Next        *string          `json:"next"`
}

// listInvitations serves `GET /v1/invitations?after=&limit=&state=`. Each item is the view a single
// read returns, which carries neither the target identifier nor its hash.
func (h *handlers) listInvitations(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireTenant(w, r); !ok {
		return
	}
	params, ok := readList(w, r, []string{"state"}, nil)
	if !ok {
		return
	}
	page, err := h.services.Invitations.List(r.Context(), invitation.ListQuery{
		After: params.After,
		Limit: params.Limit,
		State: invitation.State(params.Filters["state"]),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := invitationPageView{Invitations: make([]invitationView, 0, len(page.Invitations)), Next: nextCursor(page.Next)}
	for _, record := range page.Invitations {
		view.Invitations = append(view.Invitations, viewInvitation(record))
	}
	respond(w, http.StatusOK, view)
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

	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (workspace.Workspace, error) {
		return h.services.Workspaces.Create(ctx, workspace.CreateRequest{
			DisplayName: body.DisplayName,
			Type:        body.Type,
			TenantID:    scope.TenantID(),
		})
	}, viewWorkspace)
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
	apply func(stdcontext.Context, workspace.Command) (workspace.Workspace, error)) {
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
	cmd := workspace.Command{
		WorkspaceID:     workspaceID,
		ExpectedVersion: body.ExpectedVersion,
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (workspace.Workspace, error) {
		return apply(ctx, cmd)
	}, viewWorkspace)
}

func (h *handlers) archiveWorkspace(w http.ResponseWriter, r *http.Request) {
	h.workspaceTransition(w, r, func(ctx stdcontext.Context, cmd workspace.Command) (workspace.Workspace, error) {
		return h.services.Workspaces.Archive(ctx, cmd)
	})
}

func (h *handlers) restoreWorkspace(w http.ResponseWriter, r *http.Request) {
	h.workspaceTransition(w, r, func(ctx stdcontext.Context, cmd workspace.Command) (workspace.Workspace, error) {
		return h.services.Workspaces.Restore(ctx, cmd)
	})
}

func (h *handlers) retireWorkspace(w http.ResponseWriter, r *http.Request) {
	h.workspaceTransition(w, r, func(ctx stdcontext.Context, cmd workspace.Command) (workspace.Workspace, error) {
		return h.services.Workspaces.Retire(ctx, cmd)
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

	// The token leaves here and nowhere else. `issuedView` is the only view that carries one, and
	// `TestTokenAppearsOnlyOnIssue` asserts no other response can.
	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (invitation.Issued, error) {
		return h.services.Invitations.Issue(ctx, invitation.IssueRequest{
			TargetIdentifier: body.TargetIdentifier,
			WorkspaceID:      body.WorkspaceID,
			SubjectType:      body.SubjectType,
			Reason:           body.Reason,
			TTL:              body.TTLSeconds.Duration(),
		})
	}, func(issued invitation.Issued) issuedView {
		return issuedView{Invitation: viewInvitation(issued.Invitation), Token: string(issued.Token)}
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
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (invitation.Invitation, error) {
		return h.services.Invitations.Revoke(ctx, invitationID)
	}, viewInvitation)
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

	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (invitation.Acceptance, error) {
		record, result, err := h.services.Invitations.Accept(ctx, invitation.Token(body.Token))
		return invitation.Acceptance{Invitation: record, Membership: result}, err
	}, func(accepted invitation.Acceptance) acceptInvitationResponse {
		return acceptInvitationResponse{
			Invitation: viewInvitation(accepted.Invitation),
			Membership: viewMembershipResult(accepted.Membership),
		}
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

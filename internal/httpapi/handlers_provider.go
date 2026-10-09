package httpapi

// The provider-scoped routes. Every handler here calls `requireProvider`, which refuses a
// tenant-scoped caller and refuses a request carrying no administrative reason.
//
// The two context checks are the exception, and they use `requireFreshCheckCaller`: a registered
// consumer may perform them without provider authority, because asking whether one principal holds
// context in one Tenant does not need the authority to administer every Tenant.
//
// These paths do name their target — a Tenant, an Organization, an Offboarding — because that is
// what cross-Tenant authority means: the target cannot come from the caller's own binding, since a
// provider caller has none. The identifier is therefore a parameter of the request rather than a
// substitution for the scope, and the scope stays what it was: provider authority, recorded as
// evidence before the transaction runs.

import (
	"bytes"
	stdcontext "context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"
	"github.com/anshacerbia2/foundation-platform/id"

	occontext "github.com/anshacerbia2/organization-control/internal/context"
	"github.com/anshacerbia2/organization-control/internal/invitation"
	"github.com/anshacerbia2/organization-control/internal/offboarding"
	"github.com/anshacerbia2/organization-control/internal/organization"
	"github.com/anshacerbia2/organization-control/internal/projection"
	"github.com/anshacerbia2/organization-control/internal/tenant"
)

// providerCommand is the body a lifecycle transition takes.
//
// The reason is a header rather than a body field, so one rule covers every provider route
// including the ones with no body at all. `expected_version` is a body field because it is about
// the record, not about the caller's authority.
type providerCommand struct {
	ExpectedVersion int64 `json:"expected_version,omitempty"`
}

func (h *handlers) tenantTransition(w http.ResponseWriter, r *http.Request,
	apply func(stdcontext.Context, tenant.Command) (tenant.Result, error)) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	tenantID, ok := pathUUID(w, r, "tenant_id")
	if !ok {
		return
	}
	body, ok := decode[providerCommand](w, r)
	if !ok {
		return
	}
	cmd := tenant.Command{
		TenantID:        tenantID,
		Reason:          reason(r),
		ExpectedVersion: body.ExpectedVersion,
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (tenant.Result, error) {
		return apply(ctx, cmd)
	}, viewTenantResult)
}

func (h *handlers) activateTenant(w http.ResponseWriter, r *http.Request) {
	h.tenantTransition(w, r, func(ctx stdcontext.Context, cmd tenant.Command) (tenant.Result, error) {
		return h.services.Tenants.Activate(ctx, cmd)
	})
}

func (h *handlers) suspendTenant(w http.ResponseWriter, r *http.Request) {
	h.tenantTransition(w, r, func(ctx stdcontext.Context, cmd tenant.Command) (tenant.Result, error) {
		return h.services.Tenants.Suspend(ctx, cmd)
	})
}

func (h *handlers) restoreTenant(w http.ResponseWriter, r *http.Request) {
	h.tenantTransition(w, r, func(ctx stdcontext.Context, cmd tenant.Command) (tenant.Result, error) {
		return h.services.Tenants.Restore(ctx, cmd)
	})
}

// requestTenantRequest carries no expected version, unlike every other body on this surface.
//
// Nothing exists yet for the caller to have been shown a version of, so requiring one would be a
// field with no honest value to put in it.
type requestTenantRequest struct {
	OrganizationID   id.UUID `json:"organization_id"`
	DisplayName      string  `json:"display_name"`
	IsolationProfile string  `json:"isolation_profile"`
	ResidencyRegion  string  `json:"residency_region,omitempty"`
}

func (h *handlers) requestTenant(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[requestTenantRequest](w, r)
	if !ok {
		return
	}
	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (tenant.Requested, error) {
		return h.services.Tenants.Request(ctx, tenant.RequestTenant{
			OrganizationID:   body.OrganizationID,
			DisplayName:      body.DisplayName,
			IsolationProfile: tenant.IsolationProfile(body.IsolationProfile),
			ResidencyRegion:  body.ResidencyRegion,
			Reason:           reason(r),
		})
	}, viewRequested)
}

func (h *handlers) getTenant(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	tenantID, ok := pathUUID(w, r, "tenant_id")
	if !ok {
		return
	}
	detail, err := h.services.Tenants.Detail(r.Context(), tenantID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewTenantDetail(detail))
}

// provisionTenant records that the desired state has left, and retries a failed attempt.
//
// One route for both edges `ActionProvision` serves, because they are one act: the request has gone
// out and the Tenant is now waiting on it. Two routes would have made "retry" a different operation
// from "dispatch" and left a caller to decide which state the Tenant was in before choosing.
func (h *handlers) provisionTenant(w http.ResponseWriter, r *http.Request) {
	h.tenantTransition(w, r, func(ctx stdcontext.Context, cmd tenant.Command) (tenant.Result, error) {
		return h.services.Provisioning.Provision(ctx, cmd)
	})
}

// provisioningOutcomeRequest is what the provisioning system reports back.
//
// The correlation identifier is in the body rather than in the path. It is not this service's
// identifier for a resource — it is the handle the desired-state publication carried outward — and a
// path segment would have made it look addressable, inviting a GET that has no meaning.
type provisioningOutcomeRequest struct {
	CorrelationID id.UUID `json:"correlation_id"`
	Detail        string  `json:"detail,omitempty"`
}

func (h *handlers) realizeProvisioning(w http.ResponseWriter, r *http.Request) {
	h.provisioningOutcome(w, r, func(r *http.Request, outcome tenant.Outcome) (tenant.Resolution, error) {
		return h.services.Provisioning.Realize(r.Context(), outcome)
	})
}

func (h *handlers) failProvisioning(w http.ResponseWriter, r *http.Request) {
	h.provisioningOutcome(w, r, func(r *http.Request, outcome tenant.Outcome) (tenant.Resolution, error) {
		return h.services.Provisioning.Fail(r.Context(), outcome)
	})
}

func (h *handlers) provisioningOutcome(w http.ResponseWriter, r *http.Request,
	apply func(*http.Request, tenant.Outcome) (tenant.Resolution, error)) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[provisioningOutcomeRequest](w, r)
	if !ok {
		return
	}
	resolution, err := apply(r, tenant.Outcome{
		CorrelationID: body.CorrelationID,
		Detail:        body.Detail,
		Reason:        reason(r),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	// 200 on a replay as well as on a first delivery. A provisioning system retrying a report it is
	// unsure arrived is behaving correctly, and the response says which happened in `replay` rather
	// than in a status code the retry logic would read as a failure.
	respond(w, http.StatusOK, viewResolution(resolution))
}

func (h *handlers) sweepProvisioning(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[batchRequest](w, r)
	if !ok {
		return
	}
	affected, err := h.services.Provisioning.SweepUnresolved(r.Context(), body.Size)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, batchResponse{Affected: int(affected)})
}

type registerOrganizationRequest struct {
	DisplayName    string   `json:"display_name"`
	Classification string   `json:"classification"`
	ParentID       *id.UUID `json:"parent_id,omitempty"`
}

func (h *handlers) registerOrganization(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[registerOrganizationRequest](w, r)
	if !ok {
		return
	}
	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (organization.Organization, error) {
		return h.services.Organizations.Register(ctx, organization.RegisterRequest{
			DisplayName:    body.DisplayName,
			Classification: organization.Classification(body.Classification),
			ParentID:       body.ParentID,
			Reason:         reason(r),
		})
	}, viewOrganization)
}

func (h *handlers) getOrganization(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	record, err := h.services.Organizations.Get(r.Context(), organizationID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewOrganization(record))
}

type organizationPageView struct {
	Organizations []organizationView `json:"organizations"`
	Next          *string            `json:"next"`
}

// listOrganizations serves `GET /v1/organizations?after=&limit=&status=&classification=`. Each page
// records the access with the caller's reason before it reads.
func (h *handlers) listOrganizations(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	params, ok := readList(w, r, []string{"status", "classification"}, nil)
	if !ok {
		return
	}
	page, err := h.services.Organizations.List(r.Context(), organization.ListQuery{
		After:          params.After,
		Limit:          params.Limit,
		Status:         organization.State(params.Filters["status"]),
		Classification: organization.Classification(params.Filters["classification"]),
	}, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := organizationPageView{Organizations: make([]organizationView, 0, len(page.Organizations)), Next: nextCursor(page.Next)}
	for _, record := range page.Organizations {
		view.Organizations = append(view.Organizations, viewOrganization(record))
	}
	respond(w, http.StatusOK, view)
}

type tenantPageView struct {
	Tenants []tenantRecordView `json:"tenants"`
	Next    *string            `json:"next"`
}

// listTenants serves `GET /v1/tenants?after=&limit=&status=&organization_id=`. Each page records the
// access with the caller's reason before it reads.
func (h *handlers) listTenants(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	params, ok := readList(w, r, []string{"status"}, []string{"organization_id"})
	if !ok {
		return
	}
	page, err := h.services.Tenants.List(r.Context(), tenant.ListQuery{
		After:          params.After,
		Limit:          params.Limit,
		Status:         tenant.State(params.Filters["status"]),
		OrganizationID: params.IDs["organization_id"],
	}, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := tenantPageView{Tenants: make([]tenantRecordView, 0, len(page.Tenants)), Next: nextCursor(page.Next)}
	for _, record := range page.Tenants {
		view.Tenants = append(view.Tenants, viewTenantRecord(record))
	}
	respond(w, http.StatusOK, view)
}

func (h *handlers) organizationTransition(w http.ResponseWriter, r *http.Request,
	apply func(stdcontext.Context, organization.Command) (organization.Organization, error)) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	body, ok := decode[providerCommand](w, r)
	if !ok {
		return
	}
	cmd := organization.Command{
		OrganizationID:  organizationID,
		Reason:          reason(r),
		ExpectedVersion: body.ExpectedVersion,
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (organization.Organization, error) {
		return apply(ctx, cmd)
	}, viewOrganization)
}

func (h *handlers) suspendOrganization(w http.ResponseWriter, r *http.Request) {
	h.organizationTransition(w, r, func(ctx stdcontext.Context, cmd organization.Command) (organization.Organization, error) {
		return h.services.Organizations.Suspend(ctx, cmd)
	})
}

func (h *handlers) restoreOrganization(w http.ResponseWriter, r *http.Request) {
	h.organizationTransition(w, r, func(ctx stdcontext.Context, cmd organization.Command) (organization.Organization, error) {
		return h.services.Organizations.Restore(ctx, cmd)
	})
}

func (h *handlers) retireOrganization(w http.ResponseWriter, r *http.Request) {
	h.organizationTransition(w, r, func(ctx stdcontext.Context, cmd organization.Command) (organization.Organization, error) {
		return h.services.Organizations.Retire(ctx, cmd)
	})
}

type verifiedIdentityRequest struct {
	CorrelationID id.UUID `json:"correlation_id"`
	Identifier    string  `json:"identifier"`
	PrincipalID   id.UUID `json:"principal_id"`
}

func (h *handlers) recordVerifiedIdentity(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[verifiedIdentityRequest](w, r)
	if !ok {
		return
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (invitation.Invitation, error) {
		return h.services.Invitations.RecordVerifiedIdentity(ctx, invitation.VerifiedIdentity{
			CorrelationID: body.CorrelationID,
			Identifier:    body.Identifier,
			PrincipalID:   body.PrincipalID,
		})
	}, viewInvitation)
}

type batchRequest struct {
	Size int `json:"size"`
}

type batchResponse struct {
	Affected int `json:"affected"`
}

func (h *handlers) expireLapsedInvitations(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[batchRequest](w, r)
	if !ok {
		return
	}
	affected, err := h.services.Invitations.ExpireLapsed(r.Context(), body.Size)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, batchResponse{Affected: affected})
}

type beginOffboardingRequest struct {
	TenantID        id.UUID `json:"tenant_id"`
	ExpectedVersion int64   `json:"expected_version"`
	LegalHold       bool    `json:"legal_hold,omitempty"`
}

func (h *handlers) beginOffboarding(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[beginOffboardingRequest](w, r)
	if !ok {
		return
	}
	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (offboarding.Offboarding, error) {
		return h.services.Offboardings.Begin(ctx, offboarding.BeginRequest{
			TenantID:        body.TenantID,
			ExpectedVersion: body.ExpectedVersion,
			Reason:          reason(r),
			LegalHold:       body.LegalHold,
		})
	}, viewOffboarding)
}

func (h *handlers) getOffboarding(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	record, err := h.services.Offboardings.Get(r.Context(), offboardingID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewOffboarding(record))
}

type offboardingPageView struct {
	Offboardings []offboardingView `json:"offboardings"`
	Next         *string           `json:"next"`
}

// listOffboardings serves `GET /v1/offboardings?after=&limit=&stage=&tenant_id=`. Each page records
// the access with the caller's reason before it reads.
func (h *handlers) listOffboardings(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	params, ok := readList(w, r, []string{"stage"}, []string{"tenant_id"})
	if !ok {
		return
	}
	page, err := h.services.Offboardings.List(r.Context(), offboarding.ListQuery{
		After:    params.After,
		Limit:    params.Limit,
		Stage:    offboarding.Stage(params.Filters["stage"]),
		TenantID: params.IDs["tenant_id"],
	}, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := offboardingPageView{Offboardings: make([]offboardingView, 0, len(page.Offboardings)), Next: nextCursor(page.Next)}
	for _, record := range page.Offboardings {
		view.Offboardings = append(view.Offboardings, viewOffboarding(record))
	}
	respond(w, http.StatusOK, view)
}

// freezeOffboarding runs one batch and reports how many rows it froze.
//
// One batch per call, not a loop to completion. The freeze holds `FOR UPDATE SKIP LOCKED` over a
// bounded set, and a request that looped until done would hold a transaction open for as long as
// the largest Tenant takes — which is the request that times out and leaves the work half done with
// nothing recording how far it got. The count lets the caller drive the loop and see progress.
func (h *handlers) freezeOffboarding(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	body, ok := decode[batchRequest](w, r)
	if !ok {
		return
	}

	record, err := h.services.Offboardings.Get(r.Context(), offboardingID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// A freeze belongs to the freeze stage. After a cancellation it would suspend Memberships in a
	// Tenant that is back (ADR-ORG-006); the batch also checks the Tenant itself, in its transaction.
	if record.Stage != offboarding.StageFreeze {
		writeError(w, r, fmt.Errorf("%w: %s is at %s, not freeze", offboarding.ErrStageRefused, offboardingID, record.Stage))
		return
	}
	affected, err := h.services.Offboardings.FreezeBatch(r.Context(), offboardingID, record.TenantID, body.Size, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, batchResponse{Affected: affected})
}

func (h *handlers) completeFreeze(w http.ResponseWriter, r *http.Request) {
	h.offboardingStage(w, r, func(ctx stdcontext.Context, offboardingID id.UUID) (offboarding.Offboarding, error) {
		return h.services.Offboardings.CompleteFreeze(ctx, offboardingID)
	})
}

func (h *handlers) releaseOffboarding(w http.ResponseWriter, r *http.Request) {
	h.offboardingStage(w, r, func(ctx stdcontext.Context, offboardingID id.UUID) (offboarding.Offboarding, error) {
		return h.services.Offboardings.Release(ctx, offboardingID)
	})
}

func (h *handlers) offboardingStage(w http.ResponseWriter, r *http.Request,
	apply func(stdcontext.Context, id.UUID) (offboarding.Offboarding, error)) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (offboarding.Offboarding, error) {
		return apply(ctx, offboardingID)
	}, viewOffboarding)
}

// retireOffboarding takes the Tenant version the caller read.
//
// Retirement is the irreversible stage, and it is the one transition where a stale read must not be
// applied: the version says which Tenant state the operator was looking at when they decided.
func (h *handlers) retireOffboarding(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	body, ok := decode[providerCommand](w, r)
	if !ok {
		return
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (offboarding.Offboarding, error) {
		return h.services.Offboardings.Retire(ctx, offboardingID, body.ExpectedVersion)
	}, viewOffboarding)
}

// cancelOffboarding cancels an offboarding before release, taking the Tenant version the caller was
// shown (ADR-ORG-006 §5.1). The reason header, which requireProvider insists on, is the cancellation's
// recorded reason.
func (h *handlers) cancelOffboarding(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	body, ok := decode[providerCommand](w, r)
	if !ok {
		return
	}
	record, err := h.services.Offboardings.Cancel(r.Context(), offboarding.CancelRequest{
		OffboardingID: offboardingID, ExpectedVersion: body.ExpectedVersion, Reason: reason(r),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewOffboarding(record))
}

type legalHoldRequest struct {
	Hold bool `json:"hold"`
}

func (h *handlers) setLegalHold(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	body, ok := decode[legalHoldRequest](w, r)
	if !ok {
		return
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (offboarding.Offboarding, error) {
		return h.services.Offboardings.SetLegalHold(ctx, offboardingID, body.Hold, reason(r))
	}, viewOffboarding)
}

type raiseObligationRequest struct {
	Domain string     `json:"domain"`
	Type   string     `json:"type"`
	DueAt  *time.Time `json:"due_at,omitempty"`
}

func (h *handlers) raiseObligation(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	body, ok := decode[raiseObligationRequest](w, r)
	if !ok {
		return
	}
	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (offboarding.Obligation, error) {
		return h.services.Offboardings.Raise(ctx, offboarding.RaiseRequest{
			OffboardingID: offboardingID,
			Domain:        body.Domain,
			Type:          body.Type,
			DueAt:         body.DueAt,
		})
	}, viewObligation)
}

// outstandingResponse is the obligation board. `outstanding` is the list of names it always was;
// `obligations` is every row, from TDD-organization-control-004 1.7.0.
type outstandingResponse struct {
	Outstanding []string         `json:"outstanding"`
	Obligations []obligationView `json:"obligations"`
}

func (h *handlers) outstandingObligations(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	board, err := h.services.Offboardings.Board(r.Context(), offboardingID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Empty slices rather than nil ones, so both fields marshal as `[]` and not `null`. A client
	// that reads `null` as "unknown" would treat a clean Tenant as one it could not assess.
	view := outstandingResponse{Outstanding: board.Outstanding, Obligations: make([]obligationView, 0, len(board.Obligations))}
	if view.Outstanding == nil {
		view.Outstanding = []string{}
	}
	for _, obligation := range board.Obligations {
		view.Obligations = append(view.Obligations, viewObligation(obligation))
	}
	respond(w, http.StatusOK, view)
}

type resolveObligationRequest struct {
	Domain string `json:"domain"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

func (h *handlers) resolveObligation(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	obligationID, ok := pathUUID(w, r, "obligation_id")
	if !ok {
		return
	}
	body, ok := decode[resolveObligationRequest](w, r)
	if !ok {
		return
	}
	answer(w, r, http.StatusOK, func(ctx stdcontext.Context) (offboarding.Obligation, error) {
		return h.services.Offboardings.Resolve(ctx, offboarding.Resolution{
			ObligationID: obligationID,
			Domain:       body.Domain,
			State:        offboarding.ObligationState(body.State),
			Detail:       body.Detail,
		})
	}, viewObligation)
}

type deprovisioningRequest struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

func (h *handlers) recordDeprovisioning(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	body, ok := decode[deprovisioningRequest](w, r)
	if !ok {
		return
	}
	if err := h.services.Offboardings.RecordDeprovisioning(r.Context(), offboarding.DeprovisioningOutcome{
		OffboardingID: offboardingID,
		State:         body.State,
		Detail:        body.Detail,
	}); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type registerConsumerRequest struct {
	ConsumerID            string  `json:"consumer_id"`
	PrincipalID           string  `json:"principal_id"`
	ProjectionVersion     string  `json:"projection_version"`
	MaxAcceptedAgeSeconds seconds `json:"max_accepted_age_seconds"`
	StaleBehavior         string  `json:"stale_behavior"`

	// EventTypes is the subscription: the event types the consumer applies, each one the registry
	// offers. Required (TDD-organization-control-002 §Consumer Registry).
	EventTypes []string `json:"event_types"`
}

func (h *handlers) registerConsumer(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[registerConsumerRequest](w, r)
	if !ok {
		return
	}
	// Parsed here so a malformed value is a 400 naming the field; an absent one reaches the
	// registry's own rule as the nil identifier.
	var principal id.UUID
	if strings.TrimSpace(body.PrincipalID) != "" {
		parsed, err := id.Parse(strings.TrimSpace(body.PrincipalID))
		if err != nil {
			platform.Problem(w, r, platform.ValidationFailed, "principal_id is not a valid identifier")
			return
		}
		principal = parsed
	}
	answer(w, r, http.StatusCreated, func(ctx stdcontext.Context) (projection.Consumer, error) {
		return h.services.Registry.Register(ctx, projection.Registration{
			ConsumerID:        body.ConsumerID,
			PrincipalID:       principal,
			ProjectionVersion: body.ProjectionVersion,
			MaxAcceptedAge:    body.MaxAcceptedAgeSeconds.Duration(),
			StaleBehavior:     projection.StaleBehavior(body.StaleBehavior),
			EventTypes:        body.EventTypes,
		})
	}, func(record projection.Consumer) consumerView { return viewConsumer(record, time.Now()) })
}

// retireConsumer withdraws a consumer: its registration, its subscription, and what it was still
// owed, which is abandoned (ADR-GLB-018 §5.5).
//
// Provider-only, and deliberately not available to the consumer itself: a consumer that could
// retire itself could withdraw its own authority mid-operation, and the decision to stop
// enforcing through a projection belongs to whoever is replacing it.
func (h *handlers) retireConsumer(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	if err := h.services.Registry.Retire(r.Context(), r.PathValue("consumer_id")); err != nil {
		writeError(w, r, err)
		return
	}
	// 204 rather than the record: Get refuses a retired consumer, so returning a view of one
	// would be the only place in this surface that hands back something it will not read back.
	w.WriteHeader(http.StatusNoContent)
}

// listConsumers serves `GET /v1/projections/consumers?after=&limit=&state=`, the registry for the
// projection health view (TDD-organization-control-002 1.11.0 §The Consumer List). Provider-only: a
// consumer reads its own record, and the others' are not its concern. Each page records the access with
// the caller's reason before it reads.
func (h *handlers) listConsumers(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	params, ok := readKeyedList(w, r, []string{"state"})
	if !ok {
		return
	}
	page, err := h.services.Registry.List(r.Context(), projection.ConsumerListQuery{
		After: params.Key,
		Limit: params.Limit,
		State: projection.ConsumerState(params.Filters["state"]),
	}, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	now := time.Now()
	view := consumerPageView{Consumers: make([]listedConsumerView, 0, len(page.Consumers)), Next: page.Next}
	for _, record := range page.Consumers {
		view.Consumers = append(view.Consumers, viewListedConsumer(record, now))
	}
	respond(w, http.StatusOK, view)
}

func (h *handlers) getConsumer(w http.ResponseWriter, r *http.Request) {
	// A consumer reading its own record is how it learns its snapshot and reported marks, which is
	// the input to its own freshness. Provider authority to read that would make every consumer as
	// privileged as the control plane for a question about itself.
	scope, _, ok := requireConsumerSelfOrProvider(w, r, r.PathValue("consumer_id"))
	if !ok {
		return
	}
	own, ok := h.consumerServices(w, r, scope)
	if !ok {
		return
	}
	var (
		record projection.Consumer
		err    error
	)
	if own != nil {
		record, err = own.Access.Get(r.Context(), r.PathValue("consumer_id"))
	} else {
		record, err = h.services.Registry.Get(r.Context(), r.PathValue("consumer_id"))
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewConsumer(record, time.Now()))
}

type progressRequest struct {
	AppliedMark int64 `json:"applied_mark"`
}

func (h *handlers) recordProgress(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := requireConsumerSelfOrProvider(w, r, r.PathValue("consumer_id"))
	if !ok {
		return
	}
	own, ok := h.consumerServices(w, r, scope)
	if !ok {
		return
	}
	body, ok := decode[progressRequest](w, r)
	if !ok {
		return
	}
	report := projection.Progress{
		ConsumerID:  r.PathValue("consumer_id"),
		AppliedMark: body.AppliedMark,
	}
	var (
		record projection.Consumer
		err    error
	)
	if own != nil {
		record, err = own.Access.RecordProgress(r.Context(), report)
	} else {
		record, err = h.services.Registry.RecordProgress(r.Context(), report)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewConsumer(record, time.Now()))
}

type bootstrapRequest struct {
	Mark int64 `json:"mark"`
}

func (h *handlers) bootstrapConsumer(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := requireConsumerSelfOrProvider(w, r, r.PathValue("consumer_id"))
	if !ok {
		return
	}
	own, ok := h.consumerServices(w, r, scope)
	if !ok {
		return
	}
	body, ok := decode[bootstrapRequest](w, r)
	if !ok {
		return
	}
	var (
		record projection.Consumer
		err    error
	)
	if own != nil {
		record, err = own.Access.Bootstrap(r.Context(), r.PathValue("consumer_id"), body.Mark)
	} else {
		record, err = h.services.Publisher.Bootstrap(r.Context(), r.PathValue("consumer_id"), body.Mark)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewConsumer(record, time.Now()))
}

type snapshotRequest struct {
	ConsumerID string `json:"consumer_id"`
	PageSize   int    `json:"page_size,omitempty"`
	Cursor     string `json:"cursor,omitempty"`

	// Mark is a pointer so a continuation at position zero is distinguishable from a first page.
	// A plain int64 made those two the same request, and the consumer that sent the second was
	// served the first — silently restarting its own snapshot.
	Mark *int64 `json:"mark,omitempty"`
}

func (h *handlers) snapshot(w http.ResponseWriter, r *http.Request) {
	body, ok := decode[snapshotRequest](w, r)
	if !ok {
		return
	}
	// Decoded first because the consumer it names is in the body, and the check is that a consumer
	// caller named itself. econcile deliberately keeps requireProvider: it reports across every
	// consumer, so it is an operator action rather than a consumer's own.
	scope, _, ok := requireConsumerSelfOrProvider(w, r, body.ConsumerID)
	if !ok {
		return
	}
	own, ok := h.consumerServices(w, r, scope)
	if !ok {
		return
	}
	req := projection.SnapshotRequest{
		ConsumerID: body.ConsumerID,
		PageSize:   body.PageSize,
		Cursor:     body.Cursor,
		Mark:       body.Mark,
	}
	var (
		page projection.Page
		err  error
	)
	if own != nil {
		page, err = own.Access.Snapshot(r.Context(), req)
	} else {
		page, err = h.services.Publisher.Snapshot(r.Context(), req)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	// The domain type carries JSON tags of its own and no PII, so it is the response.
	respond(w, http.StatusOK, page)
}

// providerSnapshot serves the provider authority snapshot, to the consumer it names or to a
// provider, under the same rules and the same page contract as the Organization snapshot.
func (h *handlers) providerSnapshot(w http.ResponseWriter, r *http.Request) {
	body, ok := decode[snapshotRequest](w, r)
	if !ok {
		return
	}
	scope, _, ok := requireConsumerSelfOrProvider(w, r, body.ConsumerID)
	if !ok {
		return
	}
	own, ok := h.consumerServices(w, r, scope)
	if !ok {
		return
	}
	req := projection.SnapshotRequest{
		ConsumerID: body.ConsumerID,
		PageSize:   body.PageSize,
		Cursor:     body.Cursor,
		Mark:       body.Mark,
	}
	var (
		page projection.ProviderPage
		err  error
	)
	if own != nil {
		page, err = own.Access.ProviderSnapshot(r.Context(), req)
	} else {
		page, err = h.services.Publisher.ProviderSnapshot(r.Context(), req)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, page)
}

type reconcileRequest struct {
	ConsumerID string                   `json:"consumer_id"`
	Mark       int64                    `json:"mark"`
	Rows       []projection.ReportedRow `json:"rows"`
}

type frontierResponse struct {
	HighestCommittedMark        int64   `json:"highest_committed_mark"`
	OldestUnpublishedMark       int64   `json:"oldest_unpublished_mark"`
	OldestUnpublishedAgeSeconds float64 `json:"oldest_unpublished_age_seconds"`
	Unpublished                 bool    `json:"unpublished"`

	// The debt this side has stopped attempting to deliver: authority-bearing events sitting
	// unresolved in platform.dead_letter. Separate fields rather than folded into the owed pool,
	// because they are a different fact — an unpublished row will arrive, and one of these will not
	// arrive without an operator.
	SecurityDeadLettered               int64   `json:"security_dead_lettered"`
	OldestSecurityDeadLetterAgeSeconds float64 `json:"oldest_security_dead_letter_age_seconds"`
	SecurityDebt                       bool    `json:"security_debt"`

	ObservedAt string `json:"observed_at"`
}

// frontier reports the publication frontier as facts, and computes no freshness verdict.
//
// A consumer cannot derive this for itself: the outbox allocates its sequence before the transaction
// commits, so a gap in a consumer's applied positions is indistinguishable from a number a
// rolled-back transaction consumed. Only this side knows the difference, because a rolled-back row was
// never in the outbox.
//
// Deliberately not one freshness number. Summing publication lag, delivery lag and apply lag here
// would put a policy decision in the producer, where it cannot see which operation is being
// authorised — and the same lag is acceptable for a directory read and unacceptable for a payroll
// one.
//
// Readable by a registered consumer acting as itself, and by a provider with a reason. It is a
// consumer's runtime concern, unlike econcile, which reports across every consumer and stays an
// operator action.
func (h *handlers) frontier(w http.ResponseWriter, r *http.Request) {
	// A consumer reads its own debt; a provider, naming no consumer, reads the estate's.
	scope, consumer, ok := requireConsumerSelfOrProvider(w, r, "")
	if !ok {
		return
	}
	own, ok := h.consumerServices(w, r, scope)
	if !ok {
		return
	}

	reader := h.services.Frontier
	if own != nil {
		reader = own.Frontier
	}
	report, err := reader.FrontierFor(r.Context(), consumer)
	if err != nil {
		writeError(w, r, err)
		return
	}

	respond(w, http.StatusOK, frontierResponse{
		HighestCommittedMark:        report.HighestCommittedMark,
		OldestUnpublishedMark:       report.OldestUnpublishedMark,
		OldestUnpublishedAgeSeconds: report.OldestUnpublishedAge.Seconds(),
		Unpublished:                 report.Unpublished,

		SecurityDeadLettered:               report.SecurityDeadLettered,
		OldestSecurityDeadLetterAgeSeconds: report.OldestSecurityDeadLetterAge.Seconds(),
		SecurityDebt:                       report.SecurityDebt,

		ObservedAt: report.ObservedAt.Format(time.RFC3339Nano),
	})
}

func (h *handlers) reconcile(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[reconcileRequest](w, r)
	if !ok {
		return
	}
	result, err := h.services.Reconciler.Reconcile(r.Context(), projection.Report{
		ConsumerID: body.ConsumerID,
		Mark:       body.Mark,
		Rows:       body.Rows,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	// Publishing the findings is a separate call because it is a separate concern: the comparison
	// is a read, and the repair event is a write that a caller may want without the other. A
	// failure to publish is reported rather than swallowed — findings nobody hears about are
	// findings nobody acts on.
	if err := h.services.Reconciler.PublishReconciled(r.Context(), result); err != nil {
		writeError(w, r, err)
		return
	}
	logSecurityFindings(r, result)
	respond(w, http.StatusOK, result)
}

// logSecurityFindings writes each `extra` finding at ERROR. TDD-organization-control-002 §Operational
// Notes makes one critical at any occurrence: somebody holds access nothing granted. The run itself
// records the count on the consumer, which the alert reads; this is the line an investigator starts
// from, naming the Membership, so whoever ran the reconciliation is not the only one who knows.
func logSecurityFindings(r *http.Request, result projection.Result) {
	log := signalsFrom(r.Context()).logger(r.Context())
	for _, finding := range result.SecurityFindings() {
		log.ErrorContext(r.Context(), "reconciliation found access authority does not grant",
			slog.String("consumer_id", result.ConsumerID), slog.Int64("mark", result.Mark),
			slog.String("membership_id", finding.MembershipID.String()),
			slog.String("tenant_id", finding.TenantID.String()),
			slog.String("principal_id", finding.PrincipalID.String()),
			slog.Int64("projected_version", finding.ProjectedVersion),
			slog.Int64("authoritative_version", finding.AuthoritativeVersion),
			slog.Bool("authority_holds_it", finding.State != nil))
	}
}

type verifyContextRequest struct {
	ConsumerID  string  `json:"consumer_id"`
	TenantID    id.UUID `json:"tenant_id"`
	PrincipalID id.UUID `json:"principal_id"`
}

func (h *handlers) verifyContext(w http.ResponseWriter, r *http.Request) {
	h.contextCheck(w, r, func(r *http.Request, own *ConsumerServices, req occontext.VerifyRequest) (occontext.Decision, error) {
		if own != nil {
			return own.Checks.Verify(r.Context(), req)
		}
		return h.services.Contexts.Verify(r.Context(), req)
	})
}

func (h *handlers) switchEligible(w http.ResponseWriter, r *http.Request) {
	h.contextCheck(w, r, func(r *http.Request, own *ConsumerServices, req occontext.VerifyRequest) (occontext.Decision, error) {
		if own != nil {
			return own.Checks.SwitchEligible(r.Context(), req)
		}
		return h.services.Contexts.SwitchEligible(r.Context(), req)
	})
}

// contextCheck answers a verification with 200 whether or not it granted.
//
// A refusal is not an error: the caller asked whether a principal holds context in a Tenant, and
// "no" is a complete answer to that question. Returning 403 would make a successful check
// indistinguishable, to a client's error handling, from a check that could not be performed — and
// the two require opposite responses.
func (h *handlers) contextCheck(w http.ResponseWriter, r *http.Request,
	apply func(*http.Request, *ConsumerServices, occontext.VerifyRequest) (occontext.Decision, error)) {
	body, ok := decode[verifyContextRequest](w, r)
	if !ok {
		return
	}
	scope, consumer, ok := requireConsumerSelfOrProvider(w, r, body.ConsumerID)
	if !ok {
		return
	}
	own, ok := h.consumerServices(w, r, scope)
	if !ok {
		return
	}

	// A consumer's identity comes from its token. The body's consumer_id is accepted only from a
	// provider, which may legitimately check on another consumer's behalf while explaining why.
	//
	// A consumer that could name itself in the body could spend another consumer's meter -- and
	// the meter is the only thing that makes an over-eager consumer visible. A mismatch is
	// refused rather than silently overridden: a caller sending a different identifier believes
	// something about this request that is not true.
	consumerID := body.ConsumerID
	if consumer != "" {
		if strings.TrimSpace(body.ConsumerID) != "" && body.ConsumerID != consumer {
			platform.Problem(w, r, platform.Forbidden,
				"The request names a different consumer than the token; a consumer may only check as itself")
			return
		}
		consumerID = consumer
	}

	decision, err := apply(r, own, occontext.VerifyRequest{
		ConsumerID:  consumerID,
		TenantID:    body.TenantID,
		PrincipalID: body.PrincipalID,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, decision)
}

type rateRequest struct {
	ConsumerID string `json:"consumer_id"`
	Requests   int64  `json:"requests"`
}

func (h *handlers) recordRate(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[rateRequest](w, r)
	if !ok {
		return
	}
	rate, err := h.services.Contexts.RecordRate(r.Context(), occontext.RateReport{
		ConsumerID: body.ConsumerID,
		Requests:   body.Requests,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, rate)
}

type ratesResponse struct {
	Rates []occontext.Rate `json:"rates"`
}

func (h *handlers) ratesOverThreshold(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}

	threshold, err := floatQuery(r, "threshold", 0.5)
	if err != nil {
		platform.Problem(w, r, platform.ValidationFailed, err.Error())
		return
	}

	rates, err := h.services.Contexts.OverThreshold(r.Context(), threshold)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if rates == nil {
		rates = []occontext.Rate{}
	}
	respond(w, http.StatusOK, ratesResponse{Rates: rates})
}

type replayResponse struct {
	EventID   string `json:"event_id"`
	Consumer  string `json:"consumer"`
	EventType string `json:"event_type"`
	Position  int64  `json:"position"`
	Resolved  bool   `json:"resolved"`
}

// replayDeadLetter puts an abandoned delivery back into the outbox under its original identifier.
//
// Provider-only, and audited by the scope wrapper before the transaction opens, so an attempt that
// fails still leaves a record that it was made.
//
// It resolves nothing, and the response says so in a field rather than a comment: `resolved` is
// always false here. Replay creates the opportunity for evidence; a resolution consumes it. The
// separation is enforced below this handler as well -- the provider role holds no UPDATE on
// platform.dead_letter -- so a future change here cannot quietly merge the two acts.
func (h *handlers) replayDeadLetter(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	eventID, ok := pathUUID(w, r, "event_id")
	if !ok {
		return
	}

	result, err := h.services.Replayer.Replay(r.Context(), eventID, r.PathValue("consumer"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	respond(w, http.StatusAccepted, replayResponse{
		EventID:   result.EventID.String(),
		Consumer:  result.Consumer,
		EventType: result.EventType,
		Position:  result.Position,
		Resolved:  false,
	})
}

type resolveResponse struct {
	EventID        string `json:"event_id"`
	Consumer       string `json:"consumer"`
	ResolutionType string `json:"resolution_type"`
	Reference      string `json:"resolution_reference"`
}

// resolveDeadLetter closes an incident that applied evidence supports, and refuses otherwise.
//
// Provider-only at the HTTP boundary and organization_resolution_rt at the database one — two
// different questions. The first asks whether this caller may ask; the second bounds what the
// answer can touch, which is four columns of one row and nothing else. The credential that
// replays an abandoned delivery holds no UPDATE there, so replaying and closing cannot be done by
// one process even if this handler were wrong.
//
// The path names the incident, (event_id, consumer), and the incident fixes whose receipt counts:
// its own consumer's. The caller chooses the incident and the reason, never the evidence, so one
// consumer's receipt cannot close another consumer's incident. The author is not taken from the
// request either — resolved_by comes from the bound scope, because an author taken from
// a body is an author anybody can write.
//
// The body is optional and names the reason only. No body is REPLAYED, which is what this route
// meant before SUPERSEDED existed, so an existing caller keeps its meaning.
func (h *handlers) resolveDeadLetter(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	eventID, ok := pathUUID(w, r, "event_id")
	if !ok {
		return
	}
	kind, ok := resolutionType(w, r)
	if !ok {
		return
	}

	resolve := h.services.Resolver.Resolve
	if kind == projection.ResolutionTypeSuperseded {
		resolve = h.services.Resolver.Supersede
	}
	result, err := resolve(r.Context(), eventID, r.PathValue("consumer"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	respond(w, http.StatusOK, resolveResponse{
		EventID:        result.EventID.String(),
		Consumer:       result.Consumer,
		ResolutionType: result.Type,
		Reference:      result.Reference,
	})
}

type waiveRequest struct {
	Reason    string    `json:"reason"`
	ExpiresAt time.Time `json:"expires_at"`
}

type waiveResponse struct {
	EventID      string `json:"event_id"`
	Consumer     string `json:"consumer"`
	WaivedUntil  string `json:"waived_until"`
	WaiverReason string `json:"waiver_reason"`
	Resolved     bool   `json:"resolved"`
}

// waiveDeadLetter records an operational exception on an incident no corrective path can close.
//
// Provider-only, and run as the resolution role like a resolution. The response carries
// `resolved: false` for the reason the replay response does: a waiver is not a closure, the
// frontier still counts the incident, and a client reading this answer must not conclude that the
// debt is gone. The rules -- which consumer, how long -- are projection.Resolver.Waive's.
func (h *handlers) waiveDeadLetter(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	eventID, ok := pathUUID(w, r, "event_id")
	if !ok {
		return
	}
	body, ok := decode[waiveRequest](w, r)
	if !ok {
		return
	}

	result, err := h.services.Resolver.Waive(r.Context(), eventID, r.PathValue("consumer"), body.Reason, body.ExpiresAt)
	if err != nil {
		writeError(w, r, err)
		return
	}

	respond(w, http.StatusOK, waiveResponse{
		EventID:      result.EventID.String(),
		Consumer:     result.Consumer,
		WaivedUntil:  result.Until.Format(time.RFC3339),
		WaiverReason: result.Reason,
		Resolved:     false,
	})
}

type resolveRequest struct {
	ResolutionType string `json:"resolution_type"`
}

// resolutionType reads the optional body of a resolution.
//
// An unknown reason is a 400, never a fallback to REPLAYED: a caller asking for WAIVED and getting
// a REPLAYED attempt has been answered about a question it did not ask.
func resolutionType(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		platform.Problem(w, r, platform.ValidationFailed, "The body could not be read")
		return "", false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return projection.ResolutionTypeReplayed, true
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	body, ok := decode[resolveRequest](w, r)
	if !ok {
		return "", false
	}
	switch body.ResolutionType {
	case "", projection.ResolutionTypeReplayed:
		return projection.ResolutionTypeReplayed, true
	case projection.ResolutionTypeSuperseded:
		return projection.ResolutionTypeSuperseded, true
	default:
		platform.Problem(w, r, platform.ValidationFailed, fmt.Sprintf(
			"resolution_type %q is not supported; use %q or %q",
			body.ResolutionType, projection.ResolutionTypeReplayed, projection.ResolutionTypeSuperseded))
		return "", false
	}
}

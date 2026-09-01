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
	apply func(*http.Request, tenant.Command) (tenant.Result, error)) {
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
	result, err := apply(r, tenant.Command{
		TenantID:        tenantID,
		Reason:          reason(r),
		ExpectedVersion: body.ExpectedVersion,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewTenantResult(result))
}

func (h *handlers) activateTenant(w http.ResponseWriter, r *http.Request) {
	h.tenantTransition(w, r, func(r *http.Request, cmd tenant.Command) (tenant.Result, error) {
		return h.services.Tenants.Activate(r.Context(), cmd)
	})
}

func (h *handlers) suspendTenant(w http.ResponseWriter, r *http.Request) {
	h.tenantTransition(w, r, func(r *http.Request, cmd tenant.Command) (tenant.Result, error) {
		return h.services.Tenants.Suspend(r.Context(), cmd)
	})
}

func (h *handlers) restoreTenant(w http.ResponseWriter, r *http.Request) {
	h.tenantTransition(w, r, func(r *http.Request, cmd tenant.Command) (tenant.Result, error) {
		return h.services.Tenants.Restore(r.Context(), cmd)
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
	requested, err := h.services.Tenants.Request(r.Context(), tenant.RequestTenant{
		OrganizationID:   body.OrganizationID,
		DisplayName:      body.DisplayName,
		IsolationProfile: tenant.IsolationProfile(body.IsolationProfile),
		ResidencyRegion:  body.ResidencyRegion,
		Reason:           reason(r),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusCreated, viewRequested(requested))
}

func (h *handlers) getTenant(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	tenantID, ok := pathUUID(w, r, "tenant_id")
	if !ok {
		return
	}
	record, err := h.services.Tenants.Get(r.Context(), tenantID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewTenantRecord(record))
}

// provisionTenant records that the desired state has left, and retries a failed attempt.
//
// One route for both edges `ActionProvision` serves, because they are one act: the request has gone
// out and the Tenant is now waiting on it. Two routes would have made "retry" a different operation
// from "dispatch" and left a caller to decide which state the Tenant was in before choosing.
func (h *handlers) provisionTenant(w http.ResponseWriter, r *http.Request) {
	h.tenantTransition(w, r, func(r *http.Request, cmd tenant.Command) (tenant.Result, error) {
		return h.services.Provisioning.Provision(r.Context(), cmd)
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
	record, err := h.services.Organizations.Register(r.Context(), organization.RegisterRequest{
		DisplayName:    body.DisplayName,
		Classification: organization.Classification(body.Classification),
		ParentID:       body.ParentID,
		Reason:         reason(r),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusCreated, viewOrganization(record))
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

func (h *handlers) organizationTransition(w http.ResponseWriter, r *http.Request,
	apply func(*http.Request, organization.Command) (organization.Organization, error)) {
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
	record, err := apply(r, organization.Command{
		OrganizationID:  organizationID,
		Reason:          reason(r),
		ExpectedVersion: body.ExpectedVersion,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewOrganization(record))
}

func (h *handlers) suspendOrganization(w http.ResponseWriter, r *http.Request) {
	h.organizationTransition(w, r, func(r *http.Request, cmd organization.Command) (organization.Organization, error) {
		return h.services.Organizations.Suspend(r.Context(), cmd)
	})
}

func (h *handlers) restoreOrganization(w http.ResponseWriter, r *http.Request) {
	h.organizationTransition(w, r, func(r *http.Request, cmd organization.Command) (organization.Organization, error) {
		return h.services.Organizations.Restore(r.Context(), cmd)
	})
}

func (h *handlers) retireOrganization(w http.ResponseWriter, r *http.Request) {
	h.organizationTransition(w, r, func(r *http.Request, cmd organization.Command) (organization.Organization, error) {
		return h.services.Organizations.Retire(r.Context(), cmd)
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
	record, err := h.services.Invitations.RecordVerifiedIdentity(r.Context(), invitation.VerifiedIdentity{
		CorrelationID: body.CorrelationID,
		Identifier:    body.Identifier,
		PrincipalID:   body.PrincipalID,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewInvitation(record))
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
	record, err := h.services.Offboardings.Begin(r.Context(), offboarding.BeginRequest{
		TenantID:        body.TenantID,
		ExpectedVersion: body.ExpectedVersion,
		Reason:          reason(r),
		LegalHold:       body.LegalHold,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusCreated, viewOffboarding(record))
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
	affected, err := h.services.Offboardings.FreezeBatch(r.Context(), record.TenantID, body.Size)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, batchResponse{Affected: affected})
}

func (h *handlers) completeFreeze(w http.ResponseWriter, r *http.Request) {
	h.offboardingStage(w, r, func(r *http.Request, offboardingID id.UUID) (offboarding.Offboarding, error) {
		return h.services.Offboardings.CompleteFreeze(r.Context(), offboardingID)
	})
}

func (h *handlers) releaseOffboarding(w http.ResponseWriter, r *http.Request) {
	h.offboardingStage(w, r, func(r *http.Request, offboardingID id.UUID) (offboarding.Offboarding, error) {
		return h.services.Offboardings.Release(r.Context(), offboardingID)
	})
}

func (h *handlers) offboardingStage(w http.ResponseWriter, r *http.Request,
	apply func(*http.Request, id.UUID) (offboarding.Offboarding, error)) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	record, err := apply(r, offboardingID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewOffboarding(record))
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
	record, err := h.services.Offboardings.Retire(r.Context(), offboardingID, body.ExpectedVersion)
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
	record, err := h.services.Offboardings.SetLegalHold(r.Context(), offboardingID, body.Hold, reason(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewOffboarding(record))
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
	record, err := h.services.Offboardings.Raise(r.Context(), offboarding.RaiseRequest{
		OffboardingID: offboardingID,
		Domain:        body.Domain,
		Type:          body.Type,
		DueAt:         body.DueAt,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusCreated, viewObligation(record))
}

type outstandingResponse struct {
	Outstanding []string `json:"outstanding"`
}

func (h *handlers) outstandingObligations(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	offboardingID, ok := pathUUID(w, r, "offboarding_id")
	if !ok {
		return
	}
	outstanding, err := h.services.Offboardings.Outstanding(r.Context(), offboardingID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// An empty slice rather than a nil one, so the field marshals as `[]` and not `null`. A client
	// that reads `null` as "unknown" would treat a clean Tenant as one it could not assess.
	if outstanding == nil {
		outstanding = []string{}
	}
	respond(w, http.StatusOK, outstandingResponse{Outstanding: outstanding})
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
	record, err := h.services.Offboardings.Resolve(r.Context(), offboarding.Resolution{
		ObligationID: obligationID,
		Domain:       body.Domain,
		State:        offboarding.ObligationState(body.State),
		Detail:       body.Detail,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewObligation(record))
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
	ProjectionVersion     string  `json:"projection_version"`
	MaxAcceptedAgeSeconds seconds `json:"max_accepted_age_seconds"`
	StaleBehavior         string  `json:"stale_behavior"`
}

func (h *handlers) registerConsumer(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[registerConsumerRequest](w, r)
	if !ok {
		return
	}
	record, err := h.services.Registry.Register(r.Context(), projection.Registration{
		ConsumerID:        body.ConsumerID,
		ProjectionVersion: body.ProjectionVersion,
		MaxAcceptedAge:    body.MaxAcceptedAgeSeconds.Duration(),
		StaleBehavior:     projection.StaleBehavior(body.StaleBehavior),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusCreated, viewConsumer(record))
}

func (h *handlers) getConsumer(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	record, err := h.services.Registry.Get(r.Context(), r.PathValue("consumer_id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewConsumer(record))
}

type progressRequest struct {
	AppliedMark int64 `json:"applied_mark"`
}

func (h *handlers) recordProgress(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[progressRequest](w, r)
	if !ok {
		return
	}
	record, err := h.services.Registry.RecordProgress(r.Context(), projection.Progress{
		ConsumerID:  r.PathValue("consumer_id"),
		AppliedMark: body.AppliedMark,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewConsumer(record))
}

type bootstrapRequest struct {
	Mark int64 `json:"mark"`
}

func (h *handlers) bootstrapConsumer(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[bootstrapRequest](w, r)
	if !ok {
		return
	}
	record, err := h.services.Publisher.Bootstrap(r.Context(), r.PathValue("consumer_id"), body.Mark)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond(w, http.StatusOK, viewConsumer(record))
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
	if _, ok := requireProvider(w, r); !ok {
		return
	}
	body, ok := decode[snapshotRequest](w, r)
	if !ok {
		return
	}
	page, err := h.services.Publisher.Snapshot(r.Context(), projection.SnapshotRequest{
		ConsumerID: body.ConsumerID,
		PageSize:   body.PageSize,
		Cursor:     body.Cursor,
		Mark:       body.Mark,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	// The domain type carries JSON tags of its own and no PII, so it is the response.
	respond(w, http.StatusOK, page)
}

type reconcileRequest struct {
	ConsumerID string                   `json:"consumer_id"`
	Mark       int64                    `json:"mark"`
	Rows       []projection.ReportedRow `json:"rows"`
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
	respond(w, http.StatusOK, result)
}

type verifyContextRequest struct {
	ConsumerID  string  `json:"consumer_id"`
	TenantID    id.UUID `json:"tenant_id"`
	PrincipalID id.UUID `json:"principal_id"`
}

func (h *handlers) verifyContext(w http.ResponseWriter, r *http.Request) {
	h.contextCheck(w, r, func(r *http.Request, req occontext.VerifyRequest) (occontext.Decision, error) {
		return h.services.Contexts.Verify(r.Context(), req)
	})
}

func (h *handlers) switchEligible(w http.ResponseWriter, r *http.Request) {
	h.contextCheck(w, r, func(r *http.Request, req occontext.VerifyRequest) (occontext.Decision, error) {
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
	apply func(*http.Request, occontext.VerifyRequest) (occontext.Decision, error)) {
	_, consumer, ok := requireFreshCheckCaller(w, r)
	if !ok {
		return
	}
	body, ok := decode[verifyContextRequest](w, r)
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

	decision, err := apply(r, occontext.VerifyRequest{
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

// Package httpapi is the HTTP surface. It owns no authority: every route resolves a scope, calls
// one service, and translates the answer.
//
// # Why the translation table is the important file
//
// A handler that returns 500 for a refusal tells the caller nothing it can act on, and tells the
// operator that the service is broken when it is working exactly as designed. Every sentinel the
// services define is mapped here, and `TestEverySentinelIsMapped` walks the packages to prove none
// was missed — because the failure mode of forgetting one is a correct refusal arriving as an
// internal error, which is indistinguishable from a defect until somebody reads the code.
package httpapi

import (
	"errors"
	"net/http"

	platform "github.com/anshacerbia2/foundation-platform/httpapi"

	"github.com/anshacerbia2/organization-control/internal/context"
	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/invitation"
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/offboarding"
	"github.com/anshacerbia2/organization-control/internal/organization"
	"github.com/anshacerbia2/organization-control/internal/projection"
	"github.com/anshacerbia2/organization-control/internal/tenant"
	"github.com/anshacerbia2/organization-control/internal/workspace"
)

// mapping is every sentinel the services define, and the problem each becomes.
//
// A slice rather than a map, because `errors.Is` is the matcher and order decides which of two
// matching entries wins. The specific sentinels come first: `db.ErrNoScope` and `db.ErrWrongScope`
// are programming errors that must not be reported as a caller's fault, and they would be if a
// broader entry matched first.
//
// The classification rules this table follows:
//
//   - Not found is 404, including every case Row-Level Security turns into absence. A row in
//     another Tenant is absent, and saying so is the answer — reporting that it exists elsewhere
//     would disclose a row the caller may not read.
//   - A refused transition is 409, because the caller asked the record to do something it cannot
//     do from where it is, and no retry changes that.
//   - A stale version is 409 and distinct, because the caller acted on a view that has since
//     changed and the fix is to re-read rather than to change the request.
//   - A precondition on another record is 412, not 409: obligations outstanding, a legal hold, a
//     sponsor that is not active, a Tenant that is not active. The request is well formed and the
//     world is not ready for it.
//   - A malformed request is 400.
//   - An unregistered consumer is 403, not 404. The consumer exists as a concept and is refused
//     on authority grounds; 404 would suggest the endpoint is wrong.
var mapping = []struct {
	err     error
	problem platform.ProblemType
}{
	// Scope resolution. Programming errors: reaching a service without a resolved scope means the
	// authorization layer was skipped, and reporting that as the caller's fault would send an
	// operator looking at the request instead of at the chain.
	{db.ErrNoScope, platform.Internal},
	{db.ErrWrongScope, platform.Internal},
	{db.ErrReasonRequired, platform.ValidationFailed},

	// Membership.
	{membership.ErrInvalid, platform.ValidationFailed},
	{membership.ErrNotFound, platform.NotFound},
	{membership.ErrRevoked, platform.StateTransitionRefused},
	{membership.ErrTransitionRefused, platform.StateTransitionRefused},
	{membership.ErrUnknownAction, platform.Internal},

	// Tenant.
	{tenant.ErrInvalid, platform.ValidationFailed},
	{tenant.ErrNotFound, platform.NotFound},
	{tenant.ErrVersionMismatch, platform.VersionConflict},
	{tenant.ErrRetired, platform.StateTransitionRefused},
	{tenant.ErrTransitionRefused, platform.StateTransitionRefused},
	{tenant.ErrProvisioningNotRealized, platform.PreconditionUnmet},
	{tenant.ErrSponsorNotActive, platform.PreconditionUnmet},
	{tenant.ErrUnknownAction, platform.Internal},

	// Organization.
	{organization.ErrInvalid, platform.ValidationFailed},
	{organization.ErrNotFound, platform.NotFound},
	{organization.ErrVersionMismatch, platform.VersionConflict},
	{organization.ErrRetired, platform.StateTransitionRefused},
	{organization.ErrTransitionRefused, platform.StateTransitionRefused},
	{organization.ErrTenantsNotRetired, platform.PreconditionUnmet},
	{organization.ErrUnknownAction, platform.Internal},

	// Workspace.
	{workspace.ErrInvalid, platform.ValidationFailed},
	{workspace.ErrNotFound, platform.NotFound},
	{workspace.ErrVersionMismatch, platform.VersionConflict},
	{workspace.ErrRetired, platform.StateTransitionRefused},
	{workspace.ErrTransitionRefused, platform.StateTransitionRefused},
	{workspace.ErrMembershipsPresent, platform.PreconditionUnmet},
	{workspace.ErrUnknownAction, platform.Internal},

	// Invitation.
	{invitation.ErrInvalid, platform.ValidationFailed},
	{invitation.ErrNotFound, platform.NotFound},
	{invitation.ErrSettled, platform.StateTransitionRefused},
	{invitation.ErrTransitionRefused, platform.StateTransitionRefused},
	{invitation.ErrExpired, platform.PreconditionUnmet},
	{invitation.ErrTenantNotActive, platform.PreconditionUnmet},
	{invitation.ErrAlreadyMember, platform.StateTransitionRefused},
	{invitation.ErrTTL, platform.ValidationFailed},
	{invitation.ErrToken, platform.ValidationFailed},
	{invitation.ErrUnknownAction, platform.Internal},

	// Offboarding.
	{offboarding.ErrInvalid, platform.ValidationFailed},
	{offboarding.ErrNotFound, platform.NotFound},
	{offboarding.ErrStageRefused, platform.StateTransitionRefused},
	{offboarding.ErrObligationsOutstanding, platform.PreconditionUnmet},
	{offboarding.ErrLegalHold, platform.PreconditionUnmet},
	{offboarding.ErrDeprovisioningIncomplete, platform.PreconditionUnmet},
	{offboarding.ErrAmbiguousOutcome, platform.PreconditionUnmet},
	{offboarding.ErrWrongDomain, platform.Forbidden},
	{offboarding.ErrAlreadyResolved, platform.StateTransitionRefused},

	// Projection.
	{projection.ErrNotRegistered, platform.Forbidden},
	{projection.ErrNoSnapshotMark, platform.PreconditionUnmet},
	{projection.ErrMarkWentBackwards, platform.ValidationFailed},
	{projection.ErrInvalid, platform.ValidationFailed},
	{projection.ErrPageSize, platform.ValidationFailed},
	{projection.ErrCursor, platform.ValidationFailed},
	{projection.ErrReportMarkRequired, platform.ValidationFailed},

	// Context.
	{context.ErrInvalid, platform.ValidationFailed},
	{context.ErrNotRegistered, platform.Forbidden},
	{context.ErrRequestRequired, platform.ValidationFailed},
}

// problemFor classifies an error, and reports whether the table recognised it.
//
// The boolean is what the exhaustiveness test asserts on. Without it, an unmapped sentinel and a
// genuine internal failure would both read as `Internal` and the test could not tell them apart —
// which is the same shape of blindness as a rule that reports success when it never ran.
func problemFor(err error) (platform.ProblemType, bool) {
	for _, entry := range mapping {
		if errors.Is(err, entry.err) {
			return entry.problem, true
		}
	}
	return platform.Internal, false
}

// writeError translates a service error into a problem document.
//
// The detail is the error's own message for everything except `Internal`. A refusal's message is
// written for the caller — it names the transition, the outstanding obligations, the versions — and
// suppressing it would leave a 409 with nothing to act on. An internal failure's message is not:
// it may carry a statement, a constraint name, or a path, and none of that is the caller's.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	problem, mapped := problemFor(err)
	if !mapped || problem == platform.Internal {
		platform.Problem(w, r, platform.Internal, "The request could not be completed")
		return
	}
	platform.Problem(w, r, problem, err.Error())
}

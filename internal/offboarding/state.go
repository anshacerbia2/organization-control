// Package offboarding coordinates a Tenant leaving the platform.
//
// It owns the two Tenant transitions `internal/tenant` deliberately does not expose as standalone
// commands, because each is a stage of a process that does more than move a row: entering
// offboarding also creates the record the process resumes from and suspends every Membership in
// the Tenant, and retirement is refused while an obligation is open or a legal hold is set.
//
// # Freeze before release
//
// Access stops at the first stage and data is released at the third. That ordering is the design
// rather than an implementation detail: freezing is reversible and immediate, release is neither.
// An operator who begins an offboarding by mistake has stopped access and destroyed nothing.
//
// # Completion is never inferred from one response
//
// A deprovisioning call that returns success says storage was released. It says nothing about
// whether the export a client contracted for was delivered, so SAD-004 §5.6 requires completion to
// come from the obligation registry rather than from any single infrastructure response. An
// ambiguous outcome holds the release stage; it never advances it.
package offboarding

import (
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
)

// Stage is how far an offboarding has progressed. Persisted, which is what makes the process
// resumable: a restart continues from the recorded stage rather than restarting a flow that has
// already frozen a Tenant.
type Stage string

const (
	// StageFreeze is suspending every Membership in the Tenant. Tenant-wide access already
	// stopped when offboarding began, so this stage closes the gap between the Tenant fact and
	// per-Membership authority rather than being what stops access.
	StageFreeze Stage = "freeze"

	// StageObligations holds while any domain still owes something.
	StageObligations Stage = "obligations"

	// StageRelease publishes the deprovisioning command. Reachable only with no obligation open
	// and no legal hold.
	StageRelease Stage = "release"

	// StageRetired is terminal.
	StageRetired Stage = "retired"
)

// Valid reports whether the stage is one this service persists. Mirrors `offboarding_stage_check`.
func (s Stage) Valid() bool {
	switch s {
	case StageFreeze, StageObligations, StageRelease, StageRetired:
		return true
	}
	return false
}

// stageOrder is the only permitted progression, as data.
//
// Linear and forward-only. A stage machine expressed as a table can be walked by a test; the same
// rules spread across four methods cannot, and the one that gets forgotten is the one nobody looked
// at. There is no transition backwards: an offboarding that has released data cannot return to
// obligations, because the obligation it would return to is one whose subject no longer exists.
var stageOrder = map[Stage]Stage{
	StageFreeze:      StageObligations,
	StageObligations: StageRelease,
	StageRelease:     StageRetired,
}

// Next reports the stage that follows, or false when the stage is terminal.
func Next(current Stage) (Stage, bool) {
	next, ok := stageOrder[current]
	return next, ok
}

// ObligationState is what one domain has done about its obligation.
type ObligationState string

const (
	// ObligationOpen is outstanding. Any open obligation holds the process.
	ObligationOpen ObligationState = "open"

	// ObligationCompleted means the domain did the work.
	ObligationCompleted ObligationState = "completed"

	// ObligationWaived means an accountable person decided it would not be done.
	//
	// A separate state from completed, and not a convenience. Recording a waiver as completed
	// would make an audit read as though the obligation was satisfied, which is a false record
	// rather than a lost one — and the difference is the only evidence that a decision was taken.
	ObligationWaived ObligationState = "waived"

	// ObligationFailed means the domain could not do the work. It holds the process exactly as
	// `open` does: a failure is not a resolution, and treating it as one would release data whose
	// obligations are known to be unmet.
	ObligationFailed ObligationState = "failed"
)

// Valid reports whether the state is one this service persists.
func (o ObligationState) Valid() bool {
	switch o {
	case ObligationOpen, ObligationCompleted, ObligationWaived, ObligationFailed:
		return true
	}
	return false
}

// Resolved reports whether this obligation stops holding the process.
//
// Completed and waived resolve; open and failed do not. Read from one place so the release gate and
// the operator's view cannot disagree about what "outstanding" means.
func (o ObligationState) Resolved() bool {
	return o == ObligationCompleted || o == ObligationWaived
}

var (
	// ErrNotFound reports an absent offboarding or obligation.
	ErrNotFound = errors.New("offboarding: not found")

	// ErrStageRefused reports an advance the stage machine does not permit.
	ErrStageRefused = errors.New("offboarding: the stage machine does not permit this advance")

	// ErrObligationsOutstanding refuses release while a domain still owes something. The refusal
	// names the obligations rather than counting them: an operator who has to go and find them
	// will chase the wrong one.
	ErrObligationsOutstanding = errors.New("offboarding: obligations are outstanding")

	// ErrLegalHold refuses release and retirement. SAD-004 §6.5 makes a hold block destruction
	// and nothing else — a hold that also blocked the freeze would keep access open on a Tenant
	// that is leaving.
	ErrLegalHold = errors.New("offboarding: a legal hold is set")

	// ErrWrongDomain refuses a completion reported by a domain other than the one the obligation
	// was raised against. Otherwise one domain could close another's obligation, and the registry
	// would record consent that was never given.
	ErrWrongDomain = errors.New("offboarding: obligation belongs to another domain")

	// ErrAlreadyResolved refuses a second resolution of one obligation. The first records who
	// decided; a second would overwrite that.
	ErrAlreadyResolved = errors.New("offboarding: obligation is already resolved")

	// ErrAmbiguousOutcome reports a deprovisioning result that is neither success nor failure. It
	// holds the release stage. A timeout is not proof the target rejected the request, and
	// advancing on one would retire a Tenant whose data may still exist.
	ErrAmbiguousOutcome = errors.New("offboarding: the deprovisioning outcome is ambiguous")
)

// Offboarding is one row of operation.offboarding.
type Offboarding struct {
	OffboardingID id.UUID
	TenantID      id.UUID
	Stage         Stage
	InitiatedBy   id.UUID
	Reason        string
	LegalHold     bool
	CorrelationID id.UUID
	StartedAt     time.Time
	FrozenAt      *time.Time
	RetiredAt     *time.Time
}

// Obligation is one row of operation.offboarding_obligation.
type Obligation struct {
	ObligationID  id.UUID
	OffboardingID id.UUID
	TenantID      id.UUID
	Domain        string
	Type          string
	State         ObligationState
	DueAt         *time.Time
	CompletedAt   *time.Time
	Detail        string
}

// eventTypes are the offboarding-owned events.
//
// The fifth segment carries the class, and `offboarding` is a process class rather than a security
// one: none of these stops access. The events that stop access are `tenant.security.suspended`,
// published by the Tenant transition when offboarding begins, and one
// `membership.security.suspended` per frozen Membership. `TDD-organization-control-004` states it
// directly — Identity does not infer enforcement from a lifecycle event.
var eventTypes = map[string]string{
	"started":           "com.scnehaux.organization.tenant.offboarding.started",
	"frozen":            "com.scnehaux.organization.tenant.offboarding.frozen",
	"obligation-raised": "com.scnehaux.organization.tenant.offboarding.obligation-raised",
	"released":          "com.scnehaux.organization.tenant.offboarding.released",
}

// EventType returns the validated type for one offboarding event.
func EventType(name string) (event.Type, error) {
	raw, ok := eventTypes[name]
	if !ok {
		return "", fmt.Errorf("offboarding: %q is not a published event of this process", name)
	}
	return event.ParseType(raw)
}

// EventNames returns every published event name, so a test walks the table rather than a list
// maintained beside it.
func EventNames() []string {
	all := make([]string, 0, len(eventTypes))
	for name := range eventTypes {
		all = append(all, name)
	}
	return all
}

// StagePayload is the body of a stage event.
//
// It carries the stage rather than only naming it in the type, so a consumer that has one handler
// for the process can read the position without parsing the event name.
type StagePayload struct {
	OffboardingID id.UUID `json:"offboarding_id"`
	TenantID      id.UUID `json:"tenant_id"`
	Stage         Stage   `json:"stage"`
	LegalHold     bool    `json:"legal_hold"`
}

// ObligationPayload is the body of an obligation-raised event.
type ObligationPayload struct {
	OffboardingID id.UUID    `json:"offboarding_id"`
	TenantID      id.UUID    `json:"tenant_id"`
	ObligationID  id.UUID    `json:"obligation_id"`
	Domain        string     `json:"domain"`
	Type          string     `json:"obligation_type"`
	DueAt         *time.Time `json:"due_at"`
}

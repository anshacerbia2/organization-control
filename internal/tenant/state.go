// Package tenant holds the authoritative Tenant lifecycle and the security version consumers
// compare a token against.
//
// A Tenant is the technical isolation, configuration, data, and operating boundary. Nothing here
// implements isolation — SAD-004 §5.1 puts that in the runtime and data systems — and this package
// records which boundary was chosen, what state it is in, and when contextual access inside it
// stopped being valid.
package tenant

import (
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
)

// State is the lifecycle position of one Tenant.
type State string

const (
	// StateRequested is recorded before anything has been provisioned.
	StateRequested State = "requested"

	// StateProvisioning means the isolation boundary is being built by the external system that
	// owns it. This service holds desired state and correlates the realized status back.
	StateProvisioning State = "provisioning"

	// StateActive is the only state in which Memberships may be granted. A Tenant is not active
	// until provisioning confirms, per SAD-004 §5.1: activating on request would mean Memberships
	// could be granted into a Tenant whose isolation boundary does not exist yet.
	StateActive State = "active"

	// StateFailed means provisioning was refused. It is not terminal — a retry moves back to
	// provisioning — because the cause is usually external and usually fixable.
	StateFailed State = "failed"

	// StateSuspended withholds every context in the Tenant reversibly.
	StateSuspended State = "suspended"

	// StateOffboarding freezes context while obligations are settled across domains.
	StateOffboarding State = "offboarding"

	// StateRetired is terminal. There is no transition out, because the identifiers of a retired
	// Tenant have been released to consumers as retired, and reviving one would make a downstream
	// projection wrong in a way no reconciliation would detect.
	StateRetired State = "retired"
)

// Valid reports whether the state is one this service persists. The set mirrors
// `tenant_status_check` in schema.hcl; a value passing here and failing there would surface as a
// constraint violation rather than as a refused transition.
func (s State) Valid() bool {
	switch s {
	case StateRequested, StateProvisioning, StateActive, StateFailed,
		StateSuspended, StateOffboarding, StateRetired:
		return true
	}
	return false
}

// Action is a requested transition.
type Action string

const (
	// ActionProvision starts or retries provisioning.
	ActionProvision Action = "provision"

	// ActionFail records a refusal reported back by the provisioning system.
	ActionFail Action = "fail"

	// ActionActivate is the only way into active, and it carries preconditions beyond the state
	// machine. See Service.Activate.
	ActionActivate Action = "activate"

	ActionSuspend Action = "suspend"
	ActionRestore Action = "restore"

	// ActionBeginOffboarding and ActionRetire are in the machine and are not exposed by this
	// package's Service. TDD-organization-control-004 assigns both to `OffboardingService`: each
	// one is a stage of a process that also creates an `operation.offboarding` row, raises
	// obligations across domains, and — for the freeze — suspends every Membership in the Tenant.
	// A version of either that moved only the Tenant row would look complete and leave access
	// running, so the transitions live here where the machine is whole and the commands live where
	// the rest of the work is.
	ActionBeginOffboarding Action = "begin-offboarding"
	ActionRetire           Action = "retire"
)

// rule is one row of the state machine.
type rule struct {
	from []State
	to   State

	// securityVersion increments the monotonic version consumers compare a token against.
	//
	// It is a property of the transition and not of the destination state, because the same state
	// is reached by transitions with different security consequences: `active` by activation
	// invalidates nothing, and `active` by restore invalidates every cached denial.
	securityVersion bool

	// stamp is the timestamp column this transition sets, or empty.
	stamp string

	// clear is the timestamp column this transition sets back to NULL, or empty. Only restore has
	// one: `suspended_at` records when the current suspension began, and left populated it would
	// make a restored Tenant indistinguishable from a suspended one to every report that filters
	// on the column. The history of past suspensions lives in the event stream, which is the
	// record that is supposed to be append-only.
	clear string
}

// transitions is the state machine as data rather than as a switch, so a test can walk the whole
// table and a reader deciding whether a new action belongs can see every existing one at once.
//
// The security-version column is the part worth reading twice. It follows
// TDD-organization-control-003 §"Security Version Increments" exactly: every transition that
// invalidates cached context increments, *in either direction*.
var transitions = map[Action]rule{
	// No context exists to invalidate before a Tenant has ever been active, which is why the three
	// provisioning transitions do not increment and publish nothing.
	ActionProvision: {from: []State{StateRequested, StateFailed}, to: StateProvisioning},
	ActionFail:      {from: []State{StateProvisioning}, to: StateFailed},
	ActionActivate:  {from: []State{StateProvisioning}, to: StateActive, stamp: "activated_at"},

	ActionSuspend: {
		from: []State{StateActive}, to: StateSuspended,
		securityVersion: true, stamp: "suspended_at",
	},
	// Restore increments too. This is the one that is easy to omit and expensive to omit: a
	// consumer that cached "suspended" and never sees a version change keeps denying a Tenant that
	// has been restored, and the symptom arrives as a support ticket rather than as a projection
	// failure.
	ActionRestore: {
		from: []State{StateSuspended}, to: StateActive,
		securityVersion: true, clear: "suspended_at",
	},
	ActionBeginOffboarding: {
		from: []State{StateActive, StateSuspended}, to: StateOffboarding,
		securityVersion: true, stamp: "offboarding_started_at",
	},
	ActionRetire: {
		from: []State{StateOffboarding}, to: StateRetired,
		securityVersion: true, stamp: "retired_at",
	},
}

var (
	// ErrInvalid is a malformed request: a required field absent, a value outside its permitted
	// set, or two fields that contradict each other.
	//
	// It exists so the HTTP surface can answer 400. Before it, every validation failure here was
	// a bare errors.New, indistinguishable at the transport boundary from a failed statement --
	// so a caller who omitted a field received 500, which says the service is broken rather than
	// that the request is. Constructor guards and stored-value decoders deliberately do NOT carry
	// it: those are a process built wrong and a row that should not exist, and both are 500.
	ErrInvalid = errors.New("tenant: the request is invalid")

	// ErrUnknownAction reports an action outside the state machine. It is a programming error: a
	// command surface maps a route to an Action, so an unknown one means a route exists without a
	// transition behind it.
	ErrUnknownAction = errors.New("tenant: action is not in the state machine")

	// ErrTransitionRefused reports a transition the machine does not permit from the current
	// state. It is a caller error and maps to a 409.
	ErrTransitionRefused = errors.New("tenant: transition is not permitted from the current state")

	// ErrRetired is separate so a caller can tell "not yet" from "never again". A retired Tenant
	// refuses every action permanently, and a client retrying a restore has a different problem
	// from one retrying an activation.
	ErrRetired = errors.New("tenant: a retired Tenant is terminal")

	// ErrNotFound reports an absent Tenant.
	ErrNotFound = errors.New("tenant: not found")

	// ErrVersionMismatch reports that the caller acted on a view that has since changed.
	// TDD-organization-control-003 §"API / Interface" refuses rather than retrying, because the
	// operator decided from a state that no longer holds — and a Tenant suspension decided from a
	// stale view is a decision about a different Tenant than the one on screen.
	ErrVersionMismatch = errors.New("tenant: the expected version does not match the stored one")
)

// Resolve reports the state an action moves to, or why it cannot.
func Resolve(action Action, current State) (State, error) {
	r, ok := transitions[action]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownAction, action)
	}
	if current == StateRetired {
		return "", ErrRetired
	}
	for _, permitted := range r.from {
		if permitted == current {
			return r.to, nil
		}
	}
	return "", fmt.Errorf("%w: %s from %s", ErrTransitionRefused, action, current)
}

// IncrementsSecurityVersion reports whether an action invalidates cached context in the Tenant.
func IncrementsSecurityVersion(action Action) bool { return transitions[action].securityVersion }

// Actions returns every action in the machine, so a test walks the table rather than a list
// somebody maintained beside it.
func Actions() []Action {
	all := make([]Action, 0, len(transitions))
	for action := range transitions {
		all = append(all, action)
	}
	return all
}

// eventTypes maps an action to the CloudEvents type its transition publishes.
//
// Two entries share `tenant.security.suspended`, and that is TDD-organization-control-003
// §"Published Events" as written: the event is the security consequence, not a mirror of the state
// name. Suspension and the offboarding freeze have the same consequence — every existing Tenant
// context must stop — and the payload carries the status, so a consumer that needs to tell them
// apart still can.
//
// The retirement event is named for the aggregate, not for the process that caused it.
// `TDD-organization-control-004` v1.1.0 called it `...tenant.offboarding.retired` while
// `TDD-organization-control-003` called the same fact `...tenant.lifecycle.retired`; the 003 name
// won and 004 was corrected in v1.2.0. Naming an event after its cause gives one fact two types
// depending on how it arose, leaving a consumer to subscribe to both and deduplicate — and the
// cause is already carried by the correlation identifier, which is where a cause belongs.
var eventTypes = map[Action]string{
	ActionActivate:         "com.scnehaux.organization.tenant.lifecycle.activated",
	ActionSuspend:          "com.scnehaux.organization.tenant.security.suspended",
	ActionRestore:          "com.scnehaux.organization.tenant.security.restored",
	ActionBeginOffboarding: "com.scnehaux.organization.tenant.security.suspended",
	ActionRetire:           "com.scnehaux.organization.tenant.lifecycle.retired",
}

// silentActions publish nothing, deliberately.
//
// A separate set rather than an absent key, so silence is a decision a reviewer can see. No
// context exists to invalidate before a Tenant has ever been active and no consumer projects a
// Tenant that has never existed to it, so an event here would carry a state change nobody outside
// this service can act on. `TestEveryActionEitherPublishesOrIsDeclaredSilent` asserts an action
// cannot be in neither set.
var silentActions = map[Action]bool{
	ActionProvision: true,
	ActionFail:      true,
}

// EventType returns the validated type for an action, and reports whether the action publishes at
// all. A silent action returns ("", false, nil): not publishing is a normal outcome, and folding it
// into an error would make every caller treat a designed silence as a failure.
func EventType(action Action) (event.Type, bool, error) {
	if silentActions[action] {
		return "", false, nil
	}
	name, ok := eventTypes[action]
	if !ok {
		return "", false, fmt.Errorf("%w: %q has no event type", ErrUnknownAction, action)
	}
	parsed, err := event.ParseType(name)
	if err != nil {
		return "", false, err
	}
	return parsed, true, nil
}

// Priority reports whether an action's event takes the reserved dispatch lane.
//
// Derived from the event's own classification rather than from a second table, so the name and the
// lane cannot disagree. An event named `security` that travelled the standard lane would tell a
// consumer it is urgent while the transport queued it behind a lifecycle backlog.
//
// Retirement increments the security version and is *not* priority, which is deliberate and not an
// oversight: the only way into `retired` is from `offboarding`, which already published a priority
// event and already froze context. By the time a Tenant retires there is no access left to
// withdraw, so the urgency was discharged one transition earlier.
func Priority(action Action) bool {
	return classification(eventTypes[action]) == "security"
}

// classification returns the fifth segment of a CloudEvents type, which is where this estate puts
// the class of the event.
func classification(eventType string) string {
	segments := splitDots(eventType)
	if len(segments) < 5 {
		return ""
	}
	return segments[4]
}

func splitDots(value string) []string {
	if value == "" {
		return nil
	}
	var parts []string
	current := ""
	for _, r := range value {
		if r == '.' {
			parts = append(parts, current)
			current = ""
			continue
		}
		current += string(r)
	}
	return append(parts, current)
}

// Tenant is one row of tenant.tenant, narrowed to what a transition reads and writes.
type Tenant struct {
	TenantID       id.UUID
	OrganizationID id.UUID
	Status         State

	// Version is the optimistic row version. It increments on every transition and is what a
	// caller passes back to prove it acted on the state it was shown.
	Version int64

	// SecurityVersion is the monotonic contextual-access version. STD-IAM-002 §3.5 rule 8 rejects
	// a token whose version is below the local projection's, which is what lets a suspension take
	// effect before the token expires. It increments only on transitions that invalidate context,
	// so a consumer seeing it unchanged can skip the refresh it would otherwise have to perform.
	SecurityVersion int64
}

// Payload is the event body every published Tenant transition carries.
//
// The complete state rather than a delta, for the same reason as the Membership payload: the
// priority lane may deliver a suspension before an older activation, and a consumer decides which
// desired state is newer by comparing versions rather than by arrival order — which ADR-GLB-003 §5
// states is not a guarantee the broker provides across partitions.
type Payload struct {
	TenantID       id.UUID `json:"tenant_id"`
	OrganizationID id.UUID `json:"organization_id"`
	TenantStatus   State   `json:"tenant_status"`

	// Both versions, because they answer different questions. TenantVersion orders two events
	// about this row; TenantSecurityVersion decides whether a held token is stale. Carrying only
	// the security version would leave two events about a restore-then-suspend pair ordered by
	// nothing, since a transition that does not increment it publishes the same value twice.
	TenantVersion         int64 `json:"tenant_version"`
	TenantSecurityVersion int64 `json:"tenant_security_version"`
}

// NewPayload builds the body for a committed transition.
func NewPayload(t Tenant) Payload {
	return Payload{
		TenantID:              t.TenantID,
		OrganizationID:        t.OrganizationID,
		TenantStatus:          t.Status,
		TenantVersion:         t.Version,
		TenantSecurityVersion: t.SecurityVersion,
	}
}

// Command is one requested transition on one Tenant.
type Command struct {
	TenantID id.UUID

	// Reason is required by the provider path rather than by this struct's politeness.
	// PAD-PLT-002 §5.2 makes cross-tenant administration carry a reason, and `db.WithProviderScope`
	// refuses a blank one — every Tenant transition is a provider action, so every one carries it.
	Reason string

	// ExpectedVersion is the `version` the caller was shown. Required rather than optional: a
	// check that can be skipped is skipped, and the case it protects — two operators acting on the
	// same Tenant from two stale views — is the case where the second write silently wins.
	ExpectedVersion int64
}

func (c Command) validate() error {
	switch {
	case c.TenantID.IsNil():
		return fmt.Errorf("%w: a tenant identifier is required", ErrInvalid)
	case c.Reason == "":
		return fmt.Errorf("%w: a reason is required", ErrInvalid)
	case c.ExpectedVersion <= 0:
		return fmt.Errorf("%w: the expected version the caller was shown is required", ErrInvalid)
	}
	return nil
}

// Result is what a committed transition reports back.
type Result struct {
	Tenant Tenant

	// AcceptedAt is a durability statement and not an enforcement one. Per STD-IAM-001 §3.4,
	// acknowledgement means the change is durable and queued; it MUST NOT be read as enforced
	// until the declared mechanisms have applied. A suspension is accepted here and enforced when
	// every consumer has seen the new security version, and the operational dashboard shows the
	// two separately because incident response works from the second one.
	AcceptedAt time.Time

	// Published reports whether the transition emitted an event. False for the provisioning
	// transitions, which are silent by design; returned rather than inferred so a caller does not
	// have to know the table to know whether anyone downstream will hear about this.
	Published bool
}

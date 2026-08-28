// Package membership holds the authoritative Membership state and the transitions permitted on
// it.
//
// PAD-PLT-002 makes this the sole enterprise authority for the relationship between a Principal
// and a Tenant. Every consumer holds a projection of what is decided here, and no consumer may
// promote its projection back into authority — so a transition refused here is refused
// everywhere, and a transition permitted here must reach every consumer or the estate disagrees
// with itself.
package membership

import (
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
)

// State is the lifecycle position of one Membership.
type State string

const (
	// StateActive grants the operating context.
	StateActive State = "active"

	// StateSuspended withholds it reversibly. A suspension is a containment action taken while
	// something is being decided, so it has a way back.
	StateSuspended State = "suspended"

	// StateRevoked withholds it permanently. There is no transition out: restoring revoked
	// access would make the audit trail say a Membership was revoked while the Principal holds
	// it, and a reviewer cannot tell that apart from a revocation that never applied. A new
	// Membership is granted instead, with its own provenance.
	StateRevoked State = "revoked"
)

// Valid reports whether the state is one this service persists.
func (s State) Valid() bool {
	switch s {
	case StateActive, StateSuspended, StateRevoked:
		return true
	}
	return false
}

// Action is a requested transition.
type Action string

const (
	ActionGrant   Action = "grant"
	ActionSuspend Action = "suspend"
	ActionRevoke  Action = "revoke"
	ActionRestore Action = "restore"
)

// transitions is the state machine as data rather than as a switch.
//
// A table can be printed in a test failure, walked to prove every state is reachable, and read
// by someone deciding whether a new action belongs. A switch spread across four methods cannot,
// and the transition that gets forgotten is always the one nobody looked at.
var transitions = map[Action]struct {
	from []State
	to   State

	// priority routes the resulting event to the reserved dispatch lane. Only the two that
	// withdraw access carry it: a grant arriving a minute late costs a retry, and a revocation
	// arriving a minute late is a minute of access after the decision to remove it.
	priority bool
}{
	ActionGrant:   {from: nil, to: StateActive},
	ActionSuspend: {from: []State{StateActive}, to: StateSuspended, priority: true},
	ActionRevoke:  {from: []State{StateActive, StateSuspended}, to: StateRevoked, priority: true},
	ActionRestore: {from: []State{StateSuspended}, to: StateActive},
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
	ErrInvalid = errors.New("membership: the request is invalid")

	// ErrUnknownAction reports an action outside the state machine. It is a programming error:
	// the HTTP surface maps a route to an Action, so an unknown one means a route was added
	// without a transition.
	ErrUnknownAction = errors.New("membership: action is not in the state machine")

	// ErrTransitionRefused reports a transition the state machine does not permit from the
	// current state. It is a caller error and maps to a 409.
	ErrTransitionRefused = errors.New("membership: transition is not permitted from the current state")

	// ErrNotFound reports an absent Membership. Under Row-Level Security this is also what a
	// caller bound to another Tenant sees, which is correct: telling it the Membership exists
	// elsewhere would leak the existence of a row it may not read.
	ErrNotFound = errors.New("membership: not found")

	// ErrRevoked is separate from ErrTransitionRefused so a caller can distinguish "not yet" from
	// "never again". Restoring a revoked Membership is refused permanently, and a client that
	// retries a suspension has a different problem from one that retries a revocation.
	ErrRevoked = errors.New("membership: a revoked Membership is terminal; grant a new one")
)

// Resolve reports the state an action moves to, or why it cannot.
func Resolve(action Action, current State) (State, bool, error) {
	rule, ok := transitions[action]
	if !ok {
		return "", false, fmt.Errorf("%w: %q", ErrUnknownAction, action)
	}
	if action == ActionGrant {
		return rule.to, rule.priority, nil
	}
	if current == StateRevoked {
		return "", false, ErrRevoked
	}
	for _, permitted := range rule.from {
		if permitted == current {
			return rule.to, rule.priority, nil
		}
	}
	return "", false, fmt.Errorf("%w: %s from %s", ErrTransitionRefused, action, current)
}

// Priority reports whether an action's event takes the reserved dispatch lane.
//
// Read from the table rather than from the resulting state, because the state alone does not say
// how it was reached: a Membership arriving at `active` by grant and by restore are the same row
// and different urgencies.
func Priority(action Action) bool { return transitions[action].priority }

// Actions returns every action in the state machine, so a test can walk the whole table rather
// than a list somebody maintained alongside it.
func Actions() []Action {
	all := make([]Action, 0, len(transitions))
	for action := range transitions {
		all = append(all, action)
	}
	return all
}

// Membership is one row of membership.membership.
type Membership struct {
	MembershipID id.UUID
	PrincipalID  id.UUID
	TenantID     id.UUID

	// WorkspaceID is nil for a Membership scoped to the Tenant as a whole. The composite foreign
	// key relies on MATCH SIMPLE so that case is satisfied without a lookup, which is why the
	// column is nullable rather than carrying a sentinel.
	WorkspaceID id.UUID

	SubjectType string
	Status      State

	// Version increments on every status transition and never decreases. A consumer rejects a
	// token whose version is below the one its projection holds, which is what lets a revocation
	// take effect before the token expires.
	Version int64

	ValidFrom  time.Time
	ValidUntil time.Time
	Provenance string
}

// eventTypes maps an action to the CloudEvents type its transition publishes.
//
// The names are not derived from the action. `event.ParseType` requires the fifth segment to
// classify the event, and the classification is a decision rather than a spelling: `security`
// events withdraw access and occupy the priority lane, `lifecycle` events do not. Deriving the
// name would let a new action pick its own lane by accident.
var eventTypes = map[Action]string{
	ActionGrant:   "com.scnehaux.organization.membership.lifecycle.granted",
	ActionRestore: "com.scnehaux.organization.membership.lifecycle.restored",
	ActionSuspend: "com.scnehaux.organization.membership.security.suspended",
	ActionRevoke:  "com.scnehaux.organization.membership.security.revoked",
}

// EventType returns the validated type for an action.
func EventType(action Action) (event.Type, error) {
	name, ok := eventTypes[action]
	if !ok {
		return "", fmt.Errorf("%w: %q has no event type", ErrUnknownAction, action)
	}
	return event.ParseType(name)
}

// Payload is the event body every Membership state change carries.
//
// It carries the complete security state rather than a delta, because the priority lane may
// deliver a revocation before an older grant. A consumer decides which desired state is newer by
// comparing versions, never by delivery order or stream position, and it cannot compare what the
// event did not carry.
type Payload struct {
	MembershipID          id.UUID  `json:"membership_id"`
	PrincipalID           id.UUID  `json:"principal_id"`
	TenantID              id.UUID  `json:"tenant_id"`
	WorkspaceID           *id.UUID `json:"workspace_id"`
	MembershipStatus      State    `json:"membership_status"`
	MembershipVersion     int64    `json:"membership_version"`
	TenantSecurityVersion int64    `json:"tenant_security_version"`
}

// NewPayload builds the body for a committed transition.
func NewPayload(m Membership, tenantSecurityVersion int64) Payload {
	payload := Payload{
		MembershipID:          m.MembershipID,
		PrincipalID:           m.PrincipalID,
		TenantID:              m.TenantID,
		MembershipStatus:      m.Status,
		MembershipVersion:     m.Version,
		TenantSecurityVersion: tenantSecurityVersion,
	}
	// A pointer so an absent Workspace serialises as null rather than as the nil UUID. A
	// consumer distinguishing "Tenant-wide" from "Workspace 00000000-..." on the wire needs the
	// difference to survive marshalling.
	if !m.WorkspaceID.IsNil() {
		workspace := m.WorkspaceID
		payload.WorkspaceID = &workspace
	}
	return payload
}

// Package invitation holds the intent that someone should come to hold Membership.
//
// # An invitation is an intent, not a credential
//
// It says a Tenant administrator wants a particular identifier to hold Membership. It does not say
// who holds that identifier. SAD-004 §5.5 states it directly: possession of an invitation never
// proves identity. A design that granted Membership on link click would have authenticated an email
// inbox, and an inbox is not a Principal.
//
// Every control here follows from that one sentence. Membership activates on the *join* of two
// independent facts — an unexpired invitation, and a verified identity carrying the invited
// identifier — and neither alone activates anything. The Tenant is rechecked at activation, because
// the window between issuing an invitation and accepting it is exactly what a long-lived invitation
// creates. And the anonymous lookup discloses nothing, because a distinguishable response turns an
// opaque token into an oracle.
package invitation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
)

// State is the invitation lifecycle position.
type State string

const (
	// StatePending is issued and waiting. Nothing has been verified.
	StatePending State = "pending"

	// StateIdentityVerified means the identity kernel has confirmed a Principal holds the invited
	// identifier. One of the two facts is in hand; Membership still does not exist.
	//
	// A distinct state rather than a flag, because it is the point at which `principal_id` becomes
	// known and the point from which acceptance is possible. Collapsing it into `pending` would
	// make "waiting for identity" and "waiting for acceptance" the same row, and the second is the
	// only one an operator can act on.
	StateIdentityVerified State = "identity_verified"

	// StateAccepted is terminal and successful: Membership exists.
	StateAccepted State = "accepted"

	// StateExpired is terminal. Materialised by the sweep rather than computed at read time, so
	// an expired invitation reads as expired in every listing without each reader reimplementing
	// the comparison.
	StateExpired State = "expired"

	// StateRevoked is terminal. The inviter withdrew the intent.
	StateRevoked State = "revoked"
)

// Valid reports whether the state is one this service persists. Mirrors `invitation_state_check`.
func (s State) Valid() bool {
	switch s {
	case StatePending, StateIdentityVerified, StateAccepted, StateExpired, StateRevoked:
		return true
	}
	return false
}

// Outstanding reports whether the invitation still occupies the one-per-target slot.
//
// Read from one place so the partial unique index and every gate agree on what "outstanding"
// means. The index is `WHERE state IN ('pending', 'identity_verified')`, and this is that
// predicate in Go.
func (s State) Outstanding() bool {
	return s == StatePending || s == StateIdentityVerified
}

// Action is a requested transition.
type Action string

const (
	// ActionVerifyIdentity records that the kernel confirmed the invited identifier.
	ActionVerifyIdentity Action = "verify-identity"

	// ActionAccept creates the Membership.
	ActionAccept Action = "accept"

	// ActionRevoke withdraws the intent.
	ActionRevoke Action = "revoke"

	// ActionExpire is the sweep materialising a lapsed invitation.
	ActionExpire Action = "expire"
)

// transitions is the machine as data.
//
// `accept` is reachable from `identity_verified` only. That single row is the join SAD-004 §5.5
// requires: an invitation cannot be accepted before the kernel has confirmed who holds the invited
// identifier, so possession of the token cannot create Membership on its own.
//
// `revoke` and `expire` are reachable from both outstanding states, because an intent can be
// withdrawn or lapse at any point before Membership exists.
var transitions = map[Action]struct {
	from []State
	to   State
}{
	ActionVerifyIdentity: {from: []State{StatePending}, to: StateIdentityVerified},
	ActionAccept:         {from: []State{StateIdentityVerified}, to: StateAccepted},
	ActionRevoke:         {from: []State{StatePending, StateIdentityVerified}, to: StateRevoked},
	ActionExpire:         {from: []State{StatePending, StateIdentityVerified}, to: StateExpired},
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
	ErrInvalid = errors.New("invitation: the request is invalid")

	// ErrUnknownAction reports an action outside the machine.
	ErrUnknownAction = errors.New("invitation: action is not in the state machine")

	// ErrTransitionRefused reports a transition the machine does not permit. Maps to a 409.
	ErrTransitionRefused = errors.New("invitation: transition is not permitted from the current state")

	// ErrSettled reports an invitation that has already reached a terminal state. Separate from
	// ErrTransitionRefused so a caller can tell "not yet" from "never again": a client retrying
	// acceptance on a revoked invitation has a different problem from one retrying too early.
	ErrSettled = errors.New("invitation: the invitation is already settled")

	// ErrNotFound reports an absent invitation. Under Row-Level Security this is also what a
	// caller bound to another Tenant sees.
	ErrNotFound = errors.New("invitation: not found")

	// ErrExpired reports an invitation past its expiry. Distinct from ErrSettled because the
	// invitation may still be in an outstanding state — expiry is a fact about the clock, and the
	// sweep has not necessarily materialised it yet.
	ErrExpired = errors.New("invitation: the invitation has expired")

	// ErrTenantNotActive refuses acceptance into a Tenant that is not active. The recheck exists
	// because an invitation issued while a Tenant was active may be accepted after it was
	// suspended, and that window is exactly what a long-lived invitation creates.
	ErrTenantNotActive = errors.New("invitation: the Tenant is not active")

	// ErrAlreadyMember refuses acceptance when an active Membership already exists for the
	// subject and context. The partial unique index would refuse the insert; refusing here names
	// the reason instead of surfacing a constraint violation.
	ErrAlreadyMember = errors.New("invitation: an active Membership already exists for this subject and context")

	// ErrTTL reports a requested lifetime outside the permitted range.
	ErrTTL = errors.New("invitation: the requested lifetime is outside the permitted range")

	// ErrToken reports a token that cannot be a token this service issued.
	ErrToken = errors.New("invitation: the token is not well formed")
)

// Resolve reports the state an action moves to, or why it cannot.
func Resolve(action Action, current State) (State, error) {
	rule, ok := transitions[action]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownAction, action)
	}
	if !current.Outstanding() {
		return "", fmt.Errorf("%w: %s", ErrSettled, current)
	}
	for _, permitted := range rule.from {
		if permitted == current {
			return rule.to, nil
		}
	}
	return "", fmt.Errorf("%w: %s from %s", ErrTransitionRefused, action, current)
}

// Actions returns every action in the machine, so a test walks the table rather than a list
// maintained beside it.
func Actions() []Action {
	all := make([]Action, 0, len(transitions))
	for action := range transitions {
		all = append(all, action)
	}
	return all
}

const (
	// DefaultTTL matches ORGANIZATION_INVITATION_TTL.
	DefaultTTL = 7 * 24 * time.Hour

	// MaxTTL matches ORGANIZATION_INVITATION_MAX_TTL and is a ceiling an inviter cannot exceed.
	// An invitation without an effective bound is a standing grant nobody revokes, and it will be
	// found years later still valid.
	MaxTTL = 30 * 24 * time.Hour

	// tokenBytes is the entropy behind the token the invitee carries.
	//
	// 32 bytes. The token space is the only thing protecting the flow before authentication, so it
	// is sized to be unguessable rather than to be convenient — the alternative the design's table
	// left open was `invitation_id`, a time-ordered UUIDv7 whose space an attacker who knows when
	// invitations were issued can narrow sharply.
	tokenBytes = 32
)

// Token is the value the invitee carries. It exists in memory and in the invitation link, and
// never in the database.
type Token string

// NewToken mints a token and returns it with the hash to persist.
//
// `crypto/rand` and not `math/rand`: this value is the whole protection of the pre-authentication
// path, and a predictable generator would make the entropy above decorative.
func NewToken() (Token, string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("invitation: mint token: %w", err)
	}
	token := Token(base64.RawURLEncoding.EncodeToString(raw))
	return token, token.Hash(), nil
}

// Hash returns the stored form of a token.
//
// SHA-256 rather than a password hash. A password hash exists to survive an offline attack on a
// low-entropy secret; this secret carries 256 bits of entropy from a CSPRNG, so there is nothing
// to slow down — and the lookup happens on a request path where a deliberately slow hash would be
// a denial-of-service surface an anonymous caller could reach.
func (t Token) Hash() string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// WellFormed reports whether the value could be a token this service issued.
//
// Shape only, and deliberately not a database lookup. It is what the anonymous path checks: the
// response is identical for a well-formed token that exists and one that never did, so reading the
// row would disclose nothing and would need cross-Tenant authority to do it.
func (t Token) WellFormed() bool {
	if len(t) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(string(t))
	return err == nil
}

// HashIdentifier returns the correlation form of an invited identifier.
//
// Lower-cased first, because an identifier differing only in case is the same person and two
// pending invitations for one person in one context is precisely what the partial unique index
// exists to refuse.
func HashIdentifier(identifier string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(identifier))))
	return hex.EncodeToString(sum[:])
}

// Invitation is one row of invitation.invitation.
type Invitation struct {
	InvitationID id.UUID
	TenantID     id.UUID

	// WorkspaceID is nil for an invitation into the Tenant as a whole.
	WorkspaceID *id.UUID

	// TargetIdentifier is Tier-2 identifiable PII under STD-GLB-007. It is carried here because
	// the inviter's own listing needs to show who was invited; it is never placed in an event or
	// in an anonymous response.
	TargetIdentifier string
	TargetHash       string

	SubjectType   string
	InvitedBy     id.UUID
	Reason        string
	State         State
	CorrelationID id.UUID
	PrincipalID   *id.UUID
	ExpiresAt     time.Time
	CreatedAt     time.Time
	AcceptedAt    *time.Time
	RevokedAt     *time.Time
}

// Expired reports whether the invitation has lapsed, regardless of whether the sweep has
// materialised it yet. Acceptance consults this rather than the state alone, which closes the race
// between the sweep and an acceptance arriving in the same moment.
func (i Invitation) Expired(now time.Time) bool {
	return !now.Before(i.ExpiresAt)
}

// eventTypes maps an action to the type its transition publishes.
//
// The class segment is `invitation`, and none of these is `security`: an invitation grants nothing,
// so no event here withdraws anything. The event that grants access is the Membership's own
// `membership.lifecycle.granted`, published by the Membership service inside the acceptance
// transaction.
//
// `verify-identity` publishes nothing. It records that one of the two required facts arrived, and
// no consumer outside this service can act on half a join — the actionable event is the Membership
// that follows.
var eventTypes = map[Action]string{
	ActionAccept: "com.scnehaux.organization.membership.invitation.accepted",
	ActionRevoke: "com.scnehaux.organization.membership.invitation.revoked",
	ActionExpire: "com.scnehaux.organization.membership.invitation.expired",
}

// requestedEventType is published when the intent is recorded, so the identity flow can begin.
const requestedEventType = "com.scnehaux.organization.membership.invitation.requested"

// silentActions publish nothing, declared rather than absent so a test can assert an action is
// never in neither set.
var silentActions = map[Action]bool{ActionVerifyIdentity: true}

// EventType returns the validated type for an action, and reports whether the action publishes.
func EventType(action Action) (event.Type, bool, error) {
	if silentActions[action] {
		return "", false, nil
	}
	raw, ok := eventTypes[action]
	if !ok {
		return "", false, fmt.Errorf("%w: %q has no event type", ErrUnknownAction, action)
	}
	parsed, err := event.ParseType(raw)
	if err != nil {
		return "", false, err
	}
	return parsed, true, nil
}

// Payload is the body every invitation event carries.
//
// It carries no target identifier and no display name. STD-GLB-007 makes the identifier Tier-2
// PII, and an event stream is read by consumers that have no business knowing who was invited —
// they need to know that an intent exists, changed, or lapsed, keyed by identifiers they can
// correlate.
type Payload struct {
	InvitationID  id.UUID  `json:"invitation_id"`
	TenantID      id.UUID  `json:"tenant_id"`
	WorkspaceID   *id.UUID `json:"workspace_id"`
	SubjectType   string   `json:"subject_type"`
	State         State    `json:"invitation_state"`
	CorrelationID id.UUID  `json:"correlation_id"`
	PrincipalID   *id.UUID `json:"principal_id"`
}

// NewPayload builds the body for a committed change.
func NewPayload(i Invitation) Payload {
	return Payload{
		InvitationID:  i.InvitationID,
		TenantID:      i.TenantID,
		WorkspaceID:   i.WorkspaceID,
		SubjectType:   i.SubjectType,
		State:         i.State,
		CorrelationID: i.CorrelationID,
		PrincipalID:   i.PrincipalID,
	}
}

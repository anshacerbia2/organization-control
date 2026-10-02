package authority

// Publishing provider grants to the services that enforce them (ADR-ORG-002 §5.3,
// TDD-organization-control-001 §Provider Authority Projection).
//
// The Identity Control API reads provider:identity-control grants and activations from a local
// projection. Each transition that changes what such a grant confers publishes one event, in the
// transition's own transaction, carrying the grant's whole state and a version one higher than the
// last. A consumer applies an event only when its version is higher than the one it holds, so
// delivery order never decides which state is newer.

import (
	"context"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/system"
)

// ScopeIdentityControl is the Identity Control API's provider scope (STD-IAM-002 §3.1.1).
const ScopeIdentityControl = "provider:identity-control"

// Scopes are the registered provider scopes a grant may name. provider_grant_scope_check admits
// these and nothing else.
var Scopes = []string{Scope, ScopeIdentityControl}

// PublishedScopes are the scopes whose grant transitions are published. This service enforces its
// own scope from its records on every request, so no consumer needs those grants, and publishing
// them would tell another service which Principals can administer this one (NIST SP 800-53 AC-6).
var PublishedScopes = []string{ScopeIdentityControl}

// The provider grant event types. A withdrawal -- an activation ended early, a grant revoked --
// travels in the priority lane.
const (
	EventGranted   event.Type = "com.scnehaux.organization.provider.lifecycle.granted"
	EventActivated event.Type = "com.scnehaux.organization.provider.lifecycle.activated"
	EventEnded     event.Type = "com.scnehaux.organization.provider.security.ended"
	EventRevoked   event.Type = "com.scnehaux.organization.provider.security.revoked"
)

// EventTypes are every type this package publishes.
var EventTypes = []event.Type{EventGranted, EventActivated, EventEnded, EventRevoked}

// The grant statuses an event carries.
const (
	GrantActive  = "active"
	GrantRevoked = "revoked"
)

// GrantState is a provider grant event's payload, and a provider authority snapshot row: what the
// grant confers now.
type GrantState struct {
	GrantID      id.UUID          `json:"grant_id"`
	PrincipalID  id.UUID          `json:"principal_id"`
	Scope        string           `json:"scope"`
	Kind         string           `json:"kind"`
	GrantStatus  string           `json:"grant_status"`
	GrantVersion int64            `json:"grant_version"`
	Activation   *ActivationState `json:"activation"`
}

// ActivationState is the activation in force. It confers authority until EndsAt, which the
// consumer evaluates on its own clock: no event announces a natural end.
type ActivationState struct {
	ActivationID id.UUID   `json:"activation_id"`
	EndsAt       time.Time `json:"ends_at"`
}

func published(scope string) bool {
	for _, s := range PublishedScopes {
		if s == scope {
			return true
		}
	}
	return false
}

func registeredScope(scope string) bool {
	for _, s := range Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// grantScopeStatement reads which scope a grant is for, to decide whether its transition is
// published. One row always: an unknown grant reads as the empty scope, which is published nowhere.
const grantScopeStatement = `SELECT coalesce((
    SELECT scope FROM organization.provider_grant WHERE grant_id = $1), '')`

// bumpVersionStatement advances the grant's version for the event about to be published.
const bumpVersionStatement = `UPDATE organization.provider_grant
SET grant_version = grant_version + 1
WHERE grant_id = $1
RETURNING grant_version`

// grantStateStatement reads what the grant confers now: its kind and status, and the activation in
// force, if any. A revoked grant confers nothing, so it carries no activation.
const grantStateStatement = `SELECT g.principal_id::text, g.scope, g.kind, g.revoked_at IS NOT NULL,
       coalesce(a.activation_id::text, ''), a.ends_at
  FROM organization.provider_grant g
  LEFT JOIN LATERAL (
        SELECT activation_id, ends_at
          FROM organization.provider_activation
         WHERE grant_id = g.grant_id
           AND decision = 'approved' AND ended_at IS NULL AND now() < ends_at
         ORDER BY ends_at DESC
         LIMIT 1) a ON g.revoked_at IS NULL
 WHERE g.grant_id = $1`

// recordGrantEventStatement records which version an event carries, in the publishing transaction,
// so the row exists if and only if the event does. The SUPERSEDED resolution predicate reads it
// (TDD-organization-control-005).
const recordGrantEventStatement = `INSERT INTO organization.provider_grant_event
    (event_id, grant_id, grant_version, event_type)
VALUES ($1, $2, $3, $4)`

// publish writes the event for a transition of the grant, inside the transition's transaction,
// when the grant's scope is published. It is called after the transition is written, so the state
// it reads is the state the transition produced.
func publish(ctx context.Context, tx db.Tx, grantID id.UUID, eventType event.Type) error {
	var scope string
	if err := tx.QueryRow(ctx, grantScopeStatement, grantID.String()).Scan(&scope); err != nil {
		return fmt.Errorf("authority: reading the scope of grant %s: %w", grantID, err)
	}
	if !published(scope) {
		return nil
	}

	state := GrantState{GrantID: grantID, GrantStatus: GrantActive}
	if err := tx.QueryRow(ctx, bumpVersionStatement, grantID.String()).Scan(&state.GrantVersion); err != nil {
		return fmt.Errorf("authority: advancing the version of grant %s: %w", grantID, err)
	}
	var (
		principal, activationID string
		revoked                 bool
		endsAt                  *time.Time
	)
	if err := tx.QueryRow(ctx, grantStateStatement, grantID.String()).Scan(
		&principal, &state.Scope, &state.Kind, &revoked, &activationID, &endsAt); err != nil {
		return fmt.Errorf("authority: reading the state of grant %s: %w", grantID, err)
	}
	parsed, err := id.Parse(principal)
	if err != nil {
		return fmt.Errorf("authority: grant %s names principal_id %q: %w", grantID, principal, err)
	}
	state.PrincipalID = parsed
	if revoked {
		state.GrantStatus = GrantRevoked
	}
	if activationID != "" && endsAt != nil {
		activation, err := id.Parse(activationID)
		if err != nil {
			return fmt.Errorf("authority: grant %s has activation %q: %w", grantID, activationID, err)
		}
		state.Activation = &ActivationState{ActivationID: activation, EndsAt: endsAt.UTC()}
	}

	envelope, err := event.New(system.Source, eventType, time.Now().UTC(), state)
	if err != nil {
		return fmt.Errorf("authority: building the %s event: %w", eventType, err)
	}
	var opts []outbox.Option
	if eventType == EventEnded || eventType == EventRevoked {
		opts = append(opts, outbox.Priority())
	}
	if err := outbox.Append(ctx, tx, grantID, envelope, opts...); err != nil {
		return fmt.Errorf("authority: appending the %s event: %w", eventType, err)
	}
	if _, err := tx.Exec(ctx, recordGrantEventStatement, envelope.ID.String(), grantID.String(),
		state.GrantVersion, string(eventType)); err != nil {
		return fmt.Errorf("authority: recording the %s event's version: %w", eventType, err)
	}
	return nil
}

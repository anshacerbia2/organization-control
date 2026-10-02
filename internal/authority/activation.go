package authority

// Provider activations (ADR-ORG-002, TDD-organization-control-001 §Provider Activation). An eligible
// provider grant confers authority only while an activation of it is in force. In production an
// activation is approved by another holder of a grant for the scope, a rule the database holds as
// well. An unapproved request lapses after 24 hours, and nothing is deleted: an activation is the
// record of who held cross-Tenant authority, when, why, and on whose approval.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// PendingLifetime is how long a request waits for a decision before it lapses, as Entra's does.
const PendingLifetime = 24 * time.Hour

// The decisions an activation records.
const (
	DecisionApproved = "approved"
	DecisionDenied   = "denied"
	DecisionLapsed   = "lapsed"
)

var (
	// ErrActivationInvalid means an activation request or decision is incomplete.
	ErrActivationInvalid = errors.New("authority: invalid provider activation request")

	// ErrActivationTooLong means the duration asked for exceeds the deployment's maximum.
	ErrActivationTooLong = errors.New("authority: the activation is longer than the maximum this deployment allows")

	// ErrEmergencyGrant means an activation was asked of an emergency grant, which is in force
	// without one.
	ErrEmergencyGrant = errors.New("authority: an emergency grant is in force without an activation")

	// ErrNotHolder means the caller asked to activate a grant it does not hold, or decided on an
	// activation without holding a grant for the scope.
	ErrNotHolder = errors.New("authority: the caller holds no unrevoked grant this requires")

	// ErrActivationInForce means the grant already has an activation in force.
	ErrActivationInForce = errors.New("authority: the grant already has an activation in force")

	// ErrActivationPending means the grant already has a request waiting for a decision.
	ErrActivationPending = errors.New("authority: the grant already has a request waiting for a decision")

	// ErrActivationNotFound means no activation has the identifier.
	ErrActivationNotFound = errors.New("authority: no provider activation has this identifier")

	// ErrActivationDecided means the activation is not a pending request: decided, or lapsed.
	ErrActivationDecided = errors.New("authority: the activation is no longer waiting for a decision")

	// ErrSelfApproval means the holder approved or denied its own activation (NIST AC-5).
	ErrSelfApproval = errors.New("authority: an activation is decided by a holder other than the one it activates")

	// ErrActivationNotInForce means an end was asked of an activation that is not in force.
	ErrActivationNotInForce = errors.New("authority: the activation is not in force")
)

// Activation is one activation, as recorded.
type Activation struct {
	ID               id.UUID
	Grant            id.UUID
	Principal        id.UUID
	Scope            string
	Reason           string
	DurationSeconds  int
	ApprovalRequired bool
	RequestedAt      time.Time
	DecidedBy        *id.UUID
	Decision         string
	DecisionReason   string
	DecidedAt        *time.Time
	EndsAt           *time.Time
	EndedBy          *id.UUID
	EndReason        string
	EndedAt          *time.Time
}

// InForce is whether the activation confers authority at the instant: approved, not ended, and
// before its end. A revoked grant also ends it, which the authority read checks.
func (a Activation) InForce(at time.Time) bool {
	return a.Decision == DecisionApproved && a.EndedAt == nil && a.EndsAt != nil && at.Before(*a.EndsAt)
}

// ActivationPolicy is the deployment's rules for activations.
type ActivationPolicy struct {
	// Max is the longest activation a request may ask for (ORGANIZATION_PROVIDER_ACTIVATION_MAX).
	Max time.Duration

	// ApprovalRequired is ORGANIZATION_PROVIDER_ACTIVATION_APPROVAL=required. Startup refuses
	// optional in production.
	ApprovalRequired bool
}

// Activations requests, decides and ends activations, as the provider role in the provider scope.
type Activations struct {
	pool   *db.ProviderPool
	policy ActivationPolicy
	id     func() (id.UUID, error)
}

// NewActivations builds the activation service on the provider pool.
func NewActivations(pool *db.ProviderPool, policy ActivationPolicy) (*Activations, error) {
	switch {
	case pool == nil:
		return nil, errors.New("authority: a provider pool is required")
	case policy.Max <= 0:
		return nil, errors.New("authority: a positive maximum activation is required")
	}
	return &Activations{pool: pool, policy: policy, id: id.NewV7}, nil
}

const activationColumns = `SELECT activation_id::text, grant_id::text, principal_id::text, scope, reason,
       duration_seconds, approval_required, requested_at, coalesce(decided_by::text, ''),
       coalesce(decision, ''), coalesce(decision_reason, ''), decided_at, ends_at,
       coalesce(ended_by::text, ''), coalesce(end_reason, ''), ended_at
FROM organization.provider_activation`

const listActivationsStatement = activationColumns + `
WHERE decision IS NULL OR (decision = 'approved' AND ended_at IS NULL AND now() < ends_at)
   OR activation_id IN (SELECT activation_id FROM organization.provider_activation
                        ORDER BY requested_at DESC LIMIT 100)
ORDER BY requested_at DESC`

const lockActivationStatement = activationColumns + `
WHERE activation_id = $1
FOR UPDATE`

// grantForActivationStatement reads the grant an activation is asked of, locked so a revocation and
// a request for the same grant apply one after the other.
const grantForActivationStatement = `SELECT principal_id::text, scope, kind, revoked_at IS NOT NULL
FROM organization.provider_grant
WHERE grant_id = $1
FOR UPDATE`

// holderStatement is whether the Principal holds an unrevoked grant for the scope, of either kind.
const holderStatement = `SELECT EXISTS (
    SELECT 1 FROM organization.provider_grant
    WHERE principal_id = $1 AND scope = $2 AND revoked_at IS NULL)`

const activeOfGrantStatement = `SELECT EXISTS (
    SELECT 1 FROM organization.provider_activation
    WHERE grant_id = $1 AND decision = 'approved' AND ended_at IS NULL AND now() < ends_at)`

const lapsePendingStatement = `UPDATE organization.provider_activation
SET decision = 'lapsed', decided_at = now()
WHERE grant_id = $1 AND decision IS NULL AND requested_at <= now() - make_interval(secs => $2)`

const pendingOfGrantStatement = `SELECT EXISTS (
    SELECT 1 FROM organization.provider_activation WHERE grant_id = $1 AND decision IS NULL)`

const insertActivationStatement = `INSERT INTO organization.provider_activation
    (activation_id, grant_id, principal_id, scope, reason, duration_seconds, approval_required)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

const decideActivationStatement = `UPDATE organization.provider_activation
SET decided_by = $2, decision = $3, decision_reason = $4, decided_at = now(),
    ends_at = CASE WHEN $3 = 'approved' THEN now() + make_interval(secs => duration_seconds) END
WHERE activation_id = $1 AND decision IS NULL`

const endActivationStatement = `UPDATE organization.provider_activation
SET ended_by = $2, end_reason = $3, ended_at = now()
WHERE activation_id = $1 AND decision = 'approved' AND ended_at IS NULL AND now() < ends_at`

// List reads the pending requests, the activations in force, and the last hundred, newest first.
func (a *Activations) List(ctx context.Context, reason string) ([]Activation, error) {
	var activations []Activation
	err := db.WithProviderScope(ctx, a.pool, reason, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, listActivationsStatement)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			activation, err := scanActivation(rows)
			if err != nil {
				return err
			}
			activations = append(activations, activation)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("authority: list provider activations: %w", err)
	}
	return activations, nil
}

// Request asks to activate the caller's grant for the duration. With approval required it is
// recorded pending; otherwise it is recorded approved by its holder, in force at once.
func (a *Activations) Request(ctx context.Context, grantID id.UUID, duration time.Duration, reason string) (Activation, error) {
	scope, ok := db.ScopeFrom(ctx)
	switch {
	case !ok:
		return Activation{}, db.ErrNoScope
	case grantID.IsNil():
		return Activation{}, fmt.Errorf("%w: a grant_id is required", ErrActivationInvalid)
	case duration < time.Second:
		return Activation{}, fmt.Errorf("%w: a duration of at least one second is required", ErrActivationInvalid)
	case duration > a.policy.Max:
		return Activation{}, fmt.Errorf("%w: at most %s", ErrActivationTooLong, a.policy.Max)
	case strings.TrimSpace(reason) == "":
		return Activation{}, fmt.Errorf("%w: a reason is required", ErrActivationInvalid)
	}
	activationID, err := a.id()
	if err != nil {
		return Activation{}, fmt.Errorf("authority: mint activation identifier: %w", err)
	}
	holder := scope.Actor()

	var activation Activation
	err = db.WithProviderScope(ctx, a.pool, reason, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, grantForActivationStatement, grantID.String())
		if err != nil {
			return err
		}
		if !rows.Next() {
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			return ErrNotHolder
		}
		var (
			principal, grantScope, kind string
			revoked                     bool
		)
		if err := rows.Scan(&principal, &grantScope, &kind, &revoked); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		switch {
		case revoked || principal != holder.String():
			return ErrNotHolder
		case kind == KindEmergency:
			return ErrEmergencyGrant
		}
		var active bool
		if err := tx.QueryRow(ctx, activeOfGrantStatement, grantID.String()).Scan(&active); err != nil {
			return err
		}
		if active {
			return ErrActivationInForce
		}
		if _, err := tx.Exec(ctx, lapsePendingStatement, grantID.String(), int(PendingLifetime/time.Second)); err != nil {
			return err
		}
		var pending bool
		if err := tx.QueryRow(ctx, pendingOfGrantStatement, grantID.String()).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return ErrActivationPending
		}
		if _, err := tx.Exec(ctx, insertActivationStatement, activationID.String(), grantID.String(), holder.String(),
			grantScope, strings.TrimSpace(reason), int(duration/time.Second), a.policy.ApprovalRequired); err != nil {
			return err
		}
		if !a.policy.ApprovalRequired {
			// No approval is required, so the holder's own reason is the decision's.
			if _, err := tx.Exec(ctx, decideActivationStatement, activationID.String(), holder.String(),
				DecisionApproved, strings.TrimSpace(reason)); err != nil {
				return err
			}
		}
		activation, err = readActivation(ctx, tx, activationID)
		return err
	})
	if err != nil {
		return Activation{}, activationError("request a provider activation", err)
	}
	return activation, nil
}

// Decide approves or denies a pending request. The decider holds a grant for the scope and is not
// the activation's holder.
func (a *Activations) Decide(ctx context.Context, activationID id.UUID, decision, reason string) (Activation, error) {
	scope, ok := db.ScopeFrom(ctx)
	switch {
	case !ok:
		return Activation{}, db.ErrNoScope
	case activationID.IsNil():
		return Activation{}, fmt.Errorf("%w: an activation identifier is required", ErrActivationInvalid)
	case decision != DecisionApproved && decision != DecisionDenied:
		return Activation{}, fmt.Errorf("%w: an activation is approved or denied", ErrActivationInvalid)
	case strings.TrimSpace(reason) == "":
		return Activation{}, fmt.Errorf("%w: a reason is required", ErrActivationInvalid)
	}
	decider := scope.Actor()

	var activation Activation
	err := db.WithProviderScope(ctx, a.pool, reason, func(ctx context.Context, tx db.Tx) error {
		current, err := lockActivation(ctx, tx, activationID)
		if err != nil {
			return err
		}
		var holds bool
		if err := tx.QueryRow(ctx, holderStatement, decider.String(), current.Scope).Scan(&holds); err != nil {
			return err
		}
		switch {
		case !holds:
			return ErrNotHolder
		case current.Principal == decider:
			return ErrSelfApproval
		case current.Decision != "":
			return fmt.Errorf("%w: it is %s", ErrActivationDecided, current.Decision)
		case time.Since(current.RequestedAt) > PendingLifetime:
			return fmt.Errorf("%w: it was requested more than %s ago and has lapsed", ErrActivationDecided, PendingLifetime)
		}
		tag, err := tx.Exec(ctx, decideActivationStatement, activationID.String(), decider.String(), decision,
			strings.TrimSpace(reason))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrActivationDecided
		}
		activation, err = readActivation(ctx, tx, activationID)
		return err
	})
	if err != nil {
		return Activation{}, activationError("decide a provider activation", err)
	}
	return activation, nil
}

// End ends an activation in force, by its holder or by a provider in force. holderOrProvider is the
// caller's authority to end it, decided by the transport: the activation's holder may end its own,
// and a provider in force may end any.
func (a *Activations) End(ctx context.Context, activationID id.UUID, providerInForce bool, reason string) (Activation, error) {
	scope, ok := db.ScopeFrom(ctx)
	switch {
	case !ok:
		return Activation{}, db.ErrNoScope
	case activationID.IsNil():
		return Activation{}, fmt.Errorf("%w: an activation identifier is required", ErrActivationInvalid)
	case strings.TrimSpace(reason) == "":
		return Activation{}, fmt.Errorf("%w: a reason is required", ErrActivationInvalid)
	}
	caller := scope.Actor()

	var activation Activation
	err := db.WithProviderScope(ctx, a.pool, reason, func(ctx context.Context, tx db.Tx) error {
		current, err := lockActivation(ctx, tx, activationID)
		if err != nil {
			return err
		}
		if current.Principal != caller && !providerInForce {
			return ErrNotHolder
		}
		tag, err := tx.Exec(ctx, endActivationStatement, activationID.String(), caller.String(), strings.TrimSpace(reason))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrActivationNotInForce
		}
		activation, err = readActivation(ctx, tx, activationID)
		return err
	})
	if err != nil {
		return Activation{}, activationError("end a provider activation", err)
	}
	return activation, nil
}

var activationSentinels = []error{ErrActivationInvalid, ErrActivationTooLong, ErrEmergencyGrant, ErrNotHolder,
	ErrActivationInForce, ErrActivationPending, ErrActivationNotFound, ErrActivationDecided, ErrSelfApproval,
	ErrActivationNotInForce}

func activationError(action string, err error) error {
	for _, sentinel := range activationSentinels {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("authority: %s: %w", action, err)
}

func lockActivation(ctx context.Context, tx db.Tx, activationID id.UUID) (Activation, error) {
	rows, err := tx.Query(ctx, lockActivationStatement, activationID.String())
	if err != nil {
		return Activation{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Activation{}, err
		}
		return Activation{}, ErrActivationNotFound
	}
	return scanActivation(rows)
}

func readActivation(ctx context.Context, tx db.Tx, activationID id.UUID) (Activation, error) {
	return scanActivation(tx.QueryRow(ctx, activationColumns+` WHERE activation_id = $1`, activationID.String()))
}

func scanActivation(row scanner) (Activation, error) {
	var (
		activation                                       Activation
		activationID, grantID, principal, decided, ended string
	)
	if err := row.Scan(&activationID, &grantID, &principal, &activation.Scope, &activation.Reason,
		&activation.DurationSeconds, &activation.ApprovalRequired, &activation.RequestedAt, &decided,
		&activation.Decision, &activation.DecisionReason, &activation.DecidedAt, &activation.EndsAt,
		&ended, &activation.EndReason, &activation.EndedAt); err != nil {
		return Activation{}, err
	}
	var err error
	for _, field := range []struct {
		raw  string
		into *id.UUID
	}{{activationID, &activation.ID}, {grantID, &activation.Grant}, {principal, &activation.Principal}} {
		if *field.into, err = id.Parse(field.raw); err != nil {
			return Activation{}, err
		}
	}
	for _, field := range []struct {
		raw  string
		into **id.UUID
	}{{decided, &activation.DecidedBy}, {ended, &activation.EndedBy}} {
		if field.raw == "" {
			continue
		}
		parsed, err := id.Parse(field.raw)
		if err != nil {
			return Activation{}, err
		}
		*field.into = &parsed
	}
	return activation, nil
}

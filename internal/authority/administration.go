package authority

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// Granting and revoking provider authority through the API (ADR-ORG-001 §5.11,
// TDD-organization-control-001 §Caller Authority). Every operation runs in the provider scope, so
// the access record names the acting provider and the reason before the work runs, and the reason
// is also the grant's or the revocation's own.

var (
	// ErrInvalid means a grant or revocation request is incomplete.
	ErrInvalid = errors.New("authority: invalid provider grant request")

	// ErrAlreadyGranted means the Principal already holds an active grant.
	ErrAlreadyGranted = errors.New("authority: the Principal already holds an active provider grant")

	// ErrGrantNotFound means no grant has the identifier.
	ErrGrantNotFound = errors.New("authority: no provider grant has this identifier")

	// ErrAlreadyRevoked means the grant was revoked before.
	ErrAlreadyRevoked = errors.New("authority: the provider grant is already revoked")

	// ErrLastGrant means the grant is the last active provider:organization-control grant. Revoking
	// it would leave nobody able to grant again, and the bootstrap refuses a table that holds any
	// grant. Grants of another scope confer no authority here, so the last of those may go.
	ErrLastGrant = errors.New("authority: the last active provider:organization-control grant cannot be revoked; grant another provider first")
)

// The kinds of grant (ADR-ORG-002 §5.1, §5.2).
const (
	KindEligible  = "eligible"
	KindEmergency = "emergency"
)

// Record is one provider grant, active or revoked.
type Record struct {
	ID                id.UUID
	Principal         id.UUID
	Scope             string
	Kind              string
	GrantedBy         *id.UUID
	BootstrapOperator string
	Reason            string
	GrantedAt         time.Time
	RevokedAt         *time.Time
	RevokedBy         *id.UUID
	RevokeReason      string
}

// Administration grants, revokes and lists provider grants, as the provider role in the provider
// scope.
type Administration struct {
	pool *db.ProviderPool
	id   func() (id.UUID, error)
}

// NewAdministration builds the grant administration on the provider pool.
func NewAdministration(pool *db.ProviderPool) (*Administration, error) {
	if pool == nil {
		return nil, errors.New("authority: a provider pool is required")
	}
	return &Administration{pool: pool, id: id.NewV7}, nil
}

const recordColumns = `SELECT grant_id::text, principal_id::text, scope, kind, coalesce(granted_by::text, ''),
       coalesce(bootstrap_operator, ''), reason, granted_at, revoked_at,
       coalesce(revoked_by::text, ''), coalesce(revoke_reason, '')
FROM organization.provider_grant`

const listStatement = recordColumns + `
ORDER BY granted_at DESC`

const oneStatement = recordColumns + `
WHERE grant_id = $1`

// grantStatement inserts a grant unless the Principal already holds an active one. The partial
// unique index is the guard, against two concurrent grants as well; ON CONFLICT turns its refusal
// into a count of zero, which this package can read without naming the driver's error type.
const grantStatement = `WITH inserted AS (
    INSERT INTO organization.provider_grant (grant_id, principal_id, scope, granted_by, reason, kind)
    VALUES ($1, $2, $3, $4, $5, $6)
    ON CONFLICT (principal_id, scope) WHERE revoked_at IS NULL DO NOTHING
    RETURNING grant_id)
SELECT count(*) FROM inserted`

// lockActiveStatement locks every active grant, so two revocations cannot both count the other as
// the one that remains: the second waits for the first, then counts again.
const lockActiveStatement = `SELECT grant_id::text, scope FROM organization.provider_grant
WHERE revoked_at IS NULL
FOR UPDATE`

const revokeStatement = `UPDATE organization.provider_grant
SET revoked_at = now(), revoked_by = $2, revoke_reason = $3
WHERE grant_id = $1 AND revoked_at IS NULL`

// List reads every grant, newest first. Revoked grants are included: each is the record of who was
// given cross-Tenant authority and when it ended.
func (a *Administration) List(ctx context.Context, reason string) ([]Record, error) {
	var records []Record
	err := db.WithProviderScope(ctx, a.pool, reason, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, listStatement)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			record, err := scanRecord(rows)
			if err != nil {
				return err
			}
			records = append(records, record)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("authority: list provider grants: %w", err)
	}
	return records, nil
}

// Grant gives the Principal a provider grant for the scope, of the kind, eligible or emergency,
// recording the calling provider as granted_by and the reason as the grant's. A grant of a published
// scope publishes its event in the same transaction.
func (a *Administration) Grant(ctx context.Context, principal id.UUID, grantScope, kind, reason string) (Record, error) {
	scope, ok := db.ScopeFrom(ctx)
	switch {
	case !ok:
		return Record{}, db.ErrNoScope
	case !registeredScope(grantScope):
		return Record{}, fmt.Errorf("%w: scope is %s or %s", ErrInvalid, Scope, ScopeIdentityControl)
	case kind != KindEligible && kind != KindEmergency:
		return Record{}, fmt.Errorf("%w: kind is eligible or emergency", ErrInvalid)
	case principal.IsNil():
		return Record{}, fmt.Errorf("%w: a principal_id is required", ErrInvalid)
	case strings.TrimSpace(reason) == "":
		return Record{}, fmt.Errorf("%w: a reason is required", ErrInvalid)
	}
	grantID, err := a.id()
	if err != nil {
		return Record{}, fmt.Errorf("authority: mint grant identifier: %w", err)
	}

	var record Record
	err = db.WithProviderScope(ctx, a.pool, reason, func(ctx context.Context, tx db.Tx) error {
		var inserted int
		if err := tx.QueryRow(ctx, grantStatement, grantID.String(), principal.String(), grantScope,
			scope.Actor().String(), strings.TrimSpace(reason), kind).Scan(&inserted); err != nil {
			return err
		}
		if inserted == 0 {
			return ErrAlreadyGranted
		}
		if err := publish(ctx, tx, grantID, EventGranted); err != nil {
			return err
		}
		record, err = scanRecord(tx.QueryRow(ctx, oneStatement, grantID.String()))
		return err
	})
	if err != nil {
		if errors.Is(err, ErrAlreadyGranted) {
			return Record{}, err
		}
		return Record{}, fmt.Errorf("authority: grant provider authority: %w", err)
	}
	return record, nil
}

// Revoke ends a grant, recording the calling provider and the reason. The last active grant is
// refused (ErrLastGrant).
func (a *Administration) Revoke(ctx context.Context, grantID id.UUID, reason string) (Record, error) {
	scope, ok := db.ScopeFrom(ctx)
	switch {
	case !ok:
		return Record{}, db.ErrNoScope
	case grantID.IsNil():
		return Record{}, fmt.Errorf("%w: a grant identifier is required", ErrInvalid)
	case strings.TrimSpace(reason) == "":
		return Record{}, fmt.Errorf("%w: a reason is required", ErrInvalid)
	}

	var record Record
	err := db.WithProviderScope(ctx, a.pool, reason, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, lockActiveStatement)
		if err != nil {
			return err
		}
		active := map[string]string{}
		governing := 0
		for rows.Next() {
			var held, heldScope string
			if err := rows.Scan(&held, &heldScope); err != nil {
				rows.Close()
				return err
			}
			active[held] = heldScope
			if heldScope == Scope {
				governing++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		target, isActive := active[grantID.String()]
		if !isActive {
			// Not active: unknown, or revoked already. Read it to say which.
			if _, err := scanRecord(tx.QueryRow(ctx, oneStatement, grantID.String())); err != nil {
				return ErrGrantNotFound
			}
			return ErrAlreadyRevoked
		}
		if target == Scope && governing == 1 {
			return ErrLastGrant
		}
		if _, err := tx.Exec(ctx, revokeStatement, grantID.String(), scope.Actor().String(),
			strings.TrimSpace(reason)); err != nil {
			return err
		}
		if err := publish(ctx, tx, grantID, EventRevoked); err != nil {
			return err
		}
		record, err = scanRecord(tx.QueryRow(ctx, oneStatement, grantID.String()))
		return err
	})
	if err != nil {
		for _, sentinel := range []error{ErrGrantNotFound, ErrAlreadyRevoked, ErrLastGrant} {
			if errors.Is(err, sentinel) {
				return Record{}, err
			}
		}
		return Record{}, fmt.Errorf("authority: revoke provider authority: %w", err)
	}
	return record, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRecord(row scanner) (Record, error) {
	var (
		record                                   Record
		grantID, principal, grantedBy, revokedBy string
	)
	if err := row.Scan(&grantID, &principal, &record.Scope, &record.Kind, &grantedBy, &record.BootstrapOperator,
		&record.Reason, &record.GrantedAt, &record.RevokedAt, &revokedBy, &record.RevokeReason); err != nil {
		return Record{}, err
	}
	var err error
	if record.ID, err = id.Parse(grantID); err != nil {
		return Record{}, err
	}
	if record.Principal, err = id.Parse(principal); err != nil {
		return Record{}, err
	}
	if grantedBy != "" {
		parsed, err := id.Parse(grantedBy)
		if err != nil {
			return Record{}, err
		}
		record.GrantedBy = &parsed
	}
	if revokedBy != "" {
		parsed, err := id.Parse(revokedBy)
		if err != nil {
			return Record{}, err
		}
		record.RevokedBy = &parsed
	}
	return record, nil
}

package authority

import (
	"context"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// Emergency grant validation (ADR-ORG-002 §5.2, TDD-organization-control-001 §Emergency Grant
// Validation). An emergency grant is validated by using it: every request it authorizes records its
// last use here, and one unused for longer than ValidationPeriod is overdue. Only this service's
// scope is recorded and reported here; the Identity Control API records and reports its own.

// ValidationPeriod is Microsoft's interval for an emergency access drill, "At least every 90 days".
const ValidationPeriod = 90 * 24 * time.Hour

// EmergencyValidation is one unrevoked emergency grant of this service's scope and its last use.
type EmergencyValidation struct {
	GrantID   id.UUID
	Principal id.UUID
	GrantedAt time.Time
	// LastUsedAt is nil for a grant no request has used.
	LastUsedAt *time.Time
	Uses       int64
	// DueAt is ValidationPeriod after the last use, or after the grant when it was never used.
	DueAt   time.Time
	Overdue bool
}

// recordUseStatement records a request the Principal's emergency grant of the scope authorized.
// The grant is found again here rather than carried from the standing read, so a grant revoked
// between the two records nothing.
const recordUseStatement = `INSERT INTO organization.emergency_grant_use AS u (grant_id)
SELECT grant_id FROM organization.provider_grant
WHERE principal_id = $1 AND scope = $2 AND kind = 'emergency' AND revoked_at IS NULL
ON CONFLICT (grant_id) DO UPDATE SET last_used_at = now(), uses = u.uses + 1`

// RecordEmergencyUse records that a request was authorized by the Principal's emergency grant of
// this service's scope.
func (r *Reader) RecordEmergencyUse(ctx context.Context, principal id.UUID) error {
	if err := r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, recordUseStatement, principal.String(), Scope)
		return err
	}); err != nil {
		return fmt.Errorf("authority: record emergency grant use: %w", err)
	}
	return nil
}

// validationStatement lists every unrevoked emergency grant of the scope with its last use, the
// oldest due first.
const validationStatement = `SELECT g.grant_id::text, g.principal_id::text, g.granted_at, u.last_used_at,
       coalesce(u.uses, 0)
FROM organization.provider_grant g
LEFT JOIN organization.emergency_grant_use u ON u.grant_id = g.grant_id
WHERE g.scope = $1 AND g.kind = 'emergency' AND g.revoked_at IS NULL
ORDER BY coalesce(u.last_used_at, g.granted_at), g.grant_id`

func readValidation(ctx context.Context, tx db.Tx, now time.Time) ([]EmergencyValidation, error) {
	rows, err := tx.Query(ctx, validationStatement, Scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmergencyValidation
	for rows.Next() {
		var (
			v                  EmergencyValidation
			grantID, principal string
		)
		if err := rows.Scan(&grantID, &principal, &v.GrantedAt, &v.LastUsedAt, &v.Uses); err != nil {
			return nil, err
		}
		if v.GrantID, err = id.Parse(grantID); err != nil {
			return nil, err
		}
		if v.Principal, err = id.Parse(principal); err != nil {
			return nil, err
		}
		from := v.GrantedAt
		if v.LastUsedAt != nil {
			from = *v.LastUsedAt
		}
		v.DueAt = from.Add(ValidationPeriod)
		v.Overdue = now.After(v.DueAt)
		out = append(out, v)
	}
	return out, rows.Err()
}

// EmergencyValidation reads the report on the raw connections, for the scheduled maintenance stage,
// which has no caller and so no access record to write.
func (r *Reader) EmergencyValidation(ctx context.Context, now time.Time) ([]EmergencyValidation, error) {
	var out []EmergencyValidation
	err := r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		out, err = readValidation(ctx, tx, now)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("authority: read emergency grant validation: %w", err)
	}
	return out, nil
}

// EmergencyValidation reads the report for a provider, in the provider scope, so the read is a
// privileged access with its reason like every other provider read.
func (a *Administration) EmergencyValidation(ctx context.Context, reason string, now time.Time) ([]EmergencyValidation, error) {
	var out []EmergencyValidation
	err := db.WithProviderScope(ctx, a.pool, reason, func(ctx context.Context, tx db.Tx) error {
		var err error
		out, err = readValidation(ctx, tx, now)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("authority: read emergency grant validation: %w", err)
	}
	return out, nil
}

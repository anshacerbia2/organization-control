package authority

// Pausing Tenant administration (TDD-organization-control-001 1.22.0 §Pausing Tenant Administration).
//
// SAD-004 §9.1.1 has a restore to an older point reconciled and contained "before normal operation".
// Containment there means authority stops changing under the operator while the security versions
// are reconciled: a Tenant administrator's grant or revocation made in that window is made against a
// state the operator has not yet repaired. A provider pauses every Tenant administrator's command,
// reconciles, and lifts the pause. Reads continue, and so does every provider act, since the
// operator's repairs are provider acts.
//
// The record is append-only: each row is one decision, paused or lifted, with who, why and when, and
// the latest row is the state. A pause leaves the evidence of itself that a flag overwritten in place
// would not.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// ErrPauseInvalid means a pause or a lift was asked for without saying why.
var ErrPauseInvalid = errors.New("authority: a pause or its lifting requires a reason")

// Pause is the latest decision about Tenant administration.
type Pause struct {
	// ID is the decision's row; nil when no decision was ever recorded, which is not paused.
	ID     id.UUID
	Paused bool
	Reason string

	// ActorID is the provider who decided. Nil on a pause the restore procedure recorded
	// (deploy/dev/restore.sh), which no person made through the API; only a person lifts one.
	ActorID       *id.UUID
	CorrelationID *id.UUID
	RecordedAt    *time.Time
}

const latestPause = `SELECT pause_id::text, paused, reason, actor_id::text, correlation_id::text, recorded_at
FROM organization.tenant_administration_pause
ORDER BY recorded_at DESC, pause_id DESC
LIMIT 1`

const insertPause = `INSERT INTO organization.tenant_administration_pause
    (pause_id, paused, reason, actor_id, correlation_id, recorded_at)
VALUES ($1, $2, $3, $4::uuid, $5::uuid, $6)`

func scanPause(row interface{ Scan(dest ...any) error }) (Pause, error) {
	var (
		p                        Pause
		rawID                    string
		rawActor, rawCorrelation *string
		recorded                 time.Time
	)
	if err := row.Scan(&rawID, &p.Paused, &p.Reason, &rawActor, &rawCorrelation, &recorded); err != nil {
		return Pause{}, err
	}
	var err error
	if p.ID, err = id.Parse(rawID); err != nil {
		return Pause{}, fmt.Errorf("authority: stored pause id %q: %w", rawID, err)
	}
	for _, field := range []struct {
		raw  *string
		into **id.UUID
	}{{rawActor, &p.ActorID}, {rawCorrelation, &p.CorrelationID}} {
		if field.raw == nil {
			continue
		}
		parsed, err := id.Parse(*field.raw)
		if err != nil {
			return Pause{}, fmt.Errorf("authority: stored pause identifier %q: %w", *field.raw, err)
		}
		*field.into = &parsed
	}
	p.RecordedAt = &recorded
	return p, nil
}

// readPause reads the latest decision; none recorded is not paused.
func readPause(ctx context.Context, tx db.Tx) (Pause, error) {
	rows, err := tx.Query(ctx, latestPause)
	if err != nil {
		return Pause{}, fmt.Errorf("authority: read the tenant administration pause: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return Pause{}, rows.Err()
	}
	p, err := scanPause(rows)
	if err != nil {
		return Pause{}, fmt.Errorf("authority: scan the tenant administration pause: %w", err)
	}
	return p, rows.Err()
}

// TenantAdministrationPaused reports whether Tenant administrators' commands are paused, read for each
// such command on the provider connections, as the caller records are: the table carries no policy,
// and the tenant role holds nothing in the organization schema.
func (r *Reader) TenantAdministrationPaused(ctx context.Context) (bool, error) {
	var p Pause
	err := r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		p, err = readPause(ctx, tx)
		return err
	})
	if err != nil {
		return false, err
	}
	return p.Paused, nil
}

// CurrentPause reads the latest decision, recording the provider's access with reason.
func (a *Administration) CurrentPause(ctx context.Context, reason string) (Pause, error) {
	var p Pause
	err := db.WithProviderScope(ctx, a.pool, reason, func(ctx context.Context, tx db.Tx) error {
		var err error
		p, err = readPause(ctx, tx)
		return err
	})
	return p, err
}

// SetPause records a decision: paused or lifted, by the calling provider, for reason. Recording the
// state it already has is recorded too, as a decision confirmed; the record is of decisions, not of
// changes.
func (a *Administration) SetPause(ctx context.Context, paused bool, reason string) (Pause, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Pause{}, ErrPauseInvalid
	}
	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Pause{}, db.ErrNoScope
	}
	pauseID, err := a.id()
	if err != nil {
		return Pause{}, fmt.Errorf("authority: mint pause identifier: %w", err)
	}
	actor, correlation := scope.Actor(), scope.Correlation()
	at := time.Now().UTC()
	p := Pause{ID: pauseID, Paused: paused, Reason: reason, ActorID: &actor, CorrelationID: &correlation,
		RecordedAt: &at}
	err = db.WithProviderScope(ctx, a.pool, reason, func(ctx context.Context, tx db.Tx) error {
		if _, err := tx.Exec(ctx, insertPause, pauseID.String(), paused, reason,
			actor.String(), correlation.String(), at); err != nil {
			return fmt.Errorf("authority: record the tenant administration pause: %w", err)
		}
		return db.Respond(ctx, tx, p)
	})
	if err != nil {
		return Pause{}, err
	}
	return p, nil
}

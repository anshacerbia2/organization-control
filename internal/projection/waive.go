package projection

// Waiving a dead letter: an operational exception, never a closure.
//
// Some incidents have no corrective path. The consumer that refused the event has been retired, so
// no replay can reach it and no receipt will ever justify REPLAYED or SUPERSEDED. Left alone, the
// row alerts as stale forever and keeps its restricted payload forever. A waiver records that an
// operator knows, why, and until when -- foundation-platform v0.2.9's waiver columns, which never
// touch the resolution columns.
//
// # What a waiver cannot do
//
// It cannot make a consumer fresh. The frontier reads debt from resolved_at, which a waiver leaves
// NULL, so the estate view still counts the incident; and a consumer's own view never counted
// another consumer's row in the first place. The two rules below keep it that way:
//
//   - The refusing consumer must not be active. A dead letter an active consumer refused is a live
//     outage with corrective paths -- replay it, or supersede it -- and silencing its alert would
//     hide exactly the thing the alert exists to show. The dead letter is named by its consumer
//     (ADR-GLB-018 §5.3), so one that names no consumer has no address here at all.
//   - It expires, within MaxWaiver. An exception somebody forgot becomes a question again.
//
// Like a resolution, it runs as organization_resolution_rt, which holds UPDATE on the four waiver
// columns and nothing else of the row, and it files the same two records: the attempt before the
// transaction, the outcome inside it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// MaxWaiver bounds how long one waiver may stand. It matches dead-letter retention: a waiver
// longer than the payload it lets the platform dispose of would outlive its own subject.
const MaxWaiver = 90 * 24 * time.Hour

var (
	// ErrNotWaivable refuses a waiver the rules above forbid. The message says which rule, and
	// what to do instead.
	ErrNotWaivable = errors.New("projection: that dead letter may not be waived")

	// ErrAlreadyWaived refuses a second waiver while the first still stands. Renewing is waiving
	// again after expiry, so the decision to keep the exception is made, and recorded, again.
	ErrAlreadyWaived = errors.New("projection: that dead letter is already under an unexpired waiver")
)

// Waiver is what a waiver recorded.
type Waiver struct {
	EventID  id.UUID
	Consumer string
	Until    time.Time
	Reason   string
}

const readForWaiver = `SELECT resolved_at IS NOT NULL,
       waived_until IS NOT NULL AND waived_until > now(),
       now()
  FROM platform.dead_letter
 WHERE event_id = $1 AND consumer = $2`

const consumerActive = `SELECT EXISTS (
    SELECT 1 FROM projection.consumer WHERE consumer_id = $1 AND retired_at IS NULL)`

const waiveDeadLetter = `UPDATE platform.dead_letter
   SET waived_at     = now(),
       waived_until  = $3,
       waived_by     = $4,
       waiver_reason = $5
 WHERE event_id = $1
   AND consumer = $2
   AND resolved_at IS NULL`

// Waive records a waiver on the consumer's unresolved dead letter for the event, until the given
// instant, when that consumer is no longer active.
func (r *Resolver) Waive(ctx context.Context, eventID id.UUID, consumer, reason string, until time.Time) (Waiver, error) {
	reason = strings.TrimSpace(reason)
	switch {
	case eventID.IsNil():
		return Waiver{}, fmt.Errorf("%w: an event identifier is required", ErrInvalid)
	case strings.TrimSpace(consumer) == "":
		return Waiver{}, requireConsumerName(consumer)
	case reason == "":
		return Waiver{}, fmt.Errorf("%w: a waiver must say why", ErrInvalid)
	case until.IsZero():
		return Waiver{}, fmt.Errorf("%w: a waiver must say until when", ErrInvalid)
	}
	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Waiver{}, db.ErrNoScope
	}

	var out Waiver
	err := db.WithResolutionScope(ctx, r.pool, "waive dead-lettered event "+eventID.String()+" at "+consumer,
		func(ctx context.Context, tx db.Tx) error {
			if err := requireDeadLetter(ctx, tx, eventID, consumer); err != nil {
				return err
			}
			var (
				resolved, waived bool
				now              time.Time
			)
			if err := tx.QueryRow(ctx, readForWaiver, eventID.String(), consumer).Scan(
				&resolved, &waived, &now); err != nil {
				return fmt.Errorf("projection: reading the dead letter for %s at %s: %w", eventID, consumer, err)
			}
			if resolved {
				return fmt.Errorf("%w: %s", ErrAlreadyResolved, eventID)
			}
			if waived {
				return fmt.Errorf("%w: %s; waive it again once the waiver has expired", ErrAlreadyWaived, eventID)
			}
			// The expiry is judged on the database clock, like every instant in a resolution.
			if !until.After(now) || until.Sub(now) > MaxWaiver {
				return fmt.Errorf("%w: a waiver must expire after now and within %s of it, not at %s",
					ErrInvalid, MaxWaiver, until.UTC().Format(time.RFC3339))
			}
			var active bool
			if err := tx.QueryRow(ctx, consumerActive, consumer).Scan(&active); err != nil {
				return fmt.Errorf("projection: reading whether %s is active: %w", consumer, err)
			}
			if active {
				return fmt.Errorf("%w: %s was refused by %q, which is enforcing now; its debt is a "+
					"live outage with a corrective path, so replay or supersede it instead",
					ErrNotWaivable, eventID, consumer)
			}

			tag, err := tx.Exec(ctx, waiveDeadLetter, eventID.String(), consumer, until.UTC(), scope.Actor().String(), reason)
			if err != nil {
				return fmt.Errorf("projection: waiving %s: %w", eventID, err)
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("%w: %s was closed by something else while this ran", ErrAlreadyResolved, eventID)
			}

			// The outcome, in the transaction that made it, so a rolled-back waiver leaves no
			// account of one. The word is "waived", never "closed": the incident is still open.
			outcome := fmt.Sprintf("waived dead-lettered event %s refused by %q until %s: %s",
				eventID, consumer, until.UTC().Format(time.RFC3339), reason)
			if err := db.RecordAccessInTx(ctx, tx, scope.Evidence(ctx, outcome)); err != nil {
				return fmt.Errorf("projection: recording the waiver of %s: %w", eventID, err)
			}

			out = Waiver{EventID: eventID, Consumer: consumer, Until: until.UTC(), Reason: reason}
			return nil
		})
	if err != nil {
		return Waiver{}, err
	}
	return out, nil
}

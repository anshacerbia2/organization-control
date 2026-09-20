package projection

// Replaying an abandoned delivery.
//
// A dead-lettered authority event will not reach its consumer by itself: the dispatcher marked the
// row published so redelivery stops, and the incident record is all that remains. Replay is the
// operator act that puts the same event back into the outbox so the dispatcher carries it again.
//
// # What replay is not
//
// It does not resolve anything. The dead-letter row is untouched, still unresolved, and the
// security debt still blocks. That separation is the point: replay creates the opportunity for
// evidence, and evidence is what a resolution consumes. A replay that closed the incident by
// itself would be an operator assertion wearing a delivery's clothes -- and this role holds no
// UPDATE on platform.dead_letter, so the database enforces the separation rather than trusting
// this file to observe it.
//
// # Why the same event_id
//
// The consumer deduplicates on (event_id, consumer). Replaying under a fresh identifier would
// deliver a second event the consumer has never seen, which applies rather than deduplicates --
// indistinguishable at the consumer from the original having arrived twice, and it produces a
// receipt for an event_id no dead letter names. Reusing the identifier is what makes the receipt
// answer the question the resolution predicate asks.
//
// platform.outbox is keyed (created_at, event_id) rather than by event_id alone, so the same
// identifier can appear in a later partition. That deviation from STD-GLB-004 is recorded in
// foundation-platform and is what makes replay expressible at all.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	fevent "github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
)

var (
	// ErrDeadLetterNotFound means no dead letter carries that event identifier.
	ErrDeadLetterNotFound = errors.New("projection: no dead letter for that event")

	// ErrAlreadyResolved refuses a replay of an incident that is closed.
	//
	// Not harmless-and-ignored: an operator replaying a resolved incident has a wrong picture of
	// the estate, and delivering the event again would be the smaller half of that problem.
	ErrAlreadyResolved = errors.New("projection: that dead letter is already resolved")

	// ErrUnreplayable means the row cannot be re-appended from itself.
	//
	// platform.outbox requires aggregate_id, and takes priority to choose the dispatch lane.
	// Rows written before those columns were retained carry neither, and their originating outbox
	// row is gone if its partition has passed retention.
	//
	// Refused rather than reconstructed. aggregate_id could be read out of the payload, and that
	// is exactly the guess not to make: the field naming the aggregate is domain knowledge the
	// platform schema does not hold, and a wrong one delivers a security event attributed to
	// another subject.
	ErrUnreplayable = errors.New("projection: that dead letter cannot be replayed from its own row")
)

// Replay is what a replay produced.
type Replay struct {
	EventID   id.UUID
	EventType string

	// Position is the stream position of the new outbox row. It differs from the original's:
	// the sequence is allocated at INSERT, so a replayed event carries a later position than the
	// one the consumer may already have recorded. That is harmless -- the projection orders by
	// membership_version rather than by position -- and it is stated because the two numbers not
	// matching is the first thing an operator notices.
	Position int64
}

// Replayer re-appends abandoned deliveries.
//
// Provider-scoped, unlike the frontier beside it. The frontier is polled by consumers and takes
// the raw transactor so a poll does not file an access record; a replay is an operator act that
// puts a security event back on the wire, and the record is the point. WithProviderScope writes
// it before the transaction opens, so a replay that fails still leaves evidence that it was
// attempted.
type Replayer struct {
	pool *db.ProviderPool
}

func NewReplayer(pool *db.ProviderPool) (*Replayer, error) {
	if pool == nil {
		return nil, errors.New("projection: a provider-scoped pool is required")
	}
	return &Replayer{pool: pool}, nil
}

// deadLetterExists answers before the read below, and always returns exactly one row.
//
// Without it, a QueryRow that finds nothing and a QueryRow that fails are the same error to this
// package, and the read would be reported as "no dead letter for that event" whatever went wrong.
// That is not a hypothetical: the first version of this file took `FOR UPDATE` on the read, the
// provider role holds no UPDATE on platform.dead_letter, and every replay answered "not found" for
// a permission error. A refusal naming the wrong cause sends an operator to look for a row that is
// sitting right there.
const deadLetterExists = `SELECT EXISTS (SELECT 1 FROM platform.dead_letter WHERE event_id = $1)`

// selectDeadLetter reads everything a replay needs, and nothing else.
//
// No FOR UPDATE. It would need UPDATE on the table, which this role deliberately does not hold —
// resolution is a different act performed by a different credential, and taking a write lock to
// read would quietly require the privilege that separation exists to withhold.
//
// The cost is that two operators replaying one incident at the same moment both append. The
// consumer deduplicates on (event_id, consumer), so the second delivery is a no-op that still
// produces a receipt: a wasted delivery rather than a wrong state.
const selectDeadLetter = `SELECT event_type, envelope, aggregate_id::text, priority, resolved_at IS NOT NULL
  FROM platform.dead_letter
 WHERE event_id = $1`

// Replay puts the event back into the outbox under its original identifier.
//
// The dead-letter row is left untouched. Resolution is a separate act, performed by a separate
// role, against evidence this replay may produce.
func (r *Replayer) Replay(ctx context.Context, eventID id.UUID) (Replay, error) {
	if eventID.IsNil() {
		return Replay{}, fmt.Errorf("%w: an event identifier is required", ErrInvalid)
	}

	var out Replay
	err := db.WithProviderScope(ctx, r.pool, "replay dead-lettered event "+eventID.String(),
		func(ctx context.Context, tx db.Tx) error {
			var (
				eventType   string
				envelopeRaw []byte
				aggregateID *string
				priority    *int16
				resolved    bool
			)

			var exists bool
			if err := tx.QueryRow(ctx, deadLetterExists, eventID.String()).Scan(&exists); err != nil {
				return fmt.Errorf("projection: reading the dead letter for %s: %w", eventID, err)
			}
			if !exists {
				return fmt.Errorf("%w: %s", ErrDeadLetterNotFound, eventID)
			}

			// Absence is already ruled out, so anything failing here is a fault and is reported as
			// one rather than as a missing row.
			if err := tx.QueryRow(ctx, selectDeadLetter, eventID.String()).Scan(
				&eventType, &envelopeRaw, &aggregateID, &priority, &resolved); err != nil {
				return fmt.Errorf("projection: reading the dead letter for %s: %w", eventID, err)
			}

			if resolved {
				return fmt.Errorf("%w: %s", ErrAlreadyResolved, eventID)
			}
			if len(envelopeRaw) == 0 {
				// Disposal clears envelope and payload after retention, and it only touches
				// resolved rows -- so an unresolved row with no envelope is a state the retention
				// job does not produce. Refused with its own reason rather than folded into the
				// missing-columns case, because it means something different happened.
				return fmt.Errorf("%w: %s has no retained envelope", ErrUnreplayable, eventID)
			}
			if aggregateID == nil || priority == nil {
				return fmt.Errorf("%w: %s carries no aggregate_id or priority, so it predates "+
					"their retention and its originating outbox row is gone", ErrUnreplayable, eventID)
			}

			var envelope fevent.Envelope
			if err := json.Unmarshal(envelopeRaw, &envelope); err != nil {
				return fmt.Errorf("%w: %s has an envelope that no longer decodes: %v",
					ErrUnreplayable, eventID, err)
			}

			aggregate, err := id.Parse(*aggregateID)
			if err != nil {
				return fmt.Errorf("%w: %s carries an unparseable aggregate_id: %v",
					ErrUnreplayable, eventID, err)
			}

			// The lane travels with the event. A revocation replayed into the standard lane would
			// queue behind lifecycle traffic, which is the delay the reserved lane exists to
			// prevent -- and the incident being replayed is evidence that this event already took
			// longer than it should have.
			var opts []outbox.Option
			if *priority == outbox.PriorityHigh {
				opts = append(opts, outbox.Priority())
			}

			if err := outbox.Append(ctx, tx, aggregate, envelope, opts...); err != nil {
				return fmt.Errorf("projection: re-appending %s: %w", eventID, err)
			}

			// Read back the position the append allocated, rather than computing it. The sequence
			// is assigned inside the statement, so this is the only place it exists.
			if err := tx.QueryRow(ctx,
				`SELECT sequence FROM platform.outbox
                  WHERE event_id = $1 ORDER BY created_at DESC LIMIT 1`,
				eventID.String()).Scan(&out.Position); err != nil {
				return fmt.Errorf("projection: reading the replayed position for %s: %w", eventID, err)
			}

			out.EventID = eventID
			out.EventType = eventType
			return nil
		})
	if err != nil {
		return Replay{}, err
	}
	return out, nil
}

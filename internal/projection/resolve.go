package projection

// Closing a dead letter, on evidence.
//
// This is the act that makes the publication frontier stop reporting a security debt, and
// therefore the act that lets every projection-backed check serve again. Everything else in this
// file exists to make sure it happens for a reason that can be read back.
//
// # One reason, and why the others are absent
//
// REPLAYED only. A dead letter closes when this producer's own dispatcher witnessed the consumer
// accept the event — a delivery receipt carrying consumer_applied, for this event, for the
// consumer that is actually enforcing. That is the only evidence in the contract which does not
// rest on the consumer's report about its own progress.
//
// RESNAPSHOTTED needs generation replacement, SUPERSEDED needs domain proof that a newer event
// closes the failed one's effect, and WAIVED is an operational exception rather than a correctness
// proof. None is built, and a resolver that quietly accepted a weaker reason would be the whole
// contract undone in one branch. See TDD-organization-control-005.
//
// # What this cannot recover
//
// A dead letter whose event has been superseded can never be closed here: replaying it is
// discarded by the monotonicity guard, so it never produces applied evidence. The consumer is in
// the correct state and there is no sanctioned way to say so. That is written down rather than
// discovered, and the replay procedure that avoids reaching it — lowest version first — is in the
// same document.

import (
	"context"
	"errors"
	"fmt"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

var (
	// ErrNoActiveConsumer means nothing is registered to enforce, so no receipt can be the right
	// one. Refused rather than resolved against whichever consumer happens to have a row: the
	// predicate asks whether the consumer that is enforcing holds the event, and with none
	// registered the question has no subject.
	ErrNoActiveConsumer = errors.New("projection: no active projection consumer is registered")

	// ErrNoAppliedEvidence refuses a closure that nothing justifies.
	//
	// The message names what was looked for and what was found, because the commonest cause is
	// not a missing delivery. DISPATCH_CONSUMER_NAME and the registered consumer are configured
	// in different repositories with nothing linking them, so a mismatch of one character
	// produces receipts under a name no resolution reads — and a refusal saying only "no
	// evidence" sends an operator to look at the delivery path, which is working.
	ErrNoAppliedEvidence = errors.New("projection: no applied-evidence receipt justifies closing this dead letter")
)

// Resolution is what a closure recorded.
type Resolution struct {
	EventID   id.UUID
	Consumer  string
	Type      string
	Reference string
}

// ResolutionTypeReplayed is the one reason this resolver writes.
const ResolutionTypeReplayed = "REPLAYED"

// Resolver closes dead letters that evidence supports.
//
// It runs as organization_resolution_rt, which holds column-level UPDATE on the four resolution
// columns and can reach nothing else on the row. The provider role that performs a replay holds no
// UPDATE here at all, so replaying and closing are two acts under two credentials — and the
// evidence between them is what the second one reads.
type Resolver struct {
	pool *db.ResolutionPool
}

func NewResolver(pool *db.ResolutionPool) (*Resolver, error) {
	if pool == nil {
		return nil, errors.New("projection: a resolution-scoped pool is required")
	}
	return &Resolver{pool: pool}, nil
}

// activeConsumer is the subject the evidence must be about.
//
// Derived here, never accepted from the request. The operator chooses the action; the server
// chooses whose receipt counts. A caller able to name the consumer could close an incident with a
// receipt from a consumer that is not enforcing anything.
//
// One row at most, because the estate refuses a second active projection consumer — enforced by a
// partial unique index rather than by this query trusting it.
const activeConsumer = `SELECT consumer_id
  FROM projection.consumer
 WHERE retired_at IS NULL`

const appliedEvidence = `SELECT EXISTS (
    SELECT 1 FROM platform.delivery_receipt
     WHERE event_id = $1 AND consumer = $2 AND evidence = 'consumer_applied')`

// otherReceipts names who DID acknowledge this event, when the active consumer did not.
//
// This is the diagnostic that separates "the delivery never happened" from "the delivery happened
// under a name nothing resolves against". Without it both read as an absence of evidence, and only
// one of them is a delivery problem.
const otherReceipts = `SELECT coalesce(string_agg(DISTINCT consumer || ' (' || evidence || ')', ', '), '')
  FROM platform.delivery_receipt
 WHERE event_id = $1`

const closeDeadLetter = `UPDATE platform.dead_letter
   SET resolved_at          = now(),
       resolution_type      = $2,
       resolved_by          = $3,
       resolution_reference = $4
 WHERE event_id = $1
   AND resolved_at IS NULL`

// Resolve closes the dead letter if applied evidence supports it, and refuses otherwise.
//
// resolved_by comes from the bound scope rather than from the caller's request. An author taken
// from a body is an author anybody can write, and the field exists so an investigation can ask who
// decided this incident was over.
func (r *Resolver) Resolve(ctx context.Context, eventID id.UUID) (Resolution, error) {
	if eventID.IsNil() {
		return Resolution{}, fmt.Errorf("%w: an event identifier is required", ErrInvalid)
	}
	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Resolution{}, db.ErrNoScope
	}

	var out Resolution
	err := db.WithResolutionScope(ctx, r.pool, "resolve dead-lettered event "+eventID.String(),
		func(ctx context.Context, tx db.Tx) error {
			var exists, resolved bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM platform.dead_letter WHERE event_id = $1),
				        coalesce((SELECT resolved_at IS NOT NULL FROM platform.dead_letter
				                   WHERE event_id = $1), FALSE)`,
				eventID.String()).Scan(&exists, &resolved); err != nil {
				return fmt.Errorf("projection: reading the dead letter for %s: %w", eventID, err)
			}
			if !exists {
				return fmt.Errorf("%w: %s", ErrDeadLetterNotFound, eventID)
			}
			if resolved {
				return fmt.Errorf("%w: %s", ErrAlreadyResolved, eventID)
			}

			var consumer string
			if err := tx.QueryRow(ctx, activeConsumer).Scan(&consumer); err != nil {
				// No row, or the read failed. Either way there is no subject for the predicate,
				// and closing against a guess is the outcome this refuses.
				return fmt.Errorf("%w: %v", ErrNoActiveConsumer, err)
			}

			var applied bool
			if err := tx.QueryRow(ctx, appliedEvidence, eventID.String(), consumer).Scan(&applied); err != nil {
				return fmt.Errorf("projection: reading delivery receipts for %s: %w", eventID, err)
			}
			if !applied {
				var others string
				if err := tx.QueryRow(ctx, otherReceipts, eventID.String()).Scan(&others); err != nil {
					return fmt.Errorf("projection: reading delivery receipts for %s: %w", eventID, err)
				}
				if others == "" {
					return fmt.Errorf("%w: nothing has acknowledged %s; the event has not been "+
						"delivered since it was abandoned, so replay it first",
						ErrNoAppliedEvidence, eventID)
				}
				return fmt.Errorf("%w: the active consumer is %q and %s carries receipts from %s; "+
					"a receipt under another name resolves nothing, and the commonest cause is "+
					"DISPATCH_CONSUMER_NAME disagreeing with the registered consumer",
					ErrNoAppliedEvidence, consumer, eventID, others)
			}

			// The reference points at the receipt that justified this, by its own key. Not a
			// sentence: an investigation reading resolution_reference should be able to go and
			// look at the row.
			reference := fmt.Sprintf("platform.delivery_receipt:%s:%s", eventID, consumer)

			tag, err := tx.Exec(ctx, closeDeadLetter,
				eventID.String(), ResolutionTypeReplayed, scope.Actor().String(), reference)
			if err != nil {
				return fmt.Errorf("projection: closing %s: %w", eventID, err)
			}
			if tag.RowsAffected() != 1 {
				// The row was unresolved when this transaction read it and is not now. Another
				// resolution committed in between, and reporting success would attribute a closure
				// this transaction did not make.
				return fmt.Errorf("%w: %s was closed by something else while this ran",
					ErrAlreadyResolved, eventID)
			}

			out = Resolution{
				EventID:   eventID,
				Consumer:  consumer,
				Type:      ResolutionTypeReplayed,
				Reference: reference,
			}
			return nil
		})
	if err != nil {
		return Resolution{}, err
	}
	return out, nil
}

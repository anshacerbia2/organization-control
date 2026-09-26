package projection

// Closing a dead letter, on evidence.
//
// This is the act that makes the publication frontier stop reporting a security debt, and
// therefore the act that lets every projection-backed check serve again. Everything else in this
// file exists to make sure it happens for a reason that can be read back.
//
// # Two reasons, and why the others are absent
//
// REPLAYED: this producer's own dispatcher witnessed the consumer accept the event -- a delivery
// receipt carrying consumer_applied, for this event, for the consumer that is actually enforcing.
//
// SUPERSEDED: the same witness, for a newer event about the same Membership. Every Membership event
// carries the complete security state and its membership_version, and a consumer applies an event
// only when its version is higher than the one it holds. So once the consumer has applied version
// W, the failed event at version V < W can never take effect -- a replay of it would be discarded
// by the monotonicity guard -- and the consumer already holds a state at least as new as the one it
// missed. Without this reason such an incident could never close: replaying produces no applied
// evidence, and the consumer is correct with no sanctioned way to say so.
//
// "Newer" is read from membership.membership_event, never from the receipt or the stream. A replay
// reassigns stream_position, so an older event replayed after a revocation would carry the higher
// position; a predicate reading positions would let that replay close the revocation's incident
// while the consumer holds the older grant. The version in the history row is the event's own,
// written in the transaction that published it, and no runtime role can edit it.
//
// Both reasons rest on a receipt, which is the only evidence in the contract that does not rest on
// the consumer's report about its own progress. RESNAPSHOTTED needs generation replacement and
// WAIVED is an operational exception rather than a correctness proof. Neither is built, and a
// resolver that quietly accepted a weaker reason would be the whole contract undone in one branch.
// See TDD-organization-control-005.

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

	// ErrNotSuperseded refuses a SUPERSEDED closure that no newer applied event justifies.
	//
	// Separate from ErrNoAppliedEvidence because the operator's next move differs: here the
	// answer is usually to replay the event itself, or to wait for the newer one to be applied.
	ErrNotSuperseded = errors.New("projection: no newer applied event supersedes this dead letter")
)

// Resolution is what a closure recorded.
type Resolution struct {
	EventID   id.UUID
	Consumer  string
	Type      string
	Reference string
}

// The reasons this resolver writes.
const (
	ResolutionTypeReplayed   = "REPLAYED"
	ResolutionTypeSuperseded = "SUPERSEDED"
)

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

// failedVersion is which Membership, at which version, the dead-lettered event concerned.
//
// No row means the event is not a Membership event, or was published before the history existed.
// Either way there is no version to compare, and the envelope is not consulted instead: it is a
// copy the dead-letter row carries, and the history row is the record the producer wrote.
//
// One row always, NULL when absent: this package may not name the driver, so "no rows" is not an
// error it can recognise.
const failedVersion = `SELECT h.membership_id::text, h.membership_version
  FROM (SELECT 1) one
  LEFT JOIN membership.membership_event h ON h.event_id = $1`

// supersedingEvidence finds the lowest newer version of the same Membership the active consumer
// applied. The lowest, so the reference names the first event that made the failed one moot.
const supersedingEvidence = `SELECT s.event_id::text, s.membership_version, s.event_type
  FROM (SELECT 1) one
  LEFT JOIN LATERAL (
        SELECT r.event_id, h.membership_version, h.event_type
          FROM platform.delivery_receipt r
          JOIN membership.membership_event h ON h.event_id = r.event_id
         WHERE r.consumer = $1
           AND r.evidence = 'consumer_applied'
           AND h.membership_id = $2::uuid
           AND h.membership_version > $3
         ORDER BY h.membership_version
         LIMIT 1) s ON TRUE`

const closeDeadLetter = `UPDATE platform.dead_letter
   SET resolved_at          = now(),
       resolution_type      = $2,
       resolved_by          = $3,
       resolution_reference = $4
 WHERE event_id = $1
   AND resolved_at IS NULL`

// evidenceFinder returns the receipt reference a closure rests on and a sentence saying why, or a
// refusal.
type evidenceFinder func(ctx context.Context, tx db.Tx, eventID id.UUID, consumer string) (reference, why string, err error)

// Resolve closes the dead letter as REPLAYED if applied evidence for the event itself supports it,
// and refuses otherwise.
func (r *Resolver) Resolve(ctx context.Context, eventID id.UUID) (Resolution, error) {
	return r.close(ctx, eventID, ResolutionTypeReplayed, replayedEvidence)
}

// Supersede closes the dead letter as SUPERSEDED if the active consumer applied a newer event for
// the same Membership, and refuses otherwise.
func (r *Resolver) Supersede(ctx context.Context, eventID id.UUID) (Resolution, error) {
	return r.close(ctx, eventID, ResolutionTypeSuperseded, supersededEvidence)
}

func replayedEvidence(ctx context.Context, tx db.Tx, eventID id.UUID, consumer string) (string, string, error) {
	var applied bool
	if err := tx.QueryRow(ctx, appliedEvidence, eventID.String(), consumer).Scan(&applied); err != nil {
		return "", "", fmt.Errorf("projection: reading delivery receipts for %s: %w", eventID, err)
	}
	if !applied {
		var others string
		if err := tx.QueryRow(ctx, otherReceipts, eventID.String()).Scan(&others); err != nil {
			return "", "", fmt.Errorf("projection: reading delivery receipts for %s: %w", eventID, err)
		}
		if others == "" {
			return "", "", fmt.Errorf("%w: nothing has acknowledged %s; the event has not been "+
				"delivered since it was abandoned, so replay it first",
				ErrNoAppliedEvidence, eventID)
		}
		return "", "", fmt.Errorf("%w: the active consumer is %q and %s carries receipts from %s; "+
			"a receipt under another name resolves nothing, and the commonest cause is "+
			"DISPATCH_CONSUMER_NAME disagreeing with the registered consumer",
			ErrNoAppliedEvidence, consumer, eventID, others)
	}

	// The reference points at the receipt that justified this, by its own key. Not a sentence: an
	// investigation reading resolution_reference should be able to go and look at the row.
	return fmt.Sprintf("platform.delivery_receipt:%s:%s", eventID, consumer), "", nil
}

func supersededEvidence(ctx context.Context, tx db.Tx, eventID id.UUID, consumer string) (string, string, error) {
	var (
		membershipID *string
		version      *int64
	)
	if err := tx.QueryRow(ctx, failedVersion, eventID.String()).Scan(&membershipID, &version); err != nil {
		return "", "", fmt.Errorf("projection: reading the version %s carried: %w", eventID, err)
	}
	if membershipID == nil || version == nil {
		return "", "", fmt.Errorf("%w: %s has no Membership version on record; it is not a "+
			"Membership event, or it was published before the history existed, so replay it instead",
			ErrNotSuperseded, eventID)
	}

	var (
		newer        *string
		newerVersion *int64
		newerType    *string
	)
	if err := tx.QueryRow(ctx, supersedingEvidence, consumer, *membershipID, *version).Scan(
		&newer, &newerVersion, &newerType); err != nil {
		return "", "", fmt.Errorf("projection: reading superseding receipts for %s: %w", eventID, err)
	}
	if newer == nil || newerVersion == nil || newerType == nil {
		return "", "", fmt.Errorf("%w: the active consumer %q has applied no event for Membership "+
			"%s above version %d, which %s carried; replay it, or wait for the newer event to be applied",
			ErrNotSuperseded, consumer, *membershipID, *version, eventID)
	}

	return fmt.Sprintf("platform.delivery_receipt:%s:%s", *newer, consumer),
		fmt.Sprintf("; Membership %s version %d superseded by version %d (%s)",
			*membershipID, *version, *newerVersion, *newerType), nil
}

// close is what both reasons share: the incident must exist and be open, the evidence must be about
// the consumer that is enforcing, and the closure and its account commit together.
//
// resolved_by comes from the bound scope rather than from the caller's request. An author taken
// from a body is an author anybody can write, and the field exists so an investigation can ask who
// decided this incident was over.
func (r *Resolver) close(ctx context.Context, eventID id.UUID, kind string, find evidenceFinder) (Resolution, error) {
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

			reference, why, err := find(ctx, tx, eventID, consumer)
			if err != nil {
				return err
			}

			tag, err := tx.Exec(ctx, closeDeadLetter,
				eventID.String(), kind, scope.Actor().String(), reference)
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

			// The second record, and the reason it is here rather than beside the first one.
			//
			// The scope wrapper already filed an ATTEMPT before this transaction opened, which is
			// what makes a refused or crashed resolution attributable. It cannot say what happened,
			// because at that point nothing had. This one says what happened -- which incident, on
			// whose receipt -- and it is enrolled in the transaction that did it, so a closure that
			// rolls back takes its own account of itself with it. An outcome row surviving a
			// rolled-back closure would not be an over-record; it would be a false statement, and
			// an investigation reading it has no way to tell it from a true one.
			if err := db.RecordAccessInTx(ctx, tx, db.ProviderAccess{
				Actor:       scope.Actor(),
				Correlation: scope.Correlation(),
				Reason: fmt.Sprintf("closed dead-lettered event %s as %s on %s%s",
					eventID, kind, reference, why),
			}); err != nil {
				return fmt.Errorf("projection: recording the resolution of %s: %w", eventID, err)
			}

			out = Resolution{
				EventID:   eventID,
				Consumer:  consumer,
				Type:      kind,
				Reference: reference,
			}
			return nil
		})
	if err != nil {
		return Resolution{}, err
	}
	return out, nil
}

package projection

// Replaying an abandoned delivery, against real rows.
//
// The property that matters is not that a row appears in the outbox — it is that the row appears
// under the ORIGINAL event identifier, in the original lane, while the incident stays open. Each of
// those three is a separate way for a replay to be wrong, and each is asserted below.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	fevent "github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// abandoned writes what a poisoned delivery to consumer leaves behind: the event, the consumer's
// delivery closed without being delivered, and the incident that records it under the consumer's
// name.
//
// Written through the owner connection rather than the service's, because seeding is not the system
// under test — and the provider role deliberately holds no INSERT on platform.dead_letter.
func abandoned(t *testing.T, f *fixture, consumer string, priority int16, withRetention bool) (id.UUID, id.UUID) {
	t.Helper()

	aggregate := mustID(t)
	envelope, err := fevent.New("//scnehaux.com/organization-control",
		"com.scnehaux.organization.membership.security.revoked", f.fixed,
		map[string]any{
			"membership_id":      mustID(t).String(),
			"tenant_id":          mustID(t).String(),
			"principal_id":       mustID(t).String(),
			"membership_version": 4,
		})
	if err != nil {
		t.Fatalf("minting an envelope: %v", err)
	}
	wire, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encoding the envelope: %v", err)
	}

	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		// The consumer's delivery, published = TRUE with published_at NULL: what the dispatcher
		// leaves, no longer its concern and never delivered.
		if _, err := tx.Exec(ctx, `
			WITH e AS (
			    INSERT INTO platform.outbox
			        (event_id, aggregate_id, event_type, payload, envelope, priority)
			    VALUES ($1::uuid, $2::uuid, $3, '{}'::jsonb, $4::jsonb, $5)
			    RETURNING created_at, event_id, sequence, event_type, priority)
			INSERT INTO platform.outbox_delivery
			    (created_at, event_id, consumer, sequence, event_type, priority,
			     published, published_at, attempts, first_failed_at, failure_class, last_error)
			SELECT created_at, event_id, $6, sequence, event_type, priority,
			       TRUE, NULL, 3, clock_timestamp(), 'poison', 'refused'
			  FROM e`,
			envelope.ID.String(), aggregate.String(), envelope.Type.String(), string(wire), priority, consumer); err != nil {
			return err
		}

		if withRetention {
			_, err := tx.Exec(ctx, `
				INSERT INTO platform.dead_letter
				    (event_id, event_type, envelope, payload, aggregate_id, priority,
				     failure_class, failure_detail, attempts, first_failed_at, consumer)
				VALUES ($1::uuid, $2, $3::jsonb, '{}'::jsonb, $4::uuid, $5,
				        'poison', 'the consumer refused the envelope', 3, clock_timestamp(), $6)`,
				envelope.ID.String(), envelope.Type.String(), string(wire), aggregate.String(), priority, consumer)
			return err
		}
		// The shape a row written before aggregate_id and priority were retained still has.
		_, err := tx.Exec(ctx, `
			INSERT INTO platform.dead_letter
			    (event_id, event_type, envelope, payload, failure_class, failure_detail,
			     attempts, first_failed_at, consumer)
			VALUES ($1::uuid, $2, $3::jsonb, '{}'::jsonb,
			        'poison', 'the consumer refused the envelope', 3, clock_timestamp(), $4)`,
			envelope.ID.String(), envelope.Type.String(), string(wire), consumer)
		return err
	}); err != nil {
		t.Fatalf("seeding the abandoned delivery: %v", err)
	}

	t.Cleanup(func() {
		_ = f.setup.InTx(context.Background(), func(ctx context.Context, tx fdb.Tx) error {
			_, _ = tx.Exec(ctx, `DELETE FROM platform.dead_letter WHERE event_id = $1`, envelope.ID.String())
			_, _ = tx.Exec(ctx, `DELETE FROM platform.outbox_delivery WHERE event_id = $1`, envelope.ID.String())
			_, _ = tx.Exec(ctx, `DELETE FROM platform.outbox WHERE event_id = $1`, envelope.ID.String())
			return nil
		})
	})

	return envelope.ID, aggregate
}

func replayer(t *testing.T, f *fixture) *Replayer {
	t.Helper()
	r, err := NewReplayer(f.provider)
	if err != nil {
		t.Fatalf("NewReplayer: %v", err)
	}
	return r
}

// The whole property, in one case: the same identifier, the same lane, and the incident still open.
func TestAReplayRestoresTheEventUnderItsOwnIdentifier(t *testing.T) {
	f := newFixture(t)
	consumer := f.register(t)
	eventID, aggregate := abandoned(t, f, consumer, outbox.PriorityHigh, true)

	result, err := replayer(t, f).Replay(f.ctx, eventID, consumer)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.EventID != eventID || result.Consumer != consumer {
		t.Errorf("replayed %s to %s, want %s to %s", result.EventID, result.Consumer, eventID, consumer)
	}
	if result.Position <= 0 {
		t.Errorf("position = %d; the replayed row carries no stream position", result.Position)
	}

	var (
		rows          int
		unpublished   int
		lane          int16
		sameAggregate bool
	)
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*),
			       count(*) FILTER (WHERE d.published = FALSE),
			       max(o.priority),
			       bool_and(o.aggregate_id = $2::uuid)
			  FROM platform.outbox o
			  JOIN platform.outbox_delivery d
			    ON d.created_at = o.created_at AND d.event_id = o.event_id AND d.consumer = $3
			 WHERE o.event_id = $1`,
			eventID.String(), aggregate.String(), consumer).Scan(&rows, &unpublished, &lane, &sameAggregate)
	}); err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}

	if rows != 2 {
		t.Errorf("%d outbox rows for %s, want 2 — the abandoned one and the replay", rows, eventID)
	}
	if unpublished != 1 {
		t.Errorf("%d unpublished deliveries to %s, want exactly 1; a replay the dispatcher will not "+
			"claim delivers nothing", unpublished, consumer)
	}
	if !sameAggregate {
		t.Error("the replayed row names a different aggregate than the original")
	}
	// A revocation replayed into the standard lane queues behind lifecycle traffic, which is the
	// delay the reserved lane exists to prevent — and this event is already late by definition.
	if lane != outbox.PriorityHigh {
		t.Errorf("the replay landed in lane %d, want the reserved lane %d", lane, outbox.PriorityHigh)
	}

	// And the incident is untouched. Replay creates the opportunity for evidence; resolution
	// consumes it. A replay that closed the incident would be an operator assertion wearing a
	// delivery's clothes.
	var resolved bool
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT resolved_at IS NOT NULL FROM platform.dead_letter WHERE event_id = $1`,
			eventID.String()).Scan(&resolved)
	}); err != nil {
		t.Fatalf("reading the dead letter: %v", err)
	}
	if resolved {
		t.Error("the replay resolved the dead letter; resolution is a separate act against evidence")
	}
}

// The provider role holds no UPDATE on platform.dead_letter, so the separation above is enforced by
// the database rather than by this package remembering it. Asserted here because the grant is what
// makes the previous test's last assertion structural instead of a convention.
func TestTheReplayRoleCannotResolveWhatItReplayed(t *testing.T) {
	f := newFixture(t)
	eventID, _ := abandoned(t, f, f.register(t), outbox.PriorityHigh, true)

	err := db.WithProviderScope(f.ctx, f.provider, "attempt to resolve from the replay role",
		func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE platform.dead_letter SET resolved_at = now() WHERE event_id = $1`,
				eventID.String())
			return err
		})
	if err == nil {
		t.Error("the provider role can resolve a dead letter; replay and resolution would then be " +
			"one act performed by one credential")
	}
}

// A row that predates the retention of aggregate_id and priority cannot be re-appended, and the
// refusal is the answer rather than a fallback.
//
// Reading the aggregate out of the payload would work and is exactly the guess not to make: which
// field names the aggregate is domain knowledge, and a wrong one delivers a security event
// attributed to another subject.
func TestAnUnreplayableRowIsRefusedRatherThanReconstructed(t *testing.T) {
	f := newFixture(t)
	consumer := f.register(t)
	eventID, _ := abandoned(t, f, consumer, outbox.PriorityHigh, false)

	_, err := replayer(t, f).Replay(f.ctx, eventID, consumer)
	if !errors.Is(err, ErrUnreplayable) {
		t.Fatalf("Replay returned %v, want ErrUnreplayable", err)
	}

	var rows int
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM platform.outbox WHERE event_id = $1`, eventID.String()).Scan(&rows)
	}); err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d outbox rows after a refused replay, want 1; the refusal left something behind", rows)
	}
}

func TestReplayingAResolvedIncidentIsRefused(t *testing.T) {
	f := newFixture(t)
	consumer := f.register(t)
	eventID, _ := abandoned(t, f, consumer, outbox.PriorityHigh, true)

	// All four columns, because a timestamp alone is no longer a resolution: platform migration
	// 0006 refuses an incident closed without a reason, an author, and a reference to the evidence.
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE platform.dead_letter
			   SET resolved_at = now(), resolution_type = 'REPLAYED', resolved_by = 'suite',
			       resolution_reference = 'platform.delivery_receipt:' || event_id::text
			 WHERE event_id = $1 AND consumer = $2`, eventID.String(), consumer)
		return err
	}); err != nil {
		t.Fatalf("resolving the dead letter: %v", err)
	}

	if _, err := replayer(t, f).Replay(f.ctx, eventID, consumer); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("Replay returned %v, want ErrAlreadyResolved; an operator replaying a closed "+
			"incident has a wrong picture of the estate", err)
	}
}

func TestReplayingAnUnknownEventIsRefused(t *testing.T) {
	f := newFixture(t)

	if _, err := replayer(t, f).Replay(f.ctx, mustID(t), f.register(t)); !errors.Is(err, ErrDeadLetterNotFound) {
		t.Fatalf("Replay returned %v, want ErrDeadLetterNotFound", err)
	}
}

// A dead letter is one consumer's (ADR-GLB-018 §5.3). Its replay is owed to that consumer alone, so a
// consumer that applied the event the first time is not sent it again.
func TestAReplayIsOwedToTheRefusingConsumerAlone(t *testing.T) {
	f := newFixture(t)
	refusing, bystander := f.register(t), f.register(t)
	eventID, _ := abandoned(t, f, refusing, outbox.PriorityHigh, true)

	if _, err := replayer(t, f).Replay(f.ctx, eventID, refusing); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	owed := map[string]int{}
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		rows, err := tx.Query(ctx, `SELECT consumer, count(*) FROM platform.outbox_delivery
			WHERE event_id = $1 AND published = FALSE GROUP BY consumer`, eventID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var consumer string
			var n int
			if err := rows.Scan(&consumer, &n); err != nil {
				return err
			}
			owed[consumer] = n
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("reading the deliveries: %v", err)
	}
	if owed[refusing] != 1 || owed[bystander] != 0 || len(owed) != 1 {
		t.Errorf("the replay is owed to %v, want %s alone", owed, refusing)
	}

	// And another consumer's name finds no incident, and says which consumer has one.
	_, err := replayer(t, f).Replay(f.ctx, eventID, bystander)
	if !errors.Is(err, ErrDeadLetterNotFound) || !strings.Contains(err.Error(), refusing) {
		t.Errorf("replaying at %s answered %v, want ErrDeadLetterNotFound naming %s", bystander, err, refusing)
	}
}

// A consumer that no longer subscribes would be delivered nothing, so the replay is refused rather
// than committed owed to nobody. A retired consumer's incident is waived instead.
func TestAReplayToAConsumerThatNoLongerSubscribesIsRefused(t *testing.T) {
	f := newFixture(t)
	consumer := f.register(t)
	eventID, _ := abandoned(t, f, consumer, outbox.PriorityHigh, true)
	if err := f.registry.Retire(f.ctx, consumer); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	if _, err := replayer(t, f).Replay(f.ctx, eventID, consumer); !errors.Is(err, ErrConsumerNotSubscribed) {
		t.Fatalf("replaying to a retired consumer answered %v, want ErrConsumerNotSubscribed", err)
	}
	var rows int
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM platform.outbox WHERE event_id = $1`, eventID.String()).Scan(&rows)
	}); err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d outbox rows after a refused replay, want the original alone", rows)
	}
}

// Replay files an access record before it does anything, so an attempt that fails is still
// attributable — the scope wrapper writes it outside the transaction for exactly that reason.
//
// Asserted against the fixture's recorder rather than against audit.privileged_access. This suite
// builds its provider pool with a capturing recorder, so nothing reaches the table; a first version
// of this test counted rows there, found none, and reported a missing audit record for a mechanism
// that was working. The stub is what this suite has, so the stub is what it may assert on.
func TestAFailedReplayStillLeavesAnAccessRecord(t *testing.T) {
	f := newFixture(t)

	before := f.recorder.calls
	_, err := replayer(t, f).Replay(f.ctx, mustID(t), "nobody")
	if err == nil {
		t.Fatal("replaying an unknown event succeeded")
	}
	if f.recorder.calls <= before {
		t.Error("a refused replay filed no privileged-access record; the attempt is unattributable")
	}
}

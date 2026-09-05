package projection

// The publication frontier, against a real outbox.
//
// It exists because a consumer cannot compute this: the outbox allocates its sequence before the
// transaction commits, so a gap in a consumer's applied positions is indistinguishable from a number
// a rolled-back transaction consumed. These cases assert the two facts a consumer needs, and the one
// that would otherwise poison them forever.

import (
	"context"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
)

// The frontier reads platform.outbox aggregates, so it takes the raw transactor the fixture already
// holds rather than the scoped provider pool -- the same reason the production wiring does.
func frontierReader(t *testing.T) (*FrontierReader, *fdb.Pool, context.Context) {
	t.Helper()

	f := newFixture(t)
	reader, err := NewFrontierReader(f.pool)
	if err != nil {
		t.Fatalf("NewFrontierReader: %v", err)
	}
	return reader, f.pool, f.ctx
}

// insertOutboxRow writes one row directly, because what is under test is how the frontier reads the
// outbox rather than how a domain service writes to it.
//
// The age is an interval the database subtracts from its own clock, rather than a timestamp this
// process computes. A test that dated its rows from the Go clock and then asserted on an age the
// database measures would be asserting that the two clocks agree — which is the assumption the reader
// was corrected to stop making, and it has no place in the case that checks the correction.
func insertOutboxRow(t *testing.T, ctx context.Context, pool *fdb.Pool, published bool, age time.Duration) int64 {
	t.Helper()

	var sequence int64
	if err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO platform.outbox
			    (event_id, aggregate_id, event_type, payload, envelope, priority, published, published_at, created_at)
			VALUES (gen_random_uuid(), gen_random_uuid(),
			        'com.scnehaux.organization.membership.security.revoked',
			        '{}'::jsonb, '{"specversion":"1.0"}'::jsonb, 0, $1,
			        CASE WHEN $1 THEN clock_timestamp() ELSE NULL END,
			        clock_timestamp() - $2::interval)
			RETURNING sequence`, published, age.String()).Scan(&sequence)
	}); err != nil {
		t.Fatalf("inserting an outbox row: %v", err)
	}
	return sequence
}

func TestTheFrontierReportsTheOldestOwedDelivery(t *testing.T) {
	reader, pool, ctx := frontierReader(t)

	// Old and unpublished: this is the row a consumer's freshness depends on.
	owed := insertOutboxRow(t, ctx, pool, false, 90*time.Second)

	frontier, err := reader.Frontier(ctx)
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}

	switch {
	case !frontier.Unpublished:
		t.Fatal("the frontier reports nothing owed while an unpublished row exists")
	case frontier.OldestUnpublishedMark > owed:
		t.Errorf("oldest unpublished mark is %d, and a row at %d is owed",
			frontier.OldestUnpublishedMark, owed)
	case frontier.OldestUnpublishedAge < 80*time.Second:
		t.Errorf("oldest unpublished age is %s, and the row was created 90s ago",
			frontier.OldestUnpublishedAge)
	case frontier.HighestCommittedMark < owed:
		t.Errorf("highest committed mark is %d, below the %d that is visible", frontier.HighestCommittedMark, owed)
	case frontier.ObservedAt.IsZero():
		t.Error("the frontier carries no observation instant, so a consumer cannot age it")
	}
}

// clearDeadLetters removes the rows these cases assert on.
//
// The suite shares one database, so a dead letter another case left behind — or one left by an earlier
// run — would be counted here. Scoped to the authority event types rather than truncating the table,
// so a case about some other event type is not quietly destroyed by this one.
func clearDeadLetters(t *testing.T, ctx context.Context, pool *fdb.Pool) {
	t.Helper()

	if err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM platform.dead_letter WHERE event_type = ANY ($1::text[])`,
			AuthorityEventTypes)
		return err
	}); err != nil {
		t.Fatalf("clearing dead letters: %v", err)
	}
}

// insertDeadLetter writes one dead letter as the dispatcher leaves it: the outbox row marked published
// with its error recorded, and the incident recorded in platform.dead_letter.
func insertDeadLetter(t *testing.T, ctx context.Context, pool *fdb.Pool,
	eventType string, age time.Duration, resolved bool) int64 {
	t.Helper()

	var sequence int64
	if err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		// The instant comes from the database, so the age this asserts on is not a statement about
		// the agreement between two clocks. See frontier_clock_test.go.
		var eventID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO platform.outbox
			    (event_id, aggregate_id, event_type, payload, envelope, priority, published, published_at,
			     created_at, attempts, first_failed_at, failure_class, last_error)
			VALUES (gen_random_uuid(), gen_random_uuid(), $1,
			        '{}'::jsonb, '{"specversion":"1.0"}'::jsonb, 0, TRUE, NULL,
			        clock_timestamp() - $2::interval, 3, clock_timestamp() - $2::interval,
			        'poison', 'refused')
			RETURNING event_id, sequence`, eventType, age.String()).Scan(&eventID, &sequence); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO platform.dead_letter
			    (event_id, event_type, envelope, payload, consumer, failure_class, failure_detail,
			     attempts, first_failed_at, dead_lettered_at, resolved_at)
			VALUES ($1, $2, '{"specversion":"1.0"}'::jsonb, '{}'::jsonb, 'foundation-reference',
			        'poison', 'the consumer refused the envelope', 3,
			        clock_timestamp() - $3::interval, clock_timestamp() - $3::interval,
			        CASE WHEN $4 THEN clock_timestamp() ELSE NULL END)`,
			eventID, eventType, age.String(), resolved)
		return err
	}); err != nil {
		t.Fatalf("inserting a dead letter: %v", err)
	}
	return sequence
}

// TestADeadLetteredRowIsNotOwedForever is the case that would make every consumer permanently stale.
//
// The dispatcher marks a dead-lettered row published, with its error recorded, so it leaves the
// unpublished pool. Read with `published_at IS NULL` instead of `published = FALSE`, one poison event
// would hold the oldest-unpublished age open indefinitely and every consumer would refuse traffic it
// could safely serve.
//
// And the counterpart, which is the half that was missing: leaving the pool must not mean leaving the
// report. A revocation nobody will deliver again is the single most dangerous state this frontier can
// be in, and while it was only excluded from the pool the frontier answered "nothing owed" — every
// signal a consumer could see reading fresh while it held an active row the revocation was meant to
// withdraw.
func TestADeadLetteredRowIsNotOwedForeverAndIsStillReported(t *testing.T) {
	reader, pool, ctx := frontierReader(t)
	clearDeadLetters(t, ctx, pool)

	sequence := insertDeadLetter(t, ctx, pool,
		"com.scnehaux.organization.membership.security.revoked", 48*time.Hour, false)

	frontier, err := reader.Frontier(ctx)
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}

	if frontier.Unpublished && frontier.OldestUnpublishedAge > 24*time.Hour {
		t.Errorf("a dead-lettered row is still reported as owed, %s old: one poison event would make "+
			"every consumer permanently stale", frontier.OldestUnpublishedAge)
	}
	if frontier.HighestCommittedMark < sequence {
		t.Errorf("highest committed mark is %d, below the dead-lettered row at %d",
			frontier.HighestCommittedMark, sequence)
	}

	switch {
	case !frontier.SecurityDebt:
		t.Fatal("an unresolved Membership revocation is dead-lettered and the frontier reports no debt")
	case frontier.SecurityDeadLettered != 1:
		t.Errorf("the frontier counts %d dead-lettered authority events, and one was written",
			frontier.SecurityDeadLettered)
	case frontier.OldestSecurityDeadLetterAge < 47*time.Hour:
		t.Errorf("the oldest debt is reported as %s, and it was dead-lettered 48h ago",
			frontier.OldestSecurityDeadLetterAge)
	}
}

// A resolved dead letter is not debt. Resolution is the operator's statement that the delivery was
// made good, and a frontier that kept refusing afterwards would have no exit: every consumer would
// stay stale for the life of the row.
func TestAResolvedDeadLetterIsNotDebt(t *testing.T) {
	reader, pool, ctx := frontierReader(t)
	clearDeadLetters(t, ctx, pool)

	insertDeadLetter(t, ctx, pool,
		"com.scnehaux.organization.membership.security.revoked", time.Hour, true)

	frontier, err := reader.Frontier(ctx)
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}
	if frontier.SecurityDebt || frontier.SecurityDeadLettered != 0 {
		t.Errorf("a resolved dead letter is reported as debt (%d unresolved): resolution would have "+
			"no effect and every consumer would stay stale for the life of the row",
			frontier.SecurityDeadLettered)
	}
}

// And a dead letter that carries no Membership authority is not debt either.
//
// The distinction is the whole reason the event types are listed rather than counted wholesale: a
// dead-lettered Workspace rename is an operational annoyance, and refusing every LOW_RISK read in the
// estate until somebody clears it is the kind of blanket strictness that gets a gate switched off.
func TestADeadLetterCarryingNoMembershipAuthorityIsNotDebt(t *testing.T) {
	reader, pool, ctx := frontierReader(t)
	clearDeadLetters(t, ctx, pool)

	insertDeadLetter(t, ctx, pool,
		"com.scnehaux.organization.workspace.lifecycle.archived", time.Hour, false)

	frontier, err := reader.Frontier(ctx)
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}
	if frontier.SecurityDebt {
		t.Errorf("a dead-lettered Workspace rename is counted as Membership authority debt (%d): "+
			"every projection-backed read in the estate would refuse until an operator cleared it",
			frontier.SecurityDeadLettered)
	}
}

// TestTheFrontierReportsNoVerdict is a structural assertion rather than a behavioural one, and it is
// deliberate: the facts must not be pre-combined here.
//
// A single freshness number computed on this side would put a policy decision in the producer, which
// cannot see which operation is being authorised — and the same lag is acceptable for a directory read
// and unacceptable for a payroll one.
func TestTheFrontierReportsNoVerdict(t *testing.T) {
	reader, pool, ctx := frontierReader(t)
	insertOutboxRow(t, ctx, pool, false, time.Minute)

	frontier, err := reader.Frontier(ctx)
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}

	// Every field is a measurement. If a boolean called something like Fresh or Stale ever appears
	// here, this test is the place the reason is written down.
	if frontier.OldestUnpublishedAge <= 0 {
		t.Error("the age is not a measurement")
	}
	if frontier.ObservedAt.IsZero() {
		t.Error("the frontier carries no observation instant, so a consumer cannot age it")
	}

	// Deliberately not compared against time.Now(). The observation instant is on the database's
	// clock and this process's is a different one, so a bound asserted here would be an assertion
	// about clock agreement rather than about the frontier.

}

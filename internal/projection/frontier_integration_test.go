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
func insertOutboxRow(t *testing.T, ctx context.Context, pool *fdb.Pool, published bool, createdAt time.Time) int64 {
	t.Helper()

	var sequence int64
	if err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO platform.outbox
			    (event_id, aggregate_id, event_type, payload, envelope, priority, published, published_at, created_at)
			VALUES (gen_random_uuid(), gen_random_uuid(),
			        'com.scnehaux.organization.membership.security.revoked',
			        '{}'::jsonb, '{"specversion":"1.0"}'::jsonb, 0, $1,
			        CASE WHEN $1 THEN now() ELSE NULL END, $2)
			RETURNING sequence`, published, createdAt).Scan(&sequence)
	}); err != nil {
		t.Fatalf("inserting an outbox row: %v", err)
	}
	return sequence
}

func TestTheFrontierReportsTheOldestOwedDelivery(t *testing.T) {
	reader, pool, ctx := frontierReader(t)

	// Old and unpublished: this is the row a consumer's freshness depends on.
	old := time.Now().UTC().Add(-90 * time.Second)
	owed := insertOutboxRow(t, ctx, pool, false, old)

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

// TestADeadLetteredRowIsNotOwedForever is the case that would make every consumer permanently stale.
//
// The dispatcher marks a dead-lettered row published, with its error recorded, so it leaves the
// unpublished pool. Read with `published_at IS NULL` instead of `published = FALSE`, one poison event
// would hold the oldest-unpublished age open indefinitely and every consumer would refuse traffic it
// could safely serve.
func TestADeadLetteredRowIsNotOwedForever(t *testing.T) {
	reader, pool, ctx := frontierReader(t)

	// A dead letter, as the dispatcher leaves it: published = TRUE with no published_at.
	ancient := time.Now().UTC().Add(-48 * time.Hour)
	var sequence int64
	if err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO platform.outbox
			    (event_id, aggregate_id, event_type, payload, envelope, priority, published, published_at,
			     created_at, attempts, first_failed_at, failure_class, last_error)
			VALUES (gen_random_uuid(), gen_random_uuid(),
			        'com.scnehaux.organization.membership.security.revoked',
			        '{}'::jsonb, '{"specversion":"1.0"}'::jsonb, 0, TRUE, NULL, $1, 3, $1, 'poison', 'refused')
			RETURNING sequence`, ancient).Scan(&sequence)
	}); err != nil {
		t.Fatalf("inserting a dead-lettered row: %v", err)
	}

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
}

// TestTheFrontierReportsNoVerdict is a structural assertion rather than a behavioural one, and it is
// deliberate: the facts must not be pre-combined here.
//
// A single freshness number computed on this side would put a policy decision in the producer, which
// cannot see which operation is being authorised — and the same lag is acceptable for a directory read
// and unacceptable for a payroll one.
func TestTheFrontierReportsNoVerdict(t *testing.T) {
	reader, pool, ctx := frontierReader(t)
	insertOutboxRow(t, ctx, pool, false, time.Now().UTC().Add(-time.Minute))

	frontier, err := reader.Frontier(ctx)
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}

	// Every field is a measurement. If a boolean called something like Fresh or Stale ever appears
	// here, this test is the place the reason is written down.
	if frontier.OldestUnpublishedAge <= 0 {
		t.Error("the age is not a measurement")
	}
	if frontier.ObservedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Error("the observation instant is in the future")
	}
}

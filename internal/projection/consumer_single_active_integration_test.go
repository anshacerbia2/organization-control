package projection

// The single-consumer scope, and its exit.
//
// The distributed enforcement work is scoped to one producer and one projection consumer.
// Several things that are correct at that scope are silently wrong beyond it: dead-letter
// debt is reported estate-wide, so one consumer's poison refuses traffic for all of them;
// platform.dead_letter holds one row per event_id, so an event owed to two consumers cannot
// record two outcomes; and resolution evidence attaches to an event rather than to a
// delivery, so evidence produced for one consumer would resolve another's incident.
//
// None of those announce themselves. A limitation written in a design note does not survive
// the day someone registers a second consumer and nothing goes red, which is why these tests
// exist alongside the index that enforces it.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anshacerbia2/organization-control/internal/db"
)

func (f *fixture) registerNamed(t *testing.T, consumerID string) error {
	t.Helper()
	_, err := f.registry.Register(f.ctx, Registration{
		ConsumerID:        consumerID,
		ProjectionVersion: "v1",
		MaxAcceptedAge:    30 * time.Second,
		StaleBehavior:     StaleFailClosed,
	})
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM projection.consumer WHERE consumer_id = $1`, consumerID)
	})
	return err
}

func (f *fixture) activeCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := db.WithProviderScope(f.ctx, f.provider, "projection scope suite",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM projection.consumer WHERE retired_at IS NULL`).Scan(&n)
		}); err != nil {
		t.Fatalf("counting active consumers: %v", err)
	}
	return n
}

func TestASecondActiveConsumerIsRefused(t *testing.T) {
	f := newFixture(t)

	first := "single-first-" + mustID(t).String()
	if err := f.registerNamed(t, first); err != nil {
		t.Fatalf("registering the first consumer: %v", err)
	}

	second := "single-second-" + mustID(t).String()
	err := f.registerNamed(t, second)
	if !errors.Is(err, ErrSingleConsumer) {
		t.Fatalf("registering a second consumer returned %v, want ErrSingleConsumer", err)
	}
	// The refusal has to name the consumer in the way, or the operator's next move is to
	// guess. Reading the table to find out is the step this message removes.
	if !strings.Contains(err.Error(), first) {
		t.Errorf("the refusal does not name the consumer holding the slot: %v", err)
	}
	if got := f.activeCount(t); got != 1 {
		t.Errorf("%d active consumers after a refused registration, want 1", got)
	}
}

// Re-registering the same consumer must keep working. It is how a consumer changes its
// declared freshness budget, and reading that as "a second consumer" would make the scope
// gate break the ordinary case rather than the one it exists for.
func TestReRegisteringTheActiveConsumerIsNotASecondConsumer(t *testing.T) {
	f := newFixture(t)

	consumerID := "single-same-" + mustID(t).String()
	if err := f.registerNamed(t, consumerID); err != nil {
		t.Fatalf("first registration: %v", err)
	}

	updated, err := f.registry.Register(f.ctx, Registration{
		ConsumerID:        consumerID,
		ProjectionVersion: "v2",
		MaxAcceptedAge:    90 * time.Second,
		StaleBehavior:     StaleUseWithMarker,
	})
	if err != nil {
		t.Fatalf("re-registering the same consumer: %v", err)
	}
	if updated.ProjectionVersion != "v2" || updated.MaxAcceptedAge != 90*time.Second {
		t.Errorf("the declared terms were not updated: %+v", updated)
	}
}

// The exit path. Without it the refusal above has no legitimate way past it, and a refusal
// with no way past it gets deleted rather than respected.
func TestRetireFreesTheSlotForAReplacement(t *testing.T) {
	f := newFixture(t)

	outgoing := "single-outgoing-" + mustID(t).String()
	if err := f.registerNamed(t, outgoing); err != nil {
		t.Fatalf("registering the outgoing consumer: %v", err)
	}

	incoming := "single-incoming-" + mustID(t).String()
	if err := f.registerNamed(t, incoming); !errors.Is(err, ErrSingleConsumer) {
		t.Fatalf("the slot was not held: %v", err)
	}

	if err := f.registry.Retire(f.ctx, outgoing); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if err := f.registerNamed(t, incoming); err != nil {
		t.Fatalf("registering the replacement after retirement: %v", err)
	}
	if got := f.activeCount(t); got != 1 {
		t.Errorf("%d active consumers after a rotation, want 1", got)
	}
}

// Retirement withdraws authority and keeps the record. The row is what an investigation into
// a stale enforcement decision reads after the consumer is gone.
func TestARetiredConsumerHoldsNoAuthorityAndKeepsItsRecord(t *testing.T) {
	f := newFixture(t)

	consumerID := "single-retired-" + mustID(t).String()
	if err := f.registerNamed(t, consumerID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := f.registry.Retire(f.ctx, consumerID); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	// Every runtime path -- snapshot, progress, frontier -- reads through this, so one
	// refusal here withdraws all of them.
	if _, err := f.registry.Get(f.ctx, consumerID); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("Get on a retired consumer returned %v, want ErrNotRegistered", err)
	}
	if _, err := f.registry.RecordProgress(f.ctx, Progress{ConsumerID: consumerID, AppliedMark: 1}); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("RecordProgress for a retired consumer returned %v, want ErrNotRegistered", err)
	}

	var kept int
	if err := db.WithProviderScope(f.ctx, f.provider, "projection scope suite",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM projection.consumer WHERE consumer_id = $1 AND retired_at IS NOT NULL`,
				consumerID).Scan(&kept)
		}); err != nil {
		t.Fatalf("reading the retired row: %v", err)
	}
	if kept != 1 {
		t.Error("the row was removed rather than stamped, so what this consumer was told is gone")
	}
}

func TestRetiringTwiceIsNotAnErrorAndRetiringNothingIs(t *testing.T) {
	f := newFixture(t)

	consumerID := "single-twice-" + mustID(t).String()
	if err := f.registerNamed(t, consumerID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := f.registry.Retire(f.ctx, consumerID); err != nil {
		t.Fatalf("first Retire: %v", err)
	}
	// The caller asked for a state that already holds, so there is nothing to report.
	if err := f.registry.Retire(f.ctx, consumerID); err != nil {
		t.Errorf("retiring an already-retired consumer returned %v, want nil", err)
	}
	// A mistyped name, on the other hand, must not be answered as success.
	if err := f.registry.Retire(f.ctx, "single-never-registered"); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("retiring an unknown consumer returned %v, want ErrNotRegistered", err)
	}
	if err := f.registry.Retire(f.ctx, "  "); !errors.Is(err, ErrInvalid) {
		t.Errorf("retiring a blank identifier returned %v, want ErrInvalid", err)
	}
}

// A consumer returning under an identity it previously held is a legitimate act, and the rule
// is about how many are active at once rather than about which names have ever been used.
func TestRegisteringARetiredIdentityRevivesIt(t *testing.T) {
	f := newFixture(t)

	consumerID := "single-revived-" + mustID(t).String()
	if err := f.registerNamed(t, consumerID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := f.registry.Retire(f.ctx, consumerID); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	if _, err := f.registry.Register(f.ctx, Registration{
		ConsumerID:        consumerID,
		ProjectionVersion: "v1",
		MaxAcceptedAge:    30 * time.Second,
		StaleBehavior:     StaleFailClosed,
	}); err != nil {
		t.Fatalf("re-registering a retired identity: %v", err)
	}
	if _, err := f.registry.Get(f.ctx, consumerID); err != nil {
		t.Errorf("the revived consumer does not read as registered: %v", err)
	}
}

// The one that matters most, and the reason the constraint is in the schema rather than only
// in Register.
//
// grants.sql gives organization_provider_rt INSERT and UPDATE on projection.consumer, so an
// operator with psql goes around every check in this package. A scope boundary that only the
// application respects is a boundary that ends quietly, on an afternoon nobody records.
func TestTheDatabaseRefusesASecondActiveConsumerWithoutTheRegistry(t *testing.T) {
	f := newFixture(t)

	first := "single-direct-first-" + mustID(t).String()
	if err := f.registerNamed(t, first); err != nil {
		t.Fatalf("registering the first consumer: %v", err)
	}

	second := "single-direct-second-" + mustID(t).String()
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM projection.consumer WHERE consumer_id = $1`, second)
	})

	// Straight INSERT as organization_provider_rt, exactly what psql would do.
	err := db.WithProviderScope(f.ctx, f.provider, "projection scope suite",
		func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO projection.consumer
			    (consumer_id, projection_version, max_accepted_age, stale_behavior)
			    VALUES ($1, 'v1', interval '30 seconds', 'fail_closed')`, second)
			return err
		})
	if err == nil {
		t.Fatal("the database accepted a second active consumer inserted directly; " +
			"consumer_single_active is missing, so the scope holds only while callers go through Register")
	}
	if !strings.Contains(err.Error(), "consumer_single_active") {
		t.Errorf("the insert failed for some other reason than the scope index: %v", err)
	}
	if got := f.activeCount(t); got != 1 {
		t.Errorf("%d active consumers, want 1", got)
	}
}

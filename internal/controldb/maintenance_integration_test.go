package controldb_test

// The maintenance stage, against the migrated database, as its owner.
//
// The platform's own suite proves each helper's rule. These prove the stage runs them: that the
// partitions exist afterwards, that a resolved payload and an uncited receipt are gone and a cited
// receipt is not, and that a stale incident is reported rather than disposed.

import (
	"context"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/controldb"
)

func execAdmin(t *testing.T, ctx context.Context, pool *db.Pool, statement string, args ...any) {
	t.Helper()
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, statement, args...)
		return err
	}); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

func TestMaintenanceCreatesThePartitionsAheadAndCanRunAgain(t *testing.T) {
	pool, ctx := openAdmin(t)

	first, err := controldb.RunMaintenance(ctx, pool, controldb.DefaultMaintenance)
	if err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	// Today through today + PartitionsAhead.
	if want := controldb.DefaultMaintenance.PartitionsAhead + 1; len(first.PartitionsEnsured) != want {
		t.Errorf("ensured %d partitions, want %d: %v", len(first.PartitionsEnsured), want, first.PartitionsEnsured)
	}
	for _, name := range first.PartitionsEnsured {
		var exists bool
		if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT to_regclass('platform.' || $1) IS NOT NULL`, name).Scan(&exists)
		}); err != nil {
			t.Fatalf("looking up %s: %v", name, err)
		}
		if !exists {
			t.Errorf("partition %s was reported ensured and does not exist", name)
		}
	}

	// A scheduled job runs again tomorrow, and after a failed run. A second run must succeed and
	// change nothing it already did.
	if _, err := controldb.RunMaintenance(ctx, pool, controldb.DefaultMaintenance); err != nil {
		t.Fatalf("a second RunMaintenance failed: %v", err)
	}
}

func TestMaintenanceDisposesResolvedPayloadsAndPrunesUncitedReceipts(t *testing.T) {
	pool, ctx := openAdmin(t)

	// Receipt pruning pauses while any incident is open, so an unresolved row another suite left
	// behind would make this case pass or fail for a reason it does not test. This is the
	// disposable CI database, and the platform suite owns the pause itself.
	execAdmin(t, ctx, pool, `DELETE FROM platform.dead_letter WHERE resolved_at IS NULL`)

	aged := 100 * 24 * time.Hour
	incident, cited, uncited := newUUID(t), newUUID(t), newUUID(t)
	consumer := "maintenance-suite"

	execAdmin(t, ctx, pool, `
		INSERT INTO platform.delivery_receipt (event_id, consumer, event_type, evidence, recorded_at)
		VALUES ($1::uuid, $3, 'com.scnehaux.test.maintenance', 'consumer_applied', now() - make_interval(secs => $4)),
		       ($2::uuid, $3, 'com.scnehaux.test.maintenance', 'consumer_applied', now() - make_interval(secs => $4))`,
		cited, uncited, consumer, aged.Seconds())
	execAdmin(t, ctx, pool, `
		INSERT INTO platform.dead_letter
		    (event_id, event_type, envelope, payload, failure_class, failure_detail, attempts, first_failed_at,
		     resolved_at, resolution_type, resolved_by, resolution_reference)
		VALUES ($1::uuid, 'com.scnehaux.test.maintenance', '{"a":1}'::jsonb, '{"b":2}'::jsonb, 'poison', 'refused', 3,
		        now() - make_interval(secs => $2), now() - make_interval(secs => $2), 'REPLAYED', 'suite', $3)`,
		incident, aged.Seconds(), outbox.ReceiptReference(cited, consumer))
	t.Cleanup(func() {
		execAdmin(t, context.Background(), pool, `DELETE FROM platform.dead_letter WHERE event_id = $1::uuid`, incident)
		execAdmin(t, context.Background(), pool,
			`DELETE FROM platform.delivery_receipt WHERE event_id = ANY ($1::uuid[])`, []string{cited, uncited})
	})

	report, err := controldb.RunMaintenance(ctx, pool, controldb.DefaultMaintenance)
	if err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	if report.DeadLettersDisposed < 1 || report.ReceiptsPruned < 1 {
		t.Errorf("report = %+v, want at least one disposal and one pruned receipt", report)
	}

	var payloadGone, citedKept, uncitedKept bool
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT (SELECT envelope IS NULL AND payload IS NULL AND resolution_reference IS NOT NULL
			          FROM platform.dead_letter WHERE event_id = $1::uuid),
			       EXISTS (SELECT 1 FROM platform.delivery_receipt WHERE event_id = $2::uuid),
			       EXISTS (SELECT 1 FROM platform.delivery_receipt WHERE event_id = $3::uuid)`,
			incident, cited, uncited).Scan(&payloadGone, &citedKept, &uncitedKept)
	}); err != nil {
		t.Fatalf("reading the outcome: %v", err)
	}
	if !payloadGone {
		t.Error("a dead letter resolved past retention kept its payload, or lost its resolution record")
	}
	if !citedKept {
		t.Error("the receipt the closure cites was pruned; the closure now explains nothing")
	}
	if uncitedKept {
		t.Error("an uncited receipt past retention survived maintenance")
	}
}

// A stale incident is reported and never disposed: the count is what a scheduler alerts on.
func TestMaintenanceReportsStaleIncidentsWithoutDisposingThem(t *testing.T) {
	pool, ctx := openAdmin(t)

	incident := newUUID(t)
	execAdmin(t, ctx, pool, `
		INSERT INTO platform.dead_letter
		    (event_id, event_type, envelope, payload, failure_class, failure_detail, attempts,
		     first_failed_at, dead_lettered_at)
		VALUES ($1::uuid, 'com.scnehaux.test.maintenance', '{"a":1}'::jsonb, '{}'::jsonb, 'poison', 'refused', 3,
		        now() - interval '2 days', now() - interval '2 days')`, incident)
	t.Cleanup(func() {
		execAdmin(t, context.Background(), pool, `DELETE FROM platform.dead_letter WHERE event_id = $1::uuid`, incident)
	})

	report, err := controldb.RunMaintenance(ctx, pool, controldb.DefaultMaintenance)
	if err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	if report.StaleUnresolved < 1 {
		t.Errorf("an incident open for two days is not reported stale: %+v", report)
	}
	var kept bool
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `SELECT envelope IS NOT NULL FROM platform.dead_letter WHERE event_id = $1::uuid`,
			incident).Scan(&kept)
	}); err != nil {
		t.Fatalf("reading the incident: %v", err)
	}
	if !kept {
		t.Error("an unresolved incident lost its payload; it can no longer be replayed")
	}
}

func TestMaintenanceRefusesANonsenseBoundary(t *testing.T) {
	pool, ctx := openAdmin(t)
	cfg := controldb.DefaultMaintenance
	cfg.ReceiptRetention = 0
	if _, err := controldb.RunMaintenance(ctx, pool, cfg); err == nil {
		t.Error("RunMaintenance accepted a zero receipt retention, which would prune every uncited receipt")
	}
}

func newUUID(t *testing.T) string {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	return value.String()
}

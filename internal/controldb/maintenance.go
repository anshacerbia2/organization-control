package controldb

// Retention and partition maintenance: the work foundation-platform's helpers describe and, until
// this stage, nothing ran.
//
// platform.outbox is partitioned by day, and ensure_outbox_partitions and drop_outbox_partitions
// existed from the first platform migration. DisposeResolvedDeadLetters existed from the same
// release, and PruneDeliveryReceipts since v0.2.8. No deployable called any of them. So every row
// landed in platform.outbox_default and none left, resolved dead letters kept restricted payloads
// forever, and evidence grew without bound -- each of which the platform's design had bounded on
// paper.
//
// This runs as the migration role, which owns the tables: dropping a partition, nulling a retained
// payload and deleting a receipt are all things no runtime role may do.
//
// # One clock
//
// Every boundary is computed from the database's now(), read once. The process clock and the
// database clock can disagree, and a retention boundary computed from the wrong one deletes early
// in exactly the direction nobody notices.

import (
	"context"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/migrations"
	"github.com/anshacerbia2/foundation-platform/outbox"
)

// MaintenanceConfig holds the boundaries. The defaults are foundation-platform TDD-001's values.
type MaintenanceConfig struct {
	PartitionsAhead     int           // days of outbox partitions created ahead of today
	OutboxRetention     time.Duration // fully published partitions older than this are dropped
	DeadLetterRetention time.Duration // resolved dead letters lose envelope and payload after this
	ReceiptRetention    time.Duration // uncited receipts are pruned after this, while no incident is open
	StaleAlert          time.Duration // unresolved dead letters older than this are reported

	// BatchPreviewRetention is how long a Membership batch preview is kept past its expiry before it
	// is purged (TDD-organization-control-002 §Membership Batches).
	BatchPreviewRetention time.Duration
}

// DefaultMaintenance is foundation-platform TDD-001 §Configuration.
var DefaultMaintenance = MaintenanceConfig{
	PartitionsAhead:     7,
	OutboxRetention:     30 * 24 * time.Hour,
	DeadLetterRetention: 90 * 24 * time.Hour,
	ReceiptRetention:    90 * 24 * time.Hour,
	StaleAlert:          24 * time.Hour,

	// A day past the 15-minute expiry: an administrator who left the screen open overnight still
	// reads the preview as expired rather than as absent, and nothing is executable meanwhile.
	BatchPreviewRetention: 24 * time.Hour,
}

// MaintenanceReport is what one run did, for the log line a scheduler keeps.
type MaintenanceReport struct {
	ObservedAt          time.Time
	PartitionsEnsured   []string
	PartitionsDropped   []string
	DeadLettersDisposed int64
	ReceiptsPruned      int64

	// BatchPreviewsPurged counts expired Membership batch previews deleted, with their items.
	BatchPreviewsPurged int64

	// StaleUnresolved counts incidents open longer than StaleAlert. They are never disposed; the
	// count exists so a scheduler can alert, and the stage reports it rather than failing on it,
	// because the retention work above is still correct to do.
	StaleUnresolved int64
}

func (c MaintenanceConfig) validate() error {
	switch {
	case c.PartitionsAhead < 1:
		return fmt.Errorf("controldb: at least one partition ahead is required, got %d", c.PartitionsAhead)
	case c.OutboxRetention <= 0, c.DeadLetterRetention <= 0, c.ReceiptRetention <= 0, c.StaleAlert <= 0,
		c.BatchPreviewRetention <= 0:
		return fmt.Errorf("controldb: every retention boundary must be positive: %+v", c)
	}
	return nil
}

// RunMaintenance creates the partitions ahead, drops expired published ones, disposes of resolved
// dead-letter payloads, prunes receipts, and counts stale incidents -- each in its own transaction,
// so a failure in one step leaves the steps before it committed and says which one failed.
func RunMaintenance(ctx context.Context, pool *db.Pool, cfg MaintenanceConfig) (MaintenanceReport, error) {
	if err := cfg.validate(); err != nil {
		return MaintenanceReport{}, err
	}

	var report MaintenanceReport
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `SELECT now()`).Scan(&report.ObservedAt)
	}); err != nil {
		return MaintenanceReport{}, fmt.Errorf("controldb: reading the database clock: %w", err)
	}
	now := report.ObservedAt

	steps := []struct {
		name string
		run  func(ctx context.Context, tx db.Tx) error
	}{
		{"ensure outbox partitions", func(ctx context.Context, tx db.Tx) error {
			var err error
			today := now.UTC()
			report.PartitionsEnsured, err = migrations.EnsureOutboxPartitions(ctx, tx,
				today, today.AddDate(0, 0, cfg.PartitionsAhead))
			return err
		}},
		{"drop expired outbox partitions", func(ctx context.Context, tx db.Tx) error {
			var err error
			report.PartitionsDropped, err = migrations.DropPublishedOutboxPartitions(ctx, tx,
				now.Add(-cfg.OutboxRetention))
			return err
		}},
		{"dispose resolved dead letters", func(ctx context.Context, tx db.Tx) error {
			var err error
			report.DeadLettersDisposed, err = outbox.DisposeResolvedDeadLetters(ctx, tx, now.Add(-cfg.DeadLetterRetention))
			return err
		}},
		{"prune delivery receipts", func(ctx context.Context, tx db.Tx) error {
			var err error
			report.ReceiptsPruned, err = outbox.PruneDeliveryReceipts(ctx, tx, now.Add(-cfg.ReceiptRetention))
			return err
		}},
		{"purge expired membership batch previews", func(ctx context.Context, tx db.Tx) error {
			var err error
			report.BatchPreviewsPurged, err = PurgeExpiredBatchPreviews(ctx, tx, now.Add(-cfg.BatchPreviewRetention))
			return err
		}},
		{"count stale unresolved dead letters", func(ctx context.Context, tx db.Tx) error {
			var err error
			report.StaleUnresolved, err = outbox.CountStaleUnresolvedDeadLetters(ctx, tx, now.Add(-cfg.StaleAlert))
			return err
		}},
	}
	for _, step := range steps {
		if err := pool.InTx(ctx, step.run); err != nil {
			return report, fmt.Errorf("controldb: maintenance step %q: %w", step.name, err)
		}
	}
	return report, nil
}

// purgeBatchItems and purgeBatches delete the previews that expired before the boundary and were
// never executed, items first for the foreign key. An executing or executed batch is never touched:
// it is the record of what a bulk action did.
const purgeBatchItems = `DELETE FROM membership.membership_batch_item i
USING membership.membership_batch b
WHERE b.batch_id = i.batch_id
  AND b.state = 'previewed'
  AND b.expires_at < $1`

const purgeBatches = `DELETE FROM membership.membership_batch
WHERE state = 'previewed'
  AND expires_at < $1`

// PurgeExpiredBatchPreviews deletes Membership batch previews whose expiry passed before the
// boundary, and returns how many.
//
// A preview past its expiry can never execute (TDD-organization-control-002 §Membership Batches), so
// keeping it keeps nothing but the Memberships and versions it read. No runtime role holds DELETE on
// either table, so this runs here, as the migration role, like the platform's retention. The two
// tables are under FORCE ROW LEVEL SECURITY, which binds their owner too; the migration role reaches
// expired previews through the membership_batch_purge and membership_batch_item_purge policies
// rls.sql gives it, which admit nothing else and refuse every insert and update.
func PurgeExpiredBatchPreviews(ctx context.Context, tx db.Tx, before time.Time) (int64, error) {
	if _, err := tx.Exec(ctx, purgeBatchItems, before); err != nil {
		return 0, fmt.Errorf("controldb: purge expired batch items: %w", err)
	}
	tag, err := tx.Exec(ctx, purgeBatches, before)
	if err != nil {
		return 0, fmt.Errorf("controldb: purge expired batches: %w", err)
	}
	return tag.RowsAffected(), nil
}

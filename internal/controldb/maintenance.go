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
	"github.com/anshacerbia2/foundation-platform/outbox"
)

// MaintenanceConfig holds the boundaries. The defaults are foundation-platform TDD-001's values.
type MaintenanceConfig struct {
	PartitionsAhead     int           // days of outbox partitions created ahead of today
	OutboxRetention     time.Duration // fully published partitions older than this are dropped
	DeadLetterRetention time.Duration // resolved dead letters lose envelope and payload after this
	ReceiptRetention    time.Duration // uncited receipts are pruned after this, while no incident is open
	StaleAlert          time.Duration // unresolved dead letters older than this are reported
}

// DefaultMaintenance is foundation-platform TDD-001 §Configuration.
var DefaultMaintenance = MaintenanceConfig{
	PartitionsAhead:     7,
	OutboxRetention:     30 * 24 * time.Hour,
	DeadLetterRetention: 90 * 24 * time.Hour,
	ReceiptRetention:    90 * 24 * time.Hour,
	StaleAlert:          24 * time.Hour,
}

// MaintenanceReport is what one run did, for the log line a scheduler keeps.
type MaintenanceReport struct {
	ObservedAt          time.Time
	PartitionsEnsured   []string
	PartitionsDropped   []string
	DeadLettersDisposed int64
	ReceiptsPruned      int64

	// StaleUnresolved counts incidents open longer than StaleAlert. They are never disposed; the
	// count exists so a scheduler can alert, and the stage reports it rather than failing on it,
	// because the retention work above is still correct to do.
	StaleUnresolved int64
}

func (c MaintenanceConfig) validate() error {
	switch {
	case c.PartitionsAhead < 1:
		return fmt.Errorf("controldb: at least one partition ahead is required, got %d", c.PartitionsAhead)
	case c.OutboxRetention <= 0, c.DeadLetterRetention <= 0, c.ReceiptRetention <= 0, c.StaleAlert <= 0:
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

	names := func(ctx context.Context, tx db.Tx, statement string, args ...any) ([]string, error) {
		rows, err := tx.Query(ctx, statement, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			out = append(out, name)
		}
		return out, rows.Err()
	}

	steps := []struct {
		name string
		run  func(ctx context.Context, tx db.Tx) error
	}{
		{"ensure outbox partitions", func(ctx context.Context, tx db.Tx) error {
			var err error
			report.PartitionsEnsured, err = names(ctx, tx,
				`SELECT partition_name FROM platform.ensure_outbox_partitions(
				     ($1::timestamptz AT TIME ZONE 'UTC')::date,
				     ($1::timestamptz AT TIME ZONE 'UTC')::date + $2::int)`, now, cfg.PartitionsAhead)
			return err
		}},
		{"drop expired outbox partitions", func(ctx context.Context, tx db.Tx) error {
			var err error
			report.PartitionsDropped, err = names(ctx, tx,
				`SELECT partition_name FROM platform.drop_outbox_partitions($1)`, now.Add(-cfg.OutboxRetention))
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

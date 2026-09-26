// Command organization-migrate applies the parts of the Organization Database schema that
// Atlas does not own.
//
// Four sources build one database, and each is owned by whoever owns the SQL:
//
//	roles.sql                     this repository    cluster roles only, no schema, no table
//	schema.hcl via Atlas          this repository    the eight owned schemas and their tables
//	foundation-platform platform  the shared module  the platform schema, shipped in Go
//	rls.sql + grants.sql          this repository    policies, then privileges
//
// Atlas runs second, which is why the stages are separate invocations rather than one:
//
//	organization-migrate -stage=pre     # roles
//	atlas migrate apply --env ci        # the eight owned schemas and their tables
//	organization-migrate -stage=post    # platform schema, then RLS, then privileges
//
// A fourth stage is not part of a deploy. It is run on a schedule, daily, as the same role:
//
//	organization-migrate -stage=maintenance   # partitions, retention, the stale-incident count
//
// It exits 3 when an unresolved dead letter is older than -stale-alert, after doing its work, so
// the scheduler alerts on the one condition retention must never touch. controldb.RunMaintenance
// says what each step does.
//
// # Why the platform schema is applied after Atlas rather than before
//
// identity-control applies it first, and this service cannot. Atlas refuses to apply against a
// database it considers unclean, and in database scope any pre-existing schema counts —
// including `platform`, and including one named in `exclude`, which the clean check does not
// consult. The first pipeline here failed with `connected database is not clean: found schema
// "platform"`.
//
// Roles are cluster objects rather than schema objects, so creating them leaves the database
// clean. That is what makes a three-step order possible at all.
//
// The platform schema is applied from the module rather than copied into this repository. A
// column added to platform.outbox and a change to outbox.Append are one change, and splitting
// them across repositories permits a deployment where one has shipped and the other has not.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/migrations"

	"github.com/anshacerbia2/organization-control/internal/controldb"
)

const (
	stagePre         = "pre"
	stagePost        = "post"
	stageMaintenance = "maintenance"
)

// errStale reports stale incidents after a maintenance run that otherwise succeeded.
var errStale = errors.New("unresolved dead letters are older than the alert boundary")

func main() {
	stage := flag.String("stage", "", "pre (cluster roles), post (platform schema, Row-Level Security, privileges), or maintenance (partitions and retention, run on a schedule)")
	timeout := flag.Duration("timeout", 2*time.Minute, "upper bound on the whole run")
	maintenance := controldb.DefaultMaintenance
	flag.IntVar(&maintenance.PartitionsAhead, "partitions-ahead", maintenance.PartitionsAhead, "maintenance: days of outbox partitions created ahead")
	flag.DurationVar(&maintenance.OutboxRetention, "outbox-retention", maintenance.OutboxRetention, "maintenance: published partitions older than this are dropped")
	flag.DurationVar(&maintenance.DeadLetterRetention, "dead-letter-retention", maintenance.DeadLetterRetention, "maintenance: resolved dead letters lose their payload after this")
	flag.DurationVar(&maintenance.ReceiptRetention, "receipt-retention", maintenance.ReceiptRetention, "maintenance: uncited receipts are pruned after this, while no incident is open")
	flag.DurationVar(&maintenance.StaleAlert, "stale-alert", maintenance.StaleAlert, "maintenance: unresolved dead letters older than this exit 3")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*stage, *timeout, maintenance, logger); err != nil {
		if errors.Is(err, errStale) {
			logger.Error("maintenance completed with stale incidents", slog.String("error", err.Error()))
			os.Exit(3)
		}
		logger.Error("migration failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(stage string, timeout time.Duration, maintenance controldb.MaintenanceConfig, logger *slog.Logger) error {
	if stage != stagePre && stage != stagePost && stage != stageMaintenance {
		return fmt.Errorf("-stage must be %q, %q or %q, got %q", stagePre, stagePost, stageMaintenance, stage)
	}

	dsn := os.Getenv("ORGANIZATION_MIGRATION_DATABASE_URL")
	if dsn == "" {
		return errors.New("ORGANIZATION_MIGRATION_DATABASE_URL is required")
	}

	// A migration run holds DDL locks. Cancelling on a signal lets an operator abort a stuck
	// deploy without waiting for the timeout, and the transaction rolls back.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// No SessionBinder. Row-Level Security binds a Tenant, and a migration is not
	// tenant-scoped; binding one here would scope DDL to a Tenant that does not exist yet.
	// This is also the one connection in the estate that must not be subject to the policies
	// it creates, which is why it authenticates as the migration role rather than a runtime
	// one — and why FORCE ROW LEVEL SECURITY matters, since the migration role owns the
	// tables.
	pool, err := db.Open(ctx, db.Config{
		Name:     "organization-migration",
		DSN:      dsn,
		MaxConns: 2,
	})
	if err != nil {
		return fmt.Errorf("open migration pool: %w", err)
	}
	defer pool.Close()

	switch stage {
	case stagePre:
		return applyStage(ctx, pool, controldb.StageRoles, logger)
	case stageMaintenance:
		return runMaintenance(ctx, pool, maintenance, logger)
	default:
		// The platform schema first: grants.sql names its tables, and the RLS stage runs
		// between them so a window where privileges exist without policies never opens.
		if err := applyPlatform(ctx, pool, logger); err != nil {
			return err
		}
		for _, post := range controldb.PostStages {
			if err := applyStage(ctx, pool, post, logger); err != nil {
				return err
			}
		}
		return verifyIsolation(ctx, pool, logger)
	}
}

// verifyIsolation is the post-condition of the post stage.
//
// Applying SQL and reporting success are different claims. rls.sql can run without error and
// still leave the posture wrong — a table added to a tenant-scoped schema outside this pipeline,
// a role that a restored dump brought in with BYPASSRLS, a policy count that is one instead of
// two. Every one of those is a deploy that reports green and ships an inert control.
//
// Asserted here rather than only in CI because CI asserts a throwaway database. This runs against
// the database the deploy just changed.
func verifyIsolation(ctx context.Context, pool *db.Pool, logger *slog.Logger) error {
	report, err := controldb.AssertIsolation(ctx, pool)
	if err != nil {
		return fmt.Errorf("verify tenant isolation: %w", err)
	}
	if !report.OK() {
		for _, problem := range report.Problems {
			logger.Error("isolation posture", slog.String("problem", problem))
		}
		return report.Err()
	}
	logger.Info("tenant isolation verified",
		slog.Int("protected_tables", len(report.Tables)),
		slog.Any("schemas", controldb.RLSSchemas))
	return nil
}

// applyStage runs one embedded statement file in a single transaction.
func applyStage(ctx context.Context, pool *db.Pool, stage controldb.Stage, logger *slog.Logger) error {
	body, err := controldb.SQL(stage)
	if err != nil {
		return err
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, execErr := tx.Exec(ctx, body)
		return execErr
	}); err != nil {
		return fmt.Errorf("apply %s: %w", stage, err)
	}
	logger.Info("applied", slog.String("source", string(stage)))
	return nil
}

// applyPlatform applies every migration foundation-platform ships, in name order, inside one
// transaction.
//
// One transaction for the whole set rather than one each: a partially applied platform schema
// is a database the dispatcher starts against and fails on at the first claim. Every shipped
// statement is idempotent — asserted upstream by TestEveryPlatformMigrationIsIdempotent, added
// after v0.2.0 shipped a set that could only be applied once — so a retry after a rollback is
// safe.
func applyPlatform(ctx context.Context, pool *db.Pool, logger *slog.Logger) error {
	set, err := migrations.PlatformMigrations()
	if err != nil {
		return fmt.Errorf("load platform migrations: %w", err)
	}
	if len(set) == 0 {
		return errors.New("foundation-platform shipped no migrations; the dependency is broken")
	}

	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		for _, migration := range set {
			if _, execErr := tx.Exec(ctx, migration.SQL); execErr != nil {
				return fmt.Errorf("%s: %w", migration.Name, execErr)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("apply platform schema: %w", err)
	}

	for _, migration := range set {
		logger.Info("applied",
			slog.String("source", "foundation-platform"),
			slog.String("migration", migration.Name))
	}
	return nil
}

// runMaintenance runs the scheduled stage and logs what it did.
func runMaintenance(ctx context.Context, pool *db.Pool, cfg controldb.MaintenanceConfig, logger *slog.Logger) error {
	report, err := controldb.RunMaintenance(ctx, pool, cfg)
	if err != nil {
		return err
	}
	logger.Info("maintenance",
		slog.Time("observed_at", report.ObservedAt),
		slog.Int("partitions_ensured", len(report.PartitionsEnsured)),
		slog.Any("partitions_dropped", report.PartitionsDropped),
		slog.Int64("dead_letters_disposed", report.DeadLettersDisposed),
		slog.Int64("receipts_pruned", report.ReceiptsPruned),
		slog.Int64("stale_unresolved", report.StaleUnresolved))
	if report.StaleUnresolved > 0 {
		return fmt.Errorf("%w: %d older than %s", errStale, report.StaleUnresolved, cfg.StaleAlert)
	}
	return nil
}

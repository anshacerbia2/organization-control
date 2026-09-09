// Package projection publishes the Organization projection and reconciles it against authority.
//
// The projection carries context and never authorization: Tenant identity, Workspace identity,
// Membership status, and the two versions. It carries no Product permission, no Entitlement, and
// no business role. A projection that grows to carry permissions has recreated the
// token-as-permission-snapshot pattern EAD-006 rejects and STD-IAM-001 §3.3 prohibits, and it
// would do so without any single change looking wrong.
//
// Repair runs in one direction. A projection is never promoted into authority — not on
// reconciliation, not on a consumer's report, not during an incident.
package projection

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// StaleBehavior is what a consumer does once its projection is older than its declared budget.
//
// Declared per consumer rather than configured globally, because a work queue and a financial
// approval path do not share a freshness requirement, and a single global value would be set to
// whichever of the two complains first.
type StaleBehavior string

const (
	// StaleUseWithMarker serves the local model and exposes a staleness indicator to the caller.
	StaleUseWithMarker StaleBehavior = "use_with_marker"

	// StaleRevalidate calls the authoritative fresh check for that one decision.
	StaleRevalidate StaleBehavior = "revalidate"

	// StaleFailClosed denies. Token issuance must declare this: minting a token from a projection
	// of unknown age creates authority that outlives the uncertainty, and no downstream control
	// can withdraw it.
	StaleFailClosed StaleBehavior = "fail_closed"
)

// Valid reports whether the behavior is one this registry persists. The set mirrors
// `stale_behavior_check` in schema.hcl.
func (s StaleBehavior) Valid() bool {
	switch s {
	case StaleUseWithMarker, StaleRevalidate, StaleFailClosed:
		return true
	}
	return false
}

var (
	// ErrInvalid is a malformed request: a required field absent, a value outside its permitted
	// set, or two fields that contradict each other.
	//
	// It exists so the HTTP surface can answer 400. Before it, every validation failure here was
	// a bare errors.New, indistinguishable at the transport boundary from a failed statement --
	// so a caller who omitted a field received 500, which says the service is broken rather than
	// that the request is. Constructor guards and stored-value decoders deliberately do NOT carry
	// it: those are a process built wrong and a row that should not exist, and both are 500.
	ErrInvalid = errors.New("projection: the request is invalid")

	// ErrNotRegistered means the consumer has no registry row. A consumer that has not registered
	// receives no projection: without a declared freshness budget and stale behavior, nothing can
	// state what its copy of the projection is allowed to be used for.
	ErrNotRegistered = errors.New("projection: consumer is not registered")

	// ErrNoSnapshotMark means a progress report arrived from a consumer that never took a
	// snapshot. Reading the stream alone yields a model missing everything that happened before
	// the subscription, and a position accepted for such a consumer would make an incomplete
	// model look like a current one.
	ErrNoSnapshotMark = errors.New("projection: progress reported before a snapshot was taken")

	// ErrMarkWentBackwards means a report claimed a position below one already accepted. The
	// stream position is monotonic per publisher, so a lower value is either a replay being
	// misreported as progress or two processes sharing one consumer identity.
	ErrMarkWentBackwards = errors.New("projection: reported position is below the accepted one")

	// ErrSingleConsumer refuses a second active projection consumer.
	//
	// The distributed enforcement work is scoped, deliberately, to one producer and one
	// projection consumer. Several things that are correct at that scope are silently wrong
	// beyond it, and none of them announce themselves:
	//
	//   - platform.dead_letter has one row per event_id and platform.outbox one `published`
	//     flag, so an event owed to two consumers cannot record two outcomes;
	//   - dead-letter debt is therefore reported estate-wide, so one consumer's poison event
	//     refuses traffic for every other consumer;
	//   - resolution evidence is attributed to an event rather than to a delivery, so evidence
	//     produced for one consumer would resolve an incident belonging to another.
	//
	// This refusal is that scope made load-bearing rather than documented. A limitation written
	// in a design note does not survive the day someone registers a second consumer and nothing
	// goes red; a refusal that names its reason is read by the person who needs to lift it, and
	// lifting it becomes a decision instead of an accident.
	//
	// The exit is Retire: retire the consumer that is finishing, then register its replacement.
	// An invariant with no legitimate way past it gets removed rather than respected, and a
	// removed guard leaves no trace at all.
	ErrSingleConsumer = errors.New("projection: another projection consumer is already active")
)

// Consumer is one row of projection.consumer.
type Consumer struct {
	ConsumerID        string
	ProjectionVersion string
	MaxAcceptedAge    time.Duration
	StaleBehavior     StaleBehavior
	RegisteredAt      time.Time

	// SnapshotMark is the high-water mark of the snapshot this consumer bootstrapped from. Nil
	// until a snapshot has been taken, which is exactly the condition that refuses a progress
	// report.
	SnapshotMark *int64

	// LastReportedMark and LastReportedAt are what the consumer said about itself. They are a
	// report and not an authority: the publisher measures freshness against them and never infers
	// a stream position from them.
	LastReportedMark *int64
	LastReportedAt   *time.Time
}

// Registration is a consumer declaring what it needs.
type Registration struct {
	ConsumerID        string
	ProjectionVersion string
	MaxAcceptedAge    time.Duration
	StaleBehavior     StaleBehavior
}

func (r Registration) validate() error {
	switch {
	case strings.TrimSpace(r.ConsumerID) == "":
		return fmt.Errorf("%w: a consumer identifier is required", ErrInvalid)
	case strings.TrimSpace(r.ProjectionVersion) == "":
		// The projection is a contract, and a consumer that cannot name the version it reads
		// cannot be told that the contract changed under it.
		return fmt.Errorf("%w: a projection version is required", ErrInvalid)
	case r.MaxAcceptedAge <= 0:
		// Zero would read as "no staleness is acceptable" and behave as "no budget is declared".
		// The two are opposite, so neither is inferred from an absent value.
		return fmt.Errorf("%w: a positive max_accepted_age is required", ErrInvalid)
	case !r.StaleBehavior.Valid():
		return fmt.Errorf("%w: stale_behavior %q is not a declared behavior", ErrInvalid, r.StaleBehavior)
	}
	return nil
}

// Registry is the consumer registry. It is owned by the publisher and held in this database; each
// consumer's own stream position lives in that consumer's database.
type Registry struct {
	pool *db.ProviderPool
	now  func() time.Time
}

// NewRegistry constructs the registry.
//
// Provider-scoped: `projection.consumer` carries no tenant column at all, so it is protected by
// grant rather than by policy, and `organization_rt` holds nothing on it.
func NewRegistry(pool *db.ProviderPool) (*Registry, error) {
	if pool == nil {
		return nil, errors.New("projection: a provider-scoped pool is required")
	}
	return &Registry{pool: pool, now: time.Now}, nil
}

// upsertStatement registers a consumer or updates its declared terms.
//
// The declared terms are replaced and the progress columns are not. A consumer raising its
// freshness budget has not un-bootstrapped itself, and clearing `snapshot_mark` here would refuse
// its next progress report for a reason unrelated to what it changed.
// upsertStatement registers a consumer or updates its declared terms.
//
// `retired_at = NULL` on conflict revives a retired consumer rather than refusing it. A
// consumer coming back under an identity it previously held is a legitimate act, and the
// single-consumer rule is about how many are active at once rather than about which names have
// ever been used.
const upsertStatement = `INSERT INTO projection.consumer
    (consumer_id, projection_version, max_accepted_age, stale_behavior)
VALUES ($1, $2, $3, $4)
ON CONFLICT (consumer_id) DO UPDATE
SET projection_version = excluded.projection_version,
    max_accepted_age   = excluded.max_accepted_age,
    stale_behavior     = excluded.stale_behavior,
    retired_at         = NULL
RETURNING registered_at`

// otherActiveConsumer names an active consumer that is not this one, if there is one.
//
// FOR UPDATE, so two registrations arriving together cannot both read zero and both insert.
// The database's partial unique index is the real guarantee -- it holds against psql as well
// as against this code -- and this read exists to turn that constraint violation into an
// answer that says which consumer is in the way.
const otherActiveConsumer = `SELECT consumer_id
FROM projection.consumer
WHERE retired_at IS NULL AND consumer_id <> $1
FOR UPDATE`

// activeHolder reports the active consumer other than the named one, and whether there is one.
func activeHolder(ctx context.Context, tx db.Tx, consumerID string) (string, bool, error) {
	rows, err := tx.Query(ctx, otherActiveConsumer, consumerID)
	if err != nil {
		return "", false, fmt.Errorf("projection: checking for an active consumer: %w", err)
	}
	defer rows.Close()

	var holder string
	held := rows.Next()
	if held {
		if err := rows.Scan(&holder); err != nil {
			return "", false, fmt.Errorf("projection: reading the active consumer: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("projection: checking for an active consumer: %w", err)
	}
	return holder, held, nil
}

// Register records or updates a consumer's declared terms.
//
// Refused when a different consumer is already active. See ErrSingleConsumer for why the
// scope is enforced rather than documented.
func (r *Registry) Register(ctx context.Context, reg Registration) (Consumer, error) {
	if err := reg.validate(); err != nil {
		return Consumer{}, err
	}

	consumer := Consumer{
		ConsumerID:        reg.ConsumerID,
		ProjectionVersion: reg.ProjectionVersion,
		MaxAcceptedAge:    reg.MaxAcceptedAge,
		StaleBehavior:     reg.StaleBehavior,
	}

	if err := db.WithProviderScope(ctx, r.pool,
		"register projection consumer "+reg.ConsumerID,
		func(ctx context.Context, tx db.Tx) error {
			// Iterated rather than QueryRow'd: no row is the expected case here, and this
			// package has no no-rows sentinel to distinguish that from a failed statement.
			// Treating them alike would report a broken database as a free slot.
			holder, held, err := activeHolder(ctx, tx, reg.ConsumerID)
			if err != nil {
				return err
			}
			if held {
				return fmt.Errorf("%w: %s holds it; retire that consumer first", ErrSingleConsumer, holder)
			}

			return tx.QueryRow(ctx, upsertStatement,
				reg.ConsumerID, reg.ProjectionVersion, reg.MaxAcceptedAge, string(reg.StaleBehavior),
			).Scan(&consumer.RegisteredAt)
		}); err != nil {
		if errors.Is(err, ErrSingleConsumer) {
			return Consumer{}, err
		}
		return Consumer{}, fmt.Errorf("projection: register consumer: %w", err)
	}
	return consumer, nil
}

const retireStatement = `UPDATE projection.consumer
SET retired_at = now()
WHERE consumer_id = $1 AND retired_at IS NULL`

// Retire withdraws a consumer, freeing the single active slot.
//
// It exists because ErrSingleConsumer would otherwise have no legitimate exit: rotating a
// consumer identity, renaming a deployable, or moving the projection to a new one would all
// dead-end at the refusal, and the fastest way past a refusal with no exit is to delete the
// guard. This is the intended path, and it leaves a row rather than removing one.
//
// The row is kept and stamped rather than deleted. `snapshot_mark` and the reported positions
// are the record of what that consumer was told and what it claimed to have applied, and an
// investigation into a stale enforcement decision needs them after the consumer is gone.
//
// Retiring an already-retired consumer is not an error: the caller asked for a state that
// holds. Retiring one that was never registered is, because that request names nothing.
func (r *Registry) Retire(ctx context.Context, consumerID string) error {
	if strings.TrimSpace(consumerID) == "" {
		return fmt.Errorf("%w: a consumer identifier is required", ErrInvalid)
	}

	return db.WithProviderScope(ctx, r.pool, "retire projection consumer "+consumerID,
		func(ctx context.Context, tx db.Tx) error {
			tag, err := tx.Exec(ctx, retireStatement, consumerID)
			if err != nil {
				return fmt.Errorf("projection: retire consumer: %w", err)
			}
			if tag.RowsAffected() == 1 {
				return nil
			}

			// Nothing was updated: either the consumer is already retired, or it does not
			// exist. Distinguished with a second read rather than reported as one outcome,
			// because an operator retiring a name they mistyped must not be told it worked.
			var exists bool
			if err := tx.QueryRow(ctx,
				"SELECT true FROM projection.consumer WHERE consumer_id = $1", consumerID,
			).Scan(&exists); err != nil {
				return fmt.Errorf("%w: %s", ErrNotRegistered, consumerID)
			}
			return nil
		})
}

const selectConsumer = `SELECT consumer_id,
       projection_version,
       max_accepted_age,
       stale_behavior,
       registered_at,
       snapshot_mark,
       last_reported_mark,
       last_reported_at
FROM projection.consumer
WHERE consumer_id = $1 AND retired_at IS NULL`

// Get reads one consumer, or reports that it is not registered.
//
// A retired consumer reads as not registered, and that is the point rather than a side
// effect: every runtime path -- snapshot, progress, frontier -- goes through this read, so
// retiring a consumer withdraws its access in one place instead of in each of them. The row
// survives for investigation; the authority does not.
func (r *Registry) Get(ctx context.Context, consumerID string) (Consumer, error) {
	var consumer Consumer
	if err := db.WithProviderScope(ctx, r.pool, "read projection consumer "+consumerID,
		func(ctx context.Context, tx db.Tx) error {
			return load(ctx, tx, consumerID, &consumer)
		}); err != nil {
		return Consumer{}, err
	}
	return consumer, nil
}

func load(ctx context.Context, tx db.Tx, consumerID string, consumer *Consumer) error {
	var behavior string
	if err := tx.QueryRow(ctx, selectConsumer, consumerID).Scan(
		&consumer.ConsumerID, &consumer.ProjectionVersion, &consumer.MaxAcceptedAge,
		&behavior, &consumer.RegisteredAt, &consumer.SnapshotMark,
		&consumer.LastReportedMark, &consumer.LastReportedAt); err != nil {
		return fmt.Errorf("%w: %s", ErrNotRegistered, consumerID)
	}
	consumer.StaleBehavior = StaleBehavior(behavior)
	if !consumer.StaleBehavior.Valid() {
		return fmt.Errorf("projection: stored stale_behavior %q is not a declared behavior", behavior)
	}
	return nil
}

const recordSnapshotMark = `UPDATE projection.consumer
SET snapshot_mark = $2
WHERE consumer_id = $1`

const recordProgress = `UPDATE projection.consumer
SET last_reported_mark = $2,
    last_reported_at   = $3
WHERE consumer_id = $1`

// Progress is a consumer reporting what it has applied.
type Progress struct {
	ConsumerID string

	// AppliedMark is the highest stream position the consumer has applied. Gaps below it are
	// expected: the outbox sequence is allocated before commit, so a rolled-back transaction
	// consumes a value that no event will ever carry.
	AppliedMark int64
}

// RecordProgress accepts a consumer's report of its own position.
//
// Refused when the consumer never took a snapshot. That refusal is the whole point of the bootstrap
// contract: a consumer that subscribed and started applying without a snapshot holds a model
// containing everything that happened since it connected and nothing that happened before, and
// accepting a position for it would record that model as current.
func (r *Registry) RecordProgress(ctx context.Context, report Progress) (Consumer, error) {
	if strings.TrimSpace(report.ConsumerID) == "" {
		return Consumer{}, fmt.Errorf("%w: a consumer identifier is required", ErrInvalid)
	}

	var consumer Consumer
	at := r.now().UTC()

	if err := db.WithProviderScope(ctx, r.pool,
		"record projection progress for "+report.ConsumerID,
		func(ctx context.Context, tx db.Tx) error {
			if err := load(ctx, tx, report.ConsumerID, &consumer); err != nil {
				return err
			}
			if consumer.SnapshotMark == nil {
				return fmt.Errorf("%w: %s", ErrNoSnapshotMark, report.ConsumerID)
			}
			if consumer.LastReportedMark != nil && report.AppliedMark < *consumer.LastReportedMark {
				return fmt.Errorf("%w: reported %d, accepted %d",
					ErrMarkWentBackwards, report.AppliedMark, *consumer.LastReportedMark)
			}
			if _, err := tx.Exec(ctx, recordProgress, report.ConsumerID, report.AppliedMark, at); err != nil {
				return fmt.Errorf("projection: record progress: %w", err)
			}
			return nil
		}); err != nil {
		return Consumer{}, err
	}

	mark, reported := report.AppliedMark, at
	consumer.LastReportedMark, consumer.LastReportedAt = &mark, &reported
	return consumer, nil
}

// Age reports how long ago the consumer last reported, and whether that exceeds its declared
// budget. A consumer that has never reported is stale by definition rather than by measurement:
// nothing is known about its copy.
func (c Consumer) Age(now time.Time) (time.Duration, bool) {
	if c.LastReportedAt == nil {
		return 0, true
	}
	age := now.Sub(*c.LastReportedAt)
	return age, age > c.MaxAcceptedAge
}

package projection

// The publication frontier: what a consumer needs from this side to know whether its own model has
// caught up, and which it cannot derive for itself.
//
// A consumer knows the highest stream position it has applied. It cannot tell a gap below that
// position apart from a value no event will ever carry: `platform.outbox` takes its sequence from a
// sequence generator before the transaction commits, so a rolled-back transaction consumes a number
// permanently. Waiting for such a number to arrive would wait forever, and ignoring gaps is the same
// as reading only the highest applied position — which reports a consumer as current while an older
// event is still in flight.
//
// Only this side can tell the two apart, because a rolled-back row was never in the outbox at all.
// This reader therefore reports facts and no verdict: what is committed here, what is still owed, and
// what this side has given up on. Whether that is fresh enough is the consumer's policy decision, per
// operation, and it is not this service's to make.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// AuthorityEventTypes are the published events a Membership projection's authority depends on.
//
// The list exists because a dead-lettered delivery is not the same fact for every event type. A
// dead-lettered Workspace rename is an operational annoyance; a dead-lettered Membership revocation is
// a withdrawal that will never reach the consumer by itself, and a consumer that cannot see the
// difference must either ignore all of them or refuse on all of them.
//
// It mirrors internal/membership's action-to-event-type table, which this package may not import:
// arch.json gives internal/projection edges to internal/db and internal/system only, because a
// read-only publisher with a path into the Membership state machine is a mutation path behind a
// snapshot. TestTheFrontierDebtCoversEveryMembershipAuthorityEvent in internal/httpapi — which
// already imports both — keeps the copy honest.
var AuthorityEventTypes = []string{
	"com.scnehaux.organization.membership.lifecycle.granted",
	"com.scnehaux.organization.membership.lifecycle.restored",
	"com.scnehaux.organization.membership.security.suspended",
	"com.scnehaux.organization.membership.security.revoked",
}

// Frontier is the answer, as facts.
type Frontier struct {
	// HighestCommittedMark is the largest stream position visible here. Uncommitted rows are
	// invisible by definition, so this is a committed position rather than an allocated one.
	HighestCommittedMark int64

	// OldestUnpublishedMark and OldestUnpublishedAge describe the oldest delivery still owed.
	//
	// They may name different rows, and that is not a defect: the mark is the smallest allocated
	// sequence still unpublished, while the age is measured from the earliest creation among
	// unpublished rows — and allocation order is not commit order. A consumer comparing its own
	// applied position against the mark, and its staleness budget against the age, is using each for
	// what it can answer.
	OldestUnpublishedMark int64
	OldestUnpublishedAge  time.Duration

	// Unpublished is false when nothing is owed, in which case the two fields above are zero. A
	// caller reading zero without this flag could not tell "nothing pending" from "pending since the
	// epoch".
	Unpublished bool

	// SecurityDeadLettered and OldestSecurityDeadLetterAge describe deliveries this side has stopped
	// attempting: authority-bearing events whose platform.dead_letter row is still unresolved.
	//
	// They are separate facts rather than part of the unpublished pool, because they are a different
	// fact. `published = FALSE` excludes a dead-lettered row deliberately — see the statement below —
	// so without these fields a poison Membership revocation leaves the frontier reporting that this
	// side owes nothing, while the consumer holds an active row the revocation was supposed to
	// withdraw. Every local signal reads fresh and the enforcement answer is wrong for as long as
	// nobody looks at the table.
	//
	// Reported separately from the owed pool for a second reason: the two are not the same kind of
	// wait. An unpublished row will be delivered, so age against a budget is meaningful. A
	// dead-lettered row will not be delivered by anything except an operator, so no budget makes it
	// acceptable — which is a consumer's judgement to make, and it can only make it if the two
	// arrive apart.
	SecurityDeadLettered        int64
	OldestSecurityDeadLetterAge time.Duration

	// SecurityDebt is false when nothing authority-bearing is unresolved, for the same reason
	// Unpublished exists.
	SecurityDebt bool

	// ObservedAt is the instant this was read, so a consumer holding the answer can age it rather
	// than treating a cached frontier as current.
	//
	// Read from the database, not from this process: see the statement below.
	ObservedAt time.Time
}

// FrontierReader reads the outbox's publication state.
//
// It holds the raw transactor rather than a scoped pool, for the reason ClaimStore does: the tables
// it reads carry no `tenant_id` and no Row-Level Security policy, so there is no binding for the read
// to need. Routing it through `WithProviderScope` would write a privileged-access record for every
// poll, filling the evidence table an investigation reads with rows about nothing — and consumers are
// expected to poll this.
type FrontierReader struct {
	tx     db.Transactor
	events []string
}

func NewFrontierReader(tx db.Transactor) (*FrontierReader, error) {
	if tx == nil {
		return nil, errors.New("projection: a transactor is required")
	}
	return &FrontierReader{tx: tx, events: AuthorityEventTypes}, nil
}

// One statement, so every fact describes one instant on one clock.
//
// Read separately, a publication committing between two reads would produce a highest mark that
// includes a row the oldest-unpublished read still reports as owed — a frontier contradicting itself,
// which is precisely the confusion it exists to remove.
//
// # Why the instant comes from the database
//
// `observed_at` and every age here are computed from `clock_timestamp()` inside this statement, and
// this reader keeps no clock of its own. The earlier version stamped `observed_at` from the Go
// process and subtracted `created_at` from it — a subtraction across two clocks, and the failure was
// directional rather than noisy: an application clock behind the database's shrinks the age and
// understates the backlog, and one far enough ahead of it makes the age grow while the backlog
// stands still. Both are the same defect the frontier exists to remove, arriving through arithmetic.
//
// `clock_timestamp()` rather than `now()`: `now()` is the transaction's start, so a reader inside a
// transaction that began earlier would report ages measured from an instant already past. A single
// wall-clock reading, taken once in a CTE and reused, keeps `observed_at` and the two ages consistent
// with each other rather than microseconds apart.
//
// A negative age is unreachable by construction and therefore not clamped: a row is visible to this
// statement only if it committed before the snapshot, and `created_at` precedes its own commit, so
// `clock_timestamp()` is after both.
//
// # Why the owed pool excludes dead-lettered rows
//
// `published = FALSE` rather than `published_at IS NULL`: a dead-lettered row is marked published with
// its error recorded, so it leaves the unpublished pool. Otherwise one poison event would make the
// oldest-unpublished age grow forever and every consumer would read itself as permanently stale.
//
// That is the right treatment of the pool and the wrong end of the story on its own, so the debt those
// rows represent is reported beside it rather than folded into it.
const frontierStatement = `WITH observed AS (
    SELECT clock_timestamp() AS at
), committed AS (
    SELECT max(sequence) AS mark FROM platform.outbox
), owed AS (
    SELECT min(sequence) AS mark, min(created_at) AS oldest
      FROM platform.outbox
     WHERE published = FALSE
), debt AS (
    SELECT count(*) AS rows, min(dead_lettered_at) AS oldest
      FROM platform.dead_letter
     WHERE resolved_at IS NULL
       AND event_type = ANY ($1::text[])
)
SELECT observed.at,
       coalesce(committed.mark, 0),
       coalesce(owed.mark, 0),
       extract(epoch FROM observed.at - owed.oldest)::double precision,
       debt.rows,
       extract(epoch FROM observed.at - debt.oldest)::double precision
  FROM observed, committed, owed, debt`

func (f *FrontierReader) Frontier(ctx context.Context) (Frontier, error) {
	var frontier Frontier

	err := f.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// Nullable because both aggregates are over a possibly empty set. A pointer rather than a
		// zero, so "owes nothing" stays distinguishable from "owes something created at the epoch".
		var owedSeconds, debtSeconds *float64

		if err := tx.QueryRow(ctx, frontierStatement, f.events).Scan(
			&frontier.ObservedAt,
			&frontier.HighestCommittedMark,
			&frontier.OldestUnpublishedMark,
			&owedSeconds,
			&frontier.SecurityDeadLettered,
			&debtSeconds); err != nil {
			return fmt.Errorf("projection: reading the publication frontier: %w", err)
		}

		frontier.ObservedAt = frontier.ObservedAt.UTC()
		if owedSeconds != nil {
			frontier.Unpublished = true
			frontier.OldestUnpublishedAge = seconds(*owedSeconds)
		}
		if debtSeconds != nil {
			frontier.SecurityDebt = true
			frontier.OldestSecurityDeadLetterAge = seconds(*debtSeconds)
		}
		return nil
	})
	if err != nil {
		return Frontier{}, err
	}
	return frontier, nil
}

func seconds(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}

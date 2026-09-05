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
// This reader therefore reports facts and no verdict: the highest position committed here, the oldest
// position still unpublished, and how old it is. Whether that is fresh enough is the consumer's
// policy decision, per operation, and it is not this service's to make.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/organization-control/internal/db"
)

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

	// ObservedAt is the instant this was read, so a consumer holding the answer can age it rather
	// than treating a cached frontier as current.
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
	tx  db.Transactor
	now func() time.Time
}

func NewFrontierReader(tx db.Transactor) (*FrontierReader, error) {
	if tx == nil {
		return nil, errors.New("projection: a transactor is required")
	}
	return &FrontierReader{tx: tx, now: time.Now}, nil
}

// One statement, so the three facts describe one instant. Read separately, a publication committing
// between them would produce a highest mark that includes a row the oldest-unpublished read still
// reports as owed — a frontier contradicting itself, which is precisely the confusion it exists to
// remove.
//
// `published = FALSE` rather than `published_at IS NULL`: a dead-lettered row is marked published with
// its error recorded, so it leaves the unpublished pool. Otherwise one poison event would make the
// oldest-unpublished age grow forever and every consumer would read itself as permanently stale.
const frontierStatement = `SELECT
    coalesce((SELECT max(sequence) FROM platform.outbox), 0),
    coalesce((SELECT min(sequence)   FROM platform.outbox WHERE published = FALSE), 0),
    (SELECT min(created_at)          FROM platform.outbox WHERE published = FALSE)`

func (f *FrontierReader) Frontier(ctx context.Context) (Frontier, error) {
	frontier := Frontier{ObservedAt: f.now().UTC()}

	err := f.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var oldestCreatedAt *time.Time
		if err := tx.QueryRow(ctx, frontierStatement).Scan(
			&frontier.HighestCommittedMark,
			&frontier.OldestUnpublishedMark,
			&oldestCreatedAt); err != nil {
			return fmt.Errorf("projection: reading the publication frontier: %w", err)
		}

		if oldestCreatedAt != nil {
			frontier.Unpublished = true
			frontier.OldestUnpublishedAge = frontier.ObservedAt.Sub(oldestCreatedAt.UTC())
			if frontier.OldestUnpublishedAge < 0 {
				// The row was created after this reader took its clock reading. Reported as zero
				// rather than negative: a negative age is not a fact about anything, and a consumer
				// comparing it against a budget would read it as unboundedly fresh.
				frontier.OldestUnpublishedAge = 0
			}
		}
		return nil
	})
	if err != nil {
		return Frontier{}, err
	}
	return frontier, nil
}

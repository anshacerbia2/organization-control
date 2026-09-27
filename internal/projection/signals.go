package projection

import (
	"context"
	"errors"
	"fmt"

	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// Signals are the enforcement facts an operator is alerted on, read from the database on each
// metric collection. SAD-004 §9.3.2 and TDD-foundation-platform-001 §Operational Notes set the
// thresholds; this reads the values.
type Signals struct {
	// Lanes are the unpublished outbox rows per priority lane: the dispatcher's lag.
	Lanes []LaneSignal

	// SecurityDebt counts unresolved authority-bearing dead letters, estate-wide, and the age of
	// the oldest. While it is above zero every projection-backed check refuses.
	SecurityDebt          int64
	SecurityDebtOldestAge float64

	// Stale counts unresolved dead letters of any type with no live waiver, and the age of the
	// oldest. A waiver silences the stale alert until it expires (TDD-organization-control-005).
	Stale          int64
	StaleOldestAge float64

	// Consumers are the active registered consumers.
	Consumers []ConsumerSignal
}

// LaneSignal is one outbox lane.
type LaneSignal struct {
	Lane      string // "priority" or "standard"
	Count     int64
	OldestAge float64 // seconds; zero when the lane is empty
}

// ConsumerSignal is one active consumer's freshness, as this side sees it.
type ConsumerSignal struct {
	Consumer string

	// ReportAge is the seconds since its last progress report, or since registration when it has
	// never reported: a consumer that has told us nothing is stale by definition, not by measure.
	ReportAge float64

	// MaxAcceptedAge is its declared budget, so an alert compares the two without a threshold
	// copied out of the registry into a rule file.
	MaxAcceptedAge float64

	// VerifyRatio is its last measured fresh-check ratio, when one has been measured.
	VerifyRatio *float64
}

// SignalsReader reads Signals on the raw provider connections, as FrontierReader does: these tables
// carry no tenant column, and a scoped read would file a privileged-access record every collection.
type SignalsReader struct {
	tx     db.Transactor
	events []string
}

// NewSignalsReader constructs the reader.
func NewSignalsReader(tx db.Transactor) (*SignalsReader, error) {
	if tx == nil {
		return nil, errors.New("projection: a transactor is required")
	}
	return &SignalsReader{tx: tx, events: AuthorityEventTypes}, nil
}

// estateSignals reads every aggregate on one clock, for the reason frontierStatement does.
const estateSignals = `WITH observed AS (
    SELECT clock_timestamp() AS at
), lanes AS (
    SELECT count(*) FILTER (WHERE priority = $2)                 AS priority_rows,
           min(created_at) FILTER (WHERE priority = $2)          AS priority_oldest,
           count(*) FILTER (WHERE priority <> $2)                  AS standard_rows,
           min(created_at) FILTER (WHERE priority <> $2)           AS standard_oldest
      FROM platform.outbox
     WHERE published = FALSE
), debt AS (
    SELECT count(*) AS rows, min(dead_lettered_at) AS oldest
      FROM platform.dead_letter
     WHERE resolved_at IS NULL
       AND event_type = ANY ($1::text[])
), stale AS (
    SELECT count(*) AS rows, min(dead_lettered_at) AS oldest
      FROM platform.dead_letter
     WHERE resolved_at IS NULL
       AND (waived_until IS NULL OR waived_until <= clock_timestamp())
)
SELECT lanes.priority_rows,
       coalesce(extract(epoch FROM observed.at - lanes.priority_oldest), 0)::double precision,
       lanes.standard_rows,
       coalesce(extract(epoch FROM observed.at - lanes.standard_oldest), 0)::double precision,
       debt.rows,
       coalesce(extract(epoch FROM observed.at - debt.oldest), 0)::double precision,
       stale.rows,
       coalesce(extract(epoch FROM observed.at - stale.oldest), 0)::double precision
  FROM observed, lanes, debt, stale`

const consumerSignals = `SELECT consumer_id,
       extract(epoch FROM clock_timestamp() - coalesce(last_reported_at, registered_at))::double precision,
       extract(epoch FROM max_accepted_age)::double precision,
       last_verify_ratio
  FROM projection.consumer
 WHERE retired_at IS NULL
 ORDER BY consumer_id`

// Read returns the current signals.
func (r *SignalsReader) Read(ctx context.Context) (Signals, error) {
	var s Signals
	err := r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		priority, standard := LaneSignal{Lane: "priority"}, LaneSignal{Lane: "standard"}
		if err := tx.QueryRow(ctx, estateSignals, r.events, outbox.PriorityHigh).Scan(
			&priority.Count, &priority.OldestAge, &standard.Count, &standard.OldestAge,
			&s.SecurityDebt, &s.SecurityDebtOldestAge, &s.Stale, &s.StaleOldestAge); err != nil {
			return fmt.Errorf("projection: reading estate signals: %w", err)
		}
		s.Lanes = []LaneSignal{priority, standard}

		rows, err := tx.Query(ctx, consumerSignals)
		if err != nil {
			return fmt.Errorf("projection: reading consumer signals: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c ConsumerSignal
			if err := rows.Scan(&c.Consumer, &c.ReportAge, &c.MaxAcceptedAge, &c.VerifyRatio); err != nil {
				return fmt.Errorf("projection: reading a consumer's signals: %w", err)
			}
			s.Consumers = append(s.Consumers, c)
		}
		return rows.Err()
	})
	if err != nil {
		return Signals{}, err
	}
	return s, nil
}

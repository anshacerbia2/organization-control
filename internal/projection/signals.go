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
//
// Lag and debt are per active consumer (ADR-GLB-018 §6): each consumer has its own deliveries and its
// own dead letters, and an estate total would page for one consumer's outage as though every
// consumer were refusing.
type Signals struct {
	// Lanes are each active consumer's owed deliveries per priority lane: its dispatcher's lag.
	Lanes []LaneSignal

	// Debt is each active consumer's unresolved authority-bearing dead letters -- its own, plus
	// every one that names no consumer -- and the age of the oldest. While it is above zero that
	// consumer's projection-backed checks refuse.
	Debt []DebtSignal

	// Stale counts unresolved dead letters of any type and any consumer with no live waiver, and
	// the age of the oldest. A waiver silences the stale alert until it expires
	// (TDD-organization-control-005). Estate-wide, so a retired consumer's incident still alerts
	// until it is waived.
	Stale          int64
	StaleOldestAge float64

	// Consumers are the active registered consumers.
	Consumers []ConsumerSignal
}

// LaneSignal is one lane of one consumer's deliveries.
type LaneSignal struct {
	Consumer  string
	Lane      string // "priority" or "standard"
	Count     int64
	OldestAge float64 // seconds; zero when the lane is empty
}

// DebtSignal is one consumer's security debt.
type DebtSignal struct {
	Consumer  string
	Count     int64
	OldestAge float64 // seconds; zero when there is none
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

// staleSignals reads the estate's unwaived dead letters.
const staleSignals = `SELECT count(*),
       coalesce(extract(epoch FROM clock_timestamp() - min(dead_lettered_at)), 0)::double precision
  FROM platform.dead_letter
 WHERE resolved_at IS NULL
   AND (waived_until IS NULL OR waived_until <= clock_timestamp())`

// laneSignals reads every active consumer's owed deliveries per lane, on one clock. An active
// consumer owed nothing still reports zeros, so its series never goes absent while it is healthy.
const laneSignals = `WITH observed AS (
    SELECT clock_timestamp() AS at
)
SELECT c.consumer_id,
       count(d.event_id) FILTER (WHERE d.priority = $1),
       coalesce(extract(epoch FROM observed.at - min(d.created_at) FILTER (WHERE d.priority = $1)), 0)::double precision,
       count(d.event_id) FILTER (WHERE d.priority <> $1),
       coalesce(extract(epoch FROM observed.at - min(d.created_at) FILTER (WHERE d.priority <> $1)), 0)::double precision
  FROM observed
 CROSS JOIN projection.consumer c
  LEFT JOIN platform.outbox_delivery d ON d.consumer = c.consumer_id AND d.published = FALSE
 WHERE c.retired_at IS NULL
 GROUP BY c.consumer_id, observed.at
 ORDER BY c.consumer_id`

// debtSignals reads every active consumer's debt as its frontier counts it: its own unresolved
// authority-bearing dead letters, plus those that name no consumer.
const debtSignals = `WITH observed AS (
    SELECT clock_timestamp() AS at
)
SELECT c.consumer_id,
       count(dl.event_id),
       coalesce(extract(epoch FROM observed.at - min(dl.dead_lettered_at)), 0)::double precision
  FROM observed
 CROSS JOIN projection.consumer c
  LEFT JOIN platform.dead_letter dl
         ON dl.resolved_at IS NULL
        AND dl.event_type = ANY ($1::text[])
        AND (dl.consumer IS NULL OR dl.consumer = c.consumer_id)
 WHERE c.retired_at IS NULL
 GROUP BY c.consumer_id, observed.at
 ORDER BY c.consumer_id`

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
		if err := tx.QueryRow(ctx, staleSignals).Scan(&s.Stale, &s.StaleOldestAge); err != nil {
			return fmt.Errorf("projection: reading the stale dead letters: %w", err)
		}

		lanes, err := tx.Query(ctx, laneSignals, outbox.PriorityHigh)
		if err != nil {
			return fmt.Errorf("projection: reading lane signals: %w", err)
		}
		for lanes.Next() {
			priority, standard := LaneSignal{Lane: "priority"}, LaneSignal{Lane: "standard"}
			if err := lanes.Scan(&priority.Consumer, &priority.Count, &priority.OldestAge,
				&standard.Count, &standard.OldestAge); err != nil {
				lanes.Close()
				return fmt.Errorf("projection: reading a consumer's lanes: %w", err)
			}
			standard.Consumer = priority.Consumer
			s.Lanes = append(s.Lanes, priority, standard)
		}
		lanes.Close()
		if err := lanes.Err(); err != nil {
			return fmt.Errorf("projection: reading lane signals: %w", err)
		}

		debts, err := tx.Query(ctx, debtSignals, r.events)
		if err != nil {
			return fmt.Errorf("projection: reading debt signals: %w", err)
		}
		for debts.Next() {
			var d DebtSignal
			if err := debts.Scan(&d.Consumer, &d.Count, &d.OldestAge); err != nil {
				debts.Close()
				return fmt.Errorf("projection: reading a consumer's debt: %w", err)
			}
			s.Debt = append(s.Debt, d)
		}
		debts.Close()
		if err := debts.Err(); err != nil {
			return fmt.Errorf("projection: reading debt signals: %w", err)
		}

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

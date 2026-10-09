package projection

import (
	"context"
	"errors"
	"fmt"
	"time"

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

	// Unapplied is each active consumer's oldest security event not yet applied: the accept-to-
	// enforcement delay of TDD-organization-control-002 §Operational Notes, measured while it is
	// still running rather than after it ends.
	Unapplied []UnappliedSignal
}

// UnappliedSignal is one consumer's oldest priority-lane delivery, accepted within
// UnappliedWindow, that carries no consumer_applied receipt and no open dead letter.
//
// The age runs from the delivery's created_at, which is the accepting transaction's: outbox.Append
// writes the delivery in the transaction that commits the change, so it is the acceptance instant to
// the transaction's start. An open dead letter is left out because SecurityDebt pages for it
// already, and one incident should not page twice under two names.
type UnappliedSignal struct {
	Consumer  string
	OldestAge float64 // seconds; zero when every recent security event is applied
}

// UnappliedWindow bounds how far back the unapplied read looks. The deliveries are partitioned by
// day on created_at, so the bound is also what keeps the read to the newest partitions. A security
// event unapplied for longer than this has been paged on for a day by this alert; past it, the
// consumer's report age and reconciliation are what find a delivery its consumer dropped silently.
const UnappliedWindow = 24 * time.Hour

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

	// ExtraFindings is how many `extra` findings its last reconciliation produced: access it serves
	// that authority does not grant. Zero until a reconciliation has run, and after a clean one.
	ExtraFindings int64
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

// unappliedSignals reads, per active consumer, the age of its oldest recent priority delivery with
// no consumer_applied receipt and no open dead letter. now() bounds the window, because it is stable
// within the statement and lets the planner prune partitions; clock_timestamp() measures the age.
const unappliedSignals = `WITH observed AS (
    SELECT clock_timestamp() AS at
)
SELECT c.consumer_id,
       coalesce(extract(epoch FROM observed.at - min(d.created_at)), 0)::double precision
  FROM observed
 CROSS JOIN projection.consumer c
  LEFT JOIN platform.outbox_delivery d
         ON d.consumer = c.consumer_id
        AND d.priority = $1
        AND d.created_at >= now() - make_interval(secs => $2)
        AND (d.failure_class IS NULL OR d.failure_class <> 'abandoned')
        AND NOT EXISTS (SELECT 1
                          FROM platform.delivery_receipt r
                         WHERE r.event_id = d.event_id
                           AND r.consumer = d.consumer
                           AND r.evidence = 'consumer_applied')
        AND NOT EXISTS (SELECT 1
                          FROM platform.dead_letter dl
                         WHERE dl.event_id = d.event_id
                           AND dl.consumer = d.consumer
                           AND dl.resolved_at IS NULL)
 WHERE c.retired_at IS NULL
 GROUP BY c.consumer_id, observed.at
 ORDER BY c.consumer_id`

const consumerSignals = `SELECT consumer_id,
       extract(epoch FROM clock_timestamp() - coalesce(last_reported_at, registered_at))::double precision,
       extract(epoch FROM max_accepted_age)::double precision,
       last_verify_ratio,
       coalesce(last_reconciled_extra_findings, 0)
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

		unapplied, err := tx.Query(ctx, unappliedSignals, outbox.PriorityHigh, UnappliedWindow.Seconds())
		if err != nil {
			return fmt.Errorf("projection: reading unapplied signals: %w", err)
		}
		for unapplied.Next() {
			var u UnappliedSignal
			if err := unapplied.Scan(&u.Consumer, &u.OldestAge); err != nil {
				unapplied.Close()
				return fmt.Errorf("projection: reading a consumer's unapplied security event: %w", err)
			}
			s.Unapplied = append(s.Unapplied, u)
		}
		unapplied.Close()
		if err := unapplied.Err(); err != nil {
			return fmt.Errorf("projection: reading unapplied signals: %w", err)
		}

		rows, err := tx.Query(ctx, consumerSignals)
		if err != nil {
			return fmt.Errorf("projection: reading consumer signals: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c ConsumerSignal
			if err := rows.Scan(&c.Consumer, &c.ReportAge, &c.MaxAcceptedAge, &c.VerifyRatio, &c.ExtraFindings); err != nil {
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

// LifecycleSignals are the offboarding and provisioning facts of TDD-organization-control-004 and
// TDD-organization-control-003 §Operational Notes, read as aggregates.
//
// Read separately from Signals and observed by a callback of its own, so a failure here leaves the
// enforcement gauges reporting. The rows are under Row-Level Security, which this raw connection
// binds no scope for, so they are read through operation.lifecycle_signals: a security_barrier view
// owned by the migration role, which reaches exactly the rows the counts need through three SELECT
// policies and returns one row of counts and ages, naming no Tenant (rls.sql).
type LifecycleSignals struct {
	// ObligationsOverdue are open obligations past due_at, and the age of the oldest past it.
	ObligationsOverdue      int64
	OldestOverdueObligation float64

	// OffboardingsInProgress are offboardings in freeze, obligations or release, and the age of the
	// oldest since it began.
	OffboardingsInProgress int64
	OldestOffboarding      float64

	// Requests are the provisioning requests in flight or ambiguous, per direction and state.
	Requests []RequestSignal
}

// RequestSignal is one direction and state of tenant.provisioning_request.
type RequestSignal struct {
	Operation string // "provision" or "deprovision"
	State     string // "requested" or "unresolved"
	Count     int64

	// OldestAge runs from requested_at for a request in flight, and from resolved_at, the instant the
	// sweep found it ambiguous, for an unresolved one. Zero when there is none.
	OldestAge float64
}

const lifecycleSignals = `SELECT obligations_overdue, oldest_overdue_obligation_age,
       offboardings_in_progress, oldest_offboarding_age,
       provision_requested, provision_requested_oldest_age,
       provision_unresolved, provision_unresolved_oldest_age,
       deprovision_requested, deprovision_requested_oldest_age,
       deprovision_unresolved, deprovision_unresolved_oldest_age
  FROM operation.lifecycle_signals`

// ReadLifecycle returns the current lifecycle signals.
func (r *SignalsReader) ReadLifecycle(ctx context.Context) (LifecycleSignals, error) {
	var s LifecycleSignals
	requests := []RequestSignal{
		{Operation: "provision", State: "requested"}, {Operation: "provision", State: "unresolved"},
		{Operation: "deprovision", State: "requested"}, {Operation: "deprovision", State: "unresolved"},
	}
	err := r.tx.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if err := tx.QueryRow(ctx, lifecycleSignals).Scan(
			&s.ObligationsOverdue, &s.OldestOverdueObligation,
			&s.OffboardingsInProgress, &s.OldestOffboarding,
			&requests[0].Count, &requests[0].OldestAge, &requests[1].Count, &requests[1].OldestAge,
			&requests[2].Count, &requests[2].OldestAge, &requests[3].Count, &requests[3].OldestAge); err != nil {
			return fmt.Errorf("projection: reading the lifecycle signals: %w", err)
		}
		return nil
	})
	if err != nil {
		return LifecycleSignals{}, err
	}
	s.Requests = requests
	return s, nil
}

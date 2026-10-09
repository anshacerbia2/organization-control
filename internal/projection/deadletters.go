package projection

// The list of one consumer's dead letters (TDD-organization-control-005 2.5.0 §API / Interface).
//
// The runbook diagnosed every incident with SQL on the migration credential, because no route listed
// them. platform.dead_letter carries no tenant_id and no policy, so the read is the provider scope's
// for one reason: the access record, which names the caller and its reason for every page.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// ErrConsumerNotFound reports a dead-letter list for a consumer this registry never held. A retired
// consumer is found: its dead letters are the ones a waiver closes.
var ErrConsumerNotFound = errors.New("projection: no consumer is registered under that identifier")

// DeadLetterState filters the list.
type DeadLetterState string

const (
	// DeadLetterOpen is unresolved, waived or not: an incident still open.
	DeadLetterOpen DeadLetterState = "open"

	// DeadLetterResolved is closed, as REPLAYED or SUPERSEDED.
	DeadLetterResolved DeadLetterState = "resolved"
)

// DeadLetterQuery is one page of a consumer's dead letters.
type DeadLetterQuery struct {
	Consumer string

	// After is the event_id of the last item of the previous page. Within one consumer an event is
	// dead-lettered at most once (the key is (event_id, consumer)), so the event_id is the item's key.
	After id.UUID
	Limit int
	State DeadLetterState
}

// DeadLetter is one incident, as the runbook needs to decide between replay, SUPERSEDED and a waiver.
type DeadLetter struct {
	EventID   id.UUID
	Consumer  string
	EventType string

	// AuthorityBearing is whether the type is security debt: a Membership, Tenant or provider grant
	// event, which refuses the consumer's projection-backed checks while it is open.
	AuthorityBearing bool

	// AggregateID is the Membership, Tenant or grant the event is about, and Version the version it
	// carried, read from the stored payload. Nil on a row that predates either (ROADMAP item 12), or
	// whose payload a waiver let the maintenance stage dispose of.
	AggregateID *id.UUID
	Version     *int64

	// Priority is "priority" or "standard", the lane a replay takes; nil on a row that predates it.
	Priority *string

	FailureClass  string
	FailureDetail string
	Attempts      int
	FirstFailedAt time.Time
	DeadLettered  time.Time

	ResolvedAt          *time.Time
	ResolutionType      *string
	ResolvedBy          *string
	ResolutionReference *string

	WaivedAt     *time.Time
	WaivedUntil  *time.Time
	WaivedBy     *string
	WaiverReason *string
}

// DeadLetterPage is one page, and the event_id the next starts after.
type DeadLetterPage struct {
	DeadLetters []DeadLetter
	Next        *id.UUID
}

// DeadLetterReader lists dead letters.
type DeadLetterReader struct {
	pool   *db.ProviderPool
	events map[string]bool
}

// NewDeadLetterReader constructs the reader.
func NewDeadLetterReader(pool *db.ProviderPool) (*DeadLetterReader, error) {
	if pool == nil {
		return nil, errors.New("projection: a provider-scoped pool is required")
	}
	events := map[string]bool{}
	for _, t := range AuthorityEventTypes {
		events[t] = true
	}
	return &DeadLetterReader{pool: pool, events: events}, nil
}

const consumerKnown = `SELECT EXISTS (SELECT 1 FROM projection.consumer WHERE consumer_id = $1)`

// listDeadLetters is the keyset over event_id, a UUIDv7, so the order is the order events were made.
// The version is the payload's, under whichever name the event's type carries it.
const listDeadLetters = `SELECT event_id::text, consumer, event_type, aggregate_id::text,
       CASE WHEN priority IS NULL THEN NULL WHEN priority = 0 THEN 'priority' ELSE 'standard' END,
       failure_class, failure_detail, attempts, first_failed_at, dead_lettered_at,
       resolved_at, resolution_type, resolved_by, resolution_reference,
       waived_at, waived_until, waived_by, waiver_reason,
       coalesce(payload->>'membership_version', payload->>'tenant_security_version',
                payload->>'grant_version')::bigint
  FROM platform.dead_letter
 WHERE consumer = $1
   AND ($2::text = '' OR ($2 = 'open' AND resolved_at IS NULL) OR ($2 = 'resolved' AND resolved_at IS NOT NULL))
   AND ($3::uuid IS NULL OR event_id > $3::uuid)
 ORDER BY event_id
 LIMIT $4`

// List returns one page of a consumer's dead letters, recording the access with reason.
func (r *DeadLetterReader) List(ctx context.Context, query DeadLetterQuery, reason string) (DeadLetterPage, error) {
	limit, err := db.ListLimit(query.Limit)
	if err != nil {
		return DeadLetterPage{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	switch query.State {
	case "", DeadLetterOpen, DeadLetterResolved:
	default:
		return DeadLetterPage{}, fmt.Errorf("%w: state must be open or resolved", ErrInvalid)
	}
	if query.Consumer == "" {
		return DeadLetterPage{}, fmt.Errorf("%w: a consumer identifier is required", ErrInvalid)
	}

	page := DeadLetterPage{DeadLetters: []DeadLetter{}}
	if err := db.WithProviderScope(ctx, r.pool, reason, func(ctx context.Context, tx db.Tx) error {
		var known bool
		if err := tx.QueryRow(ctx, consumerKnown, query.Consumer).Scan(&known); err != nil {
			return fmt.Errorf("projection: read the consumer: %w", err)
		}
		if !known {
			return fmt.Errorf("%w: %s", ErrConsumerNotFound, query.Consumer)
		}
		rows, err := tx.Query(ctx, listDeadLetters, query.Consumer, string(query.State),
			db.Keyset(query.After), limit+1)
		if err != nil {
			return fmt.Errorf("projection: list dead letters: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				item         DeadLetter
				rawEvent     string
				rawAggregate *string
			)
			if err := rows.Scan(&rawEvent, &item.Consumer, &item.EventType, &rawAggregate, &item.Priority,
				&item.FailureClass, &item.FailureDetail, &item.Attempts, &item.FirstFailedAt, &item.DeadLettered,
				&item.ResolvedAt, &item.ResolutionType, &item.ResolvedBy, &item.ResolutionReference,
				&item.WaivedAt, &item.WaivedUntil, &item.WaivedBy, &item.WaiverReason, &item.Version); err != nil {
				return fmt.Errorf("projection: scan dead letter: %w", err)
			}
			if item.EventID, err = id.Parse(rawEvent); err != nil {
				return fmt.Errorf("projection: stored event id %q: %w", rawEvent, err)
			}
			if rawAggregate != nil {
				aggregate, err := id.Parse(*rawAggregate)
				if err != nil {
					return fmt.Errorf("projection: stored aggregate id %q: %w", *rawAggregate, err)
				}
				item.AggregateID = &aggregate
			}
			item.AuthorityBearing = r.events[item.EventType]
			page.DeadLetters = append(page.DeadLetters, item)
		}
		return rows.Err()
	}); err != nil {
		return DeadLetterPage{}, err
	}
	if len(page.DeadLetters) > limit {
		page.DeadLetters = page.DeadLetters[:limit]
		next := page.DeadLetters[limit-1].EventID
		page.Next = &next
	}
	return page, nil
}

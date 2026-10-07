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
	"sort"
	"strings"
	"time"

	fevent "github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

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
)

// Consumer is one row of projection.consumer.
type Consumer struct {
	ConsumerID string

	// PrincipalID is the workload Principal this consumer is. A workload token is this consumer's
	// when its principal_id matches and the consumer is active (ADR-ORG-001 §5.11).
	PrincipalID id.UUID

	ProjectionVersion string
	MaxAcceptedAge    time.Duration
	StaleBehavior     StaleBehavior
	RegisteredAt      time.Time

	// EventTypes are the types its active subscription names, in platform.subscription: the
	// events it is owed a delivery of (ADR-GLB-018 §5.1).
	EventTypes []string

	// SnapshotMark is the high-water mark of the snapshot this consumer bootstrapped from. Nil
	// until a snapshot has been taken, which is exactly the condition that refuses a progress
	// report.
	SnapshotMark *int64

	// LastReportedMark and LastReportedAt are what the consumer said about itself. They are a
	// report and not an authority: the publisher measures freshness against them and never infers
	// a stream position from them.
	LastReportedMark *int64
	LastReportedAt   *time.Time

	// LastReconciledAt, LastReconciledMark and LastReconciledFindings are the last reconciliation run
	// against this consumer's report: when this service ran it, the mark the report stated, and how
	// many findings it produced. A measurement this service made, unlike the two above. Nil until a
	// reconciliation has run.
	LastReconciledAt       *time.Time
	LastReconciledMark     *int64
	LastReconciledFindings *int
}

// Registration is a consumer declaring what it needs.
type Registration struct {
	ConsumerID string

	// PrincipalID is the consumer's workload Principal. Required, and fixed at the first
	// registration: re-registering under another is refused (TDD-organization-control-002
	// §Consumer Registry).
	PrincipalID id.UUID

	ProjectionVersion string
	MaxAcceptedAge    time.Duration
	StaleBehavior     StaleBehavior

	// EventTypes are the types the consumer applies, each one SubscribableEventTypes offers. The
	// registration is the subscription: from its commit every event of these types owes the
	// consumer a delivery (TDD-organization-control-002 §Consumer Registry).
	EventTypes []string
}

// subscription returns the registration's event types, deduplicated and in a stable order, or a
// refusal naming the first one this registry does not offer.
func (r Registration) subscription() ([]string, error) {
	if len(r.EventTypes) == 0 {
		return nil, fmt.Errorf("%w: event_types names no event type; a consumer owed nothing has nothing to register for", ErrInvalid)
	}
	offered := map[string]bool{}
	for _, t := range SubscribableEventTypes {
		offered[t] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range r.EventTypes {
		t := strings.TrimSpace(raw)
		if !offered[t] {
			return nil, fmt.Errorf("%w: event type %q is not one this registry offers", ErrInvalid, raw)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r Registration) validate() error {
	switch {
	case strings.TrimSpace(r.ConsumerID) == "":
		return fmt.Errorf("%w: a consumer identifier is required", ErrInvalid)
	case r.PrincipalID.IsNil():
		// The consumer's token is recognized by this and nothing else. A consumer registered
		// without it could never authenticate.
		return fmt.Errorf("%w: the consumer's workload principal_id is required", ErrInvalid)
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
	_, err := r.subscription()
	return err
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
// The declared terms are replaced and the progress columns are not: a consumer raising its
// freshness budget has not un-bootstrapped itself. Progress is cleared separately, and only when
// the subscription is written (see Register).
//
// `retired_at = NULL` on conflict revives a retired consumer rather than refusing it. A consumer
// coming back under an identity it previously held is a legitimate act.
//
// `principal_id` is written on insert and never updated: identityConflict refuses a re-registration
// naming another one before this runs.
const upsertStatement = `INSERT INTO projection.consumer
    (consumer_id, principal_id, projection_version, max_accepted_age, stale_behavior)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (consumer_id) DO UPDATE
SET projection_version = excluded.projection_version,
    max_accepted_age   = excluded.max_accepted_age,
    stale_behavior     = excluded.stale_behavior,
    retired_at         = NULL
RETURNING registered_at`

// priorStatement reads what the registration replaces: whether the consumer exists, whether it is
// retired, and the event types of its active subscription. At most one row, iterated rather than
// QueryRow'd because no row is the expected case for a new consumer.
const priorStatement = `SELECT c.retired_at IS NOT NULL, s.event_types
FROM projection.consumer c
LEFT JOIN platform.subscription s ON s.consumer = c.consumer_id AND s.retired_at IS NULL
WHERE c.consumer_id = $1
FOR UPDATE OF c`

// resetProgressStatement clears what a consumer reported about a model built from deliveries it
// will no longer receive in full. Its next progress report is refused until it bootstraps again.
const resetProgressStatement = `UPDATE projection.consumer
SET snapshot_mark = NULL, last_reported_mark = NULL, last_reported_at = NULL
WHERE consumer_id = $1`

// identityStatement reads the rows that would make the registration change whose workload a consumer
// is: this consumer under another principal_id, or another consumer, retired or not, under this one.
const identityStatement = `SELECT consumer_id, principal_id::text
FROM projection.consumer
WHERE (consumer_id = $1 AND principal_id <> $2) OR (consumer_id <> $1 AND principal_id = $2)
LIMIT 1`

// identityConflict refuses a registration that would move a consumer to another workload, or give a
// workload a second consumer.
//
// Refused rather than updated. A consumer's records -- its snapshot mark, its reported positions, its
// dead-letter debt -- are about one workload, and moving them to another would let that workload
// inherit an authority and a history it never had. consumer_principal is the database's guarantee for
// the second case; this read turns its violation into an answer that names the consumer in the way.
func identityConflict(ctx context.Context, tx db.Tx, reg Registration) error {
	rows, err := tx.Query(ctx, identityStatement, reg.ConsumerID, reg.PrincipalID.String())
	if err != nil {
		return fmt.Errorf("projection: checking the consumer's principal: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return rows.Err()
	}
	var consumer, principal string
	if err := rows.Scan(&consumer, &principal); err != nil {
		return fmt.Errorf("projection: reading the consumer's principal: %w", err)
	}
	if consumer == reg.ConsumerID {
		return fmt.Errorf("%w: %s is registered with another principal_id; a consumer's workload does not change",
			ErrInvalid, consumer)
	}
	return fmt.Errorf("%w: principal_id is already registered as consumer %s", ErrInvalid, consumer)
}

// prior reports whether the consumer exists, whether it is retired, and its active subscription.
func prior(ctx context.Context, tx db.Tx, consumerID string) (exists, retired bool, types []string, err error) {
	rows, err := tx.Query(ctx, priorStatement, consumerID)
	if err != nil {
		return false, false, nil, fmt.Errorf("projection: reading the consumer's registration: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		exists = true
		if err := rows.Scan(&retired, &types); err != nil {
			return false, false, nil, fmt.Errorf("projection: reading the consumer's registration: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, nil, fmt.Errorf("projection: reading the consumer's registration: %w", err)
	}
	return exists, retired, types, nil
}

func sameTypes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// Register records or updates a consumer's declared terms and its subscription, in one transaction.
//
// The subscription is written when the consumer is new, is being revived, has none, or names other
// types; otherwise it is left alone. Writing it clears the consumer's snapshot mark and reported
// position. A revived consumer was delivered nothing while it was retired, and one whose types
// changed was never delivered the events of a newly added type that committed before the change.
// Either way its model is not one a stream position can vouch for, so it bootstraps again.
// That is the cost a subscription change carries (ADR-GLB-018 §5.1).
func (r *Registry) Register(ctx context.Context, reg Registration) (Consumer, error) {
	if err := reg.validate(); err != nil {
		return Consumer{}, err
	}
	types, err := reg.subscription()
	if err != nil {
		return Consumer{}, err
	}

	consumer := Consumer{
		ConsumerID:        reg.ConsumerID,
		PrincipalID:       reg.PrincipalID,
		ProjectionVersion: reg.ProjectionVersion,
		MaxAcceptedAge:    reg.MaxAcceptedAge,
		StaleBehavior:     reg.StaleBehavior,
		EventTypes:        types,
	}

	if err := db.WithProviderScope(ctx, r.pool,
		"register projection consumer "+reg.ConsumerID,
		func(ctx context.Context, tx db.Tx) error {
			if err := identityConflict(ctx, tx, reg); err != nil {
				return err
			}
			exists, retired, current, err := prior(ctx, tx, reg.ConsumerID)
			if err != nil {
				return err
			}

			if err := tx.QueryRow(ctx, upsertStatement,
				reg.ConsumerID, reg.PrincipalID.String(), reg.ProjectionVersion, reg.MaxAcceptedAge,
				string(reg.StaleBehavior),
			).Scan(&consumer.RegisteredAt); err != nil {
				return err
			}

			if exists && !retired && current != nil && sameTypes(current, types) {
				return nil
			}
			eventTypes := make([]fevent.Type, len(types))
			for i, t := range types {
				eventTypes[i] = fevent.Type(t)
			}
			if err := outbox.Subscribe(ctx, tx, reg.ConsumerID, eventTypes); err != nil {
				return fmt.Errorf("projection: subscribing %s: %w", reg.ConsumerID, err)
			}
			if exists {
				if _, err := tx.Exec(ctx, resetProgressStatement, reg.ConsumerID); err != nil {
					return fmt.Errorf("projection: clearing the progress of %s: %w", reg.ConsumerID, err)
				}
			}
			return nil
		}); err != nil {
		if errors.Is(err, ErrInvalid) {
			return Consumer{}, err
		}
		return Consumer{}, fmt.Errorf("projection: register consumer: %w", err)
	}
	return consumer, nil
}

const retireStatement = `UPDATE projection.consumer
SET retired_at = now()
WHERE consumer_id = $1 AND retired_at IS NULL`

// Retire withdraws a consumer: its registry row, its subscription, and what it was still owed.
//
// In one transaction, the row is stamped retired, the subscription retired (outbox.Unsubscribe),
// and every delivery still owed to it abandoned (outbox.Abandon, ADR-GLB-018 §5.5). Its dispatcher
// refuses to start once it is retired, so nothing would deliver those, and left owed they would hold
// outbox retention for every day they belong to. Retiring an already-retired consumer runs the same
// two steps again, which finds nothing to do on a consumer retired this way and finishes one retired
// before subscriptions existed.
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
			if tag.RowsAffected() != 1 {
				// Nothing was updated: either the consumer is already retired, or it does not
				// exist. Distinguished with a second read rather than reported as one outcome,
				// because an operator retiring a name they mistyped must not be told it worked.
				var exists bool
				if err := tx.QueryRow(ctx,
					"SELECT true FROM projection.consumer WHERE consumer_id = $1", consumerID,
				).Scan(&exists); err != nil {
					return fmt.Errorf("%w: %s", ErrNotRegistered, consumerID)
				}
			}
			if err := outbox.Unsubscribe(ctx, tx, consumerID); err != nil {
				return fmt.Errorf("projection: retiring the subscription of %s: %w", consumerID, err)
			}
			if _, err := outbox.Abandon(ctx, tx, consumerID, "projection consumer "+consumerID+" retired"); err != nil {
				return fmt.Errorf("projection: abandoning what %s was owed: %w", consumerID, err)
			}
			return nil
		})
}

const selectConsumer = `SELECT c.consumer_id,
       c.principal_id::text,
       c.projection_version,
       c.max_accepted_age,
       c.stale_behavior,
       c.registered_at,
       c.snapshot_mark,
       c.last_reported_mark,
       c.last_reported_at,
       coalesce(s.event_types, '{}'),
       c.last_reconciled_at,
       c.last_reconciled_mark,
       c.last_reconciled_findings
FROM projection.consumer c
LEFT JOIN platform.subscription s ON s.consumer = c.consumer_id AND s.retired_at IS NULL
WHERE c.consumer_id = $1 AND c.retired_at IS NULL`

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
	var behavior, principal string
	if err := tx.QueryRow(ctx, selectConsumer, consumerID).Scan(
		&consumer.ConsumerID, &principal, &consumer.ProjectionVersion, &consumer.MaxAcceptedAge,
		&behavior, &consumer.RegisteredAt, &consumer.SnapshotMark,
		&consumer.LastReportedMark, &consumer.LastReportedAt, &consumer.EventTypes,
		&consumer.LastReconciledAt, &consumer.LastReconciledMark, &consumer.LastReconciledFindings); err != nil {
		return fmt.Errorf("%w: %s", ErrNotRegistered, consumerID)
	}
	return decodeStored(consumer, principal, behavior)
}

// decodeStored checks and sets the two stored values a row carries as text. A value that fails either
// check is this service's own defect, and carries no ErrInvalid.
func decodeStored(consumer *Consumer, principal, behavior string) error {
	parsed, err := id.Parse(principal)
	if err != nil {
		return fmt.Errorf("projection: stored principal_id %q is not an identifier", principal)
	}
	consumer.PrincipalID = parsed
	consumer.StaleBehavior = StaleBehavior(behavior)
	if !consumer.StaleBehavior.Valid() {
		return fmt.Errorf("projection: stored stale_behavior %q is not a declared behavior", behavior)
	}
	return nil
}

// ConsumerState is whether a registry row is a consumer's live registration or the record of a
// retired one. It is derived from `retired_at`, which is the only thing that separates the two.
type ConsumerState string

const (
	// ConsumerActive is a registration whose consumer may act: retired_at is NULL.
	ConsumerActive ConsumerState = "active"

	// ConsumerRetired is a row kept for investigation after its consumer was retired.
	ConsumerRetired ConsumerState = "retired"
)

// Valid reports whether the state is one the list filters on.
func (s ConsumerState) Valid() bool {
	return s == ConsumerActive || s == ConsumerRetired
}

// ListedConsumer is one item of the consumer list: the registration, whether it is retired, and
// whether its report is stale (TDD-organization-control-002 1.11.0 §The Consumer List).
type ListedConsumer struct {
	Consumer

	State     ConsumerState
	RetiredAt *time.Time

	// Stale is Consumer.Age's verdict at the instant the page was read, for an active consumer. A
	// retired one is never stale: it enforces nothing and is owed nothing.
	Stale bool
}

// ConsumerListQuery selects one page of the registry (STD-GLB-001 1.3.0 §Pagination).
type ConsumerListQuery struct {
	// After is the consumer_id of the last item of the previous page; empty starts at the first.
	After string

	// Limit is the page size, 1 to db.MaxListLimit; zero takes db.DefaultListLimit.
	Limit int

	// State narrows the list to active or retired consumers; empty is both.
	State ConsumerState
}

// ConsumerPage is one page of the registry in consumer_id order. Next is the After of the following
// page, and nil on the last.
type ConsumerPage struct {
	Consumers []ListedConsumer
	Next      *string
}

// listStatement is one keyset page of the registry, in consumer_id order, with the columns
// selectConsumer reads and retired_at. The subscription join is the same one: a retired consumer's
// subscription is retired with it, so its event types read as none.
const listStatement = `SELECT c.consumer_id,
       c.principal_id::text,
       c.projection_version,
       c.max_accepted_age,
       c.stale_behavior,
       c.registered_at,
       c.snapshot_mark,
       c.last_reported_mark,
       c.last_reported_at,
       coalesce(s.event_types, '{}'),
       c.retired_at,
       c.last_reconciled_at,
       c.last_reconciled_mark,
       c.last_reconciled_findings
FROM projection.consumer c
LEFT JOIN platform.subscription s ON s.consumer = c.consumer_id AND s.retired_at IS NULL
WHERE ($1::text = ''
       OR ($1::text = 'active' AND c.retired_at IS NULL)
       OR ($1::text = 'retired' AND c.retired_at IS NOT NULL))
  AND ($2::text IS NULL OR c.consumer_id > $2::text)
ORDER BY c.consumer_id
LIMIT $3`

// List reads one page of the registry, retired consumers included unless the query names a state.
//
// Provider-scoped like every registry read, with the caller's reason recorded before the page is read.
// Stale is computed here, on the registry's clock, which is the clock RecordProgress stamps
// last_reported_at with; one instant serves the whole page.
func (r *Registry) List(ctx context.Context, query ConsumerListQuery, reason string) (ConsumerPage, error) {
	limit, err := db.ListLimit(query.Limit)
	if err != nil {
		return ConsumerPage{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if query.State != "" && !query.State.Valid() {
		return ConsumerPage{}, fmt.Errorf("%w: state must be active or retired", ErrInvalid)
	}
	var after any
	if query.After != "" {
		after = query.After
	}

	page := ConsumerPage{Consumers: []ListedConsumer{}}
	if err := db.WithProviderScope(ctx, r.pool, reason, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, listStatement, string(query.State), after, limit+1)
		if err != nil {
			return fmt.Errorf("projection: list consumers: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				item                ListedConsumer
				principal, behavior string
			)
			if err := rows.Scan(&item.ConsumerID, &principal, &item.ProjectionVersion, &item.MaxAcceptedAge,
				&behavior, &item.RegisteredAt, &item.SnapshotMark, &item.LastReportedMark,
				&item.LastReportedAt, &item.EventTypes, &item.RetiredAt, &item.LastReconciledAt,
				&item.LastReconciledMark, &item.LastReconciledFindings); err != nil {
				return fmt.Errorf("projection: scan consumer list: %w", err)
			}
			if err := decodeStored(&item.Consumer, principal, behavior); err != nil {
				return err
			}
			page.Consumers = append(page.Consumers, item)
		}
		return rows.Err()
	}); err != nil {
		return ConsumerPage{}, err
	}

	now := r.now()
	for i := range page.Consumers {
		item := &page.Consumers[i]
		if item.RetiredAt != nil {
			item.State = ConsumerRetired
			continue
		}
		item.State = ConsumerActive
		_, item.Stale = item.Age(now)
	}
	if len(page.Consumers) > limit {
		page.Consumers = page.Consumers[:limit]
		next := page.Consumers[limit-1].ConsumerID
		page.Next = &next
	}
	return page, nil
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
			return recordProgressIn(ctx, tx, report, at, &consumer)
		}); err != nil {
		return Consumer{}, err
	}

	mark, reported := report.AppliedMark, at
	consumer.LastReportedMark, consumer.LastReportedAt = &mark, &reported
	return consumer, nil
}

// recordProgressIn is RecordProgress's transaction body, shared by the provider and consumer paths.
func recordProgressIn(ctx context.Context, tx db.Tx, report Progress, at time.Time, consumer *Consumer) error {
	if err := load(ctx, tx, report.ConsumerID, consumer); err != nil {
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

// ReconciliationAge reports how long ago this service last reconciled the consumer's report, or nil
// when it never has (TDD-organization-control-002 §Reconciliation). Read on the clock the run was
// stamped with, as Age is.
func (c Consumer) ReconciliationAge(now time.Time) *time.Duration {
	if c.LastReconciledAt == nil {
		return nil
	}
	age := now.Sub(*c.LastReconciledAt)
	return &age
}

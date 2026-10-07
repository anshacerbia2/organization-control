package membership

// Membership batches: a bulk action the server previews, then executes (ADR-ORG-004 §5.1,
// TDD-organization-control-002 1.10.0 §Membership Batches).
//
// The preview runs every item through the checks the single transition runs -- the read under the
// Tenant's policy, Command.validate and decide -- and writes nothing but the batch. The execution
// runs TransitionWithin per item, in a transaction of its own, named with the version the preview
// read. There is no second state machine here, and no second copy of the version check.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

const (
	// MaxBatchItems bounds a batch, as SCIM requires a declared maximum (RFC 7644 §3.7.4).
	MaxBatchItems = 500

	// BatchTTL is how long a preview stays executable. Past it the preview describes a state the
	// operator saw too long ago to commit, and execution is refused.
	BatchTTL = 15 * time.Minute

	// BatchLease is how long an execution may go without a heartbeat before the request executing it
	// is taken to have ended (TDD-organization-control-002 §Resuming an execution). Every item's
	// transaction writes one, and no request lives this long: the composition root refuses an
	// HTTP_REQUEST_TIMEOUT at or above it, so a heartbeat this old belongs to a request that is gone.
	BatchLease = 30 * time.Second
)

// BatchState is where a batch is. `expired` is never stored: it is a `previewed` batch read after
// its expiry.
type BatchState string

const (
	BatchPreviewed BatchState = "previewed"
	BatchExecuting BatchState = "executing"
	BatchExecuted  BatchState = "executed"
	BatchExpired   BatchState = "expired"
)

// The outcome of one item once its batch executed.
const (
	OutcomeSucceeded    = "succeeded"
	OutcomeFailed       = "failed"
	OutcomeNotAttempted = "not_attempted"

	// Why an item was not attempted.
	ReasonRefusedAtPreview = "refused_at_preview"
	ReasonErrorAllowance   = "error_allowance"
	ReasonExpired          = "expired"
)

var (
	// ErrBatchNotFound reports a batch absent from the bound Tenant, another Tenant's included.
	ErrBatchNotFound = errors.New("membership: batch not found")

	// ErrBatchExpired refuses executing a preview past its expiry.
	ErrBatchExpired = errors.New("membership: the batch preview has expired")

	// ErrBatchNotPreviewed refuses executing a batch that is executing or has executed.
	ErrBatchNotPreviewed = errors.New("membership: the batch is not awaiting execution")

	// ErrBatchTooLarge refuses a preview naming more than MaxBatchItems Memberships. SCIM answers it
	// 413, and "the returned response MUST specify the limit exceeded in the body" (RFC 7644 §3.7.4),
	// so its detail names the bound (ADR-ORG-004 §5.1).
	ErrBatchTooLarge = errors.New("membership: the batch is too large")

	// ErrBatchExecuting refuses an execute while another request holds a live lease on the batch.
	ErrBatchExecuting = errors.New("membership: the batch is being executed")

	// ErrBatchLeaseLost stops an execution whose lease another request took over after this one went
	// silent for longer than BatchLease. What it had not committed is the other request's to do.
	ErrBatchLeaseLost = errors.New("membership: the batch execution was taken over")
)

// Problem is the problem document the single command would return for an item: what the preview
// records as a refusal and the execution as a failure (RFC 7644 §3.7.3).
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Classifier turns an item's error into the problem the single command would answer with. The HTTP
// surface supplies it, because the translation table is that surface's, and a copy of it here would
// be a second one that drifts.
type Classifier func(error) Problem

// BatchRequest previews a bulk action.
type BatchRequest struct {
	Action        Action
	MembershipIDs []id.UUID

	// Continues names an executed batch of the same Tenant whose failed items this resubmits. The
	// new batch keeps its correlation identifier.
	Continues id.UUID

	// Reason is required for a revocation and recorded whenever given.
	Reason string
}

// BatchItem is one Membership a batch names.
type BatchItem struct {
	Position     int
	MembershipID id.UUID

	// Read at preview. Nil when the Membership was not found.
	PrincipalID   *id.UUID
	CurrentStatus *State
	VersionRead   *int64

	// ResultingStatus is the status the action would produce; nil when refused.
	ResultingStatus *State

	// Refusal is the problem the single command would return; nil when the item would change.
	Refusal *Problem

	// Outcome is nil until the batch executes.
	Outcome *ItemOutcome
}

// ItemOutcome is what execution did with one item.
type ItemOutcome struct {
	Status string

	// Succeeded.
	AcceptedAt *time.Time
	EventID    *id.UUID
	Version    *int64

	// Failed.
	Problem *Problem

	// Not attempted.
	Reason string
}

// Batch is one bulk action and every item it names, in the order named.
type Batch struct {
	BatchID       id.UUID
	TenantID      id.UUID
	Action        Action
	State         BatchState
	Reason        *string
	CorrelationID id.UUID
	Continues     *id.UUID
	CreatedBy     id.UUID
	CreatedAt     time.Time
	ExpiresAt     time.Time

	WouldChange    int
	WouldNotChange int

	FailOnErrors *int
	ExecutedBy   *id.UUID
	ExecutedAt   *time.Time
	CompletedAt  *time.Time

	// HeartbeatAt is the last sign of life of the request executing the batch; ResumedBy and
	// ResumedAt record the last time a later request took the execution over.
	HeartbeatAt *time.Time
	ResumedBy   *id.UUID
	ResumedAt   *time.Time

	Items []BatchItem
}

// Counts reports how many items succeeded, failed and were not attempted.
func (b Batch) Counts() (succeeded, failed, notAttempted int) {
	for _, item := range b.Items {
		if item.Outcome == nil {
			continue
		}
		switch item.Outcome.Status {
		case OutcomeSucceeded:
			succeeded++
		case OutcomeFailed:
			failed++
		case OutcomeNotAttempted:
			notAttempted++
		}
	}
	return succeeded, failed, notAttempted
}

// settle reports a preview read after its expiry as expired, every item not attempted. Nothing is
// written: an expired batch is a fact about the clock, and storing it would be a write on a read.
func (b *Batch) settle(now time.Time) {
	if b.State != BatchPreviewed || !now.After(b.ExpiresAt) {
		return
	}
	b.State = BatchExpired
	for i := range b.Items {
		b.Items[i].Outcome = &ItemOutcome{Status: OutcomeNotAttempted, Reason: ReasonExpired}
	}
}

func (r BatchRequest) validate() error {
	switch r.Action {
	case ActionSuspend, ActionRestore, ActionRevoke:
	default:
		return fmt.Errorf("%w: action must be suspend, restore or revoke", ErrInvalid)
	}
	switch {
	case len(r.MembershipIDs) == 0:
		return fmt.Errorf("%w: membership_ids must name at least one Membership", ErrInvalid)
	case len(r.MembershipIDs) > MaxBatchItems:
		return fmt.Errorf("%w: a batch carries at most %d items, and membership_ids names %d",
			ErrBatchTooLarge, MaxBatchItems, len(r.MembershipIDs))
	case r.Action == ActionRevoke && strings.TrimSpace(r.Reason) == "":
		return ErrReasonRequired
	}
	seen := make(map[id.UUID]bool, len(r.MembershipIDs))
	for _, membershipID := range r.MembershipIDs {
		if membershipID.IsNil() {
			return fmt.Errorf("%w: membership_ids holds a nil identifier", ErrInvalid)
		}
		if seen[membershipID] {
			return fmt.Errorf("%w: membership_ids names %s more than once", ErrInvalid, membershipID)
		}
		seen[membershipID] = true
	}
	return nil
}

const selectParentBatch = `SELECT state, correlation_id::text
FROM membership.membership_batch
WHERE batch_id = $1`

const insertBatch = `INSERT INTO membership.membership_batch
    (batch_id, tenant_id, action, state, reason, correlation_id, continues, created_by,
     created_at, expires_at, would_change, would_not_change)
VALUES ($1, $2, $3, 'previewed', $4, $5, $6, $7, $8, $9, $10, $11)`

const insertBatchItem = `INSERT INTO membership.membership_batch_item
    (batch_id, tenant_id, position, membership_id, principal_id, current_status, version_read,
     resulting_status, refusal)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)`

// PreviewBatch evaluates every item as its single command would and stores the batch.
//
// Validate-only in the AIP-163 sense: each item is read under the Tenant's policy and run through
// Command.validate and decide, the functions TransitionWithin runs, and the transaction writes the
// batch and nothing else -- no Membership, no event, no outbox row. An error the classifier calls a
// server fault aborts the preview rather than being recorded as the item's refusal.
func (s *Service) PreviewBatch(ctx context.Context, req BatchRequest, classify Classifier) (Batch, error) {
	if classify == nil {
		return Batch{}, errors.New("membership: a classifier is required")
	}
	if err := req.validate(); err != nil {
		return Batch{}, err
	}
	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Batch{}, db.ErrNoScope
	}
	batchID, err := s.newID()
	if err != nil {
		return Batch{}, fmt.Errorf("membership: mint batch identifier: %w", err)
	}
	now := s.now().UTC()

	reason := strings.TrimSpace(req.Reason)
	batch := Batch{
		BatchID: batchID, TenantID: scope.TenantID(), Action: req.Action, State: BatchPreviewed,
		CorrelationID: scope.Correlation(), CreatedBy: scope.Actor(),
		CreatedAt: now, ExpiresAt: now.Add(BatchTTL),
	}
	if reason != "" {
		batch.Reason = &reason
	}
	if !req.Continues.IsNil() {
		continues := req.Continues
		batch.Continues = &continues
	}

	if err := db.WithTenantScope(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		if batch.Continues != nil {
			correlation, err := parentCorrelation(ctx, tx, *batch.Continues)
			if err != nil {
				return err
			}
			batch.CorrelationID = correlation
		}
		if batch.CorrelationID.IsNil() {
			minted, err := s.newID()
			if err != nil {
				return fmt.Errorf("membership: mint correlation identifier: %w", err)
			}
			batch.CorrelationID = minted
		}

		for position, membershipID := range req.MembershipIDs {
			item, err := s.previewItem(ctx, tx, req.Action, reason, position, membershipID, classify)
			if err != nil {
				return err
			}
			if item.Refusal == nil {
				batch.WouldChange++
			} else {
				batch.WouldNotChange++
			}
			batch.Items = append(batch.Items, item)
		}

		if _, err := tx.Exec(ctx, insertBatch, batch.BatchID.String(), batch.TenantID.String(),
			string(batch.Action), nullableText(reason), batch.CorrelationID.String(),
			nullableUUIDPointer(batch.Continues), batch.CreatedBy.String(), batch.CreatedAt,
			batch.ExpiresAt, batch.WouldChange, batch.WouldNotChange); err != nil {
			return fmt.Errorf("membership: insert batch: %w", err)
		}
		for _, item := range batch.Items {
			refusal, err := problemJSON(item.Refusal)
			if err != nil {
				return err
			}
			var current, resulting any
			if item.CurrentStatus != nil {
				current = string(*item.CurrentStatus)
			}
			if item.ResultingStatus != nil {
				resulting = string(*item.ResultingStatus)
			}
			var version any
			if item.VersionRead != nil {
				version = *item.VersionRead
			}
			if _, err := tx.Exec(ctx, insertBatchItem, batch.BatchID.String(), batch.TenantID.String(),
				item.Position, item.MembershipID.String(), nullableUUIDPointer(item.PrincipalID),
				current, version, resulting, refusal); err != nil {
				return fmt.Errorf("membership: insert batch item: %w", err)
			}
		}
		return nil
	}); err != nil {
		return Batch{}, err
	}
	return batch, nil
}

// previewItem is one item through the single command's checks.
func (s *Service) previewItem(ctx context.Context, tx db.Tx, action Action, reason string,
	position int, membershipID id.UUID, classify Classifier) (BatchItem, error) {
	item := BatchItem{Position: position, MembershipID: membershipID}

	refuse := func(err error) (BatchItem, error) {
		problem := classify(err)
		if problem.Status >= 500 {
			return BatchItem{}, err
		}
		item.Refusal = &problem
		return item, nil
	}

	current, err := load(ctx, tx, selectOne, membershipID)
	if err != nil {
		return refuse(err)
	}
	principal, status, version := current.PrincipalID, current.Status, current.Version
	item.PrincipalID, item.CurrentStatus, item.VersionRead = &principal, &status, &version

	cmd := Command{MembershipID: membershipID, ExpectedVersion: current.Version, Reason: reason}
	if err := cmd.validate(action); err != nil {
		return refuse(err)
	}
	next, err := decide(action, current, cmd.ExpectedVersion)
	if err != nil {
		return refuse(err)
	}
	item.ResultingStatus = &next
	return item, nil
}

// parentCorrelation reads the correlation a continuation keeps. The parent must be an executed
// batch of this Tenant: a continuation resubmits failed items, and a batch that never executed has
// none.
func parentCorrelation(ctx context.Context, tx db.Tx, parentID id.UUID) (id.UUID, error) {
	rows, err := tx.Query(ctx, selectParentBatch, parentID.String())
	if err != nil {
		return id.UUID{}, fmt.Errorf("membership: read continued batch: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return id.UUID{}, fmt.Errorf("membership: read continued batch: %w", err)
		}
		return id.UUID{}, fmt.Errorf("%w: continues names %s, which is not a batch of this Tenant",
			ErrInvalid, parentID)
	}
	var state, raw string
	if err := rows.Scan(&state, &raw); err != nil {
		return id.UUID{}, fmt.Errorf("membership: scan continued batch: %w", err)
	}
	if BatchState(state) != BatchExecuted {
		return id.UUID{}, fmt.Errorf("%w: continues names %s, which is %s and not executed",
			ErrInvalid, parentID, state)
	}
	correlation, err := id.Parse(raw)
	if err != nil {
		return id.UUID{}, fmt.Errorf("membership: stored correlation %q: %w", raw, err)
	}
	return correlation, nil
}

const batchColumns = `batch_id::text,
       tenant_id::text,
       action,
       state,
       reason,
       correlation_id::text,
       continues::text,
       created_by::text,
       created_at,
       expires_at,
       would_change,
       would_not_change,
       fail_on_errors,
       executed_by::text,
       executed_at,
       completed_at,
       heartbeat_at,
       resumed_by::text,
       resumed_at`

const selectBatch = `SELECT ` + batchColumns + `
FROM membership.membership_batch
WHERE batch_id = $1`

const selectBatchForUpdate = selectBatch + `
FOR UPDATE`

const selectBatchItems = `SELECT position,
       membership_id::text,
       principal_id::text,
       current_status,
       version_read,
       resulting_status,
       refusal::text,
       outcome,
       outcome_reason,
       accepted_at,
       event_id::text,
       resulting_version,
       problem::text
FROM membership.membership_batch_item
WHERE batch_id = $1
ORDER BY position`

// startExecution moves a preview to `executing` and gives the request executing it the lease.
const startExecution = `UPDATE membership.membership_batch
SET state = 'executing', executed_by = $2, executed_at = $3, fail_on_errors = $4,
    lease_id = $5, heartbeat_at = $3
WHERE batch_id = $1`

// resumeExecution hands the lease of an `executing` batch whose heartbeat is stale to the request
// resuming it. The old lease_id stops fencing anything the moment this commits.
const resumeExecution = `UPDATE membership.membership_batch
SET lease_id = $2, heartbeat_at = $3, resumed_by = $4, resumed_at = $3
WHERE batch_id = $1 AND state = 'executing'`

// heartbeat is the fence. It renews the lease of the request holding it and matches nothing for a
// request whose lease was taken over, which then rolls back the transaction it is in.
const heartbeat = `UPDATE membership.membership_batch
SET heartbeat_at = $3
WHERE batch_id = $1 AND lease_id = $2 AND state = 'executing'`

// lockItem takes the item's row lock and reports whether it still has no outcome. Read after the
// lock, so a request that waited behind another one's commit sees that commit's outcome.
const lockItem = `SELECT outcome IS NULL
FROM membership.membership_batch_item
WHERE batch_id = $1 AND position = $2
FOR UPDATE`

const recordSucceeded = `UPDATE membership.membership_batch_item
SET outcome = 'succeeded', accepted_at = $3, event_id = $4, resulting_version = $5
WHERE batch_id = $1 AND position = $2 AND outcome IS NULL`

const recordFailed = `UPDATE membership.membership_batch_item
SET outcome = 'failed', problem = $3::jsonb
WHERE batch_id = $1 AND position = $2 AND outcome IS NULL`

// settleUnattempted closes every item execution did not reach: refused at preview, or past the
// error allowance.
const settleUnattempted = `UPDATE membership.membership_batch_item
SET outcome = 'not_attempted',
    outcome_reason = CASE WHEN refusal IS NOT NULL THEN 'refused_at_preview' ELSE 'error_allowance' END
WHERE batch_id = $1 AND outcome IS NULL`

const completeExecution = `UPDATE membership.membership_batch
SET state = 'executed', completed_at = $3, heartbeat_at = $3
WHERE batch_id = $1 AND lease_id = $2 AND state = 'executing'`

// haltError is what the test seam's failure becomes: a stop, not an item failure.
type haltError struct{ error }

func (h haltError) Unwrap() error { return h.error }

// ExecuteBatch commits a preview, item by item, or resumes an execution whose request ended.
//
// The first transaction moves the batch to `executing` under its row lock and gives this request the
// lease -- so a second execution is refused rather than run twice -- and is the one an
// Idempotency-Key claim commits with. Each item that would change then runs TransitionWithin in a
// transaction of its own, named with the version its preview read, and records its outcome in that
// transaction; a Membership changed since the preview fails with the single command's version
// conflict. A failure is recorded in a transaction of its own. failOnErrors, when set, is the number
// of failures tolerated (SCIM's failOnErrors): the next one stops the run, and the rest are not
// attempted.
//
// An execute on a batch already `executing` resumes it when its heartbeat is older than BatchLease,
// and is refused with ErrBatchExecuting otherwise (TDD-organization-control-002 §Resuming an
// execution). The resume takes the lease over and continues the items with no outcome, each still
// held to the version its preview read; an item with an outcome is never applied again, because
// each item's transaction locks the item, finds its outcome and writes nothing. Every item's
// transaction renews the lease through the fence, so a request whose lease was taken over rolls its
// item back and stops with ErrBatchLeaseLost. The same Idempotency-Key that started the execution
// may resume it: its claim, never completed, is adopted, and completed with this response.
func (s *Service) ExecuteBatch(ctx context.Context, batchID id.UUID, failOnErrors *int, classify Classifier) (Batch, error) {
	switch {
	case classify == nil:
		return Batch{}, errors.New("membership: a classifier is required")
	case batchID.IsNil():
		return Batch{}, fmt.Errorf("%w: a batch identifier is required", ErrInvalid)
	case failOnErrors != nil && *failOnErrors < 0:
		return Batch{}, fmt.Errorf("%w: fail_on_errors must be 0 or more", ErrInvalid)
	}
	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Batch{}, db.ErrNoScope
	}
	lease, err := s.newID()
	if err != nil {
		return Batch{}, fmt.Errorf("membership: mint execution lease: %w", err)
	}
	now := s.now().UTC()
	ctx = db.AdoptInProgressClaim(ctx)

	var (
		batch    Batch
		answered bool
	)
	if err := db.WithTenantScope(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		var err error
		batch, err = loadBatch(ctx, tx, selectBatchForUpdate, batchID)
		if err != nil {
			return err
		}
		switch batch.State {
		case BatchPreviewed:
			if now.After(batch.ExpiresAt) {
				return fmt.Errorf("%w: %s expired at %s; preview it again",
					ErrBatchExpired, batchID, batch.ExpiresAt.Format(time.RFC3339))
			}
			var allowance any
			if failOnErrors != nil {
				allowance = *failOnErrors
			}
			if _, err := tx.Exec(ctx, startExecution, batchID.String(), scope.Actor().String(), now,
				allowance, lease.String()); err != nil {
				return fmt.Errorf("membership: start batch execution: %w", err)
			}
			batch.State, batch.FailOnErrors = BatchExecuting, failOnErrors
			return nil

		case BatchExecuting:
			if live := batch.HeartbeatAt; live != nil && !now.After(live.Add(BatchLease)) {
				return fmt.Errorf("%w: %s was last heard from at %s; read it, or send execute again after %s",
					ErrBatchExecuting, batchID, live.Format(time.RFC3339),
					live.Add(BatchLease).Format(time.RFC3339))
			}
			if failOnErrors != nil && (batch.FailOnErrors == nil || *batch.FailOnErrors != *failOnErrors) {
				return fmt.Errorf("%w: fail_on_errors was fixed when %s began executing; resume it without one or with the same value",
					ErrInvalid, batchID)
			}
			if _, err := tx.Exec(ctx, resumeExecution, batchID.String(), lease.String(), now,
				scope.Actor().String()); err != nil {
				return fmt.Errorf("membership: resume batch execution: %w", err)
			}
			return nil

		default:
			// An adopted claim on an executed batch is a request whose execution committed and whose
			// response was never recorded. Its answer is the batch as it ended.
			if batch.State == BatchExecuted && db.ClaimAdopted(ctx) {
				answered = true
				return nil
			}
			return fmt.Errorf("%w: %s is %s", ErrBatchNotPreviewed, batchID, batch.State)
		}
	}); err != nil {
		return Batch{}, err
	}
	if answered {
		return batch, nil
	}

	var reason string
	if batch.Reason != nil {
		reason = *batch.Reason
	}
	failures := 0
	for _, item := range batch.Items {
		if item.Outcome != nil && item.Outcome.Status == OutcomeFailed {
			failures++
		}
	}
	for _, item := range batch.Items {
		if item.Refusal != nil || item.VersionRead == nil || item.Outcome != nil {
			continue
		}
		if batch.FailOnErrors != nil && failures > *batch.FailOnErrors {
			break
		}

		err := s.executeItem(ctx, batch, lease, item, reason)
		if err == nil {
			continue
		}
		// A request that ended, a lease taken over, or an injected halt stops here and records
		// nothing for this item, which keeps it for whoever resumes the batch.
		var halted haltError
		if ctx.Err() != nil || errors.Is(err, ErrBatchLeaseLost) || errors.As(err, &halted) {
			return Batch{}, err
		}

		failures++
		problem := classify(err)
		encoded, encodeErr := problemJSON(&problem)
		if encodeErr != nil {
			return Batch{}, encodeErr
		}
		if err := db.WithTenantScope(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
			if err := fence(ctx, tx, batchID, lease, s.now().UTC()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, recordFailed, batchID.String(), item.Position, encoded); err != nil {
				return fmt.Errorf("membership: record batch item failure: %w", err)
			}
			return nil
		}); err != nil {
			return Batch{}, err
		}
	}

	completed := s.now().UTC()
	if err := db.WithTenantScope(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		tag, err := tx.Exec(ctx, completeExecution, batchID.String(), lease.String(), completed)
		if err != nil {
			return fmt.Errorf("membership: complete batch execution: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: %s", ErrBatchLeaseLost, batchID)
		}
		if _, err := tx.Exec(ctx, settleUnattempted, batchID.String()); err != nil {
			return fmt.Errorf("membership: settle unattempted batch items: %w", err)
		}
		batch, err = loadBatch(ctx, tx, selectBatch, batchID)
		return err
	}); err != nil {
		return Batch{}, err
	}
	return batch, nil
}

// executeItem applies one item under the lease, in its own transaction: the fence first, so a request
// that lost the lease does nothing; then the item's lock, so an item another request already settled
// is left alone; then the single transition, held to the version the preview read, and its outcome.
func (s *Service) executeItem(ctx context.Context, batch Batch, lease id.UUID, item BatchItem, reason string) error {
	return db.WithTenantScope(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		if err := fence(ctx, tx, batch.BatchID, lease, s.now().UTC()); err != nil {
			return err
		}
		var open bool
		if err := tx.QueryRow(ctx, lockItem, batch.BatchID.String(), item.Position).Scan(&open); err != nil {
			return fmt.Errorf("membership: lock batch item: %w", err)
		}
		if !open {
			return nil
		}
		result, err := s.TransitionWithin(ctx, tx, batch.Action, Command{
			MembershipID: item.MembershipID, ExpectedVersion: *item.VersionRead, Reason: reason,
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, recordSucceeded, batch.BatchID.String(), item.Position,
			result.AcceptedAt, result.EventID.String(), result.Membership.Version); err != nil {
			return fmt.Errorf("membership: record batch item outcome: %w", err)
		}
		if s.halt != nil {
			if err := s.halt(ctx, item.Position); err != nil {
				return haltError{err}
			}
		}
		return nil
	})
}

// fence renews the lease, or reports that it is no longer this request's.
func fence(ctx context.Context, tx db.Tx, batchID, lease id.UUID, at time.Time) error {
	tag, err := tx.Exec(ctx, heartbeat, batchID.String(), lease.String(), at)
	if err != nil {
		return fmt.Errorf("membership: renew batch lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: %s", ErrBatchLeaseLost, batchID)
	}
	return nil
}

// GetBatch reads one batch of the bound Tenant. Another Tenant's is absent under its policy.
func (s *Service) GetBatch(ctx context.Context, batchID id.UUID) (Batch, error) {
	if batchID.IsNil() {
		return Batch{}, fmt.Errorf("%w: a batch identifier is required", ErrInvalid)
	}
	var batch Batch
	if err := db.WithTenantRead(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		var err error
		batch, err = loadBatch(ctx, tx, selectBatch, batchID)
		return err
	}); err != nil {
		return Batch{}, err
	}
	batch.settle(s.now().UTC())
	return batch, nil
}

func loadBatch(ctx context.Context, tx db.Tx, statement string, batchID id.UUID) (Batch, error) {
	rows, err := tx.Query(ctx, statement, batchID.String())
	if err != nil {
		return Batch{}, fmt.Errorf("membership: read batch: %w", err)
	}
	if !rows.Next() {
		rows.Close()
		if err := rows.Err(); err != nil {
			return Batch{}, fmt.Errorf("membership: read batch: %w", err)
		}
		return Batch{}, fmt.Errorf("%w: %s", ErrBatchNotFound, batchID)
	}
	batch, err := scanBatch(rows)
	rows.Close()
	if err != nil {
		return Batch{}, err
	}

	items, err := tx.Query(ctx, selectBatchItems, batchID.String())
	if err != nil {
		return Batch{}, fmt.Errorf("membership: read batch items: %w", err)
	}
	defer items.Close()
	for items.Next() {
		item, err := scanBatchItem(items)
		if err != nil {
			return Batch{}, err
		}
		batch.Items = append(batch.Items, item)
	}
	if err := items.Err(); err != nil {
		return Batch{}, fmt.Errorf("membership: read batch items: %w", err)
	}
	return batch, nil
}

func scanBatch(r rowScanner) (Batch, error) {
	var (
		batch                              Batch
		rawBatch, rawTenant, action, state string
		rawCorrelation, rawCreator         string
		rawContinues, rawExecutor          *string
		rawResumer                         *string
		failOnErrors                       *int
	)
	if err := r.Scan(&rawBatch, &rawTenant, &action, &state, &batch.Reason, &rawCorrelation,
		&rawContinues, &rawCreator, &batch.CreatedAt, &batch.ExpiresAt, &batch.WouldChange,
		&batch.WouldNotChange, &failOnErrors, &rawExecutor, &batch.ExecutedAt, &batch.CompletedAt,
		&batch.HeartbeatAt, &rawResumer, &batch.ResumedAt); err != nil {
		return Batch{}, fmt.Errorf("membership: scan batch: %w", err)
	}
	parsed, err := parseAll(rawBatch, rawTenant, rawCorrelation, rawCreator)
	if err != nil {
		return Batch{}, err
	}
	batch.BatchID, batch.TenantID, batch.CorrelationID, batch.CreatedBy = parsed[0], parsed[1], parsed[2], parsed[3]
	if batch.Continues, err = parseOptional(rawContinues); err != nil {
		return Batch{}, err
	}
	if batch.ExecutedBy, err = parseOptional(rawExecutor); err != nil {
		return Batch{}, err
	}
	if batch.ResumedBy, err = parseOptional(rawResumer); err != nil {
		return Batch{}, err
	}
	batch.Action, batch.State, batch.FailOnErrors = Action(action), BatchState(state), failOnErrors
	return batch, nil
}

func scanBatchItem(r rowScanner) (BatchItem, error) {
	var (
		item                   BatchItem
		rawMembership          string
		rawPrincipal, rawEvent *string
		current, resulting     *string
		refusal, problem       *string
		outcome, outcomeReason *string
		acceptedAt             *time.Time
		resultingVersion       *int64
	)
	if err := r.Scan(&item.Position, &rawMembership, &rawPrincipal, &current, &item.VersionRead,
		&resulting, &refusal, &outcome, &outcomeReason, &acceptedAt, &rawEvent, &resultingVersion,
		&problem); err != nil {
		return BatchItem{}, fmt.Errorf("membership: scan batch item: %w", err)
	}
	membershipID, err := id.Parse(rawMembership)
	if err != nil {
		return BatchItem{}, fmt.Errorf("membership: stored identifier %q: %w", rawMembership, err)
	}
	item.MembershipID = membershipID
	if item.PrincipalID, err = parseOptional(rawPrincipal); err != nil {
		return BatchItem{}, err
	}
	if current != nil {
		state := State(*current)
		item.CurrentStatus = &state
	}
	if resulting != nil {
		state := State(*resulting)
		item.ResultingStatus = &state
	}
	if item.Refusal, err = decodeProblem(refusal); err != nil {
		return BatchItem{}, err
	}
	if outcome == nil {
		return item, nil
	}
	item.Outcome = &ItemOutcome{Status: *outcome, AcceptedAt: acceptedAt, Version: resultingVersion}
	if outcomeReason != nil {
		item.Outcome.Reason = *outcomeReason
	}
	if item.Outcome.EventID, err = parseOptional(rawEvent); err != nil {
		return BatchItem{}, err
	}
	if item.Outcome.Problem, err = decodeProblem(problem); err != nil {
		return BatchItem{}, err
	}
	return item, nil
}

func problemJSON(problem *Problem) (any, error) {
	if problem == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(problem)
	if err != nil {
		return nil, fmt.Errorf("membership: encode problem: %w", err)
	}
	return string(encoded), nil
}

func decodeProblem(raw *string) (*Problem, error) {
	if raw == nil {
		return nil, nil
	}
	var problem Problem
	if err := json.Unmarshal([]byte(*raw), &problem); err != nil {
		return nil, fmt.Errorf("membership: stored problem: %w", err)
	}
	return &problem, nil
}

func parseOptional(raw *string) (*id.UUID, error) {
	if raw == nil {
		return nil, nil
	}
	parsed, err := id.Parse(*raw)
	if err != nil {
		return nil, fmt.Errorf("membership: stored identifier %q: %w", *raw, err)
	}
	return &parsed, nil
}

func nullableUUIDPointer(value *id.UUID) any {
	if value == nil {
		return nil
	}
	return value.String()
}

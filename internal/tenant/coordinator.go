package tenant

// The ProvisioningCoordinator of TDD-organization-control-003 §"Component Design": desired-state
// publication and realized-status correlation.
//
// It owns the three transitions the table in §"Who issues which transition" assigns to it —
// `requested -> provisioning`, `provisioning -> failed`, `failed -> provisioning` — and none of the
// others. Activation stays a deliberate act in `Service.Activate`, because a provisioning system
// reporting success is not the same statement as an operator deciding the Tenant may take
// Memberships.
//
// # Why the transitions run through Service and not through SQL here
//
// Every move this package makes goes through `transitionWithin`, so the refusal rules, the version
// increment, the timestamp, and the silence are the same code the operator-driven commands run. A
// coordinator that wrote its own UPDATE would be a second implementation of the state machine,
// reachable only from a path no operator exercises — which is where a divergence would survive
// longest before anybody noticed.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// RequestState is the lifecycle of one provisioning request.
type RequestState string

const (
	// RequestRequested is the state a command is recorded in when it is sent outward.
	RequestRequested RequestState = "requested"

	// RequestRealized means the external system confirmed the boundary exists.
	RequestRealized RequestState = "realized"

	// RequestFailed means provisioning was refused.
	RequestFailed RequestState = "failed"

	// RequestUnresolved is an ambiguous outcome, and a first-class state rather than an error code.
	//
	// SAD-004 §7.5 requires an ambiguous provisioning outcome to remain pending or failed and never
	// to be inferred as success. A timeout after the request left is `unresolved`: the target may
	// have built the boundary or may not, and treating that as failure and retrying is how a Tenant
	// gets provisioned twice.
	RequestUnresolved RequestState = "unresolved"
)

// Valid reports whether the state is one this service persists. The set mirrors
// `provisioning_state_check` in schema.hcl.
func (r RequestState) Valid() bool {
	switch r {
	case RequestRequested, RequestRealized, RequestFailed, RequestUnresolved:
		return true
	}
	return false
}

// resolved reports whether the state is an outcome rather than a pending command.
func (r RequestState) resolved() bool {
	return r == RequestRealized || r == RequestFailed
}

var (
	// ErrNoProvisioningRequest reports that no provisioning command matches the correlation
	// identifier an outcome was reported against.
	//
	// A caller error rather than an internal one: the identifier came from the request. It maps to
	// 404, which tells the provisioning system it is reporting against a command this service has
	// no record of sending — the case worth investigating, because the alternative reading is that
	// two systems disagree about what was asked for.
	ErrNoProvisioningRequest = errors.New("tenant: no provisioning request matches that correlation identifier")

	// ErrAmbiguousCorrelation reports that one correlation identifier matches provisioning requests
	// for more than one Tenant.
	//
	// Refused rather than resolved by picking the most recent. A correlation identifier is per
	// request, so two Tenants sharing one means both were created by a single call — and resolving
	// the newest would silently mark the wrong Tenant's boundary as built. Fail closed: an operator
	// resolves these one Tenant at a time.
	ErrAmbiguousCorrelation = errors.New("tenant: that correlation identifier matches more than one Tenant")

	// ErrOutcomeAlreadyRecorded reports an attempt to change a resolved outcome.
	//
	// A realized request cannot become failed and a failed one cannot become realized. A retry is a
	// new attempt with its own request row and its own correlation identifier, which is what keeps
	// the history of a twice-provisioned Tenant readable — overwriting the first outcome would erase
	// the evidence that there had been two.
	ErrOutcomeAlreadyRecorded = errors.New("tenant: that provisioning request already has an outcome")

	// ErrProvisioningNotRequested refuses to advance a Tenant into `provisioning` when nothing has
	// been asked for. Without it, `requested -> provisioning` would be an operator asserting that a
	// boundary is being built, and activation would then be waiting on a realized status for a
	// command that was never sent.
	ErrProvisioningNotRequested = errors.New("tenant: no provisioning command is outstanding for this Tenant")
)

// Coordinator holds desired provisioning state and correlates realized status back to it.
type Coordinator struct {
	pool    *db.ProviderPool
	tenants *Service

	// timeout is the age at which an unanswered request becomes `unresolved`, from
	// `ORGANIZATION_PROVISIONING_TIMEOUT`.
	timeout time.Duration

	now   func() time.Time
	newID func() (id.UUID, error)
}

// NewCoordinator constructs the coordinator.
//
// It takes the Service rather than reaching into the table itself, so the state machine has one
// implementation. A non-positive timeout is refused rather than defaulted: the value decides when
// this service declares an outcome unknown, and a coordinator that invented its own would sweep on a
// schedule no operator chose.
func NewCoordinator(pool *db.ProviderPool, tenants *Service, timeout time.Duration) (*Coordinator, error) {
	switch {
	case pool == nil:
		return nil, errors.New("tenant: a provider-scoped pool is required")
	case tenants == nil:
		return nil, errors.New("tenant: the tenant service is required")
	case timeout <= 0:
		return nil, errors.New("tenant: a positive provisioning timeout is required")
	}
	return &Coordinator{pool: pool, tenants: tenants, timeout: timeout, now: time.Now, newID: id.NewV7}, nil
}

// request is one row of tenant.provisioning_request, narrowed to what correlation reads.
type request struct {
	RequestID id.UUID
	TenantID  id.UUID
	State     RequestState
}

// Resolution is what a recorded outcome reports back.
type Resolution struct {
	RequestID id.UUID
	TenantID  id.UUID

	// State is the outcome now stored against the request.
	State RequestState

	// Tenant is the Tenant after any transition this outcome drove. On a replay it is the Tenant as
	// it stands, unchanged.
	Tenant Tenant

	// Replay reports that the outcome was already recorded and this call changed nothing.
	//
	// Returned rather than folded into an error, because a provisioning system retrying a delivery it
	// is not sure arrived is behaving correctly, and answering it with a failure would make the
	// correct behaviour look broken. TDD-organization-control-003 §Testing requires a duplicate
	// realized status to produce one effect; this is how a caller can tell that is what happened.
	Replay bool

	ResolvedAt time.Time
}

// Outcome is a realized status reported back by the provisioning system.
type Outcome struct {
	// CorrelationID is what the request is matched on, per
	// TDD-organization-control-003 §"Provisioning Correlation": "match by correlation identifier".
	CorrelationID id.UUID

	// Detail is free text for an operator. Required on a failure and optional on a success: a
	// refusal that says only "failed" leaves the retry decision with nothing to go on.
	Detail string

	// Reason is the administrative reason recorded as provider-access evidence.
	Reason string
}

func (o Outcome) validate(requireDetail bool) error {
	switch {
	case o.CorrelationID.IsNil():
		return fmt.Errorf("%w: a correlation identifier is required", ErrInvalid)
	case strings.TrimSpace(o.Reason) == "":
		return fmt.Errorf("%w: a reason is required", ErrInvalid)
	case requireDetail && strings.TrimSpace(o.Detail) == "":
		return fmt.Errorf("%w: a failed provisioning outcome requires a detail", ErrInvalid)
	}
	return nil
}

// correlate reads every provisioning request against one correlation identifier.
//
// `FOR UPDATE` because the outcome about to be written depends on the state read here, and two
// deliveries of the same realized status would otherwise both see `requested` and both act.
//
// The operation filter keeps this to the provisioning direction. `internal/offboarding` records its
// deprovisioning commands in the same table and correlates them itself; without the filter a
// realized deprovisioning could be resolved by this path and would then advance a Tenant that is
// being wound down.
const correlateStatement = `SELECT request_id::text, tenant_id::text, state
FROM tenant.provisioning_request
WHERE correlation_id = $1
  AND coalesce(desired_profile->>'operation', 'provision') = 'provision'
ORDER BY requested_at DESC, request_id DESC
FOR UPDATE`

func correlate(ctx context.Context, tx db.Tx, correlationID id.UUID) (request, error) {
	rows, err := tx.Query(ctx, correlateStatement, correlationID.String())
	if err != nil {
		return request{}, fmt.Errorf("tenant: correlate provisioning request: %w", err)
	}
	defer rows.Close()

	var matched []request
	for rows.Next() {
		var (
			next     request
			rawReq   string
			rawTen   string
			rawState string
		)
		if err := rows.Scan(&rawReq, &rawTen, &rawState); err != nil {
			return request{}, fmt.Errorf("tenant: scan provisioning request: %w", err)
		}
		if next.RequestID, err = id.Parse(rawReq); err != nil {
			return request{}, fmt.Errorf("tenant: stored request identifier %q: %w", rawReq, err)
		}
		if next.TenantID, err = id.Parse(rawTen); err != nil {
			return request{}, fmt.Errorf("tenant: stored tenant identifier %q: %w", rawTen, err)
		}
		next.State = RequestState(rawState)
		if !next.State.Valid() {
			return request{}, fmt.Errorf("tenant: stored provisioning state %q is not declared", rawState)
		}
		matched = append(matched, next)
	}
	if err := rows.Err(); err != nil {
		return request{}, fmt.Errorf("tenant: read provisioning requests: %w", err)
	}

	switch len(matched) {
	case 0:
		return request{}, fmt.Errorf("%w: %s", ErrNoProvisioningRequest, correlationID)
	case 1:
		return matched[0], nil
	}

	// More than one row is only ambiguous if they belong to different Tenants. Two attempts on one
	// Tenant sharing a correlation identifier is unusual but unambiguous, and the most recent is the
	// one the outcome is about.
	for _, candidate := range matched[1:] {
		if candidate.TenantID != matched[0].TenantID {
			return request{}, fmt.Errorf("%w: %s", ErrAmbiguousCorrelation, correlationID)
		}
	}
	return matched[0], nil
}

const resolveStatement = `UPDATE tenant.provisioning_request
SET state = $2, resolved_at = $3, detail = $4
WHERE request_id = $1`

// Realize records a confirmed boundary and leaves activation to an operator.
//
// The Tenant is advanced to `provisioning` if it is still `requested`, and otherwise left alone. It
// is not activated: TDD-organization-control-003 §"Provisioning Correlation" ends this step at
// "awaiting explicit activation", and the reason is in §"Tenant Activation" — activation also checks
// the sponsoring Organization, which is a decision about the customer relationship rather than about
// infrastructure, and the provisioning system has no view of it.
func (c *Coordinator) Realize(ctx context.Context, outcome Outcome) (Resolution, error) {
	return c.record(ctx, outcome, RequestRealized, false)
}

// Fail records a refusal and moves the Tenant to `failed`.
//
// `failed` is not terminal. The cause is usually external and usually fixable, so a retry moves back
// to `provisioning` through `Provision`, with a new request row.
func (c *Coordinator) Fail(ctx context.Context, outcome Outcome) (Resolution, error) {
	return c.record(ctx, outcome, RequestFailed, true)
}

func (c *Coordinator) record(ctx context.Context, outcome Outcome, to RequestState,
	requireDetail bool) (Resolution, error) {
	if err := outcome.validate(requireDetail); err != nil {
		return Resolution{}, err
	}

	resolvedAt := c.now().UTC()
	var resolution Resolution

	if err := db.WithProviderScope(ctx, c.pool, outcome.Reason,
		func(ctx context.Context, tx db.Tx) error {
			matched, err := correlate(ctx, tx, outcome.CorrelationID)
			if err != nil {
				return err
			}

			current, err := load(ctx, tx, matched.TenantID)
			if err != nil {
				return err
			}

			// A replay of the same outcome changes nothing and reports success. A *different*
			// outcome against a resolved request is refused: an attempt has one result, and a second
			// one is a new attempt with its own row.
			if matched.State.resolved() {
				if matched.State != to {
					return fmt.Errorf("%w: %s is %s and cannot become %s",
						ErrOutcomeAlreadyRecorded, matched.RequestID, matched.State, to)
				}
				resolution = Resolution{
					RequestID: matched.RequestID, TenantID: matched.TenantID, State: matched.State,
					Tenant: current, Replay: true, ResolvedAt: resolvedAt,
				}
				return nil
			}

			if _, err := tx.Exec(ctx, resolveStatement, matched.RequestID.String(),
				string(to), resolvedAt, nullableText(strings.TrimSpace(outcome.Detail))); err != nil {
				return fmt.Errorf("tenant: record provisioning outcome: %w", err)
			}

			current, err = c.advance(ctx, tx, current, to, outcome.Reason)
			if err != nil {
				return err
			}

			resolution = Resolution{
				RequestID: matched.RequestID, TenantID: matched.TenantID, State: to,
				Tenant: current, ResolvedAt: resolvedAt,
			}
			return nil
		}); err != nil {
		return Resolution{}, err
	}
	return resolution, nil
}

// advance moves the Tenant to the position the outcome implies.
//
// # Why a realized status can advance `requested -> provisioning`
//
// The design's happy path records the dispatch first, so a realized status normally arrives at a
// Tenant already in `provisioning`. A provisioning system that confirms before the dispatch was
// recorded is not misbehaving — it is faster than the round trip — and refusing it would leave a
// Tenant in `requested` with a realized boundary and no path to activation. So the transition is
// applied here if it has not been already.
//
// # Why a failure traverses two transitions
//
// The machine has no `requested -> failed` edge, and this does not add one. A refusal reported
// against a Tenant still in `requested` walks the declared path — `provision` then `fail` — which is
// safe precisely because both are silent and neither increments the security version. Adding the
// missing edge would have been the smaller diff and the larger change: the state machine is
// asserted as a whole table, and an edge added for one caller's convenience is an edge every other
// caller inherits.
func (c *Coordinator) advance(ctx context.Context, tx db.Tx, current Tenant, to RequestState,
	reason string) (Tenant, error) {
	steps := make([]Action, 0, 2)
	switch {
	case to == RequestRealized && current.Status == StateRequested:
		steps = append(steps, ActionProvision)
	case to == RequestFailed && current.Status == StateRequested:
		steps = append(steps, ActionProvision, ActionFail)
	case to == RequestFailed && current.Status == StateProvisioning:
		steps = append(steps, ActionFail)
	}

	for _, action := range steps {
		// The expected version is read from the row rather than supplied.
		//
		// The optimistic check exists to stop two operators acting from two stale views; this caller
		// has no view — it is acting on a correlated status about an attempt this service recorded
		// itself, and the row is already locked by `correlate` and `load`. Passing the stored version
		// keeps `Command.validate` honest without inventing a check that would only ever compare a
		// value to itself.
		result, err := c.tenants.transitionWithin(ctx, tx, action, Command{
			TenantID:        current.TenantID,
			Reason:          reason,
			ExpectedVersion: current.Version,
		}, nil)
		if err != nil {
			return Tenant{}, err
		}
		current = result.Tenant
	}
	return current, nil
}

// Provision records that the desired state has left and moves the Tenant into `provisioning`.
//
// It serves both edges the machine gives `ActionProvision`. From `requested` it is the dispatch of
// the request intake already recorded, and it refuses unless that request is still outstanding —
// without the check, an operator could assert that a boundary is being built when nothing was asked
// for, and activation would then wait on a status no system will send. From `failed` it is a retry,
// and it writes a *new* request row with its own correlation identifier, because the previous
// attempt's outcome is evidence and reusing its row would erase it.
func (c *Coordinator) Provision(ctx context.Context, cmd Command) (Result, error) {
	if err := cmd.validate(); err != nil {
		return Result{}, err
	}

	requestID, err := c.newID()
	if err != nil {
		return Result{}, fmt.Errorf("tenant: mint provisioning-request identifier: %w", err)
	}
	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Result{}, fmt.Errorf("tenant: no scope is bound to this context")
	}
	requestedAt := c.now().UTC()

	var result Result
	if err := db.WithProviderScope(ctx, c.pool, cmd.Reason,
		func(ctx context.Context, tx db.Tx) error {
			var err error
			result, err = c.tenants.transitionWithin(ctx, tx, ActionProvision, cmd,
				func(ctx context.Context, tx db.Tx, current Tenant) error {
					if current.Status == StateFailed {
						return c.retry(ctx, tx, current, requestID, scope.Correlation(), requestedAt)
					}
					return outstandingRequest(ctx, tx, current.TenantID)
				})
			return err
		}); err != nil {
		return Result{}, err
	}
	return result, nil
}

// retry copies the desired profile from the Tenant row into a fresh request.
//
// From the row rather than from the caller: the profile was fixed at creation and a retry is the
// same intention sent again. Taking it from the request body would let a retry quietly change what
// is being built, and the Tenant row would then disagree with the last thing anybody was asked for.
const retryStatement = `INSERT INTO tenant.provisioning_request
    (request_id, tenant_id, desired_profile, state, correlation_id, requested_at)
SELECT $1, tenant_id, jsonb_strip_nulls(jsonb_build_object(
           'operation', 'provision',
           'isolation_profile', isolation_profile,
           'residency_region', residency_region)),
       'requested', $3, $4
FROM tenant.tenant
WHERE tenant_id = $2`

func (c *Coordinator) retry(ctx context.Context, tx db.Tx, current Tenant,
	requestID, correlationID id.UUID, requestedAt time.Time) error {
	if _, err := tx.Exec(ctx, retryStatement, requestID.String(), current.TenantID.String(),
		correlationID.String(), requestedAt); err != nil {
		return fmt.Errorf("tenant: record provisioning retry: %w", err)
	}
	return nil
}

// outstandingRequest reports whether a provisioning command is still awaiting an outcome.
//
// `unresolved` counts as outstanding. A request that timed out may still be in flight — that is what
// makes it unresolved rather than failed — so a dispatch recorded against it is a statement about
// the same attempt, not a new one.
const outstandingStatement = `SELECT count(*)
FROM tenant.provisioning_request
WHERE tenant_id = $1
  AND coalesce(desired_profile->>'operation', 'provision') = 'provision'
  AND state IN ('requested', 'unresolved')`

func outstandingRequest(ctx context.Context, tx db.Tx, tenantID id.UUID) error {
	var count int
	if err := tx.QueryRow(ctx, outstandingStatement, tenantID.String()).Scan(&count); err != nil {
		return fmt.Errorf("tenant: count outstanding provisioning requests: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: %s", ErrProvisioningNotRequested, tenantID)
	}
	return nil
}

// sweepStatement ages unanswered commands into `unresolved`.
//
// Both directions, not only provisioning. The timeout is a property of the correlation table rather
// than of one flow, and `internal/offboarding` already refuses retirement on an `unresolved`
// deprovisioning — a gate that nothing could reach until something produced the state.
//
// It sets no Tenant transition and starts no retry. TDD-organization-control-003 §"Provisioning
// Correlation" is explicit: an unresolved request is never retried automatically, because retrying
// an operation whose outcome is unknown is how a Tenant gets provisioned twice. Reconciliation
// queries the provisioning system and resolves it, which is an operator's act with a real answer
// behind it.
const sweepStatement = `UPDATE tenant.provisioning_request
SET state = 'unresolved',
    resolved_at = $1,
    detail = coalesce(detail, 'no realized status within the provisioning timeout')
WHERE request_id IN (
    SELECT request_id FROM tenant.provisioning_request
    WHERE state = 'requested' AND requested_at < $2
    ORDER BY requested_at
    LIMIT $3
)`

// SweepUnresolved ages out commands that never came back.
//
// Batched like `invitation.ExpireLapsed`, and for the same reason: an unbounded UPDATE over a table
// that grows with the estate is a lock held for as long as it takes, and the caller that triggers it
// is a scheduler with a request timeout.
func (c *Coordinator) SweepUnresolved(ctx context.Context, size int) (int64, error) {
	if size <= 0 {
		return 0, fmt.Errorf("%w: a positive batch size is required", ErrInvalid)
	}

	at := c.now().UTC()
	cutoff := at.Add(-c.timeout)

	var affected int64
	if err := db.WithProviderScope(ctx, c.pool, "age unanswered provisioning requests to unresolved",
		func(ctx context.Context, tx db.Tx) error {
			tag, err := tx.Exec(ctx, sweepStatement, at, cutoff, size)
			if err != nil {
				return fmt.Errorf("tenant: sweep unresolved provisioning requests: %w", err)
			}
			affected = tag.RowsAffected()
			return nil
		}); err != nil {
		return 0, err
	}
	return affected, nil
}

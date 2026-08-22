package tenant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/system"
)

// Service performs authoritative Tenant transitions.
//
// # Why the provider pool
//
// Every transition here is cross-Tenant administration. A Tenant does not suspend, restore, or
// activate itself: the decision is taken by the provider, and `organization_rt` holds no SELECT on
// `organization.organization` at all — so the activation precondition on the sponsoring
// Organization cannot even be evaluated on a tenant-scoped connection. Binding this service to the
// provider pool is therefore not a convenience; it is the only binding under which the checks the
// design requires are executable, and it carries the reason and the recorded evidence that
// PAD-PLT-002 §3.3 invariant 22 requires with it.
type Service struct {
	pool *db.ProviderPool

	// now is a seam for tests. The accepted timestamp is a recorded origin rather than an observed
	// one, and a service that read the wall clock could not be asserted against a fixed instant.
	now func() time.Time

	// beforeAppend runs after the row is updated and before the outbox append, and is nil outside
	// tests. It exists because the atomicity of those two writes is the property this design rests
	// on, and the only honest way to assert it is to fail inside the window it protects.
	beforeAppend func(context.Context) error
}

// New constructs the service.
func New(pool *db.ProviderPool) (*Service, error) {
	if pool == nil {
		return nil, errors.New("tenant: a provider-scoped pool is required")
	}
	return &Service{pool: pool, now: time.Now}, nil
}

var (
	// ErrProvisioningNotRealized refuses activation while the external system that owns the
	// isolation boundary has not confirmed it exists. Activating first would let Memberships be
	// granted into a boundary that is not there.
	ErrProvisioningNotRealized = errors.New("tenant: provisioning has not been realized")

	// ErrSponsorNotActive refuses activation under a suspended or retired Organization. Checked at
	// activation rather than at creation, because a Tenant may legitimately be requested while its
	// Organization is still being onboarded — it may not become active under a sponsor that is not.
	ErrSponsorNotActive = errors.New("tenant: the sponsoring Organization is not active")
)

// Activate is the only path into active.
//
// It carries two preconditions beyond the state machine, both from
// TDD-organization-control-003 §"Tenant Activation", and both evaluated inside the same
// transaction as the update. Evaluated before the transaction they would be a check against a
// state that can change before the write lands, which for the sponsor check means a Tenant going
// active under an Organization suspended a moment earlier.
func (s *Service) Activate(ctx context.Context, cmd Command) (Result, error) {
	return s.transition(ctx, ActionActivate, cmd, func(ctx context.Context, tx db.Tx, current Tenant) error {
		realized, err := provisioningRealized(ctx, tx, current.TenantID)
		if err != nil {
			return err
		}
		if !realized {
			return ErrProvisioningNotRealized
		}
		return sponsorActive(ctx, tx, current.OrganizationID)
	})
}

// Suspend withholds every context in the Tenant reversibly, and increments the security version so
// consumers reject tokens issued before it.
func (s *Service) Suspend(ctx context.Context, cmd Command) (Result, error) {
	return s.transition(ctx, ActionSuspend, cmd, nil)
}

// Restore returns a suspended Tenant to active, and increments the security version so consumers
// stop honouring a cached denial.
func (s *Service) Restore(ctx context.Context, cmd Command) (Result, error) {
	return s.transition(ctx, ActionRestore, cmd, nil)
}

// guard is a precondition evaluated inside the transaction, after the row is locked and before
// anything is written.
type guard func(ctx context.Context, tx db.Tx, current Tenant) error

const selectForUpdate = `SELECT tenant_id::text,
       organization_id::text,
       status,
       version,
       tenant_security_version
FROM tenant.tenant
WHERE tenant_id = $1
FOR UPDATE`

func (s *Service) transition(ctx context.Context, action Action, cmd Command, check guard) (Result, error) {
	if err := cmd.validate(); err != nil {
		return Result{}, err
	}

	acceptedAt := s.now().UTC()
	var (
		record    Tenant
		published bool
	)

	if err := db.WithProviderScope(ctx, s.pool, cmd.Reason, func(ctx context.Context, tx db.Tx) error {
		current, err := load(ctx, tx, cmd.TenantID)
		if err != nil {
			return err
		}

		// Resolved before anything is written. A refused transition must leave no trace, and a
		// check performed after the update would be relying on the rollback rather than on not
		// having tried.
		next, err := Resolve(action, current.Status)
		if err != nil {
			return err
		}

		// The version check comes after the state check on purpose. A caller acting on a stale
		// view most often gets both wrong, and "this transition is not permitted from suspended"
		// tells an operator what happened; "version 4 is not version 5" tells them only that
		// something did.
		if current.Version != cmd.ExpectedVersion {
			return fmt.Errorf("%w: expected %d, stored %d",
				ErrVersionMismatch, cmd.ExpectedVersion, current.Version)
		}

		if check != nil {
			if err := check(ctx, tx, current); err != nil {
				return err
			}
		}

		updated, err := apply(ctx, tx, action, cmd.TenantID, next, acceptedAt)
		if err != nil {
			return err
		}
		current.Status = next
		current.Version = updated.version
		current.SecurityVersion = updated.securityVersion
		record = current

		published, err = s.appendEvent(ctx, tx, action, record, acceptedAt)
		return err
	}); err != nil {
		return Result{}, err
	}

	return Result{Tenant: record, AcceptedAt: acceptedAt, Published: published}, nil
}

type versions struct {
	version         int64
	securityVersion int64
}

// apply builds the UPDATE for one action.
//
// Assembled rather than written out five times, because the parts that vary between actions are
// exactly the parts that must not drift: whether the security version increments, and which
// lifecycle timestamp is stamped. Five hand-written statements would let one of them forget the
// increment, and the omission reads as a missing line rather than as a wrong one.
//
// The column names are interpolated and the values are not. Both come from `transitions`, which is
// a constant table in this package — no request field reaches this string, and a column named here
// that does not exist in schema.hcl fails on the first call rather than silently.
func apply(ctx context.Context, tx db.Tx, action Action, tenantID id.UUID, next State,
	acceptedAt time.Time) (versions, error) {
	r := transitions[action]

	// `version = version + 1` rather than a value computed in Go: two transitions that read the
	// same version would have one write a number the other already used. The row lock above
	// serialises them, and this keeps the increment correct even if the lock is ever removed.
	sets := []string{"status = $2", "version = version + 1", "updated_at = now()"}
	if r.securityVersion {
		sets = append(sets, "tenant_security_version = tenant_security_version + 1")
	}
	if r.stamp != "" {
		// Stamped with the accepted instant rather than with now(), so the lifecycle fact in the
		// row and the time in the published envelope are the same instant. `updated_at` keeps
		// now() because it is database housekeeping and answers a different question.
		sets = append(sets, r.stamp+" = $3")
	}
	if r.clear != "" {
		sets = append(sets, r.clear+" = NULL")
	}

	statement := `UPDATE tenant.tenant SET ` + strings.Join(sets, ", ") +
		` WHERE tenant_id = $1 RETURNING version, tenant_security_version`

	args := []any{tenantID.String(), string(next)}
	if r.stamp != "" {
		args = append(args, acceptedAt)
	}

	var updated versions
	if err := tx.QueryRow(ctx, statement, args...).Scan(&updated.version, &updated.securityVersion); err != nil {
		return versions{}, fmt.Errorf("tenant: apply %s: %w", action, err)
	}
	return updated, nil
}

// appendEvent writes the event inside the caller's transaction, and reports whether there was one.
func (s *Service) appendEvent(ctx context.Context, tx db.Tx, action Action, record Tenant,
	occurredAt time.Time) (bool, error) {
	if s.beforeAppend != nil {
		if err := s.beforeAppend(ctx); err != nil {
			return false, err
		}
	}

	eventType, publishes, err := EventType(action)
	if err != nil {
		return false, err
	}
	if !publishes {
		return false, nil
	}

	envelope, err := event.New(system.Source, eventType, occurredAt, NewPayload(record))
	if err != nil {
		return false, fmt.Errorf("tenant: build envelope: %w", err)
	}

	// The aggregate is the Tenant, which is also the partition key. Kafka preserves order only
	// inside one partition, so partitioning by the aggregate is what gives a consumer per-Tenant
	// ordering — the guarantee it depends on, as opposed to the global ordering ADR-GLB-003 §5
	// states is not available.
	//
	// The lane comes from the action rather than from the state just written. Asking the machine
	// again after the update would answer about a transition out of the new state, which for a
	// suspension is refused — and the event would quietly take the standard lane, queued behind a
	// lifecycle backlog.
	var opts []outbox.Option
	if Priority(action) {
		opts = append(opts, outbox.Priority())
	}

	if err := outbox.Append(ctx, tx, record.TenantID, envelope, opts...); err != nil {
		return false, fmt.Errorf("tenant: append event: %w", err)
	}
	return true, nil
}

func load(ctx context.Context, tx db.Tx, tenantID id.UUID) (Tenant, error) {
	var (
		record       Tenant
		rawTenant    string
		rawOrganizat string
		status       string
	)
	if err := tx.QueryRow(ctx, selectForUpdate, tenantID.String()).Scan(
		&rawTenant, &rawOrganizat, &status, &record.Version, &record.SecurityVersion); err != nil {
		return Tenant{}, fmt.Errorf("%w: %s", ErrNotFound, tenantID)
	}

	parsed, err := id.Parse(rawTenant)
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant: stored identifier %q is unparseable: %w", rawTenant, err)
	}
	organization, err := id.Parse(rawOrganizat)
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant: stored organization %q is unparseable: %w", rawOrganizat, err)
	}
	record.TenantID, record.OrganizationID = parsed, organization

	record.Status = State(status)
	if !record.Status.Valid() {
		return Tenant{}, fmt.Errorf("tenant: stored status %q is not in the state machine", status)
	}
	return record, nil
}

// provisioningRealized reports whether the most recent provisioning request for this Tenant came
// back realized.
//
// The most recent rather than any: a failed attempt followed by a successful retry must activate,
// and a realized attempt followed by a later failure must not. `EXISTS ... AND state = 'realized'`
// would satisfy the first case and get the second one wrong in the permissive direction.
const provisioningStatement = `SELECT state
FROM tenant.provisioning_request
WHERE tenant_id = $1
ORDER BY requested_at DESC, request_id DESC
LIMIT 1`

func provisioningRealized(ctx context.Context, tx db.Tx, tenantID id.UUID) (bool, error) {
	var state string
	if err := tx.QueryRow(ctx, provisioningStatement, tenantID.String()).Scan(&state); err != nil {
		// No request at all is not realized. Reported as the same refusal rather than as an
		// internal error: from the caller's side "provisioning has not confirmed" is true either
		// way, and the distinction belongs in the operator's view of the Tenant.
		return false, nil
	}
	return state == "realized", nil
}

const sponsorStatement = `SELECT status FROM organization.organization WHERE organization_id = $1`

func sponsorActive(ctx context.Context, tx db.Tx, organizationID id.UUID) error {
	var status string
	if err := tx.QueryRow(ctx, sponsorStatement, organizationID.String()).Scan(&status); err != nil {
		return fmt.Errorf("tenant: read sponsoring organization: %w", err)
	}
	if status != "active" {
		return fmt.Errorf("%w: %s is %s", ErrSponsorNotActive, organizationID, status)
	}
	return nil
}

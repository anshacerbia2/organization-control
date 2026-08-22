package membership

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anshacerbia2/foundation-platform/event"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/system"
)

// Service performs authoritative Membership mutations.
//
// Every transition commits the status change, the version increment, and the outbox append in one
// transaction. A revocation that commits without its event is unreachable by every consumer,
// which is the exact failure the transactional outbox exists to prevent: authority says revoked,
// every projection says active, and nothing in the system disagrees out loud.
type Service struct {
	pool *db.TenantPool

	// now is a seam for tests. TDD-organization-control-002 requires acknowledgement to carry an
	// accepted timestamp so enforcement delay is measured from a recorded origin, and a service
	// that reads the wall clock cannot be asserted against a fixed one.
	now func() time.Time

	// newID mints Membership identifiers. A seam for the same reason as `now`, and not
	// configuration: production always mints a UUIDv7.
	newID func() (id.UUID, error)

	// beforeAppend runs after the status change and before the outbox append, and is nil outside
	// tests.
	//
	// It exists because the atomicity claim is the exit criterion of this design, and the only
	// honest way to assert it is to fail in the window it protects. Without a seam a test can
	// prove the two statements run, never that they roll back together.
	beforeAppend func(context.Context) error
}

// New constructs the service.
func New(pool *db.TenantPool) (*Service, error) {
	if pool == nil {
		return nil, errors.New("membership: a tenant-scoped pool is required")
	}
	return &Service{pool: pool, now: time.Now, newID: id.NewV7}, nil
}

// GrantRequest creates a Membership.
type GrantRequest struct {
	PrincipalID id.UUID
	TenantID    id.UUID

	// WorkspaceID is optional. Left nil, the Membership is scoped to the Tenant as a whole.
	WorkspaceID id.UUID

	SubjectType string
	Provenance  string
	ValidFrom   time.Time
	ValidUntil  time.Time
}

// Result is what a committed transition reports back.
type Result struct {
	Membership Membership

	// AcceptedAt is a durability statement and not an enforcement one. Per
	// TDD-organization-control-002 and STD-IAM-001 §3.4, acknowledgement means the change is
	// durable and queued; it MUST NOT be read as enforced until the declared mechanisms have
	// applied. The operational dashboard shows accepted and enforced separately for that reason,
	// because incident response works from the second one and the first is the convenient one.
	AcceptedAt time.Time

	// TenantSecurityVersion is carried in the event, so it is returned as well: a caller
	// correlating its own audit record with what consumers received needs the same pair.
	TenantSecurityVersion int64
}

const insertStatement = `INSERT INTO membership.membership
    (membership_id, principal_id, tenant_id, workspace_id, subject_type, status,
     valid_from, valid_until, provenance)
VALUES ($1, $2, $3, $4, $5, 'active', $6, $7, $8)`

// Grant creates an active Membership.
//
// The Tenant is not taken from the request. It is read from the bound scope, so a caller cannot
// grant a Membership in a Tenant it is not administering — and the RLS `WITH CHECK` refuses the
// row as a second line of defence if this check is ever removed.
func (s *Service) Grant(ctx context.Context, req GrantRequest) (Result, error) {
	scope, ok := db.ScopeFrom(ctx)
	if !ok {
		return Result{}, db.ErrNoScope
	}
	if err := validateGrant(req, scope.TenantID()); err != nil {
		return Result{}, err
	}

	membershipID, err := s.newID()
	if err != nil {
		return Result{}, fmt.Errorf("membership: mint identifier: %w", err)
	}

	record := Membership{
		MembershipID: membershipID,
		PrincipalID:  req.PrincipalID,
		TenantID:     scope.TenantID(),
		WorkspaceID:  req.WorkspaceID,
		SubjectType:  req.SubjectType,
		Status:       StateActive,
		Version:      1,
		ValidFrom:    req.ValidFrom,
		ValidUntil:   req.ValidUntil,
		Provenance:   req.Provenance,
	}

	acceptedAt := s.now().UTC()
	var securityVersion int64

	if err := db.WithTenantScope(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		version, readErr := tenantSecurityVersion(ctx, tx, scope.TenantID())
		if readErr != nil {
			return readErr
		}
		securityVersion = version

		if _, execErr := tx.Exec(ctx, insertStatement,
			record.MembershipID.String(), record.PrincipalID.String(), record.TenantID.String(),
			nullableUUID(record.WorkspaceID), record.SubjectType,
			record.ValidFrom, nullableTime(record.ValidUntil), record.Provenance); execErr != nil {
			return fmt.Errorf("membership: insert: %w", execErr)
		}
		return s.appendEvent(ctx, tx, ActionGrant, record, securityVersion, acceptedAt)
	}); err != nil {
		return Result{}, err
	}

	return Result{Membership: record, AcceptedAt: acceptedAt, TenantSecurityVersion: securityVersion}, nil
}

// Suspend withholds the context reversibly.
func (s *Service) Suspend(ctx context.Context, membershipID id.UUID) (Result, error) {
	return s.transition(ctx, ActionSuspend, membershipID)
}

// Revoke withholds it permanently.
func (s *Service) Revoke(ctx context.Context, membershipID id.UUID) (Result, error) {
	return s.transition(ctx, ActionRevoke, membershipID)
}

// Restore returns a suspended Membership to active.
func (s *Service) Restore(ctx context.Context, membershipID id.UUID) (Result, error) {
	return s.transition(ctx, ActionRestore, membershipID)
}

const selectForUpdate = `SELECT membership_id::text,
       principal_id::text,
       tenant_id::text,
       coalesce(workspace_id::text, ''),
       subject_type,
       status,
       membership_version
FROM membership.membership
WHERE membership_id = $1
FOR UPDATE`

// updateStatement increments the version in the same statement that changes the status.
//
// `membership_version = membership_version + 1` rather than a value computed in Go: two concurrent
// transitions read the same version, and the one that computed it would write a version the other
// already used. The row lock above serialises them, and this makes the increment correct even if
// the lock is ever removed.
const updateStatement = `UPDATE membership.membership
SET status = $2,
    membership_version = membership_version + 1,
    updated_at = now()
WHERE membership_id = $1
RETURNING membership_version`

func (s *Service) transition(ctx context.Context, action Action, membershipID id.UUID) (Result, error) {
	if membershipID.IsNil() {
		return Result{}, errors.New("membership: a membership identifier is required")
	}
	if _, ok := db.ScopeFrom(ctx); !ok {
		return Result{}, db.ErrNoScope
	}

	acceptedAt := s.now().UTC()
	var (
		record          Membership
		securityVersion int64
	)

	if err := db.WithTenantScope(ctx, s.pool, func(ctx context.Context, tx db.Tx) error {
		current, loadErr := load(ctx, tx, membershipID)
		if loadErr != nil {
			return loadErr
		}

		// Resolved before anything is written. A refused transition must leave no trace, and a
		// check performed after the update would rely on the rollback rather than on not having
		// tried.
		next, _, resolveErr := Resolve(action, current.Status)
		if resolveErr != nil {
			return resolveErr
		}

		version, readErr := tenantSecurityVersion(ctx, tx, current.TenantID)
		if readErr != nil {
			return readErr
		}
		securityVersion = version

		var updatedVersion int64
		if scanErr := tx.QueryRow(ctx, updateStatement,
			membershipID.String(), string(next)).Scan(&updatedVersion); scanErr != nil {
			return fmt.Errorf("membership: update status: %w", scanErr)
		}

		current.Status = next
		current.Version = updatedVersion
		record = current

		return s.appendEvent(ctx, tx, action, record, securityVersion, acceptedAt)
	}); err != nil {
		return Result{}, err
	}

	return Result{Membership: record, AcceptedAt: acceptedAt, TenantSecurityVersion: securityVersion}, nil
}

// appendEvent writes the event inside the caller's transaction.
func (s *Service) appendEvent(ctx context.Context, tx db.Tx, action Action, record Membership,
	securityVersion int64, occurredAt time.Time) error {
	if s.beforeAppend != nil {
		if err := s.beforeAppend(ctx); err != nil {
			return err
		}
	}

	eventType, err := EventType(action)
	if err != nil {
		return err
	}
	envelope, err := event.New(system.Source, eventType, occurredAt, NewPayload(record, securityVersion))
	if err != nil {
		return fmt.Errorf("membership: build envelope: %w", err)
	}

	// The aggregate is the Membership, which is also the partition key a producer uses. Kafka
	// preserves order only inside one partition, so partitioning by the aggregate is what gives
	// a consumer per-Membership ordering — the guarantee it actually depends on, as opposed to
	// the global ordering ADR-GLB-003 §5 states is not available.
	// The lane comes from the action, read straight out of the state machine. It cannot be
	// derived from the state just written: Resolve refuses a transition out of the state it just
	// produced, so asking it again after the update would answer "refused" and quietly drop the
	// event into the standard lane — a revocation queued behind a lifecycle backlog.
	var opts []outbox.Option
	if Priority(action) {
		opts = append(opts, outbox.Priority())
	}

	if err := outbox.Append(ctx, tx, record.MembershipID, envelope, opts...); err != nil {
		return fmt.Errorf("membership: append event: %w", err)
	}
	return nil
}

func load(ctx context.Context, tx db.Tx, membershipID id.UUID) (Membership, error) {
	var (
		record       Membership
		rawID        string
		rawPrincipal string
		rawTenant    string
		rawWorkspace string
		status       string
	)
	if err := tx.QueryRow(ctx, selectForUpdate, membershipID.String()).Scan(
		&rawID, &rawPrincipal, &rawTenant, &rawWorkspace,
		&record.SubjectType, &status, &record.Version); err != nil {
		// Under Row-Level Security a Membership in another Tenant is simply absent, which is the
		// correct answer to give: reporting that it exists elsewhere would leak the existence of
		// a row this caller may not read.
		return Membership{}, fmt.Errorf("%w: %s", ErrNotFound, membershipID)
	}

	parsed, err := parseAll(rawID, rawPrincipal, rawTenant, rawWorkspace)
	if err != nil {
		return Membership{}, err
	}
	record.MembershipID, record.PrincipalID, record.TenantID, record.WorkspaceID = parsed[0], parsed[1], parsed[2], parsed[3]
	record.Status = State(status)
	if !record.Status.Valid() {
		return Membership{}, fmt.Errorf("membership: stored status %q is not in the state machine", status)
	}
	return record, nil
}

const tenantSecurityVersionStatement = `SELECT tenant_security_version FROM tenant.tenant WHERE tenant_id = $1`

// tenantSecurityVersion is read inside the same transaction as the mutation.
//
// Read separately and earlier, a Tenant suspension committing in between would produce an event
// carrying a version older than the state it describes — and a consumer comparing versions would
// classify the newer Membership change as superseded and keep serving revoked access.
func tenantSecurityVersion(ctx context.Context, tx db.Tx, tenantID id.UUID) (int64, error) {
	var version int64
	if err := tx.QueryRow(ctx, tenantSecurityVersionStatement, tenantID.String()).Scan(&version); err != nil {
		return 0, fmt.Errorf("membership: read tenant security version: %w", err)
	}
	return version, nil
}

func validateGrant(req GrantRequest, boundTenant id.UUID) error {
	switch {
	case req.PrincipalID.IsNil():
		return errors.New("membership: a principal identifier is required")
	case req.SubjectType != "human" && req.SubjectType != "workload":
		return fmt.Errorf("membership: subject_type %q is not human or workload", req.SubjectType)
	case req.Provenance == "":
		// PAD-PLT-002 §3.2 defines provenance as how the Membership came to exist. A row without
		// it cannot answer whether access arrived by invitation, migration, or provider grant,
		// which is the first question an access review asks.
		return errors.New("membership: provenance is required")
	case req.ValidFrom.IsZero():
		return errors.New("membership: valid_from is required")
	case !req.ValidUntil.IsZero() && !req.ValidUntil.After(req.ValidFrom):
		return errors.New("membership: valid_until must be after valid_from")
	}
	// A request naming a Tenant other than the bound one is refused here rather than left to the
	// policy. SAD-004 §8.3: a Tenant identifier arriving with a request is a *requested* scope,
	// and the mismatch is refused before any statement runs.
	if !req.TenantID.IsNil() && req.TenantID != boundTenant {
		return fmt.Errorf("membership: the request names Tenant %s and the bound scope is %s", req.TenantID, boundTenant)
	}
	return nil
}

func parseAll(values ...string) ([]id.UUID, error) {
	parsed := make([]id.UUID, 0, len(values))
	for _, value := range values {
		if value == "" {
			parsed = append(parsed, id.UUID{})
			continue
		}
		next, err := id.Parse(value)
		if err != nil {
			return nil, fmt.Errorf("membership: stored identifier %q is unparseable: %w", value, err)
		}
		parsed = append(parsed, next)
	}
	return parsed, nil
}

func nullableUUID(value id.UUID) any {
	if value.IsNil() {
		return nil
	}
	return value.String()
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

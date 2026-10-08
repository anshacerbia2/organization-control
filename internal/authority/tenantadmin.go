package authority

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/membership"
)

// Tenant administration grants (ADR-ORG-003, TDD-organization-control-001 §The Tenant Administration
// Grant). A token's tenant_id selects a Tenant and confers nothing: a Principal administers the Tenant
// while it holds an active Membership there, the Tenant is active, and a grant recorded here is in
// force. A provider grants and revokes, inside the one Tenant, through db.WithProviderInTenant, so
// the writes run under that Tenant's policy and the restrictive policies on the table see the acting
// provider.

var (
	// ErrTenantAdminInvalid means a grant or revocation request is incomplete.
	ErrTenantAdminInvalid = errors.New("authority: invalid tenant administration request")

	// ErrAdminTenantNotFound means no Tenant has the identifier.
	ErrAdminTenantNotFound = errors.New("authority: no Tenant has this identifier")

	// ErrAdminTenantNotActive means the Tenant is not active, and nobody administers it but a
	// provider (ADR-ORG-003 §5.3).
	ErrAdminTenantNotActive = errors.New("authority: the Tenant is not active; only a provider administers it")

	// ErrAlreadyAdministrator means the Principal already holds a grant in force in the Tenant.
	ErrAlreadyAdministrator = errors.New("authority: the Principal already administers this Tenant")

	// ErrMembershipSuspended means the Principal's Tenant-wide Membership is suspended. A second,
	// active one would undo the suspension without anyone restoring it.
	ErrMembershipSuspended = errors.New("authority: the Principal's Membership in this Tenant is suspended; restore it before granting administration")

	// ErrAdminGrantNotFound means no grant in the Tenant has the identifier.
	ErrAdminGrantNotFound = errors.New("authority: no tenant administration grant in this Tenant has this identifier")

	// ErrAdminGrantRevoked means the grant was revoked before.
	ErrAdminGrantRevoked = errors.New("authority: the tenant administration grant is already revoked")
)

// TenantAdminGrant is one tenant administration grant, in force or revoked.
type TenantAdminGrant struct {
	ID           id.UUID
	TenantID     id.UUID
	Principal    id.UUID
	GrantedBy    id.UUID
	Reason       string
	GrantedAt    time.Time
	RevokedAt    *time.Time
	RevokedBy    *id.UUID
	RevokeReason string
}

// Administrator is what a grant did: the grant, and the Principal's Tenant-wide Membership, which
// the grant created when the Principal held none.
type Administrator struct {
	Grant             TenantAdminGrant
	MembershipID      id.UUID
	MembershipCreated bool
}

// TenantAdministration grants, revokes and lists tenant administration grants, as a provider acting
// inside one Tenant.
type TenantAdministration struct {
	provider    *db.ProviderPool
	tenants     *db.TenantPool
	memberships *membership.Service
	id          func() (id.UUID, error)
	now         func() time.Time
}

// NewTenantAdministration builds it. The provider pool records each access; the tenant pool runs
// the work; the Membership service grants a first administrator's Membership.
func NewTenantAdministration(provider *db.ProviderPool, tenants *db.TenantPool,
	memberships *membership.Service) (*TenantAdministration, error) {
	switch {
	case provider == nil:
		return nil, errors.New("authority: a provider pool is required")
	case tenants == nil:
		return nil, errors.New("authority: a tenant pool is required")
	case memberships == nil:
		return nil, errors.New("authority: a membership service is required")
	}
	return &TenantAdministration{provider: provider, tenants: tenants, memberships: memberships,
		id: id.NewV7, now: time.Now}, nil
}

const adminGrantColumns = `SELECT grant_id::text, tenant_id::text, principal_id::text, granted_by::text, reason,
       granted_at, revoked_at, coalesce(revoked_by::text, ''), coalesce(revoke_reason, '')
FROM membership.tenant_admin_grant`

const adminGrantListStatement = adminGrantColumns + `
WHERE tenant_id = $1
ORDER BY granted_at DESC`

const adminGrantOneStatement = adminGrantColumns + `
WHERE grant_id = $1 AND tenant_id = $2`

// adminTenantStatusStatement reads the Tenant's status. Under the Tenant's policy, a Tenant that does
// not exist reads as no row, which coalesce turns into an empty status.
const adminTenantStatusStatement = `SELECT coalesce((SELECT status FROM tenant.tenant WHERE tenant_id = $1), '')`

// adminGrantInsertStatement inserts the grant unless one is in force. The partial unique index is the
// guard, against two concurrent grants as well; ON CONFLICT turns its refusal into a count of zero.
const adminGrantInsertStatement = `WITH inserted AS (
    INSERT INTO membership.tenant_admin_grant (grant_id, tenant_id, principal_id, granted_by, reason)
    VALUES ($1, $2, $3, $4, $5)
    ON CONFLICT (principal_id, tenant_id) WHERE revoked_at IS NULL DO NOTHING
    RETURNING grant_id)
SELECT count(*) FROM inserted`

// adminMembershipStatement reads the Principal's Tenant-wide Membership that is not revoked, an
// active one first, locked so a concurrent suspension waits for this grant. One row always: none
// reads as two empty strings.
const adminMembershipStatement = `WITH held AS (
    SELECT membership_id, status FROM membership.membership
    WHERE principal_id = $1 AND tenant_id = $2 AND workspace_id IS NULL AND status <> 'revoked'
    ORDER BY (status = 'active') DESC
    LIMIT 1
    FOR UPDATE)
SELECT coalesce((SELECT membership_id::text FROM held), ''), coalesce((SELECT status FROM held), '')`

const adminGrantCountStatement = `SELECT count(*) FROM membership.tenant_admin_grant
WHERE grant_id = $1 AND tenant_id = $2`

const adminGrantRevokeStatement = `UPDATE membership.tenant_admin_grant
SET revoked_at = now(), revoked_by = $3, revoke_reason = $4
WHERE grant_id = $1 AND tenant_id = $2 AND revoked_at IS NULL`

// List reads every grant in the Tenant, newest first, revoked ones included: each is the record of
// who administered the Tenant and when that ended (AC-2(7)(b), ADR-ORG-003 §5.4).
func (a *TenantAdministration) List(ctx context.Context, tenantID id.UUID, reason string) ([]TenantAdminGrant, error) {
	grants := []TenantAdminGrant{}
	err := db.WithProviderInTenant(ctx, a.provider, a.tenants, tenantID, reason, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, adminGrantListStatement, tenantID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			grant, err := scanAdminGrant(rows)
			if err != nil {
				return err
			}
			grants = append(grants, grant)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("authority: list tenant administration grants: %w", err)
	}
	return grants, nil
}

// Grant makes the Principal an administrator of the Tenant, recording the calling provider as
// granted_by and the reason as the grant's. A Principal with no Tenant-wide Membership gets one, in
// the same transaction, with its event (ADR-ORG-003 §5.2).
func (a *TenantAdministration) Grant(ctx context.Context, tenantID, principal id.UUID, reason string) (Administrator, error) {
	scope, ok := db.ScopeFrom(ctx)
	switch {
	case !ok:
		return Administrator{}, db.ErrNoScope
	case tenantID.IsNil():
		return Administrator{}, fmt.Errorf("%w: a tenant_id is required", ErrTenantAdminInvalid)
	case principal.IsNil():
		return Administrator{}, fmt.Errorf("%w: a principal_id is required", ErrTenantAdminInvalid)
	case strings.TrimSpace(reason) == "":
		return Administrator{}, fmt.Errorf("%w: a reason is required", ErrTenantAdminInvalid)
	}
	grantID, err := a.id()
	if err != nil {
		return Administrator{}, fmt.Errorf("authority: mint grant identifier: %w", err)
	}

	var result Administrator
	err = db.WithProviderInTenant(ctx, a.provider, a.tenants, tenantID, reason, func(ctx context.Context, tx db.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, adminTenantStatusStatement, tenantID.String()).Scan(&status); err != nil {
			return err
		}
		switch status {
		case "":
			return ErrAdminTenantNotFound
		case "active":
		default:
			return ErrAdminTenantNotActive
		}

		// The grant first: a second concurrent grant waits on the index here, then stops, and never
		// reaches the Membership's own unique index.
		var inserted int
		if err := tx.QueryRow(ctx, adminGrantInsertStatement, grantID.String(), tenantID.String(), principal.String(),
			scope.Actor().String(), strings.TrimSpace(reason)).Scan(&inserted); err != nil {
			return err
		}
		if inserted == 0 {
			return ErrAlreadyAdministrator
		}

		var held, heldStatus string
		if err := tx.QueryRow(ctx, adminMembershipStatement, principal.String(), tenantID.String()).
			Scan(&held, &heldStatus); err != nil {
			return err
		}
		switch membership.State(heldStatus) {
		case membership.StateActive:
			if result.MembershipID, err = id.Parse(held); err != nil {
				return err
			}
		case "":
			granted, err := a.memberships.GrantWithin(ctx, tx, membership.GrantRequest{
				PrincipalID: principal,
				TenantID:    tenantID,
				SubjectType: "human",
				Provenance:  "tenant administration grant " + grantID.String(),
				ValidFrom:   a.now().UTC(),
			})
			if err != nil {
				return err
			}
			result.MembershipID, result.MembershipCreated = granted.Membership.MembershipID, true
		default:
			return ErrMembershipSuspended
		}

		if result.Grant, err = scanAdminGrant(tx.QueryRow(ctx, adminGrantOneStatement, grantID.String(), tenantID.String())); err != nil {
			return err
		}
		return db.Respond(ctx, tx, result)
	})
	if err != nil {
		for _, sentinel := range []error{ErrAdminTenantNotFound, ErrAdminTenantNotActive, ErrAlreadyAdministrator,
			ErrMembershipSuspended} {
			if errors.Is(err, sentinel) {
				return Administrator{}, sentinel
			}
		}
		return Administrator{}, fmt.Errorf("authority: grant tenant administration: %w", err)
	}
	return result, nil
}

// Revoke ends a grant, recording the calling provider and the reason. The Principal's Membership is
// untouched: it stays a member, and stops administering at the next request.
func (a *TenantAdministration) Revoke(ctx context.Context, tenantID, grantID id.UUID, reason string) (TenantAdminGrant, error) {
	scope, ok := db.ScopeFrom(ctx)
	switch {
	case !ok:
		return TenantAdminGrant{}, db.ErrNoScope
	case tenantID.IsNil() || grantID.IsNil():
		return TenantAdminGrant{}, fmt.Errorf("%w: a tenant_id and a grant identifier are required", ErrTenantAdminInvalid)
	case strings.TrimSpace(reason) == "":
		return TenantAdminGrant{}, fmt.Errorf("%w: a reason is required", ErrTenantAdminInvalid)
	}

	var grant TenantAdminGrant
	err := db.WithProviderInTenant(ctx, a.provider, a.tenants, tenantID, reason, func(ctx context.Context, tx db.Tx) error {
		var found int
		if err := tx.QueryRow(ctx, adminGrantCountStatement, grantID.String(), tenantID.String()).Scan(&found); err != nil {
			return err
		}
		if found == 0 {
			return ErrAdminGrantNotFound
		}
		tag, err := tx.Exec(ctx, adminGrantRevokeStatement, grantID.String(), tenantID.String(),
			scope.Actor().String(), strings.TrimSpace(reason))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrAdminGrantRevoked
		}
		if grant, err = scanAdminGrant(tx.QueryRow(ctx, adminGrantOneStatement, grantID.String(), tenantID.String())); err != nil {
			return err
		}
		return db.Respond(ctx, tx, grant)
	})
	if err != nil {
		for _, sentinel := range []error{ErrAdminGrantNotFound, ErrAdminGrantRevoked} {
			if errors.Is(err, sentinel) {
				return TenantAdminGrant{}, sentinel
			}
		}
		return TenantAdminGrant{}, fmt.Errorf("authority: revoke tenant administration: %w", err)
	}
	return grant, nil
}

func scanAdminGrant(row scanner) (TenantAdminGrant, error) {
	var (
		grant                                       TenantAdminGrant
		grantID, tenantID, principal, by, revokedBy string
	)
	if err := row.Scan(&grantID, &tenantID, &principal, &by, &grant.Reason, &grant.GrantedAt, &grant.RevokedAt,
		&revokedBy, &grant.RevokeReason); err != nil {
		return TenantAdminGrant{}, err
	}
	parsed, err := parseAll(grantID, tenantID, principal, by)
	if err != nil {
		return TenantAdminGrant{}, err
	}
	grant.ID, grant.TenantID, grant.Principal, grant.GrantedBy = parsed[0], parsed[1], parsed[2], parsed[3]
	if revokedBy != "" {
		value, err := id.Parse(revokedBy)
		if err != nil {
			return TenantAdminGrant{}, err
		}
		grant.RevokedBy = &value
	}
	return grant, nil
}

func parseAll(values ...string) ([]id.UUID, error) {
	parsed := make([]id.UUID, len(values))
	for i, value := range values {
		var err error
		if parsed[i], err = id.Parse(value); err != nil {
			return nil, err
		}
	}
	return parsed, nil
}

// TenantStanding is what a Tenant's records say about a Principal (ADR-ORG-003 §5.3).
type TenantStanding struct {
	// Member is an active Membership in the Tenant, inside its validity window.
	Member bool
	// TenantActive is the Tenant's status being active.
	TenantActive bool
	// Granted is a tenant administration grant in force.
	Granted bool
}

// Administers is all three: the Principal administers the Tenant.
func (s TenantStanding) Administers() bool { return s.Member && s.TenantActive && s.Granted }

// TenantRecords reads a tenant caller's standing, on the tenant connections, under the Tenant's own
// policy.
type TenantRecords struct {
	tenants *db.TenantPool
}

// NewTenantRecords builds the reader on the tenant pool.
func NewTenantRecords(tenants *db.TenantPool) (*TenantRecords, error) {
	if tenants == nil {
		return nil, errors.New("authority: a tenant pool is required")
	}
	return &TenantRecords{tenants: tenants}, nil
}

// tenantStandingStatement is one row always: three indexed lookups by principal_id and tenant_id.
const tenantStandingStatement = `SELECT
    EXISTS (SELECT 1 FROM membership.membership m
            WHERE m.principal_id = $1 AND m.tenant_id = $2 AND m.status = 'active'
              AND m.valid_from <= now() AND (m.valid_until IS NULL OR now() < m.valid_until)),
    EXISTS (SELECT 1 FROM tenant.tenant t WHERE t.tenant_id = $2 AND t.status = 'active'),
    EXISTS (SELECT 1 FROM membership.tenant_admin_grant g
            WHERE g.principal_id = $1 AND g.tenant_id = $2 AND g.revoked_at IS NULL)`

// TenantStanding reads the Principal's standing in the Tenant, in one read-only transaction bound to
// it. A revoked Membership or grant, or a Tenant no longer active, confers nothing from the next
// request on.
func (r *TenantRecords) TenantStanding(ctx context.Context, principal, tenantID, correlation id.UUID) (TenantStanding, error) {
	scope, err := db.TenantScope(tenantID, principal, correlation)
	if err != nil {
		return TenantStanding{}, fmt.Errorf("authority: %w", err)
	}
	var standing TenantStanding
	if err := db.WithTenantRead(db.WithScope(ctx, scope), r.tenants, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, tenantStandingStatement, principal.String(), tenantID.String()).
			Scan(&standing.Member, &standing.TenantActive, &standing.Granted)
	}); err != nil {
		return TenantStanding{}, fmt.Errorf("authority: read tenant standing: %w", err)
	}
	return standing, nil
}

package controldb_test

// The self role, executed as itself (ADR-ORG-005, TDD-organization-control-001 1.17.0 §Roles): the
// tenant login sets it LOCAL, binds one Principal, and reads that Principal's rows and nothing else.
// The tenant role, which may set it and does not inherit it, gains none of its policies.

import (
	"context"
	"testing"

	"github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
)

func TestTheSelfRoleReadsOnlyItsPrincipal(t *testing.T) {
	admin, ctx := openAdmin(t)
	tenantLogin, _ := tenantPool(t)

	exec := func(statement string, args ...any) {
		t.Helper()
		if err := admin.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, statement, args...)
			return err
		}); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	newID := func() id.UUID {
		value, err := id.NewV7()
		if err != nil {
			t.Fatalf("NewV7: %v", err)
		}
		return value
	}

	person, other := newID(), newID()
	organizationID, shared, elsewhere := newID(), newID(), newID()
	exec(`INSERT INTO organization.organization (organization_id, display_name, classification, status)
	    VALUES ($1, 'self role suite', 'customer', 'active')`, organizationID.String())
	for _, tenantID := range []id.UUID{shared, elsewhere} {
		exec(`INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
		    VALUES ($1, $2, 'self role suite', 'active', 'pooled')`, tenantID.String(), organizationID.String())
	}
	member := func(principal, tenantID id.UUID) {
		exec(`INSERT INTO membership.membership
		    (membership_id, principal_id, tenant_id, subject_type, status, membership_version, valid_from, provenance)
		    VALUES ($1, $2, $3, 'human', 'active', 1, now(), 'self role suite')`,
			newID().String(), principal.String(), tenantID.String())
	}
	member(person, shared)
	member(other, shared)
	member(other, elsewhere)
	t.Cleanup(func() {
		for _, tenantID := range []id.UUID{shared, elsewhere} {
			exec(`DELETE FROM membership.membership WHERE tenant_id = $1`, tenantID.String())
			exec(`DELETE FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String())
		}
		exec(`DELETE FROM organization.organization WHERE organization_id = $1`, organizationID.String())
	})

	// asSelf runs fn as the self role bound to principal, unless principal is nil.
	asSelf := func(principal *id.UUID, fn func(context.Context, db.Tx) error) error {
		return tenantLogin.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE organization_self_rt`); err != nil {
				return err
			}
			if principal != nil {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true)`, principal.String()); err != nil {
					return err
				}
			}
			return fn(ctx, tx)
		})
	}
	count := func(statement string, args ...any) (int, error) {
		var n int
		err := asSelf(&person, func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, statement, args...).Scan(&n)
		})
		return n, err
	}

	if n, err := count(`SELECT count(membership_id) FROM membership.membership WHERE principal_id <> $1`, person.String()); err != nil || n != 0 {
		t.Errorf("the self role read %d of another Principal's Memberships (err %v), want 0", n, err)
	}
	if n, err := count(`SELECT count(membership_id) FROM membership.membership`); err != nil || n != 1 {
		t.Errorf("the self role read %d Memberships (err %v), want its one", n, err)
	}
	if n, err := count(`SELECT count(tenant_id) FROM tenant.tenant WHERE tenant_id = $1`, elsewhere.String()); err != nil || n != 0 {
		t.Errorf("the self role read %d Tenants the Principal is not a member of (err %v), want 0", n, err)
	}
	if n, err := count(`SELECT count(tenant_id) FROM tenant.tenant WHERE tenant_id = $1`, shared.String()); err != nil || n != 1 {
		t.Errorf("the self role read %d of the Principal's own Tenant (err %v), want 1", n, err)
	}
	if _, err := count(`SELECT count(provenance) FROM membership.membership`); err == nil {
		t.Error("the self role read a Membership column it was not granted")
	}
	if _, err := count(`SELECT count(*) FROM operation.offboarding`); err == nil {
		t.Error("the self role read a table it was not granted")
	}
	if err := asSelf(&person, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE membership.membership SET status = 'revoked' WHERE principal_id = $1`, person.String())
		return err
	}); err == nil {
		t.Error("the self role updated its own Membership")
	}
	if err := asSelf(nil, func(ctx context.Context, tx db.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(membership_id) FROM membership.membership`).Scan(&n)
	}); err == nil {
		t.Error("an unbound self read returned rows instead of raising")
	}

	// The tenant role, bound to one Tenant and carrying app.principal_id, still sees that Tenant
	// alone: the self policies are not the tenant role's.
	if err := tenantLogin.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.principal_id', $2, true)`,
			shared.String(), other.String()); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM membership.membership WHERE tenant_id = $1`,
			elsewhere.String()).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("the tenant role read %d rows of another Tenant through the self policies", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("tenant read: %v", err)
	}
}

// TestTheSelfRoleReadsOnlyTheInvitationItsTokenSelects: bound to one token hash, the self role reads
// that invitation's granted columns and no other invitation, writes nothing, and raises unbound. The
// tenant role reaches nothing through invitation_self_redeem (ADR-ORG-007 §5.2).
func TestTheSelfRoleReadsOnlyTheInvitationItsTokenSelects(t *testing.T) {
	admin, ctx := openAdmin(t)
	tenantLogin, _ := tenantPool(t)

	exec := func(statement string, args ...any) {
		t.Helper()
		if err := admin.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, statement, args...)
			return err
		}); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	newID := func() id.UUID {
		value, err := id.NewV7()
		if err != nil {
			t.Fatalf("NewV7: %v", err)
		}
		return value
	}

	organizationID, tenantA, tenantB := newID(), newID(), newID()
	exec(`INSERT INTO organization.organization (organization_id, display_name, classification, status)
	    VALUES ($1, 'self invitation suite', 'customer', 'active')`, organizationID.String())
	invite := func(tenantID id.UUID, hash string) {
		exec(`INSERT INTO invitation.invitation
		    (invitation_id, tenant_id, target_identifier, target_hash, token_hash, subject_type, invited_by,
		     state, correlation_id, expires_at)
		    VALUES ($1, $2, 'person@example.test', $3, $4, 'human', $5, 'pending', $6, now() + interval '1 day')`,
			newID().String(), tenantID.String(), "target-"+hash, hash, newID().String(), newID().String())
	}
	for _, tenantID := range []id.UUID{tenantA, tenantB} {
		exec(`INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
		    VALUES ($1, $2, 'self invitation suite', 'active', 'pooled')`, tenantID.String(), organizationID.String())
	}
	mine, theirs := "hash-mine-"+newID().String(), "hash-theirs-"+newID().String()
	invite(tenantA, mine)
	invite(tenantB, theirs)
	t.Cleanup(func() {
		for _, tenantID := range []id.UUID{tenantA, tenantB} {
			exec(`DELETE FROM invitation.invitation WHERE tenant_id = $1`, tenantID.String())
			exec(`DELETE FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String())
		}
		exec(`DELETE FROM organization.organization WHERE organization_id = $1`, organizationID.String())
	})

	asSelf := func(hash string, fn func(context.Context, db.Tx) error) error {
		return tenantLogin.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE organization_self_rt`); err != nil {
				return err
			}
			if hash != "" {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.invitation_token_hash', $1, true)`, hash); err != nil {
					return err
				}
			}
			return fn(ctx, tx)
		})
	}
	count := func(hash, statement string, args ...any) (int, error) {
		var n int
		err := asSelf(hash, func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, statement, args...).Scan(&n)
		})
		return n, err
	}

	if n, err := count(mine, `SELECT count(invitation_id) FROM invitation.invitation`); err != nil || n != 1 {
		t.Errorf("bound to its token, the self role read %d invitations (err %v), want 1", n, err)
	}
	if n, err := count(mine, `SELECT count(invitation_id) FROM invitation.invitation WHERE token_hash = $1`, theirs); err != nil || n != 0 {
		t.Errorf("the self role read %d invitations of another token (err %v), want 0", n, err)
	}
	if n, err := count(mine, `SELECT count(invitation_id) FROM invitation.invitation WHERE tenant_id = $1`, tenantB.String()); err != nil || n != 0 {
		t.Errorf("the self role read %d invitations of another Tenant (err %v), want 0", n, err)
	}
	if _, err := count(mine, `SELECT count(invited_by) FROM invitation.invitation`); err == nil {
		t.Error("the self role read an invitation column it was not granted")
	}
	if err := asSelf(mine, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE invitation.invitation SET state = 'accepted' WHERE token_hash = $1`, mine)
		return err
	}); err == nil {
		t.Error("the self role updated an invitation")
	}
	// Unbound, the read raises on a connection that never bound the setting, and reads nothing on one
	// that bound it in an earlier transaction: LOCAL leaves it empty, and no token_hash is empty.
	if n, err := count("", `SELECT count(invitation_id) FROM invitation.invitation`); err == nil && n != 0 {
		t.Errorf("an unbound invitation read returned %d rows", n)
	}

	// The tenant role, bound to Tenant A and carrying the other token's hash, sees Tenant A alone.
	if err := tenantLogin.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.invitation_token_hash', $2, true)`,
			tenantA.String(), theirs); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM invitation.invitation WHERE token_hash = $1`, theirs).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("the tenant role read %d invitations of another Tenant through the self policy", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("tenant read: %v", err)
	}
}

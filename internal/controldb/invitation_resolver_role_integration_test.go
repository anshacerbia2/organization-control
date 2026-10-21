package controldb_test

// The invitation resolver role, executed as itself (ADR-IAM-011 §5.7 (c), TDD-organization-control-004
// 1.18.0): the tenant login sets it LOCAL, binds one token hash, and reads the identifier and address
// of that one invitation while it is pending and unexpired, and nothing else. The tenant role, which
// may set it and does not inherit it, gains none of its policy.

import (
	"context"
	"testing"

	"github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
)

func TestTheInvitationResolverRoleReadsOnlyOnePendingUnexpiredInvitation(t *testing.T) {
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
	    VALUES ($1, 'invitation resolver suite', 'customer', 'active')`, organizationID.String())
	for _, tenantID := range []id.UUID{tenantA, tenantB} {
		exec(`INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
		    VALUES ($1, $2, 'invitation resolver suite', 'active', 'pooled')`, tenantID.String(), organizationID.String())
	}
	invite := func(tenantID id.UUID, hash, state, expiry string) id.UUID {
		invitationID := newID()
		exec(`INSERT INTO invitation.invitation
		    (invitation_id, tenant_id, target_identifier, target_hash, token_hash, subject_type, invited_by,
		     state, correlation_id, expires_at)
		    VALUES ($1, $2, $3, $4, $5, 'human', $6, $7, $8, now() + $9::interval)`,
			invitationID.String(), tenantID.String(), hash+"@example.test", "target-"+hash, hash,
			newID().String(), state, newID().String(), expiry)
		return invitationID
	}
	suffix := newID().String()
	pending, other := "pending-"+suffix, "other-"+suffix
	expired, revoked, accepted := "expired-"+suffix, "revoked-"+suffix, "accepted-"+suffix
	pendingID := invite(tenantA, pending, "pending", "1 day")
	invite(tenantB, other, "pending", "1 day")
	invite(tenantA, expired, "pending", "-1 minute")
	invite(tenantA, revoked, "revoked", "1 day")
	invite(tenantA, accepted, "accepted", "1 day")
	t.Cleanup(func() {
		for _, tenantID := range []id.UUID{tenantA, tenantB} {
			exec(`DELETE FROM invitation.invitation WHERE tenant_id = $1`, tenantID.String())
			exec(`DELETE FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String())
		}
		exec(`DELETE FROM organization.organization WHERE organization_id = $1`, organizationID.String())
	})

	asResolver := func(hash string, fn func(context.Context, db.Tx) error) error {
		return tenantLogin.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE organization_invitation_resolver_rt`); err != nil {
				return err
			}
			if hash != "" {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.invitation_resolve_hash', $1, true)`, hash); err != nil {
					return err
				}
			}
			return fn(ctx, tx)
		})
	}
	read := func(hash string) (ids []string, addresses []string, err error) {
		err = asResolver(hash, func(ctx context.Context, tx db.Tx) error {
			rows, err := tx.Query(ctx, `SELECT invitation_id::text, target_identifier FROM invitation.invitation`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var rawID, address string
				if err := rows.Scan(&rawID, &address); err != nil {
					return err
				}
				ids, addresses = append(ids, rawID), append(addresses, address)
			}
			return rows.Err()
		})
		return ids, addresses, err
	}

	// The pending, unexpired invitation of the bound hash, and only it, with no WHERE in the statement:
	// the policy picks the row.
	ids, addresses, err := read(pending)
	if err != nil || len(ids) != 1 || ids[0] != pendingID.String() || addresses[0] != pending+"@example.test" {
		t.Errorf("bound to a pending token, the resolver read %v %v (err %v), want the one invitation", ids, addresses, err)
	}
	// Expired, revoked and accepted are the same empty result, and so is a hash nobody holds.
	for name, hash := range map[string]string{"expired": expired, "revoked": revoked, "accepted": accepted,
		"unknown": "unknown-" + suffix} {
		if ids, _, err := read(hash); err != nil || len(ids) != 0 {
			t.Errorf("bound to an %s token, the resolver read %v (err %v), want nothing", name, ids, err)
		}
	}
	// Two columns, and no other: not the hash, the Tenant, the state or the expiry.
	for _, column := range []string{"token_hash", "tenant_id", "state", "expires_at", "invited_by", "workspace_id"} {
		if err := asResolver(pending, func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, `SELECT `+column+` FROM invitation.invitation`)
			return err
		}); err == nil {
			t.Errorf("the resolver read invitation.%s, which it is not granted", column)
		}
	}
	// It writes nothing.
	if err := asResolver(pending, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE invitation.invitation SET target_identifier = 'x'`)
		return err
	}); err == nil {
		t.Error("the resolver updated an invitation")
	}
	// No other table: a Tenant, a Membership.
	for _, table := range []string{"tenant.tenant", "membership.membership"} {
		if err := asResolver(pending, func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, `SELECT 1 FROM `+table)
			return err
		}); err == nil {
			t.Errorf("the resolver read %s", table)
		}
	}
	// Unbound, the read raises on a connection that never bound the setting, and reads nothing on one
	// that bound it in an earlier transaction: LOCAL leaves it empty, and no token_hash is empty.
	if ids, _, err := read(""); err == nil && len(ids) != 0 {
		t.Errorf("an unbound resolve returned %d rows", len(ids))
	}

	// The tenant role, bound to Tenant B and carrying Tenant A's pending hash, reads Tenant B's rows
	// alone: it does not inherit the resolver role, so invitation_resolver_read never applies to it.
	if err := tenantLogin.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.invitation_resolve_hash', $2, true)`,
			tenantB.String(), pending); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM invitation.invitation WHERE token_hash = $1`, pending).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("the tenant role read %d invitations of another Tenant through the resolver policy", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("tenant read: %v", err)
	}
}

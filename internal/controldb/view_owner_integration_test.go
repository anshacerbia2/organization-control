package controldb_test

import (
	"context"
	"errors"
	"testing"

	"github.com/anshacerbia2/foundation-platform/db"
)

// The views the runtime reads and writes through without a scope (TDD-organization-control-003
// §Scheduled Sweeps, TDD-organization-control-004 §Operational Notes). Their owner's policies are the
// control, and a policy binds an owner only when the owner is neither a superuser nor BYPASSRLS.
var policyBoundViews = []string{
	"operation.lifecycle_signals",
	"operation.provisioning_sweep",
	"operation.invitation_expiry",
}

// TestTheRuntimeViewsAreOwnedByARoleThePoliciesBind holds each view to organization_migrator, and
// that role to NOSUPERUSER NOBYPASSRLS. CI runs the stages as a superuser, so a view left to whoever
// created it would be a superuser's, and "Superusers and roles with the BYPASSRLS attribute always
// bypass the row security system" (PostgreSQL 17, Row Security Policies).
func TestTheRuntimeViewsAreOwnedByARoleThePoliciesBind(t *testing.T) {
	pool, ctx := openAdmin(t)

	for _, view := range policyBoundViews {
		var owner string
		var super, bypass bool
		if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT r.rolname, r.rolsuper, r.rolbypassrls
				  FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner
				 WHERE c.oid = to_regclass($1)`, view).Scan(&owner, &super, &bypass)
		}); err != nil {
			t.Fatalf("%s: read owner: %v", view, err)
		}
		if owner != "organization_migrator" {
			t.Errorf("%s is owned by %s, want organization_migrator", view, owner)
		}
		if super || bypass {
			t.Errorf("%s's owner %s bypasses row security (superuser %t, bypassrls %t)", view, owner, super, bypass)
		}
	}
}

// TestTheLifecycleSignalsReadOnlyWhatThePoliciesAdmit shows the policies, not the view's WHERE
// clause, are what limit what a view of organization_migrator's reads. In one rolled-back
// transaction it seeds a realized and a requested provisioning request, and reads them through a
// view with no WHERE clause at all, owned and granted as operation.lifecycle_signals is. The
// realized request, which provisioning_request_signals_read does not admit, is not there. It then
// reads operation.lifecycle_signals itself as the provider role, which the owner's column grants
// must be enough for.
func TestTheLifecycleSignalsReadOnlyWhatThePoliciesAdmit(t *testing.T) {
	pool, ctx := openAdmin(t)

	organization, tenant := newUUID(t), newUUID(t)
	realized, requested := newUUID(t), newUUID(t)

	errAssertions := errors.New("assertions done; roll back")
	seen := map[string]string{}
	var provisionRequested int64
	err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		for _, step := range []struct {
			statement string
			args      []any
		}{
			{`INSERT INTO organization.organization (organization_id, display_name, classification, status)
			    VALUES ($1::uuid, 'view-owner suite', 'customer', 'active')`, []any{organization}},
			{`INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
			    VALUES ($1::uuid, $2::uuid, 'view-owner suite', 'provisioning', 'pooled')`, []any{tenant, organization}},
			{`INSERT INTO tenant.provisioning_request
			    (request_id, tenant_id, desired_profile, state, correlation_id, requested_at, resolved_at)
			  VALUES ($1::uuid, $2::uuid, '{}'::jsonb, 'realized', $1::uuid, now() - interval '2 hours', now())`,
				[]any{realized, tenant}},
			{`INSERT INTO tenant.provisioning_request
			    (request_id, tenant_id, desired_profile, state, correlation_id, requested_at)
			  VALUES ($1::uuid, $2::uuid, '{}'::jsonb, 'requested', $1::uuid, now() - interval '1 hour')`,
				[]any{requested, tenant}},
			// The widened view: the columns lifecycle_signals reads, and no WHERE clause.
			{`CREATE VIEW operation.lifecycle_signals_widened WITH (security_barrier) AS
			    SELECT request_id, desired_profile, state, requested_at, resolved_at FROM tenant.provisioning_request`, nil},
			{`ALTER VIEW operation.lifecycle_signals_widened OWNER TO organization_migrator`, nil},
			{`GRANT SELECT ON operation.lifecycle_signals_widened TO organization_provider_rt`, nil},
			{`SET LOCAL ROLE organization_provider_rt`, nil},
		} {
			if _, err := tx.Exec(ctx, step.statement, step.args...); err != nil {
				return err
			}
		}

		rows, err := tx.Query(ctx, `SELECT request_id::text, state FROM operation.lifecycle_signals_widened
		    WHERE request_id = ANY ($1::uuid[])`, []string{realized, requested})
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, state string
			if err := rows.Scan(&id, &state); err != nil {
				rows.Close()
				return err
			}
			seen[id] = state
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		if err := tx.QueryRow(ctx, `SELECT provision_requested FROM operation.lifecycle_signals`).
			Scan(&provisionRequested); err != nil {
			return err
		}
		return errAssertions
	})
	if !errors.Is(err, errAssertions) {
		t.Fatalf("reading through organization_migrator's views: %v", err)
	}

	if _, ok := seen[realized]; ok {
		t.Error("a view of organization_migrator's with no WHERE clause read a realized request, which no policy admits")
	}
	if seen[requested] != "requested" {
		t.Errorf("the requested request reads %q through the view, want requested", seen[requested])
	}
	if provisionRequested < 1 {
		t.Errorf("operation.lifecycle_signals counts %d provision requests in flight, want at least the seeded one",
			provisionRequested)
	}
}

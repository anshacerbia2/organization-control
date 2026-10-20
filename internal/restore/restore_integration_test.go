package restore_test

// The restore bump (ADR-ORG-003 §5.5, amended 2026-10-20), against the migrated database as its owner,
// inside a transaction this test rolls back: the bump moves every version in the database, and the
// suites after this one read the seeded Tenants' versions.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/restore"
)

var errRollBack = errors.New("rolled back by the test")

func TestTheRestoreBumpMovesEveryVersionAndPublishesWithdrawalsOnly(t *testing.T) {
	pool, ctx := openAdmin(t)
	ids := func() id.UUID {
		value, err := id.NewV7()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	organization := ids()
	active, suspended, retired := ids(), ids(), ids()
	revoked, ended, emergency, activated := ids(), ids(), ids(), ids()
	consumer := "restore-bump-" + ids().String()

	err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		exec := func(statement string, args ...any) {
			t.Helper()
			if _, err := tx.Exec(ctx, statement, args...); err != nil {
				t.Fatalf("seed %q: %v", statement, err)
			}
		}
		exec(`INSERT INTO organization.organization (organization_id, display_name, classification, status)
		      VALUES ($1, 'restore bump', 'customer', 'active')`, organization.String())
		for tenant, status := range map[id.UUID]string{active: "active", suspended: "suspended", retired: "retired"} {
			exec(`INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile,
			          version, tenant_security_version)
			      VALUES ($1, $2, 'restore bump', $3, 'pooled', 7, 5)`, tenant.String(), organization.String(), status)
		}
		grant := func(grantID id.UUID, kind string, isRevoked bool) {
			exec(`INSERT INTO organization.provider_grant (grant_id, principal_id, scope, granted_by, reason, kind, grant_version)
			      VALUES ($1, $2, 'provider:identity-control', $3, 'restore bump', $4, 3)`,
				grantID.String(), ids().String(), ids().String(), kind)
			if isRevoked {
				exec(`UPDATE organization.provider_grant SET revoked_at = now(), revoked_by = $2, revoke_reason = 'gone'
				      WHERE grant_id = $1`, grantID.String(), ids().String())
			}
		}
		grant(revoked, "eligible", true)
		grant(ended, "eligible", false)
		grant(emergency, "emergency", false)
		grant(activated, "eligible", false)
		exec(`INSERT INTO organization.provider_activation (activation_id, grant_id, principal_id, scope, reason,
		          duration_seconds, approval_required, decided_by, decision, decision_reason, decided_at, ends_at)
		      SELECT $1, grant_id, principal_id, scope, 'in force', 3600, true, $2, 'approved', 'ok', now(),
		             now() + interval '1 hour'
		        FROM organization.provider_grant WHERE grant_id = $3`, ids().String(), ids().String(), activated.String())
		exec(`INSERT INTO projection.consumer (consumer_id, principal_id, projection_version, max_accepted_age, stale_behavior)
		      VALUES ($1, $2, 'v1', interval '1 minute', 'fail_closed')`, consumer, ids().String())

		report, err := restore.BumpWithin(ctx, tx)
		if err != nil {
			t.Fatalf("RestoreBumpWithin: %v", err)
		}
		if report.Tenants < 3 || report.Grants < 4 || report.ConsumersRequested < 1 {
			t.Errorf("the bump touched %+v; want every Tenant, grant and active consumer", report)
		}

		for tenant, want := range map[id.UUID]bool{active: false, suspended: true, retired: true} {
			var version, security int64
			var events int
			if err := tx.QueryRow(ctx, `SELECT version, tenant_security_version FROM tenant.tenant WHERE tenant_id = $1`,
				tenant.String()).Scan(&version, &security); err != nil {
				t.Fatal(err)
			}
			if version != 7+restore.Margin || security != 5+restore.Margin {
				t.Errorf("Tenant %s is at version %d, security %d; want both moved by the margin", tenant, version, security)
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform.outbox WHERE aggregate_id = $1
			      AND (envelope->'data'->>'tenant_security_version')::bigint = $2`,
				tenant.String(), security).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if (events == 1) != want {
				t.Errorf("Tenant %s has %d events at its new version; published=%t is wanted", tenant, events, want)
			}
		}

		for grantID, want := range map[id.UUID]bool{revoked: true, ended: true, emergency: false, activated: false} {
			var version int64
			var events int
			if err := tx.QueryRow(ctx, `SELECT grant_version FROM organization.provider_grant WHERE grant_id = $1`,
				grantID.String()).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != 3+restore.Margin {
				t.Errorf("grant %s is at version %d; want it moved by the margin", grantID, version)
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform.outbox WHERE aggregate_id = $1
			      AND (envelope->'data'->>'grant_version')::bigint = $2`, grantID.String(), version).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if (events == 1) != want {
				t.Errorf("grant %s has %d events at its new version; published=%t is wanted", grantID, events, want)
			}
		}

		var requested bool
		if err := tx.QueryRow(ctx, `SELECT reconciliation_requested_at IS NOT NULL FROM projection.consumer
		      WHERE consumer_id = $1`, consumer).Scan(&requested); err != nil || !requested {
			t.Errorf("the consumer was not asked to reconcile: %t, %v", requested, err)
		}
		return errRollBack
	})
	if !errors.Is(err, errRollBack) {
		t.Fatalf("the test transaction ended with %v", err)
	}
}

func openAdmin(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("REQUIRE_INTEGRATION") != "" {
			t.Fatal("REQUIRE_INTEGRATION is set and TEST_DATABASE_URL is empty")
		}
		t.Skip("TEST_DATABASE_URL is unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, db.Config{Name: "restore-test-owner", DSN: dsn, MaxConns: 2})
	if err != nil {
		t.Fatalf("open the owner pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

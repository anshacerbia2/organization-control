package db_test

// The binding, exercised against the real engine as the real runtime roles.
//
// binding_test.go proves the scope-resolution rules with a fake transaction source: which scope is
// refused, which setting is bound, what order the evidence is written in. None of that touches
// PostgreSQL, so none of it proves the statement it writes actually satisfies a policy.
//
// This file closes that gap. It is the difference between "the code sets app.tenant_id" and "a row
// belonging to another Tenant is unreachable through this function".

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

const (
	tenantA = "11111111-1111-4111-8111-11111111111a"
	tenantB = "11111111-1111-4111-8111-11111111111b"
)

// runtimeDSN rewrites the administrative DSN to authenticate as a runtime login role.
//
// The binding is only evidence when exercised by the role that carries production traffic:
// TDD-organization-control-001 refuses an owning connection as isolation evidence, and on one the
// policies would not apply at all without FORCE.
func runtimeDSN(t *testing.T, user, password string) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		return ""
	}
	rest := base
	if index := strings.Index(base, "://"); index >= 0 {
		rest = base[index+3:]
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return fmt.Sprintf("postgres://%s:%s@%s", user, password, rest)
}

func poolAs(t *testing.T, user, passwordEnv string) (*fdb.Pool, context.Context) {
	t.Helper()

	dsn := runtimeDSN(t, user, os.Getenv(passwordEnv))
	if dsn == "" {
		if os.Getenv("REQUIRE_INTEGRATION") != "" {
			t.Fatal("REQUIRE_INTEGRATION is set and TEST_DATABASE_URL is empty")
		}
		t.Skip("TEST_DATABASE_URL is unset")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	// MaxConns: 1 so a second transaction is guaranteed to reuse the connection the first bound.
	// With a larger pool the revert assertion could pass by taking a fresh connection.
	pool, err := fdb.Open(ctx, fdb.Config{Name: "db-test-" + user, DSN: dsn, MaxConns: 1})
	if err != nil {
		t.Fatalf("open pool as %s: %v", user, err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func mustParse(t *testing.T, value string) id.UUID {
	t.Helper()
	parsed, err := id.Parse(value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// noopRecorder stands in for the evidence sink. The recording contract is asserted in
// binding_test.go; here the concern is the SQL, and a real sink would add a table this test does
// not need.
type noopRecorder struct{}

func (noopRecorder) RecordProviderAccess(context.Context, db.ProviderAccess) error { return nil }

// TestWithTenantScopeConfinesReadsToTheBoundTenant is the headline: the function, not a
// hand-written SET LOCAL, produces the isolation the policy promises.
func TestWithTenantScopeConfinesReadsToTheBoundTenant(t *testing.T) {
	pool, ctx := poolAs(t, "organization_app", "TEST_RUNTIME_PASSWORD")

	tenantPool, err := db.NewTenantPool(pool)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	scope, err := db.TenantScope(mustParse(t, tenantA), mustParse(t, tenantA), id.UUID{})
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}

	if err := db.WithTenantScope(db.WithScope(ctx, scope), tenantPool,
		func(ctx context.Context, tx db.Tx) error {
			// No WHERE clause. An application-level filter would make this pass with no policy
			// at all, which is the substitution ADR-GLB-002 exists to remove.
			var tenants int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenant.tenant`).Scan(&tenants); err != nil {
				return err
			}
			if tenants != 1 {
				t.Errorf("the bound transaction sees %d Tenants, want 1", tenants)
			}

			var foreign int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM membership.membership WHERE tenant_id = $1`, tenantB).Scan(&foreign); err != nil {
				return err
			}
			if foreign != 0 {
				t.Errorf("the bound transaction sees %d rows of another Tenant", foreign)
			}
			return nil
		}); err != nil {
		t.Fatalf("WithTenantScope: %v", err)
	}
}

// TestWithTenantScopeRefusesAWriteIntoAnotherTenant is WITH CHECK reached through the function
// rather than through a hand-issued statement.
func TestWithTenantScopeRefusesAWriteIntoAnotherTenant(t *testing.T) {
	pool, ctx := poolAs(t, "organization_app", "TEST_RUNTIME_PASSWORD")

	tenantPool, err := db.NewTenantPool(pool)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	scope, err := db.TenantScope(mustParse(t, tenantA), mustParse(t, tenantA), id.UUID{})
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}

	err = db.WithTenantScope(db.WithScope(ctx, scope), tenantPool,
		func(ctx context.Context, tx db.Tx) error {
			_, execErr := tx.Exec(ctx, `
				INSERT INTO membership.membership
				    (membership_id, principal_id, tenant_id, subject_type, status, valid_from, provenance)
				VALUES (gen_random_uuid(), gen_random_uuid(), $1, 'human', 'active', now(), 'migration')`,
				tenantB)
			return execErr
		})
	if err == nil {
		t.Fatal("a transaction bound to Tenant A wrote a row into Tenant B")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "row-level security") &&
		!strings.Contains(strings.ToLower(err.Error()), "row level security") {
		t.Errorf("the write failed for the wrong reason: %v", err)
	}
}

// TestTheBindingRevertsBetweenTransactions is why the implementation uses is_local = true.
//
// A pooled connection must not carry one request's Tenant into the next. The failure is invisible
// under low concurrency, because the second request simply sees the first request's Tenant and
// returns plausible rows.
func TestTheBindingRevertsBetweenTransactions(t *testing.T) {
	pool, ctx := poolAs(t, "organization_app", "TEST_RUNTIME_PASSWORD")

	tenantPool, err := db.NewTenantPool(pool)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	scope, err := db.TenantScope(mustParse(t, tenantA), mustParse(t, tenantA), id.UUID{})
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}

	if err := db.WithTenantScope(db.WithScope(ctx, scope), tenantPool,
		func(ctx context.Context, tx db.Tx) error {
			var count int
			return tx.QueryRow(ctx, `SELECT count(*) FROM tenant.tenant`).Scan(&count)
		}); err != nil {
		t.Fatalf("first transaction: %v", err)
	}

	// Same connection, straight through the underlying pool with no binding. It must raise.
	if err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		var count int
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant.tenant`).Scan(&count)
	}); err == nil {
		t.Fatal("an unbound transaction on the same connection read rows; the binding did not revert")
	}
}

// TestWithProviderScopeReadsAcrossTenants is the capability the second pool exists for, reached
// through the function that records the access first.
func TestWithProviderScopeReadsAcrossTenants(t *testing.T) {
	pool, ctx := poolAs(t, "organization_provider_app", "TEST_PROVIDER_PASSWORD")

	providerPool, err := db.NewProviderPool(pool, noopRecorder{})
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}
	scope, err := db.ProviderScope(mustParse(t, tenantA), mustParse(t, tenantB))
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	if err := db.WithProviderScope(db.WithScope(ctx, scope), providerPool, "incident review",
		func(ctx context.Context, tx db.Tx) error {
			var tenants int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenant.tenant`).Scan(&tenants); err != nil {
				return err
			}
			if tenants < 2 {
				t.Errorf("a provider transaction sees %d Tenants, want at least 2", tenants)
			}
			return nil
		}); err != nil {
		t.Fatalf("WithProviderScope: %v", err)
	}
}

// TestTheProviderBindingAlsoReverts closes the same window on the provider side. A provider
// connection returned to the pool still carrying app.provider_scope would make the next
// transaction on it cross-Tenant by inheritance.
func TestTheProviderBindingAlsoReverts(t *testing.T) {
	pool, ctx := poolAs(t, "organization_provider_app", "TEST_PROVIDER_PASSWORD")

	providerPool, err := db.NewProviderPool(pool, noopRecorder{})
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}
	scope, err := db.ProviderScope(mustParse(t, tenantA), mustParse(t, tenantB))
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	if err := db.WithProviderScope(db.WithScope(ctx, scope), providerPool, "incident review",
		func(ctx context.Context, tx db.Tx) error {
			var count int
			return tx.QueryRow(ctx, `SELECT count(*) FROM tenant.tenant`).Scan(&count)
		}); err != nil {
		t.Fatalf("provider transaction: %v", err)
	}

	if err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		var count int
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant.tenant`).Scan(&count)
	}); err == nil {
		t.Fatal("an unbound provider transaction read rows; the binding did not revert")
	}
}

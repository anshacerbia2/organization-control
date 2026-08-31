package controldb_test

// Tenant isolation, proven as the runtime role.
//
// TDD-organization-control-001 states the reason this file exists rather than a review checklist:
//
//	Isolation tests MUST prove cross-tenant denial using the actual application runtime role;
//	a test executed on an administrative or owning connection is not isolation evidence.
//
// A policy that has never been exercised by the role carrying production traffic is an untested
// control. Every connection below authenticates as a login role that inherits
// `organization_rt` or `organization_provider_rt` — never as the owner, and never as a
// superuser. On an owning connection every assertion here would pass for the wrong reason,
// because PostgreSQL exempts an owner from its own table's policies unless FORCE is set.
//
// The suite skips without TEST_DATABASE_URL and fails on a skip when REQUIRE_INTEGRATION is set.
// CI sets both, because a service container that never came up would otherwise leave every
// assertion skipped and the run green — which is indistinguishable from having checked something.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/db"
)

const (
	migratorRole   = "organization_migrator"
	tenantRole     = "organization_rt"
	providerRole   = "organization_provider_rt"
	tenantA        = "11111111-1111-4111-8111-11111111111a"
	tenantB        = "11111111-1111-4111-8111-11111111111b"
	principalInB   = "33333333-3333-4333-8333-33333333333b"
	newPrincipalID = "44444444-4444-4444-8444-444444444444"
)

// rlsSchemas is the set TDD-organization-control-001 classifies as tenant-scoped. `organization`
// and `projection` are deliberately absent: an Organization sponsors several Tenants so scoping
// it to one would be wrong, and the consumer registry carries no tenant column.
var rlsSchemas = []string{"tenant", "workspace", "membership", "invitation", "operation"}

// dsnFor rewrites the administrative DSN to authenticate as a runtime login role.
//
// The test connects as the role production traffic uses, so the credential cannot be the one CI
// hands the migration job. Deriving it from that DSN keeps the host, port, and database in one
// place while the role and password are the part under test.
func dsnFor(t *testing.T, user, password string) string {
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

func openAs(t *testing.T, user, password string) (*db.Pool, context.Context) {
	t.Helper()

	dsn := dsnFor(t, user, password)
	if dsn == "" {
		if os.Getenv("REQUIRE_INTEGRATION") != "" {
			t.Fatal("REQUIRE_INTEGRATION is set and TEST_DATABASE_URL is empty: the database this suite asserts against never came up")
		}
		t.Skip("TEST_DATABASE_URL is unset; set it to run isolation assertions against a real server")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	pool, err := db.Open(ctx, db.Config{Name: "controldb-test-" + user, DSN: dsn, MaxConns: 2})
	if err != nil {
		t.Fatalf("open pool as %s: %v", user, err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// tenantPool authenticates as the tenant-scoped runtime role.
func tenantPool(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()
	return openAs(t, "organization_app", os.Getenv("TEST_RUNTIME_PASSWORD"))
}

// providerPool authenticates as the provider-scoped runtime role.
func providerPool(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()
	return openAs(t, "organization_provider_app", os.Getenv("TEST_PROVIDER_PASSWORD"))
}

// bound runs fn in a transaction with the tenant binding set, the way db.WithTenantScope will.
//
// `SET LOCAL` rather than `SET`: it reverts at commit or rollback, which is what makes connection
// pooling and Row-Level Security safe together. TestBindingRevertsBetweenTransactions is the
// assertion that this is true rather than assumed.
func bound(ctx context.Context, pool *db.Pool, tenant string, fn func(context.Context, db.Tx) error) error {
	return pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = %s", quote(tenant))); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

// quote renders a SQL string literal. SET LOCAL takes no parameters, so the value cannot be
// bound — and every value passed here is a constant in this file rather than input.
func quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'" //nolint:gosec // test-local constants only
}

func scanInt(t *testing.T, ctx context.Context, tx db.Tx, statement string, args ...any) int {
	t.Helper()
	var result int
	if err := tx.QueryRow(ctx, statement, args...).Scan(&result); err != nil {
		t.Fatalf("query %q: %v", statement, err)
	}
	return result
}

// ---------------------------------------------------------------------------------------------
// Structural: the control exists and is not inert.
// ---------------------------------------------------------------------------------------------

// TestEveryTenantScopedTableIsProtected reads the catalog rather than rls.sql.
//
// ADR-GLB-002 §5.3 requires exactly this: compliance is verified by reading
// `pg_class.relforcerowsecurity` in addition to `relrowsecurity`, because confirming that
// policies exist is not sufficient. Atlas does not model RLS, so nothing reconciles a policy the
// way it reconciles a column — a policy dropped by hand stays dropped while the schema still
// matches its declared state, and this test is the only thing that notices.
func TestEveryTenantScopedTableIsProtected(t *testing.T) {
	pool, ctx := openAdmin(t)

	type protection struct {
		name     string
		enabled  bool
		forced   bool
		policies int
	}
	var found []protection

	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT n.nspname || '.' || c.relname,
			       c.relrowsecurity,
			       c.relforcerowsecurity,
			       (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid)
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = ANY($1) AND c.relkind = 'r'
			 ORDER BY 1`, rlsSchemas)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p protection
			if err := rows.Scan(&p.name, &p.enabled, &p.forced, &p.policies); err != nil {
				return err
			}
			found = append(found, p)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("reading the catalog: %v", err)
	}

	if len(found) == 0 {
		t.Fatal("no tables found in the RLS schemas; the migration did not run")
	}
	for _, p := range found {
		if !p.enabled {
			t.Errorf("%s: row level security is not enabled", p.name)
		}
		// The one that is routinely skipped. Without FORCE, policies do not apply to the table
		// owner, and the control is inert against exactly the connection most likely to be
		// misused during an incident.
		if !p.forced {
			t.Errorf("%s: row level security is enabled but not FORCED, so it does not apply to the owner", p.name)
		}
		// Two: one tenant-scoped, one provider-scoped. A table with one policy is a table where
		// one of the two callers has no access path, or where a single permissive policy serves
		// both — which is the conflation TDD-organization-control-001 separates at the role level.
		if p.policies != 2 {
			t.Errorf("%s: %d policies, want 2 (tenant scope and provider scope)", p.name, p.policies)
		}
	}
}

// TestEveryProtectedTableCarriesTenantID is the modelling rule, asserted after the fact.
//
// rls.sql raises on this during migration, which is the right place to stop a bad deploy. This
// asserts the outcome, because a table could be added to an RLS schema by a path that did not run
// that stage — a restored dump, or a hand-run CREATE TABLE during an incident.
func TestEveryProtectedTableCarriesTenantID(t *testing.T) {
	pool, ctx := openAdmin(t)

	var offending int
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		offending = scanInt(t, ctx, tx, `
			SELECT count(*)
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = ANY($1)
			   AND c.relkind = 'r'
			   AND NOT EXISTS (
			         SELECT 1 FROM pg_attribute a
			          WHERE a.attrelid = c.oid AND a.attname = 'tenant_id'
			            AND a.attnotnull AND a.attnum > 0 AND NOT a.attisdropped)`, rlsSchemas)
		return nil
	}); err != nil {
		t.Fatalf("reading the catalog: %v", err)
	}
	if offending != 0 {
		t.Errorf("%d table(s) in an RLS schema carry no non-nullable tenant_id; the policy would raise at query time", offending)
	}
}

// TestRuntimeRolesHoldNothingDangerous asserts the half of ADR-GLB-002 §5.2 that makes the
// policies mean anything.
//
// A role holding SUPERUSER or BYPASSRLS makes every policy in this database inert while the
// catalog still reports RLS as enabled — a control that reads as present and is not.
func TestRuntimeRolesHoldNothingDangerous(t *testing.T) {
	pool, ctx := openAdmin(t)

	for _, role := range []string{tenantRole, providerRole} {
		if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			var super, bypass, createRole, login bool
			if err := tx.QueryRow(ctx,
				`SELECT rolsuper, rolbypassrls, rolcreaterole, rolcanlogin FROM pg_roles WHERE rolname = $1`,
				role).Scan(&super, &bypass, &createRole, &login); err != nil {
				return err
			}
			if super {
				t.Errorf("%s holds SUPERUSER", role)
			}
			if bypass {
				t.Errorf("%s holds BYPASSRLS, which makes every policy inert", role)
			}
			if createRole {
				t.Errorf("%s holds CREATEROLE, so it can grant itself another role's privileges", role)
			}
			// NOLOGIN: these are group roles carrying privilege. A deployable authenticates as a
			// login role that inherits one, so rotating a credential never touches a grant.
			if login {
				t.Errorf("%s can log in; it is a group role and must not be authenticated as directly", role)
			}
			return nil
		}); err != nil {
			t.Fatalf("reading %s: %v", role, err)
		}
	}
}

// TestRuntimeRolesOwnNothing is the other half. Ownership is what would make the DML grants
// insufficient: an owner can ALTER and DROP its own tables regardless of which privileges were
// granted, and — absent FORCE — is exempt from its own table's policies.
func TestRuntimeRolesOwnNothing(t *testing.T) {
	pool, ctx := openAdmin(t)

	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		tables := scanInt(t, ctx, tx,
			`SELECT count(*) FROM pg_tables WHERE tableowner = ANY($1)`, []string{tenantRole, providerRole})
		if tables != 0 {
			t.Errorf("the runtime roles own %d table(s); they must own none", tables)
		}
		schemas := scanInt(t, ctx, tx,
			`SELECT count(*) FROM information_schema.schemata WHERE schema_owner = ANY($1)`, []string{tenantRole, providerRole})
		if schemas != 0 {
			t.Errorf("the runtime roles own %d schema(s); they must own none", schemas)
		}
		owner := ""
		if err := tx.QueryRow(ctx,
			`SELECT schema_owner FROM information_schema.schemata WHERE schema_name = 'membership'`).Scan(&owner); err != nil {
			return err
		}
		// Somebody must own them, and it must be the migration role. If the objects belonged to
		// whichever superuser ran the migration, the separation this suite asserts would exist
		// only on paper.
		if owner != migratorRole {
			t.Errorf("membership is owned by %q, want %q", owner, migratorRole)
		}
		return nil
	}); err != nil {
		t.Fatalf("reading ownership: %v", err)
	}
}

// TestTenantRoleHoldsNothingOnOrganization closes the gap the classification opens.
//
// `organization` is deliberately outside the RLS set, because an Organization sponsors several
// Tenants and scoping it to one would be wrong. That leaves it with no row-level control at all,
// which makes the grant the only boundary: a tenant-scoped caller with SELECT here could read
// every customer in the estate.
func TestTenantRoleHoldsNothingOnOrganization(t *testing.T) {
	pool, ctx := openAdmin(t)

	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			var held bool
			if err := tx.QueryRow(ctx,
				`SELECT has_table_privilege($1, 'organization.organization', $2)`,
				tenantRole, privilege).Scan(&held); err != nil {
				return err
			}
			if held {
				t.Errorf("%s holds %s on organization.organization, which carries no RLS", tenantRole, privilege)
			}
		}
		// The provider role does reach it, through its own connection, so the access stays
		// attributable at the role level rather than hidden inside a shared one.
		var providerReads bool
		if err := tx.QueryRow(ctx,
			`SELECT has_table_privilege($1, 'organization.organization', 'SELECT')`,
			providerRole).Scan(&providerReads); err != nil {
			return err
		}
		if !providerReads {
			t.Errorf("%s cannot read organization.organization; provider administration needs it", providerRole)
		}
		return nil
	}); err != nil {
		t.Fatalf("reading privileges: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// Isolation, executed as the tenant-scoped runtime role.
// ---------------------------------------------------------------------------------------------

// TestReadBoundToOneTenantSeesNoOther is the headline assertion of the whole design.
func TestReadBoundToOneTenantSeesNoOther(t *testing.T) {
	pool, ctx := tenantPool(t)

	if err := bound(ctx, pool, tenantA, func(ctx context.Context, tx db.Tx) error {
		mine := scanInt(t, ctx, tx, `SELECT count(*) FROM membership.membership WHERE tenant_id = $1`, tenantA)
		if mine == 0 {
			t.Error("a caller bound to Tenant A sees none of its own rows; the fixture or the policy is wrong")
		}
		// The predicate is the control, so the query deliberately does not filter. An
		// application-level WHERE would make this test pass with no policy at all.
		others := scanInt(t, ctx, tx, `SELECT count(*) FROM membership.membership WHERE tenant_id <> $1`, tenantA)
		if others != 0 {
			t.Errorf("a caller bound to Tenant A sees %d row(s) belonging to another Tenant", others)
		}
		// tenant.tenant is the case most likely to be got wrong, because its primary key is the
		// discriminator: a bound caller must see exactly one Tenant, its own.
		tenants := scanInt(t, ctx, tx, `SELECT count(*) FROM tenant.tenant`)
		if tenants != 1 {
			t.Errorf("a caller bound to Tenant A sees %d Tenant rows, want 1", tenants)
		}
		return nil
	}); err != nil {
		t.Fatalf("bound read: %v", err)
	}
}

// TestWriteIntoAnotherTenantIsRefused is WITH CHECK. A policy carrying only USING restricts reads
// and leaves INSERT and UPDATE open, which is quiet: reads look correctly isolated while writes
// are not.
func TestWriteIntoAnotherTenantIsRefused(t *testing.T) {
	pool, ctx := tenantPool(t)

	t.Run("insert carrying another Tenant's id", func(t *testing.T) {
		err := bound(ctx, pool, tenantA, func(ctx context.Context, tx db.Tx) error {
			_, execErr := tx.Exec(ctx, `
				INSERT INTO membership.membership
				    (membership_id, principal_id, tenant_id, subject_type, status, valid_from, provenance)
				VALUES (gen_random_uuid(), $1, $2, 'human', 'active', now(), 'migration')`,
				newPrincipalID, tenantB)
			return execErr
		})
		if err == nil {
			t.Fatal("a caller bound to Tenant A inserted a row into Tenant B")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "row-level security") &&
			!strings.Contains(strings.ToLower(err.Error()), "row level security") {
			t.Errorf("the insert failed for the wrong reason: %v", err)
		}
	})

	t.Run("update moving a row into another Tenant", func(t *testing.T) {
		// The row is invisible to this caller, so the UPDATE matches nothing rather than being
		// refused. That is the correct outcome and worth asserting: the row must still be in
		// Tenant B afterwards.
		if err := bound(ctx, pool, tenantA, func(ctx context.Context, tx db.Tx) error {
			tag, execErr := tx.Exec(ctx,
				`UPDATE membership.membership SET tenant_id = $1 WHERE principal_id = $2`, tenantA, principalInB)
			if execErr != nil {
				return execErr
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("an update bound to Tenant A affected %d row(s) belonging to Tenant B", tag.RowsAffected())
			}
			return nil
		}); err != nil {
			t.Fatalf("bound update: %v", err)
		}
	})

	t.Run("delete reaching another Tenant", func(t *testing.T) {
		if err := bound(ctx, pool, tenantA, func(ctx context.Context, tx db.Tx) error {
			tag, execErr := tx.Exec(ctx,
				`DELETE FROM membership.membership WHERE tenant_id = $1`, tenantB)
			if execErr != nil {
				return execErr
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("a delete bound to Tenant A removed %d row(s) from Tenant B", tag.RowsAffected())
			}
			return nil
		}); err != nil {
			t.Fatalf("bound delete: %v", err)
		}
	})
}

// TestUnboundQueryRaisesRatherThanReturningNothing is why the policy reads the setting with
// missing_ok = false.
//
// A NULL predicate is false, so an unbound connection would return zero rows. A query returning
// nothing looks like an empty result and gets dismissed as normal; a query raising looks like the
// defect it is. ADR-GLB-002 §5.2 now requires this, after its own example specified the opposite.
func TestUnboundQueryRaisesRatherThanReturningNothing(t *testing.T) {
	pool, ctx := tenantPool(t)

	err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var count int
		return tx.QueryRow(ctx, `SELECT count(*) FROM membership.membership`).Scan(&count)
	})
	if err == nil {
		t.Fatal("an unbound query succeeded; the policy is reading the setting with missing_ok = true")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unrecognized configuration parameter") {
		t.Errorf("the query failed for the wrong reason: %v", err)
	}
}

// TestBindingRevertsBetweenTransactions is why the binding uses SET LOCAL rather than SET.
//
// A pooled connection must not carry one request's Tenant into the next. This is the failure mode
// that makes connection pooling and RLS interact badly, and it is invisible under low
// concurrency: the second request simply sees the first request's Tenant.
func TestBindingRevertsBetweenTransactions(t *testing.T) {
	// MaxConns: 1 so the second transaction is guaranteed to reuse the connection the first one
	// bound. With a larger pool the test could pass by taking a fresh connection.
	dsn := dsnFor(t, "organization_app", os.Getenv("TEST_RUNTIME_PASSWORD"))
	if dsn == "" {
		if os.Getenv("REQUIRE_INTEGRATION") != "" {
			t.Fatal("REQUIRE_INTEGRATION is set and TEST_DATABASE_URL is empty")
		}
		t.Skip("TEST_DATABASE_URL is unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, db.Config{Name: "controldb-revert", DSN: dsn, MaxConns: 1})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := bound(ctx, pool, tenantA, func(ctx context.Context, tx db.Tx) error {
		_ = scanInt(t, ctx, tx, `SELECT count(*) FROM membership.membership`)
		return nil
	}); err != nil {
		t.Fatalf("first transaction: %v", err)
	}

	// Same connection, no binding. It must raise, which proves SET LOCAL reverted at commit.
	second := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var count int
		return tx.QueryRow(ctx, `SELECT count(*) FROM membership.membership`).Scan(&count)
	})
	if second == nil {
		t.Fatal("a second transaction on the same connection read without binding; SET LOCAL did not revert")
	}
}

// ---------------------------------------------------------------------------------------------
// Provider scope, executed as the provider runtime role.
// ---------------------------------------------------------------------------------------------

// TestProviderScopeReadsAcrossTenants is the capability the second role exists for.
func TestProviderScopeReadsAcrossTenants(t *testing.T) {
	pool, ctx := providerPool(t)

	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL app.provider_scope = 'true'"); err != nil {
			return err
		}
		tenants := scanInt(t, ctx, tx, `SELECT count(*) FROM tenant.tenant`)
		if tenants < 2 {
			t.Errorf("a provider-scoped transaction sees %d Tenants, want at least 2", tenants)
		}
		return nil
	}); err != nil {
		t.Fatalf("provider read: %v", err)
	}
}

// TestUnboundProviderConnectionIsRefused is why the provider policy reads a session setting
// instead of granting unconditional access.
//
// The role is deliberately cross-Tenant and still not BYPASSRLS: an unbound provider connection
// fails closed the same way an unbound tenant one does. A role holding BYPASSRLS would be
// indistinguishable in the catalog from one that never needed it, and the cross-tenant access
// would stop being attributable.
func TestUnboundProviderConnectionIsRefused(t *testing.T) {
	pool, ctx := providerPool(t)

	err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var count int
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant.tenant`).Scan(&count)
	})
	if err == nil {
		t.Fatal("an unbound provider connection read across Tenants")
	}
}

// TestProviderRoleCannotReadAsATenant states that the two paths do not overlap.
//
// The provider policy is the only one granted to this role, so binding a tenant identifier on a
// provider connection changes nothing: the tenant policy names `organization_rt` and this is not
// that role. A defect that set the wrong binding therefore cannot silently narrow provider
// access into a tenant-shaped read.
func TestProviderRoleCannotReadAsATenant(t *testing.T) {
	pool, ctx := providerPool(t)

	err := bound(ctx, pool, tenantA, func(ctx context.Context, tx db.Tx) error {
		var count int
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant.tenant`).Scan(&count)
	})
	if err == nil {
		t.Fatal("a provider connection carrying only a tenant binding read rows; the policies overlap")
	}
}

// openAdmin uses TEST_DATABASE_URL exactly as given, rather than rewriting it.
//
// The structural assertions need whatever role owns the schema, and that role's name is part of
// the credential the environment hands us: `postgres` locally, `organization` in CI. Rewriting the
// user to a hardcoded `postgres` while keeping the password from the DSN produced
// "password authentication failed for user postgres" in CI — a message about a credential, for a
// test that had invented the role name. Every other pool here rewrites the user on purpose,
// because connecting as a specific runtime role IS the assertion; this one has no such reason.
func openAdmin(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("REQUIRE_INTEGRATION") != "" {
			t.Fatal("REQUIRE_INTEGRATION is set and TEST_DATABASE_URL is empty: the database this suite asserts against never came up")
		}
		t.Skip("TEST_DATABASE_URL is unset; set it to run isolation assertions against a real server")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	pool, err := db.Open(ctx, db.Config{Name: "controldb-test-admin", DSN: dsn, MaxConns: 2})
	if err != nil {
		t.Fatalf("open administrative pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// The administrative password used to be parsed back out of TEST_DATABASE_URL and paired with a
// hardcoded user. openAdmin uses the DSN whole instead, so there is nothing left to parse: the
// owner's name and password travel together, which is what a DSN is.

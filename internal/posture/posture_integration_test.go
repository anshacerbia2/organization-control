package posture_test

// AssertIsolation is production code, so it is tested for what it detects rather than only for
// agreeing that a healthy database is healthy.
//
// A checker that always reports "intact" passes every happy-path test ever written for it, and
// the whole reason this function exists is to be the last thing between a weakened control and
// production traffic. Each case below removes one control and asserts the report names it.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/db"

	"github.com/anshacerbia2/organization-control/internal/posture"
)

// adminPool authenticates as the administrative role, because these cases change the schema.
func adminPool(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()
	return open(t, os.Getenv("TEST_DATABASE_URL"), "posture-test-admin")
}

// runtimePool authenticates as organization_app, the tenant login role the serving process uses,
// which is the role the startup check and the readiness probe run as.
func runtimePool(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		return open(t, "", "")
	}
	rest := base
	if index := strings.Index(base, "://"); index >= 0 {
		rest = base[index+3:]
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return open(t, fmt.Sprintf("postgres://organization_app:%s@%s", os.Getenv("TEST_RUNTIME_PASSWORD"), rest),
		"posture-test-runtime")
}

func open(t *testing.T, dsn, name string) (*db.Pool, context.Context) {
	t.Helper()
	if dsn == "" {
		if os.Getenv("REQUIRE_INTEGRATION") != "" {
			t.Fatal("REQUIRE_INTEGRATION is set and TEST_DATABASE_URL is empty: the database this suite asserts against never came up")
		}
		t.Skip("TEST_DATABASE_URL is unset; set it to run isolation assertions against a real server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, db.Config{Name: name, DSN: dsn, MaxConns: 2})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func TestAssertIsolationAcceptsAnIntactDatabase(t *testing.T) {
	pool, ctx := adminPool(t)

	report, err := posture.AssertIsolation(ctx, pool)
	if err != nil {
		t.Fatalf("AssertIsolation: %v", err)
	}
	if !report.OK() {
		t.Fatalf("a freshly migrated database reports problems: %v", report.Problems)
	}
	// Thirteen tables across five schemas, the two Membership batch tables (ADR-ORG-004) and the
	// offboarding freeze record (ADR-ORG-006) included. Asserted rather than left implicit, because
	// every loop in AssertIsolation is vacuous over an empty set — a report with no tables and no
	// problems would otherwise read as intact.
	if len(report.Tables) != 13 {
		t.Errorf("report covers %d tables, want 13", len(report.Tables))
	}
	for _, table := range report.Tables {
		want := 2 + len(posture.AdditionalPolicies[table.Qualified()])
		if !table.Enabled || !table.Forced || table.Policies != want {
			t.Errorf("%s: enabled=%v forced=%v policies=%d", table.Qualified(), table.Enabled, table.Forced, table.Policies)
		}
	}
}

// TestAssertIsolationDetectsEachWeakening is the test that makes the function worth having.
//
// Every case is a real way the posture degrades, and every one of them leaves the schema matching
// its declared state — which is exactly why Atlas cannot catch them and why this runs at deploy
// time and, once the service exists, at startup.
func TestAssertIsolationDetectsEachWeakening(t *testing.T) {
	cases := []struct {
		name    string
		break_  string
		restore string
		expect  string
	}{
		{
			name:    "FORCE removed",
			break_:  "ALTER TABLE membership.membership NO FORCE ROW LEVEL SECURITY",
			restore: "ALTER TABLE membership.membership FORCE ROW LEVEL SECURITY",
			expect:  "not FORCED",
		},
		{
			name:    "row level security disabled",
			break_:  "ALTER TABLE membership.membership DISABLE ROW LEVEL SECURITY",
			restore: "ALTER TABLE membership.membership ENABLE ROW LEVEL SECURITY",
			expect:  "no row-level security",
		},
		{
			name:    "tenant policy dropped",
			break_:  "DROP POLICY membership_tenant_scope ON membership.membership",
			restore: tenantPolicyFor("membership", "membership"),
			// By name rather than by count: the table also carries the consumer's declared read
			// policy, so a count would change with every declared addition.
			expect: "missing [membership_tenant_scope]",
		},
		{
			// Counted, a table with a required policy dropped and an undeclared one added would
			// still carry two. Named, both are reported.
			name: "a required policy replaced by an undeclared one",
			// One DO block per side: the driver's extended protocol takes one statement at a time.
			break_: `DO $$ BEGIN
				DROP POLICY membership_provider_scope ON membership.membership;
				CREATE POLICY membership_open ON membership.membership FOR ALL TO organization_provider_rt USING (true);
			END $$`,
			restore: `DO $$ BEGIN
				DROP POLICY membership_open ON membership.membership;
				` + providerPolicyFor("membership", "membership") + `;
			END $$`,
			expect: "undeclared [membership_open]",
		},
		{
			name:   "a table in an RLS schema with no tenant_id",
			break_: "CREATE TABLE membership.unprotected (id uuid PRIMARY KEY)",
			// Dropped rather than fixed: the table exists only for this case.
			restore: "DROP TABLE membership.unprotected",
			expect:  "carries no non-nullable tenant_id",
		},
		{
			name:    "runtime role granted BYPASSRLS",
			break_:  "ALTER ROLE organization_rt BYPASSRLS",
			restore: "ALTER ROLE organization_rt NOBYPASSRLS",
			expect:  "holds BYPASSRLS",
		},
		{
			name:    "runtime role granted LOGIN",
			break_:  "ALTER ROLE organization_rt LOGIN",
			restore: "ALTER ROLE organization_rt NOLOGIN",
			expect:  "can log in",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, ctx := adminPool(t)

			exec(t, ctx, pool, tc.break_)
			// Cleanup rather than a deferred statement in the body: a failed assertion calls
			// Goexit, and a database left weakened would fail every later case for the wrong
			// reason.
			t.Cleanup(func() { exec(t, ctx, pool, tc.restore) })

			report, err := posture.AssertIsolation(ctx, pool)
			if err != nil {
				t.Fatalf("AssertIsolation: %v", err)
			}
			if report.OK() {
				t.Fatalf("the report is clean after %q; the check does not detect it", tc.break_)
			}
			if !containsSubstring(report.Problems, tc.expect) {
				t.Errorf("no problem mentions %q; got %v", tc.expect, report.Problems)
			}
			// The error carries every problem, because a caller logs one line and an operator
			// should not have to run the check again to see the rest.
			if err := report.Err(); err == nil || !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("Err() does not carry the problem: %v", err)
			}
		})
	}

	// Everything restored. Asserted explicitly, because a leaked weakening would make the next
	// package's tests fail somewhere unrelated.
	pool, ctx := adminPool(t)
	report, err := posture.AssertIsolation(ctx, pool)
	if err != nil {
		t.Fatalf("AssertIsolation: %v", err)
	}
	if !report.OK() {
		t.Errorf("the database was left weakened after the subtests: %v", report.Problems)
	}
}

// tenantPolicyFor rebuilds the policy the corresponding case drops. It is written out rather than
// re-running rls.sql, so the test restores exactly what it removed.
func tenantPolicyFor(schema, table string) string {
	return fmt.Sprintf(`
		CREATE POLICY %s_tenant_scope ON %s.%s
		    FOR ALL
		    TO organization_rt
		    USING      (tenant_id = current_setting('app.tenant_id', false)::uuid)
		    WITH CHECK (tenant_id = current_setting('app.tenant_id', false)::uuid)`,
		table, schema, table)
}

func providerPolicyFor(schema, table string) string {
	return fmt.Sprintf(`
		CREATE POLICY %s_provider_scope ON %s.%s
		    FOR ALL
		    TO organization_provider_rt
		    USING      (current_setting('app.provider_scope', false)::boolean)
		    WITH CHECK (current_setting('app.provider_scope', false)::boolean)`,
		table, schema, table)
}

func exec(t *testing.T, ctx context.Context, pool *db.Pool, statement string) {
	t.Helper()
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, statement)
		return err
	}); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

// TestTheRuntimeRoleSeesTheWholePosture is what makes the startup check and the readiness probe
// evidence. They run as organization_app, and a role that could not read the catalog rows of tables
// it holds no privilege on would see a smaller posture than exists -- possibly an intact one.
func TestTheRuntimeRoleSeesTheWholePosture(t *testing.T) {
	pool, ctx := runtimePool(t)

	report, err := posture.AssertIsolation(ctx, pool)
	if err != nil {
		t.Fatalf("AssertIsolation as the runtime role: %v", err)
	}
	if !report.OK() {
		t.Fatalf("the runtime role reports problems on an intact database: %v", report.Problems)
	}
	admin, adminCtx := adminPool(t)
	owner, err := posture.AssertIsolation(adminCtx, admin)
	if err != nil {
		t.Fatalf("AssertIsolation as the owner: %v", err)
	}
	if len(report.Tables) != len(owner.Tables) {
		t.Fatalf("the runtime role sees %d protected tables and the owner %d", len(report.Tables), len(owner.Tables))
	}
	for i, table := range report.Tables {
		if table.Policies != owner.Tables[i].Policies || table.Forced != owner.Tables[i].Forced {
			t.Errorf("%s: the runtime role sees policies=%d forced=%v, the owner policies=%d forced=%v",
				table.Qualified(), table.Policies, table.Forced, owner.Tables[i].Policies, owner.Tables[i].Forced)
		}
	}
}

// TestReadinessAndStartupRefuseAWeakenedDatabase drops FORCE from one table while the service's
// own role watches. The probe passes before, fails during and passes after; the startup check
// refuses during. Without this, the two calls in cmd/organization-control could be removed or
// pointed at a check that always passes, and every other test would stay green.
func TestReadinessAndStartupRefuseAWeakenedDatabase(t *testing.T) {
	pool, ctx := runtimePool(t)
	probe, err := posture.NewProbe(pool)
	if err != nil {
		t.Fatalf("NewProbe: %v", err)
	}
	if err := probe.Ping(ctx); err != nil {
		t.Fatalf("the probe fails on an intact database: %v", err)
	}
	if _, err := posture.Startup(ctx, pool, 5*time.Second); err != nil {
		t.Fatalf("the startup check fails on an intact database: %v", err)
	}

	admin, adminCtx := adminPool(t)
	exec(t, adminCtx, admin, "ALTER TABLE membership.membership NO FORCE ROW LEVEL SECURITY")
	restored := false
	t.Cleanup(func() {
		if !restored {
			exec(t, adminCtx, admin, "ALTER TABLE membership.membership FORCE ROW LEVEL SECURITY")
		}
	})

	if err := probe.Ping(ctx); err == nil || !strings.Contains(err.Error(), "membership.membership") ||
		!strings.Contains(err.Error(), "not FORCED") {
		t.Errorf("the probe passes, or does not name the table, with FORCE removed: %v", err)
	}
	if _, err := posture.Startup(ctx, pool, 5*time.Second); err == nil || !strings.Contains(err.Error(), "not FORCED") {
		t.Errorf("the startup check passes with FORCE removed: %v", err)
	}

	exec(t, adminCtx, admin, "ALTER TABLE membership.membership FORCE ROW LEVEL SECURITY")
	restored = true
	if err := probe.Ping(ctx); err != nil {
		t.Errorf("the probe still fails after the posture was repaired: %v", err)
	}
}

func TestNewProbeRequiresAPool(t *testing.T) {
	if _, err := posture.NewProbe(nil); err == nil {
		t.Fatal("NewProbe accepted a nil pool")
	}
}

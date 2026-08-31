package controldb_test

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

	"github.com/anshacerbia2/foundation-platform/db"

	"github.com/anshacerbia2/organization-control/internal/controldb"
)

// adminPool authenticates as the administrative role, because these cases change the schema.
// The isolation assertions in rls_integration_test.go connect as the runtime roles instead —
// that distinction is the point of that file and is not what this one tests.
func adminPool(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()
	return openAdmin(t)
}

func TestAssertIsolationAcceptsAnIntactDatabase(t *testing.T) {
	pool, ctx := adminPool(t)

	report, err := controldb.AssertIsolation(ctx, pool)
	if err != nil {
		t.Fatalf("AssertIsolation: %v", err)
	}
	if !report.OK() {
		t.Fatalf("a freshly migrated database reports problems: %v", report.Problems)
	}
	// Seven tables across five schemas. Asserted rather than left implicit, because every loop
	// in AssertIsolation is vacuous over an empty set — a report with no tables and no problems
	// would otherwise read as intact.
	if len(report.Tables) != 7 {
		t.Errorf("report covers %d tables, want 7", len(report.Tables))
	}
	for _, table := range report.Tables {
		if !table.Enabled || !table.Forced || table.Policies != 2 {
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
			expect:  "carries 1 policies",
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

			report, err := controldb.AssertIsolation(ctx, pool)
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
	report, err := controldb.AssertIsolation(ctx, pool)
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

// TestRLSSchemasMatchesTheGrantedSet keeps the Go constant and the SQL from drifting apart.
//
// AssertIsolation reads RLSSchemas and rls.sql hardcodes the same list. Two copies of a set is
// two chances to add a schema to one of them: a schema added to rls.sql and not here would be
// protected and unverified, and the reverse would fail every deploy for a schema with no tables.
func TestRLSSchemasMatchesTheGrantedSet(t *testing.T) {
	statements, err := controldb.SQL(controldb.StageRLS)
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, schema := range controldb.RLSSchemas {
		if !strings.Contains(statements, "'"+schema+"'") {
			t.Errorf("rls.sql does not mention schema %q, which AssertIsolation verifies", schema)
		}
	}
	// The two schemas deliberately outside the set. Their absence is a decision
	// TDD-organization-control-001 states, so it is asserted rather than left to a reader
	// noticing they are missing.
	for _, outside := range []string{"organization", "projection"} {
		for _, inside := range controldb.RLSSchemas {
			if inside == outside {
				t.Errorf("%q is in RLSSchemas; it is deliberately not tenant-scoped", outside)
			}
		}
	}
}

func TestSQLRejectsAnUnknownStage(t *testing.T) {
	if _, err := controldb.SQL(controldb.Stage("nonexistent.sql")); err == nil {
		t.Fatal("SQL accepted a stage that is not embedded")
	}
	for _, stage := range append([]controldb.Stage{controldb.StageRoles}, controldb.PostStages...) {
		body, err := controldb.SQL(stage)
		if err != nil {
			t.Errorf("SQL(%s): %v", stage, err)
		}
		if strings.TrimSpace(body) == "" {
			t.Errorf("SQL(%s) is empty", stage)
		}
	}
}

func TestPostStagesRunRLSBeforeGrants(t *testing.T) {
	// Both orders work, and this one means a window where privileges exist without policies
	// never opens: if the run fails between them, the runtime roles cannot reach the tables yet.
	if len(controldb.PostStages) != 2 {
		t.Fatalf("PostStages has %d entries, want 2", len(controldb.PostStages))
	}
	if controldb.PostStages[0] != controldb.StageRLS || controldb.PostStages[1] != controldb.StageGrants {
		t.Errorf("PostStages = %v, want [rls.sql grants.sql]", controldb.PostStages)
	}
}

func init() {
	// Keeps the linter from flagging the unused import when TEST_DATABASE_URL is absent and
	// every integration case skips.
	_ = os.Getenv
}

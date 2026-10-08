package access_test

// The recorder, exercised against the real engine as the real provider login role.
//
// There is no unit test for this package, deliberately. Everything worth asserting about a recorder
// is a property of the row it leaves behind and of the privileges the writer holds — whether the
// evidence survives a rollback, whether the writer can amend it, whether a blank reason is refused
// at the table as well as in Go. A fake transaction source can show that Exec was called with four
// arguments, which is the one thing nobody doubts.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/access"
	"github.com/anshacerbia2/organization-control/internal/db"
)

// providerPool opens a pool authenticating as the provider login role.
//
// As the provider role rather than the administrative one, because the privileges are half the
// contract: an owning connection would write the row and would prove nothing about whether the role
// that carries production traffic can.
func providerPool(t *testing.T) (*fdb.Pool, context.Context) {
	t.Helper()

	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		if os.Getenv("REQUIRE_INTEGRATION") != "" {
			t.Fatal("REQUIRE_INTEGRATION is set and TEST_DATABASE_URL is empty")
		}
		t.Skip("TEST_DATABASE_URL is unset")
	}

	rest := base
	if index := strings.Index(base, "://"); index >= 0 {
		rest = base[index+3:]
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	dsn := fmt.Sprintf("postgres://organization_provider_app:%s@%s",
		os.Getenv("TEST_PROVIDER_PASSWORD"), rest)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	pool, err := fdb.Open(ctx, fdb.Config{Name: "access-test", DSN: dsn, MaxConns: 2})
	if err != nil {
		t.Fatalf("open the provider pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// countFor reads the evidence back through the administrative connection.
//
// Through the admin DSN rather than the provider pool. The provider role reads the record for the
// review (ADR-ORG-002 §5.6), and a count through the writer's own connection would still be one the
// writer could shape; the owner's is not.
func countFor(t *testing.T, ctx context.Context, correlation id.UUID) int {
	t.Helper()

	admin, err := fdb.Open(ctx, fdb.Config{
		Name: "access-test-admin", DSN: os.Getenv("TEST_DATABASE_URL"), MaxConns: 1,
	})
	if err != nil {
		t.Fatalf("open the administrative pool: %v", err)
	}
	defer admin.Close()

	var count int
	if err := admin.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit.privileged_access WHERE correlation_id = $1`,
			correlation.String()).Scan(&count)
	}); err != nil {
		t.Fatalf("count evidence: %v", err)
	}
	return count
}

func newRecorder(t *testing.T, pool *fdb.Pool) *access.Recorder {
	t.Helper()
	recorder, err := access.New(pool)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	return recorder
}

func TestEvidenceIsWrittenForAProviderTransaction(t *testing.T) {
	pool, ctx := providerPool(t)

	actor, err := id.NewV7()
	if err != nil {
		t.Fatalf("actor: %v", err)
	}
	correlation, err := id.NewV7()
	if err != nil {
		t.Fatalf("correlation: %v", err)
	}

	scope, err := db.ProviderScope(actor, correlation, db.EmergencyAuthority())
	if err != nil {
		t.Fatalf("scope: %v", err)
	}

	providerScoped, err := db.NewProviderPool(pool, newRecorder(t, pool))
	if err != nil {
		t.Fatalf("provider pool: %v", err)
	}

	// Driven through WithProviderScope rather than by calling the recorder directly, so what is
	// asserted is the wiring: that opening a cross-Tenant transaction is what produces the row.
	if err := db.WithProviderScope(db.WithScope(ctx, scope), providerScoped,
		"read every Tenant for a compliance report",
		func(ctx context.Context, tx db.Tx) error {
			var one int
			return tx.QueryRow(ctx, `SELECT 1`).Scan(&one)
		}); err != nil {
		t.Fatalf("provider transaction: %v", err)
	}

	if count := countFor(t, ctx, correlation); count != 1 {
		t.Fatalf("the correlation has %d evidence rows, want 1", count)
	}
}

// TestEvidenceSurvivesADomainRollback is why the recorder writes in its own transaction.
//
// The access an investigation asks about is the one that failed. Evidence enrolled in the domain
// transaction would roll back with exactly those cases, leaving the failed cross-Tenant access as
// the one nothing records.
func TestEvidenceSurvivesADomainRollback(t *testing.T) {
	pool, ctx := providerPool(t)

	actor, err := id.NewV7()
	if err != nil {
		t.Fatalf("actor: %v", err)
	}
	correlation, err := id.NewV7()
	if err != nil {
		t.Fatalf("correlation: %v", err)
	}
	scope, err := db.ProviderScope(actor, correlation, db.EmergencyAuthority())
	if err != nil {
		t.Fatalf("scope: %v", err)
	}

	providerScoped, err := db.NewProviderPool(pool, newRecorder(t, pool))
	if err != nil {
		t.Fatalf("provider pool: %v", err)
	}

	sentinel := errors.New("the domain work failed")
	err = db.WithProviderScope(db.WithScope(ctx, scope), providerScoped,
		"attempt something that fails",
		func(context.Context, db.Tx) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("the transaction returned %v, want the sentinel", err)
	}

	if count := countFor(t, ctx, correlation); count != 1 {
		t.Errorf("a rolled-back access left %d evidence rows, want 1", count)
	}
}

// TestEvidenceIsAppendOnlyForTheWriter asserts the privilege, not the intention.
//
// grants.sql revokes SELECT, UPDATE, and DELETE on the audit schema from the provider role, because
// the loop that grants DML on every owned schema would otherwise hand the role being audited the
// ability to rewrite or erase its own trail. That revocation was missing on the first clean deploy
// and was found by querying has_table_privilege, so it is asserted here rather than trusted.
func TestEvidenceIsAppendOnlyForTheWriter(t *testing.T) {
	pool, ctx := providerPool(t)

	actor, err := id.NewV7()
	if err != nil {
		t.Fatalf("actor: %v", err)
	}
	correlation, err := id.NewV7()
	if err != nil {
		t.Fatalf("correlation: %v", err)
	}

	if err := newRecorder(t, pool).RecordProviderAccess(ctx, db.ProviderAccess{
		Actor: actor, Correlation: correlation, Reason: "establish a row to attack",
		Authority: db.AuthorityEmergency,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	// SELECT is not among them from 1.21.0: a provider in force reads the record to review it
	// (TDD-organization-control-001 §Privileged Access Review). Reading changes nothing; the refusals
	// below are what keep the evidence evidence.
	statements := map[string]string{
		"update":   `UPDATE audit.privileged_access SET reason = 'rewritten' WHERE correlation_id = $1`,
		"delete":   `DELETE FROM audit.privileged_access WHERE correlation_id = $1`,
		"truncate": `TRUNCATE audit.privileged_access`,
	}
	for name, statement := range statements {
		t.Run(name, func(t *testing.T) {
			err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
				var args []any
				if strings.Contains(statement, "$1") {
					args = append(args, correlation.String())
				}
				_, execErr := tx.Exec(ctx, statement, args...)
				return execErr
			})
			if err == nil {
				t.Errorf("the provider role could %s its own evidence", name)
			}
		})
	}

	// And the row is still there and unchanged, which is the property those refusals exist for.
	if count := countFor(t, ctx, correlation); count != 1 {
		t.Errorf("the evidence row count is %d after the attempts, want 1", count)
	}
}

// TestBlankReasonIsRefusedTwice checks the Go guard and the table constraint independently.
//
// Two layers because they fail for different callers: the Go check answers this repository's own
// paths, and the CHECK answers anything else that ever writes the table.
func TestBlankReasonIsRefusedTwice(t *testing.T) {
	pool, ctx := providerPool(t)

	actor, err := id.NewV7()
	if err != nil {
		t.Fatalf("actor: %v", err)
	}
	correlation, err := id.NewV7()
	if err != nil {
		t.Fatalf("correlation: %v", err)
	}

	if err := newRecorder(t, pool).RecordProviderAccess(ctx, db.ProviderAccess{
		Actor: actor, Correlation: correlation, Reason: "", Authority: db.AuthorityEmergency,
	}); !errors.Is(err, db.ErrReasonRequired) {
		t.Errorf("a blank reason returned %v, want db.ErrReasonRequired", err)
	}

	// Whitespace only, straight at the table, bypassing the Go guard. `length(btrim(reason)) > 0`
	// is what makes "   " not a reason; without btrim the constraint would accept it.
	accessID, err := id.NewV7()
	if err != nil {
		t.Fatalf("access id: %v", err)
	}
	err = pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		_, execErr := tx.Exec(ctx,
			`INSERT INTO audit.privileged_access (access_id, actor_id, correlation_id, reason, authority)
			 VALUES ($1, $2, $3, '   ', 'emergency')`,
			accessID.String(), actor.String(), correlation.String())
		return execErr
	})
	if err == nil {
		t.Error("the table accepted a whitespace-only reason")
	}

	if count := countFor(t, ctx, correlation); count != 0 {
		t.Errorf("a refused reason left %d evidence rows, want 0", count)
	}
}

// adminRow reads the columns a review filters on for the one row of a correlation.
func adminRow(t *testing.T, ctx context.Context, correlation id.UUID) (authority, activation, tenant, operation string) {
	t.Helper()
	admin, err := fdb.Open(ctx, fdb.Config{
		Name: "access-test-admin", DSN: os.Getenv("TEST_DATABASE_URL"), MaxConns: 1,
	})
	if err != nil {
		t.Fatalf("open the administrative pool: %v", err)
	}
	defer admin.Close()
	if err := admin.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `SELECT authority, coalesce(activation_id::text, ''),
		        coalesce(tenant_id::text, ''), coalesce(operation, '')
		   FROM audit.privileged_access WHERE correlation_id = $1`, correlation.String()).
			Scan(&authority, &activation, &tenant, &operation)
	}); err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	return authority, activation, tenant, operation
}

// TestEvidenceNamesItsAuthorityRouteAndTenant is ADR-ORG-002 §5.6: a row says what authority the
// access acted on, which activation, which route served it and which Tenant the path named, so a
// review can filter on each without joining the request logs.
func TestEvidenceNamesItsAuthorityRouteAndTenant(t *testing.T) {
	pool, ctx := providerPool(t)
	providerScoped, err := db.NewProviderPool(pool, newRecorder(t, pool))
	if err != nil {
		t.Fatalf("provider pool: %v", err)
	}
	mint := func() id.UUID {
		t.Helper()
		value, err := id.NewV7()
		if err != nil {
			t.Fatalf("id: %v", err)
		}
		return value
	}

	activation, tenant := mint(), mint()
	cases := []struct {
		name      string
		authority db.Authority
		tenant    id.UUID
		want      [4]string
	}{
		{"an activation, on a route naming a Tenant", db.ActivationAuthority(activation), tenant,
			[4]string{db.AuthorityActivation, activation.String(), tenant.String(), "GET /v1/tenants/{tenant_id}"}},
		{"an emergency grant, on a route naming none", db.EmergencyAuthority(), id.UUID{},
			[4]string{db.AuthorityEmergency, "", "", "GET /v1/tenants/{tenant_id}"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			correlation := mint()
			scope, err := db.ProviderScope(mint(), correlation, c.authority)
			if err != nil {
				t.Fatalf("scope: %v", err)
			}
			routed := db.WithAccessRoute(db.WithScope(ctx, scope), "GET /v1/tenants/{tenant_id}", c.tenant)
			if err := db.WithProviderScope(routed, providerScoped, "read one Tenant",
				func(context.Context, db.Tx) error { return nil }); err != nil {
				t.Fatalf("provider transaction: %v", err)
			}
			a, act, ten, op := adminRow(t, ctx, correlation)
			if got := [4]string{a, act, ten, op}; got != c.want {
				t.Errorf("recorded %v, want %v", got, c.want)
			}
		})
	}
}

// TestTheTableRefusesAnAuthorityWithoutItsActivation: the activation check holds for any writer, not
// only for this repository's Go guard.
func TestTheTableRefusesAnAuthorityWithoutItsActivation(t *testing.T) {
	pool, ctx := providerPool(t)
	statements := map[string]string{
		"an activation naming none": `INSERT INTO audit.privileged_access
		    (access_id, actor_id, correlation_id, reason, authority)
		    VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'r', 'activation')`,
		"an emergency naming one": `INSERT INTO audit.privileged_access
		    (access_id, actor_id, correlation_id, reason, authority, activation_id)
		    VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'r', 'emergency', gen_random_uuid())`,
		"an unknown authority": `INSERT INTO audit.privileged_access
		    (access_id, actor_id, correlation_id, reason, authority)
		    VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'r', 'root')`,
	}
	for name, statement := range statements {
		t.Run(name, func(t *testing.T) {
			if err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
				_, err := tx.Exec(ctx, statement)
				return err
			}); err == nil {
				t.Error("the table accepted it")
			}
		})
	}
}

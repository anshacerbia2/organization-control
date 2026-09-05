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
// Through the admin DSN rather than the provider pool, because the provider role deliberately holds
// no SELECT on this table. A test that read it back through the writer's own connection would only
// pass if that revocation were missing.
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

	scope, err := db.ProviderScope(actor, correlation)
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
	scope, err := db.ProviderScope(actor, correlation)
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
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	statements := map[string]string{
		"update": `UPDATE audit.privileged_access SET reason = 'rewritten' WHERE correlation_id = $1`,
		"delete": `DELETE FROM audit.privileged_access WHERE correlation_id = $1`,
		"select": `SELECT count(*) FROM audit.privileged_access WHERE correlation_id = $1`,
	}
	for name, statement := range statements {
		t.Run(name, func(t *testing.T) {
			err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
				_, execErr := tx.Exec(ctx, statement, correlation.String())
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
		Actor: actor, Correlation: correlation, Reason: "",
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
			`INSERT INTO audit.privileged_access (access_id, actor_id, correlation_id, reason)
			 VALUES ($1, $2, $3, '   ')`,
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

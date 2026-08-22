package membership

// The Week 2 exit criterion, against a real engine as the real runtime role:
//
//	injecting a failure after the status change and before the outbox append rolls back both;
//	membership_version never decreases.
//
// An in-package test file, because the injection seam is unexported. That is deliberate: the seam
// exists so the atomicity claim can be falsified, and exporting it would put a way to skip the
// outbox append into the production API.

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
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
)

const (
	tenantA      = "11111111-1111-4111-8111-11111111111a"
	tenantB      = "11111111-1111-4111-8111-11111111111b"
	fixedNowText = "2026-08-23T04:05:06Z"
)

func runtimeDSN(t *testing.T) string {
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
	return fmt.Sprintf("postgres://organization_app:%s@%s", os.Getenv("TEST_RUNTIME_PASSWORD"), rest)
}

// newFixture opens a pool as the tenant-scoped runtime role and returns a service with fixed time.
//
// The role matters: TDD-organization-control-001 refuses an owning connection as isolation
// evidence, and every assertion here also depends on the policy applying. On an administrative
// connection the cross-Tenant case below would pass while proving nothing.
func newFixture(t *testing.T) (*Service, context.Context, *fdb.Pool) {
	t.Helper()

	dsn := runtimeDSN(t)
	if dsn == "" {
		if os.Getenv("REQUIRE_INTEGRATION") != "" {
			t.Fatal("REQUIRE_INTEGRATION is set and TEST_DATABASE_URL is empty")
		}
		t.Skip("TEST_DATABASE_URL is unset")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	pool, err := fdb.Open(ctx, fdb.Config{Name: "membership-test", DSN: dsn, MaxConns: 2})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	tenantPool, err := db.NewTenantPool(pool)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	service, err := New(tenantPool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fixed, err := time.Parse(time.RFC3339, fixedNowText)
	if err != nil {
		t.Fatalf("parse fixed time: %v", err)
	}
	service.now = func() time.Time { return fixed }

	return service, boundTo(t, ctx, tenantA), pool
}

func boundTo(t *testing.T, ctx context.Context, tenant string) context.Context {
	t.Helper()
	tenantID, err := id.Parse(tenant)
	if err != nil {
		t.Fatalf("parse tenant: %v", err)
	}
	scope, err := db.TenantScope(tenantID, tenantID, id.UUID{})
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}
	return db.WithScope(ctx, scope)
}

func grantOne(t *testing.T, service *Service, ctx context.Context) Result {
	t.Helper()
	principal, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	result, err := service.Grant(ctx, GrantRequest{
		PrincipalID: principal,
		SubjectType: "human",
		Provenance:  "migration",
		ValidFrom:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	t.Cleanup(func() { cleanup(t, service, ctx, result.Membership.MembershipID) })
	return result
}

// cleanup removes the row and its events. The database is shared across the suite, and a leaked
// active Membership would make the partial unique index refuse a later grant for a reason
// unrelated to the test that failed.
func cleanup(t *testing.T, service *Service, ctx context.Context, membershipID id.UUID) {
	t.Helper()
	_ = db.WithTenantScope(ctx, service.pool, func(ctx context.Context, tx db.Tx) error {
		_, _ = tx.Exec(ctx, `DELETE FROM platform.outbox WHERE aggregate_id = $1`, membershipID.String())
		_, _ = tx.Exec(ctx, `DELETE FROM membership.membership WHERE membership_id = $1`, membershipID.String())
		return nil
	})
}

func statusAndVersion(t *testing.T, service *Service, ctx context.Context, membershipID id.UUID) (State, int64) {
	t.Helper()
	var (
		status  string
		version int64
	)
	if err := db.WithTenantScope(ctx, service.pool, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status, membership_version FROM membership.membership WHERE membership_id = $1`,
			membershipID.String()).Scan(&status, &version)
	}); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return State(status), version
}

func outboxCount(t *testing.T, service *Service, ctx context.Context, membershipID id.UUID) int {
	t.Helper()
	var count int
	if err := db.WithTenantScope(ctx, service.pool, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM platform.outbox WHERE aggregate_id = $1`,
			membershipID.String()).Scan(&count)
	}); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return count
}

// TestTheStatusChangeAndTheEventCommitTogether is the exit criterion.
//
// A revocation that commits without its event is unreachable by every consumer: authority says
// revoked, every projection says active, and nothing in the system disagrees out loud. That is the
// exact failure the transactional outbox exists to prevent, and the only honest way to assert it
// is to fail inside the window it protects.
func TestTheStatusChangeAndTheEventCommitTogether(t *testing.T) {
	service, ctx, _ := newFixture(t)
	granted := grantOne(t, service, ctx)

	statusBefore, versionBefore := statusAndVersion(t, service, ctx, granted.Membership.MembershipID)
	eventsBefore := outboxCount(t, service, ctx, granted.Membership.MembershipID)

	injected := errors.New("failure between the status change and the append")
	service.beforeAppend = func(context.Context) error { return injected }
	t.Cleanup(func() { service.beforeAppend = nil })

	_, err := service.Revoke(ctx, granted.Membership.MembershipID)
	if !errors.Is(err, injected) {
		t.Fatalf("error = %v, want the injected failure", err)
	}

	statusAfter, versionAfter := statusAndVersion(t, service, ctx, granted.Membership.MembershipID)
	if statusAfter != statusBefore {
		t.Errorf("status = %s after a rolled-back revocation, want %s", statusAfter, statusBefore)
	}
	if versionAfter != versionBefore {
		t.Errorf("version = %d after a rolled-back revocation, want %d", versionAfter, versionBefore)
	}
	if got := outboxCount(t, service, ctx, granted.Membership.MembershipID); got != eventsBefore {
		t.Errorf("the outbox holds %d events, want %d", got, eventsBefore)
	}

	// The same revocation succeeds once the injected failure is removed, so the rollback left the
	// row usable rather than merely unchanged.
	service.beforeAppend = nil
	revoked, err := service.Revoke(ctx, granted.Membership.MembershipID)
	if err != nil {
		t.Fatalf("Revoke after the rollback: %v", err)
	}
	if revoked.Membership.Status != StateRevoked {
		t.Errorf("status = %s, want revoked", revoked.Membership.Status)
	}
	if revoked.Membership.Version != versionBefore+1 {
		t.Errorf("version = %d, want %d", revoked.Membership.Version, versionBefore+1)
	}
}

// TestVersionNeverDecreases is the other half of the exit criterion.
//
// The version is the staleness test a consumer applies without a remote call: it rejects a token
// whose version is below the one its projection holds. A version that ever went backwards would
// make an older grant look newer than the revocation that followed it.
func TestVersionNeverDecreases(t *testing.T) {
	service, ctx, _ := newFixture(t)
	granted := grantOne(t, service, ctx)

	versions := []int64{granted.Membership.Version}

	suspended, err := service.Suspend(ctx, granted.Membership.MembershipID)
	if err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	versions = append(versions, suspended.Membership.Version)

	restored, err := service.Restore(ctx, granted.Membership.MembershipID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	versions = append(versions, restored.Membership.Version)

	revoked, err := service.Revoke(ctx, granted.Membership.MembershipID)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	versions = append(versions, revoked.Membership.Version)

	for i := 1; i < len(versions); i++ {
		if versions[i] <= versions[i-1] {
			t.Errorf("version %d did not increase over %d at step %d", versions[i], versions[i-1], i)
		}
	}

	// One event per transition, all four on one aggregate. A transition that published nothing
	// would leave consumers on the previous state with no error anywhere.
	if got := outboxCount(t, service, ctx, granted.Membership.MembershipID); got != len(versions) {
		t.Errorf("the outbox holds %d events for %d transitions", got, len(versions))
	}
}

// TestWithdrawalTakesThePriorityLaneOnTheWire asserts the classification reached the row, not just
// the option. ADR-GLB-003 §5 reserves a separate topic and consumer group for priority events, and
// the lane is a column the dispatcher reads.
func TestWithdrawalTakesThePriorityLaneOnTheWire(t *testing.T) {
	service, ctx, _ := newFixture(t)
	granted := grantOne(t, service, ctx)

	if _, err := service.Revoke(ctx, granted.Membership.MembershipID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	type row struct {
		eventType string
		priority  int16
	}
	var rows []row
	if err := db.WithTenantScope(ctx, service.pool, func(ctx context.Context, tx db.Tx) error {
		result, err := tx.Query(ctx,
			`SELECT event_type, priority FROM platform.outbox WHERE aggregate_id = $1 ORDER BY sequence`,
			granted.Membership.MembershipID.String())
		if err != nil {
			return err
		}
		defer result.Close()
		for result.Next() {
			var next row
			if err := result.Scan(&next.eventType, &next.priority); err != nil {
				return err
			}
			rows = append(rows, next)
		}
		return result.Err()
	}); err != nil {
		t.Fatalf("read outbox: %v", err)
	}

	if len(rows) != 2 {
		t.Fatalf("the outbox holds %d events, want 2", len(rows))
	}
	// The named constants rather than their values: the dispatcher claims with `ORDER BY priority
	// ASC`, so the reserved lane is the *lower* number and a literal here would read backwards.
	if !strings.Contains(rows[0].eventType, "lifecycle.granted") || rows[0].priority != outbox.PriorityStandard {
		t.Errorf("grant = %+v; want a lifecycle type in the standard lane", rows[0])
	}
	if !strings.Contains(rows[1].eventType, "security.revoked") || rows[1].priority != outbox.PriorityHigh {
		t.Errorf("revoke = %+v; want a security type in the priority lane", rows[1])
	}
}

// TestGrantRefusesATenantOtherThanTheBoundOne is SAD-004 §8.3 at the service layer.
//
// A Tenant identifier arriving with a request is a *requested* scope. Refused before any statement
// runs, so the RLS `WITH CHECK` stays a second line of defence rather than the first — and a
// cross-tenant attempt never reaches the database at all.
func TestGrantRefusesATenantOtherThanTheBoundOne(t *testing.T) {
	service, ctx, _ := newFixture(t)

	other, err := id.Parse(tenantB)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	principal, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}

	if _, err := service.Grant(ctx, GrantRequest{
		PrincipalID: principal,
		TenantID:    other,
		SubjectType: "human",
		Provenance:  "migration",
		ValidFrom:   time.Now().UTC(),
	}); err == nil {
		t.Fatal("a grant naming another Tenant was accepted")
	}
}

// TestASecondActiveMembershipIsRefused is the partial unique index. Two active Memberships for one
// subject in one context would give a consumer two versions to compare and no rule for choosing.
func TestASecondActiveMembershipIsRefused(t *testing.T) {
	service, ctx, _ := newFixture(t)
	granted := grantOne(t, service, ctx)

	if _, err := service.Grant(ctx, GrantRequest{
		PrincipalID: granted.Membership.PrincipalID,
		SubjectType: "human",
		Provenance:  "migration",
		ValidFrom:   time.Now().UTC(),
	}); err == nil {
		t.Fatal("a second active Membership was accepted for the same subject and context")
	}

	// Revoking the first frees the slot: the index is partial on `status = 'active'`, so the
	// terminal row does not block a new grant with its own provenance — which is the documented
	// way back from a revocation.
	if _, err := service.Revoke(ctx, granted.Membership.MembershipID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	replacement, err := service.Grant(ctx, GrantRequest{
		PrincipalID: granted.Membership.PrincipalID,
		SubjectType: "human",
		Provenance:  "provider grant",
		ValidFrom:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("a replacement grant after revocation was refused: %v", err)
	}
	t.Cleanup(func() { cleanup(t, service, ctx, replacement.Membership.MembershipID) })
}

// TestAcceptedAtIsRecordedNotObserved is the durability statement.
//
// STD-IAM-001 §3.4 makes acknowledgement mean durable and queued, never enforced, and the
// operational dashboard shows accepted and enforced separately for that reason. The value must come
// from a recorded origin rather than from whenever a log line was written, which is why the clock
// is a seam.
func TestAcceptedAtIsRecordedNotObserved(t *testing.T) {
	service, ctx, _ := newFixture(t)
	granted := grantOne(t, service, ctx)

	fixed, err := time.Parse(time.RFC3339, fixedNowText)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !granted.AcceptedAt.Equal(fixed) {
		t.Errorf("AcceptedAt = %s, want %s", granted.AcceptedAt, fixed)
	}

	// The same instant reaches the envelope, so a consumer measuring propagation and the caller
	// measuring acknowledgement work from one origin rather than two clocks.
	var occurred time.Time
	if err := db.WithTenantScope(ctx, service.pool, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT (envelope->>'time')::timestamptz FROM platform.outbox WHERE aggregate_id = $1`,
			granted.Membership.MembershipID.String()).Scan(&occurred)
	}); err != nil {
		t.Fatalf("read envelope time: %v", err)
	}
	if !occurred.Equal(fixed) {
		t.Errorf("the envelope carries %s, want %s", occurred, fixed)
	}
}

// TestAMembershipInAnotherTenantIsNotFound closes the leak an honest error message would open.
//
// Under Row-Level Security the row is simply absent, and reporting that it exists elsewhere would
// disclose the existence of a row this caller may not read — which is a cross-tenant disclosure
// through an error string rather than through a query.
func TestAMembershipInAnotherTenantIsNotFound(t *testing.T) {
	service, ctxA, _ := newFixture(t)
	granted := grantOne(t, service, ctxA)

	ctxB := boundTo(t, context.Background(), tenantB)
	_, err := service.Revoke(ctxB, granted.Membership.MembershipID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if strings.Contains(strings.ToLower(fmt.Sprint(err)), tenantA) {
		t.Error("the error names the owning Tenant")
	}

	// And it is still active for its own Tenant, so the refused attempt changed nothing.
	if status, _ := statusAndVersion(t, service, ctxA, granted.Membership.MembershipID); status != StateActive {
		t.Errorf("status = %s after a cross-tenant attempt, want active", status)
	}
}

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
	_ = ownerPool(t, ctx).InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		_, _ = tx.Exec(ctx, `DELETE FROM platform.outbox WHERE aggregate_id = $1`, membershipID.String())
		_, _ = tx.Exec(ctx, `DELETE FROM membership.membership_event WHERE membership_id = $1`, membershipID.String())
		_, _ = tx.Exec(ctx, `DELETE FROM membership.membership WHERE membership_id = $1`, membershipID.String())
		return nil
	})
}

// at is a transition command at the Membership's current version, with a reason, which is what a
// caller that has just read the Membership sends.
func at(t *testing.T, service *Service, ctx context.Context, membershipID id.UUID) Command {
	t.Helper()
	_, version := statusAndVersion(t, service, ctx, membershipID)
	return Command{MembershipID: membershipID, ExpectedVersion: version, Reason: "the test acts on what it read"}
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
	if err := ownerPool(t, ctx).InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
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

	_, err := service.Revoke(ctx, at(t, service, ctx, granted.Membership.MembershipID))
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
	revoked, err := service.Revoke(ctx, at(t, service, ctx, granted.Membership.MembershipID))
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

	suspended, err := service.Suspend(ctx, at(t, service, ctx, granted.Membership.MembershipID))
	if err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	versions = append(versions, suspended.Membership.Version)

	restored, err := service.Restore(ctx, at(t, service, ctx, granted.Membership.MembershipID))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	versions = append(versions, restored.Membership.Version)

	revoked, err := service.Revoke(ctx, at(t, service, ctx, granted.Membership.MembershipID))
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

// historyMatchingOutbox counts history rows that agree with a published event: same identifier,
// same Membership, and the version the envelope carries. A row that disagrees with its event is the
// SUPERSEDED predicate comparing the wrong number.
func historyMatchingOutbox(t *testing.T, ctx context.Context, membershipID id.UUID) (matching, total int) {
	t.Helper()
	if err := ownerPool(t, ctx).InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE o.event_id IS NOT NULL
			                          AND o.aggregate_id = h.membership_id
			                          AND o.event_type = h.event_type
			                          AND (o.envelope->'data'->>'membership_version')::bigint = h.membership_version),
			       count(*)
			  FROM membership.membership_event h
			  LEFT JOIN platform.outbox o ON o.event_id = h.event_id
			 WHERE h.membership_id = $1`,
			membershipID.String()).Scan(&matching, &total)
	}); err != nil {
		t.Fatalf("read membership history: %v", err)
	}
	return matching, total
}

// TestEveryPublishedEventRecordsItsVersion is the other half of SUPERSEDED: the history row exists
// if and only if the event does, and says what the event says.
//
// Asserted through a failure as well as a success. A history insert outside the publishing
// transaction would survive a rolled-back transition, and a version recorded for an event nobody
// published is a newer version the resolver would believe in.
func TestEveryPublishedEventRecordsItsVersion(t *testing.T) {
	service, ctx, _ := newFixture(t)
	granted := grantOne(t, service, ctx)
	membershipID := granted.Membership.MembershipID

	if _, err := service.Suspend(ctx, at(t, service, ctx, membershipID)); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if _, err := service.Restore(ctx, at(t, service, ctx, membershipID)); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	injected := errors.New("failure inside the publishing transaction")
	service.beforeAppend = func(context.Context) error { return injected }
	if _, err := service.Revoke(ctx, at(t, service, ctx, membershipID)); !errors.Is(err, injected) {
		t.Fatalf("error = %v, want the injected failure", err)
	}
	service.beforeAppend = nil

	if _, err := service.Revoke(ctx, at(t, service, ctx, membershipID)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	matching, total := historyMatchingOutbox(t, ctx, membershipID)
	events := outboxCount(t, service, ctx, membershipID)
	if total != events || matching != events {
		t.Errorf("%d events published, %d history rows, %d agreeing with their event; want all equal",
			events, total, matching)
	}
}

// TestWithdrawalTakesThePriorityLaneOnTheWire asserts the classification reached the row, not just
// the option. ADR-GLB-003 §5 reserves a separate topic and consumer group for priority events, and
// the lane is a column the dispatcher reads.
func TestWithdrawalTakesThePriorityLaneOnTheWire(t *testing.T) {
	service, ctx, _ := newFixture(t)
	granted := grantOne(t, service, ctx)

	if _, err := service.Revoke(ctx, at(t, service, ctx, granted.Membership.MembershipID)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	type row struct {
		eventType string
		priority  int16
	}
	var rows []row
	if err := ownerPool(t, ctx).InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
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
	if _, err := service.Revoke(ctx, at(t, service, ctx, granted.Membership.MembershipID)); err != nil {
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
	if err := ownerPool(t, ctx).InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
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
	_, err := service.Revoke(ctxB, Command{MembershipID: granted.Membership.MembershipID, ExpectedVersion: granted.Membership.Version, Reason: "from another Tenant"})
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

// ownerPool is the owner connection, for assertions that read platform tables.
//
// No production path under tenant scope reads platform.outbox, so the tenant runtime role holds
// no SELECT there. These assertions are the test inspecting what was published, and inspecting
// is not the system under test -- routing them through the service's credential is what made an
// unnecessary grant look necessary.
func ownerPool(t *testing.T, ctx context.Context) *fdb.Pool {
	t.Helper()
	pool, err := fdb.Open(ctx, fdb.Config{
		Name: "membership-test-owner", DSN: os.Getenv("TEST_DATABASE_URL"), MaxConns: 2})
	if err != nil {
		t.Fatalf("open owner pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestAStaleVersionIsRefusedAndChangesNothing is the optimistic check TDD-organization-control-002
// §API requires of every mutation. A caller acting on a view that has since changed is answered
// ErrVersionMismatch, and neither the row, its version nor the outbox moves.
func TestAStaleVersionIsRefusedAndChangesNothing(t *testing.T) {
	service, ctx, _ := newFixture(t)
	granted := grantOne(t, service, ctx)
	membershipID := granted.Membership.MembershipID

	shown := Command{MembershipID: membershipID, ExpectedVersion: granted.Membership.Version}
	if _, err := service.Suspend(ctx, shown); err != nil {
		t.Fatalf("Suspend at the version shown: %v", err)
	}

	statusBefore, versionBefore := statusAndVersion(t, service, ctx, membershipID)
	eventsBefore := outboxCount(t, service, ctx, membershipID)

	// A second administrator restores from the view the first one acted on.
	if _, err := service.Restore(ctx, shown); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("Restore at a stale version: error = %v, want ErrVersionMismatch", err)
	}
	statusAfter, versionAfter := statusAndVersion(t, service, ctx, membershipID)
	if statusAfter != statusBefore || versionAfter != versionBefore {
		t.Errorf("a refused transition moved the row: %s/%d, want %s/%d", statusAfter, versionAfter, statusBefore, versionBefore)
	}
	if got := outboxCount(t, service, ctx, membershipID); got != eventsBefore {
		t.Errorf("a refused transition published: %d events, want %d", got, eventsBefore)
	}

	// The state machine answers before the version does: a stale suspension of a suspended
	// Membership says what happened rather than only that something did.
	if _, err := service.Suspend(ctx, shown); !errors.Is(err, ErrTransitionRefused) {
		t.Errorf("Suspend of a suspended Membership at a stale version: error = %v, want ErrTransitionRefused", err)
	}
}

// TestATransitionNamesAVersionAndARevocationAReason refuses the two omissions before any statement
// runs.
func TestATransitionNamesAVersionAndARevocationAReason(t *testing.T) {
	service, ctx, _ := newFixture(t)
	granted := grantOne(t, service, ctx)
	membershipID := granted.Membership.MembershipID

	if _, err := service.Suspend(ctx, Command{MembershipID: membershipID}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Suspend without a version: error = %v, want ErrInvalid", err)
	}
	if _, err := service.Revoke(ctx, Command{MembershipID: membershipID, ExpectedVersion: 1, Reason: "  "}); !errors.Is(err, ErrReasonRequired) {
		t.Errorf("Revoke with a blank reason: error = %v, want ErrReasonRequired", err)
	}
	if status, version := statusAndVersion(t, service, ctx, membershipID); status != StateActive || version != 1 {
		t.Errorf("a refused command moved the row to %s/%d", status, version)
	}
}

// TestTheEventRecordsWhoActedAndWhy is the "record acting subject, reason, and correlation
// identifier" step of TDD-organization-control-002 §Revocation, on the history row the transition
// writes in its own transaction.
func TestTheEventRecordsWhoActedAndWhy(t *testing.T) {
	service, ctx, _ := newFixture(t)
	granted := grantOne(t, service, ctx)
	membershipID := granted.Membership.MembershipID

	scope, _ := db.ScopeFrom(ctx)
	correlation, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	correlated, err := db.TenantScope(scope.TenantID(), scope.Actor(), correlation)
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}
	ctx = db.WithScope(ctx, correlated)

	if _, err := service.Suspend(ctx, Command{MembershipID: membershipID, ExpectedVersion: 1}); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if _, err := service.Revoke(ctx, Command{MembershipID: membershipID, ExpectedVersion: 2,
		Reason: "left the organisation"}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	type recorded struct {
		version     int64
		actor       *string
		correlation *string
		reason      *string
	}
	var history []recorded
	if err := ownerPool(t, ctx).InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		rows, err := tx.Query(ctx, `SELECT membership_version, actor_id::text, correlation_id::text, reason
			FROM membership.membership_event WHERE membership_id = $1 ORDER BY membership_version`,
			membershipID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var next recorded
			if err := rows.Scan(&next.version, &next.actor, &next.correlation, &next.reason); err != nil {
				return err
			}
			history = append(history, next)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read history: %v", err)
	}

	if len(history) != 3 {
		t.Fatalf("%d history rows, want 3 (grant, suspend, revoke)", len(history))
	}
	for _, row := range history {
		if row.actor == nil || *row.actor != scope.Actor().String() {
			t.Errorf("version %d records actor %v, want %s", row.version, row.actor, scope.Actor())
		}
	}
	if history[1].reason != nil {
		t.Errorf("a suspension sent without a reason recorded %q", *history[1].reason)
	}
	if history[2].reason == nil || *history[2].reason != "left the organisation" {
		t.Errorf("the revocation recorded reason %v, want the one sent", history[2].reason)
	}
	if history[2].correlation == nil || *history[2].correlation != correlation.String() {
		t.Errorf("the revocation recorded correlation %v, want %s", history[2].correlation, correlation)
	}
}

// TestReadsAreKeysetPagesConfinedToTheTenant is the read side STD-GLB-001 1.3.0 §Pagination fixes:
// key order, a page of `limit`, `next` naming the last item and null on the last page, filters that
// hold for every page, and Row-Level Security confining both reads to the bound Tenant.
func TestReadsAreKeysetPagesConfinedToTheTenant(t *testing.T) {
	service, ctx, _ := newFixture(t)

	// Three Memberships for one Principal, which only that Principal's filter selects: a human
	// one, a workload one, and a second human one after the first is revoked.
	first := grantOne(t, service, ctx)
	principal := first.Membership.PrincipalID
	grant := func(subject string) Result {
		t.Helper()
		result, err := service.Grant(ctx, GrantRequest{
			PrincipalID: principal, SubjectType: subject, Provenance: "migration", ValidFrom: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("Grant %s: %v", subject, err)
		}
		t.Cleanup(func() { cleanup(t, service, ctx, result.Membership.MembershipID) })
		return result
	}
	second := grant("workload")
	if _, err := service.Revoke(ctx, at(t, service, ctx, first.Membership.MembershipID)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	third := grant("human")

	page, err := service.List(ctx, ListQuery{PrincipalID: principal, Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Memberships) != 2 || page.Next == nil {
		t.Fatalf("first page: %d items, next %v; want 2 and a cursor", len(page.Memberships), page.Next)
	}
	if page.Memberships[0].MembershipID != first.Membership.MembershipID ||
		page.Memberships[1].MembershipID != second.Membership.MembershipID {
		t.Errorf("first page is not in creation order")
	}
	if *page.Next != second.Membership.MembershipID {
		t.Errorf("next = %s, want the last item of the page", *page.Next)
	}
	if page.Memberships[0].Provenance == "" || page.Memberships[0].ValidFrom.IsZero() {
		t.Errorf("a listed Membership lost its provenance or valid_from: %+v", page.Memberships[0])
	}

	last, err := service.List(ctx, ListQuery{PrincipalID: principal, Limit: 2, After: *page.Next})
	if err != nil {
		t.Fatalf("List after: %v", err)
	}
	if len(last.Memberships) != 1 || last.Memberships[0].MembershipID != third.Membership.MembershipID || last.Next != nil {
		t.Errorf("last page: %+v, want the third Membership and no cursor", last)
	}

	revoked, err := service.List(ctx, ListQuery{PrincipalID: principal, Status: StateRevoked})
	if err != nil {
		t.Fatalf("List revoked: %v", err)
	}
	if len(revoked.Memberships) != 1 || revoked.Memberships[0].Status != StateRevoked {
		t.Errorf("the revoked filter returned %+v", revoked.Memberships)
	}

	if _, err := service.List(ctx, ListQuery{Status: "lapsed"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown status: error = %v, want ErrInvalid", err)
	}
	for _, limit := range []int{-1, 101} {
		if _, err := service.List(ctx, ListQuery{Limit: limit}); !errors.Is(err, ErrInvalid) {
			t.Errorf("limit %d: error = %v, want ErrInvalid", limit, err)
		}
	}

	read, err := service.Get(ctx, third.Membership.MembershipID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if read.Version != third.Membership.Version || read.Status != StateActive || read.PrincipalID != principal {
		t.Errorf("Get = %+v", read)
	}

	other := boundTo(t, context.Background(), tenantB)
	if _, err := service.Get(other, third.Membership.MembershipID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get from another Tenant: error = %v, want ErrNotFound", err)
	}
	elsewhere, err := service.List(other, ListQuery{PrincipalID: principal})
	if err != nil {
		t.Fatalf("List from another Tenant: %v", err)
	}
	if len(elsewhere.Memberships) != 0 {
		t.Errorf("another Tenant listed %d of this Tenant's Memberships", len(elsewhere.Memberships))
	}
}

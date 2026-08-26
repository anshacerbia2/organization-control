package projection

// The Week 3 exit criterion, against a real engine as the provider runtime role:
//
//	a consumer that has not registered receives no projection; a snapshot plus the events after
//	its mark reconstruct the authoritative set with no gap and no duplicate.
//
// In-package, because the clock seams are unexported and one test needs to hold a transaction open
// while a snapshot runs — which no exported surface can express.

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

	"github.com/anshacerbia2/organization-control/internal/db"
)

const fixedNowText = "2026-08-24T06:07:08Z"

type recorder struct{ calls int }

func (r *recorder) RecordProviderAccess(context.Context, db.ProviderAccess) error {
	r.calls++
	return nil
}

type fixture struct {
	pool       *fdb.Pool
	provider   *db.ProviderPool
	registry   *Registry
	publisher  *Publisher
	reconciler *Reconciler
	ctx        context.Context
	fixed      time.Time
}

func newFixture(t *testing.T) *fixture {
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	// Six connections: the in-flight-transaction test holds one open while a snapshot, a commit,
	// and each cleanup helper run on their own.
	pool, err := fdb.Open(ctx, fdb.Config{Name: "projection-test", DSN: dsn, MaxConns: 6})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	provider, err := db.NewProviderPool(pool, &recorder{})
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}
	registry, err := NewRegistry(provider)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	publisher, err := NewPublisher(provider, registry)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	reconciler, err := NewReconciler(provider)
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}

	fixed, err := time.Parse(time.RFC3339, fixedNowText)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	registry.now = func() time.Time { return fixed }
	publisher.now = func() time.Time { return fixed }
	reconciler.now = func() time.Time { return fixed }

	actor, correlation := mustID(t), mustID(t)
	scope, err := db.ProviderScope(actor, correlation)
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	return &fixture{
		pool: pool, provider: provider, registry: registry, publisher: publisher,
		reconciler: reconciler, ctx: db.WithScope(ctx, scope), fixed: fixed,
	}
}

func mustID(t *testing.T) id.UUID {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	return value
}

func (f *fixture) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	if err := db.WithProviderScope(f.ctx, f.provider, "projection suite fixture",
		func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, statement, args...)
			return err
		}); err != nil {
		t.Fatalf("fixture statement: %v", err)
	}
}

// register creates a consumer with a name unique to this test, so tests can run concurrently
// against one database without colliding on the registry's primary key.
func (f *fixture) register(t *testing.T) string {
	t.Helper()
	consumerID := "test-" + mustID(t).String()
	if _, err := f.registry.Register(f.ctx, Registration{
		ConsumerID:        consumerID,
		ProjectionVersion: "v1",
		MaxAcceptedAge:    30 * time.Second,
		StaleBehavior:     StaleFailClosed,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM projection.consumer WHERE consumer_id = $1`, consumerID)
	})
	return consumerID
}

// seedTenant creates an Organization and an active Tenant, returning the Tenant identifier.
func (f *fixture) seedTenant(t *testing.T) id.UUID {
	t.Helper()
	organizationID, tenantID := mustID(t), mustID(t)
	f.exec(t, `INSERT INTO organization.organization (organization_id, display_name, classification, status)
	    VALUES ($1, 'projection suite', 'customer', 'active')`, organizationID.String())
	f.exec(t, `INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
	    VALUES ($1, $2, 'projection suite', 'active', 'pooled')`, tenantID.String(), organizationID.String())
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM organization.organization WHERE organization_id = $1`, organizationID.String())
	})
	return tenantID
}

// seedMembership inserts an active Membership directly. Direct SQL rather than the Membership
// service, because these tests are about what the publisher reads, and routing through the service
// would also append outbox rows that move the very mark under test.
func (f *fixture) seedMembership(t *testing.T, tenantID id.UUID, version int64) id.UUID {
	t.Helper()
	membershipID, principalID := mustID(t), mustID(t)
	f.exec(t, `INSERT INTO membership.membership
	    (membership_id, principal_id, tenant_id, subject_type, status, membership_version, valid_from, provenance)
	    VALUES ($1, $2, $3, 'human', 'active', $4, now(), 'projection suite')`,
		membershipID.String(), principalID.String(), tenantID.String(), version)
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM membership.membership WHERE membership_id = $1`, membershipID.String())
	})
	return membershipID
}

func (f *fixture) rowsFor(t *testing.T, consumerID string, tenantID id.UUID) []Row {
	t.Helper()
	var (
		all    []Row
		cursor string
		mark   *int64
	)
	for page := 0; page < 50; page++ {
		got, err := f.publisher.Snapshot(f.ctx, SnapshotRequest{
			ConsumerID: consumerID, PageSize: 2, Cursor: cursor, Mark: mark,
		})
		if err != nil {
			t.Fatalf("Snapshot page %d: %v", page, err)
		}
		carried := got.HighWaterMark
		mark = &carried
		for _, row := range got.Rows {
			if row.TenantID == tenantID {
				all = append(all, row)
			}
		}
		if got.Cursor == "" {
			return all
		}
		cursor = got.Cursor
	}
	t.Fatal("snapshot did not terminate within 50 pages")
	return nil
}

// TestAnUnregisteredConsumerReceivesNoProjection is the first half of the exit criterion.
//
// Without a registration there is no declared freshness budget and no declared stale behavior, so
// nothing can state what a copy of the projection is allowed to be used for. Serving it anyway
// would put context data in a consumer that has made no commitment about how it treats staleness.
func TestAnUnregisteredConsumerReceivesNoProjection(t *testing.T) {
	f := newFixture(t)

	_, err := f.publisher.Snapshot(f.ctx, SnapshotRequest{ConsumerID: "never-registered", PageSize: 10})
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("error = %v, want ErrNotRegistered", err)
	}

	// And a registered consumer does receive one, so the refusal above is the registration check
	// rather than the snapshot being broken for everybody.
	consumerID := f.register(t)
	if _, err := f.publisher.Snapshot(f.ctx, SnapshotRequest{ConsumerID: consumerID, PageSize: 10}); err != nil {
		t.Fatalf("Snapshot for a registered consumer: %v", err)
	}
}

// TestAProgressReportBeforeASnapshotIsRefused is the bootstrap contract's teeth.
//
// A consumer that subscribed and started applying without a snapshot holds everything that
// happened since it connected and nothing that happened before. Accepting a position for it would
// record that incomplete model as a current one, and every later freshness measurement would agree.
func TestAProgressReportBeforeASnapshotIsRefused(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)

	_, err := f.registry.RecordProgress(f.ctx, Progress{ConsumerID: consumerID, AppliedMark: 42})
	if !errors.Is(err, ErrNoSnapshotMark) {
		t.Fatalf("error = %v, want ErrNoSnapshotMark", err)
	}

	// After bootstrapping, the same report is accepted.
	if _, err := f.publisher.Bootstrap(f.ctx, consumerID, 40); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	consumer, err := f.registry.RecordProgress(f.ctx, Progress{ConsumerID: consumerID, AppliedMark: 42})
	if err != nil {
		t.Fatalf("RecordProgress after bootstrap: %v", err)
	}
	if consumer.LastReportedMark == nil || *consumer.LastReportedMark != 42 {
		t.Errorf("reported mark = %v, want 42", consumer.LastReportedMark)
	}
	if consumer.LastReportedAt == nil || !consumer.LastReportedAt.Equal(f.fixed) {
		t.Errorf("reported at = %v, want %s", consumer.LastReportedAt, f.fixed)
	}
}

// TestAReportedPositionCannotGoBackwards. The outbox sequence is monotonic per publisher, so a
// lower reported value is either a replay being misreported as progress or two processes sharing
// one consumer identity — and under the second reading, freshness measured from the lower value
// would be wrong for the other process.
func TestAReportedPositionCannotGoBackwards(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)

	if _, err := f.publisher.Bootstrap(f.ctx, consumerID, 10); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, err := f.registry.RecordProgress(f.ctx, Progress{ConsumerID: consumerID, AppliedMark: 100}); err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}

	_, err := f.registry.RecordProgress(f.ctx, Progress{ConsumerID: consumerID, AppliedMark: 99})
	if !errors.Is(err, ErrMarkWentBackwards) {
		t.Fatalf("error = %v, want ErrMarkWentBackwards", err)
	}

	// Re-reporting the same position is accepted: a consumer that made no progress this cycle is
	// still reporting liveness, and refusing it would make an idle consumer look disconnected.
	if _, err := f.registry.RecordProgress(f.ctx, Progress{ConsumerID: consumerID, AppliedMark: 100}); err != nil {
		t.Fatalf("re-reporting the same position: %v", err)
	}
}

// TestPagingCoversTheSetOnceWithNoDuplicate is the "no gap and no duplicate" half of the exit
// criterion, at the paging layer.
//
// Keyset paging on membership_id rather than OFFSET: the cursor is what makes a page boundary
// stable, and an OFFSET-paged snapshot re-reads and discards everything it skips.
func TestPagingCoversTheSetOnceWithNoDuplicate(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)
	tenantID := f.seedTenant(t)

	want := map[id.UUID]bool{}
	for i := 0; i < 5; i++ {
		want[f.seedMembership(t, tenantID, int64(i+1))] = false
	}

	for _, row := range f.rowsFor(t, consumerID, tenantID) {
		seen, expected := want[row.MembershipID]
		if !expected {
			t.Errorf("snapshot returned %s, which this test did not seed", row.MembershipID)
			continue
		}
		if seen {
			t.Errorf("snapshot returned %s twice", row.MembershipID)
		}
		want[row.MembershipID] = true
	}
	for membershipID, seen := range want {
		if !seen {
			t.Errorf("snapshot omitted %s", membershipID)
		}
	}
}

// TestEveryPageOfOneSnapshotCarriesOneMark. Each page is its own transaction and therefore its own
// database snapshot, so the caller carries the mark forward. Without that, page two would report a
// later mark and a consumer stitching the pages together would hold rows from two instants under
// one watermark — and would then discard events belonging to the gap between them.
func TestEveryPageOfOneSnapshotCarriesOneMark(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)
	tenantID := f.seedTenant(t)
	for i := 0; i < 3; i++ {
		f.seedMembership(t, tenantID, 1)
	}

	first, err := f.publisher.Snapshot(f.ctx, SnapshotRequest{ConsumerID: consumerID, PageSize: 1})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if first.Cursor == "" {
		t.Fatal("a one-row page over three rows returned no cursor")
	}

	second, err := f.publisher.Snapshot(f.ctx, SnapshotRequest{
		ConsumerID: consumerID, PageSize: 1, Cursor: first.Cursor, Mark: &first.HighWaterMark,
	})
	if err != nil {
		t.Fatalf("Snapshot page 2: %v", err)
	}
	if second.HighWaterMark != first.HighWaterMark {
		t.Errorf("page 2 mark = %d, page 1 mark = %d", second.HighWaterMark, first.HighWaterMark)
	}

	// Continuing without the mark is refused rather than silently re-derived.
	if _, err := f.publisher.Snapshot(f.ctx, SnapshotRequest{
		ConsumerID: consumerID, PageSize: 1, Cursor: first.Cursor,
	}); !errors.Is(err, ErrCursor) {
		t.Errorf("continuing without a mark returned %v, want ErrCursor", err)
	}
}

// TestAMutationInFlightDuringASnapshotIsNotSilentlyLost is the case the bootstrap contract's
// wording gets wrong, kept as an executable statement of why the contract was amended.
//
// `platform.outbox.sequence` is allocated at INSERT, not at COMMIT. So a transaction can hold
// sequence N while a later transaction takes N+1 and commits first. A snapshot taken in between
// sees the mark N+1 and does not see the row at N — its Membership is invisible too, because it is
// the same uncommitted transaction. A consumer told "discard everything at or below the mark" then
// discards the only event that would have delivered it, permanently.
//
// This test does not assert that the mark excludes N; making it exact would need either transaction
// id arithmetic or a lock serialising every append against every snapshot. It asserts the property
// the amended contract actually rests on: the row is absent from the snapshot, so the consumer must
// apply buffered events by version comparison rather than by discarding at the mark.
func TestAMutationInFlightDuringASnapshotIsNotSilentlyLost(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)
	tenantID := f.seedTenant(t)

	// A transaction that inserts a Membership and its outbox row and then waits, holding both
	// uncommitted. Driven from a goroutine because `db.Pool` exposes only `InTx` and `arch.json`
	// denies this repository the driver, so there is no way to hold a transaction open inline —
	// which is the correct trade: the one thing a test wants here is the one thing production code
	// must never do.
	hidden := mustID(t)
	inserted := make(chan int64, 1)
	release := make(chan struct{})
	rolledBack := make(chan error, 1)
	errAbandon := errors.New("abandon the in-flight transaction")

	go func() {
		rolledBack <- db.WithProviderScope(f.ctx, f.provider, "in-flight transaction under test",
			func(ctx context.Context, tx db.Tx) error {
				principalID := mustID(t)
				if _, err := tx.Exec(ctx, `INSERT INTO membership.membership
				    (membership_id, principal_id, tenant_id, subject_type, status, membership_version, valid_from, provenance)
				    VALUES ($1, $2, $3, 'human', 'active', 1, now(), 'in flight')`,
					hidden.String(), principalID.String(), tenantID.String()); err != nil {
					return err
				}
				var seq int64
				if err := tx.QueryRow(ctx, `INSERT INTO platform.outbox
				    (event_id, event_type, aggregate_id, payload, envelope, priority)
				    VALUES ($1, 'com.scnehaux.organization.membership.lifecycle.granted', $2, '{}'::jsonb, '{}'::jsonb, 100)
				    RETURNING sequence`, mustID(t).String(), hidden.String()).Scan(&seq); err != nil {
					return err
				}
				inserted <- seq
				<-release
				// Returned so the transaction rolls back: nothing this test wrote should survive it.
				return errAbandon
			})
	}()

	var heldSequence int64
	select {
	case heldSequence = <-inserted:
	case err := <-rolledBack:
		t.Fatalf("the in-flight transaction failed before holding its sequence: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the in-flight transaction never reported its sequence")
	}
	defer func() {
		close(release)
		if err := <-rolledBack; !errors.Is(err, errAbandon) {
			t.Errorf("the in-flight transaction ended with %v, want the abandon error", err)
		}
	}()

	// A second, committed append takes a higher sequence, so the visible maximum is above the one
	// the in-flight transaction is holding.
	f.exec(t, `INSERT INTO platform.outbox
	    (event_id, event_type, aggregate_id, payload, envelope, priority)
	    VALUES ($1, 'com.scnehaux.organization.membership.lifecycle.granted', $2, '{}'::jsonb, '{}'::jsonb, 100)`,
		mustID(t).String(), mustID(t).String())

	page, err := f.publisher.Snapshot(f.ctx, SnapshotRequest{ConsumerID: consumerID, PageSize: MaxPageSize})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	for _, row := range page.Rows {
		if row.MembershipID == hidden {
			t.Fatal("the snapshot returned a row from an uncommitted transaction")
		}
	}

	// The mark is at or above the sequence the invisible row is holding. That is the hole: a
	// consumer discarding at the mark would drop the event that carries this Membership.
	if page.HighWaterMark < heldSequence {
		t.Skipf("the visible mark %d is below the held sequence %d; this run did not reproduce the interleaving",
			page.HighWaterMark, heldSequence)
	}
	t.Logf("mark %d is at or above the held sequence %d while the row is absent from the snapshot: "+
		"discarding at the mark would lose it", page.HighWaterMark, heldSequence)
}

// TestReconciliationClassifiesEachDifferenceAndIsIdempotent.
func TestReconciliationClassifiesEachDifferenceAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)
	tenantID := f.seedTenant(t)

	agreed := f.seedMembership(t, tenantID, 7)
	diverged := f.seedMembership(t, tenantID, 9)
	absent := f.seedMembership(t, tenantID, 3)
	invented := mustID(t)

	report := Report{
		ConsumerID: consumerID,
		Mark:       100,
		Rows: []ReportedRow{
			{MembershipID: agreed, MembershipVersion: 7},
			{MembershipID: diverged, MembershipVersion: 8},
			{MembershipID: invented, MembershipVersion: 1},
		},
	}

	result, err := f.reconciler.Reconcile(f.ctx, report)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	byMembership := map[id.UUID]Classification{}
	for _, finding := range result.Findings {
		byMembership[finding.MembershipID] = finding.Classification
	}
	if got := byMembership[absent]; got != ClassMissing {
		t.Errorf("an unreported authoritative Membership = %q, want missing", got)
	}
	if got := byMembership[diverged]; got != ClassMismatch {
		t.Errorf("a version divergence = %q, want mismatch", got)
	}
	if got := byMembership[invented]; got != ClassExtra {
		t.Errorf("a projected context authority does not grant = %q, want extra", got)
	}
	if _, reported := byMembership[agreed]; reported {
		t.Error("an agreeing Membership produced a finding")
	}

	// `extra` leads, because whoever reads the first line of a sweep should read the privilege
	// escalation rather than whichever row a hash bucket happened to hold first.
	if len(result.Findings) == 0 || result.Findings[0].Classification != ClassExtra {
		t.Errorf("first finding = %+v, want the extra", result.Findings[0])
	}
	if got := len(result.SecurityFindings()); got != 1 {
		t.Errorf("security findings = %d, want 1", got)
	}

	// Idempotent across repeated runs against a consistent state, which map iteration order would
	// otherwise break: Go randomises it deliberately, so an unsorted result would differ per run
	// and any diff of two sweeps would be meaningless.
	again, err := f.reconciler.Reconcile(f.ctx, report)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if fmt.Sprint(again.Findings) != fmt.Sprint(result.Findings) {
		t.Error("two sweeps over one state returned different output")
	}
}

// TestAReportWithoutAPositionIsRefused. Compared against authority read now, every change made
// since the report would be classified as a divergence, and the sweep would manufacture findings
// out of ordinary progress.
func TestAReportWithoutAPositionIsRefused(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)

	if _, err := f.reconciler.Reconcile(f.ctx, Report{ConsumerID: consumerID}); !errors.Is(err, ErrReportMarkRequired) {
		t.Fatalf("error = %v, want ErrReportMarkRequired", err)
	}
}

// TestASweepWithNoFindingsPublishesNothing, and one with findings publishes exactly one event.
// A finding-per-event stream would let a consumer apply half a sweep and report itself reconciled.
func TestASweepPublishesOneEventOnlyWhenItFoundSomething(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)

	clean := Result{ConsumerID: consumerID, Mark: 5, RunAt: f.fixed}
	if err := f.reconciler.PublishReconciled(f.ctx, clean); err != nil {
		t.Fatalf("PublishReconciled on a clean sweep: %v", err)
	}
	if got := f.reconciledEvents(t, consumerID); got != 0 {
		t.Errorf("a clean sweep published %d events", got)
	}

	dirty := Result{
		ConsumerID: consumerID, Mark: 6, RunAt: f.fixed,
		Findings: []Finding{{
			Classification: ClassExtra, MembershipID: mustID(t), ProjectedVersion: 1,
		}},
	}
	if err := f.reconciler.PublishReconciled(f.ctx, dirty); err != nil {
		t.Fatalf("PublishReconciled: %v", err)
	}
	if got := f.reconciledEvents(t, consumerID); got != 1 {
		t.Errorf("a sweep with one finding published %d events, want 1", got)
	}
}

func (f *fixture) reconciledEvents(t *testing.T, consumerID string) int {
	t.Helper()
	var count int
	if err := db.WithProviderScope(f.ctx, f.provider, "projection suite read",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM platform.outbox
			    WHERE event_type = $1 AND envelope->'data'->>'consumer_id' = $2`,
				ReconciledEventType, consumerID).Scan(&count)
		}); err != nil {
		t.Fatalf("count reconciled events: %v", err)
	}
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM platform.outbox
		    WHERE event_type = $1 AND envelope->'data'->>'consumer_id' = $2`,
			ReconciledEventType, consumerID)
	})
	return count
}

// TestRegistrationRefusesAnUndeclaredContract.
func TestRegistrationRefusesAnUndeclaredContract(t *testing.T) {
	f := newFixture(t)

	cases := map[string]Registration{
		"no identifier":     {ProjectionVersion: "v1", MaxAcceptedAge: time.Second, StaleBehavior: StaleFailClosed},
		"no version":        {ConsumerID: "c", MaxAcceptedAge: time.Second, StaleBehavior: StaleFailClosed},
		"no freshness":      {ConsumerID: "c", ProjectionVersion: "v1", StaleBehavior: StaleFailClosed},
		"unknown behaviour": {ConsumerID: "c", ProjectionVersion: "v1", MaxAcceptedAge: time.Second, StaleBehavior: "best_effort"},
	}
	for name, reg := range cases {
		if _, err := f.registry.Register(f.ctx, reg); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestAConsumerThatNeverReportedIsStaleByDefinition. Nothing is known about its copy, and treating
// an absent report as fresh would make a disconnected consumer indistinguishable from a current one.
func TestAConsumerThatNeverReportedIsStaleByDefinition(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)

	consumer, err := f.registry.Get(f.ctx, consumerID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, stale := consumer.Age(f.fixed); !stale {
		t.Error("a consumer that never reported is not stale")
	}

	if _, err := f.publisher.Bootstrap(f.ctx, consumerID, 1); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, err := f.registry.RecordProgress(f.ctx, Progress{ConsumerID: consumerID, AppliedMark: 1}); err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}
	fresh, err := f.registry.Get(f.ctx, consumerID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, stale := fresh.Age(f.fixed); stale {
		t.Error("a consumer that reported at the current instant is stale")
	}
	if age, stale := fresh.Age(f.fixed.Add(time.Hour)); !stale {
		t.Errorf("age %s beyond a 30s budget is not stale", age)
	}
}

// TestPageSizeIsAdmissionControl. One consumer asking for the whole estate in a single page is an
// unbounded allocation on a shared publisher, so the cost is capped before it is served.
func TestPageSizeIsAdmissionControl(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)

	for _, size := range []int{MaxPageSize + 1, -1} {
		if _, err := f.publisher.Snapshot(f.ctx, SnapshotRequest{
			ConsumerID: consumerID, PageSize: size,
		}); !errors.Is(err, ErrPageSize) {
			t.Errorf("page size %d returned %v, want ErrPageSize", size, err)
		}
	}

	// Zero means "use the default" rather than "no rows", because a caller that omitted the field
	// wants a page and an empty page would look like an empty estate.
	if _, err := f.publisher.Snapshot(f.ctx, SnapshotRequest{ConsumerID: consumerID}); err != nil {
		t.Errorf("an omitted page size was refused: %v", err)
	}
}

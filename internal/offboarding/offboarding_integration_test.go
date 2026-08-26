package offboarding

// The Week 4 offboarding exit criterion, against a real engine:
//
//	offboarding is resumable and infers completion from no single response.
//
// In-package, because the clock, identifier, and advance seams are unexported. The advance seam
// exists so resumability can be falsified rather than asserted: a process that claims to resume is
// only interesting if it survives failing at the moment that would corrupt it.

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
	"github.com/anshacerbia2/organization-control/internal/membership"
	"github.com/anshacerbia2/organization-control/internal/tenant"
)

const fixedNowText = "2026-08-24T11:12:13Z"

type recorder struct{ calls int }

func (r *recorder) RecordProviderAccess(context.Context, db.ProviderAccess) error {
	r.calls++
	return nil
}

type fixture struct {
	service      *Service
	provider     *db.ProviderPool
	providerCtx  context.Context
	tenantCtxFor func(id.UUID) context.Context
	fixed        time.Time
}

func dsnFor(t *testing.T, user, password string) string {
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
	return fmt.Sprintf("postgres://%s:%s@%s", user, password, rest)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	// Two pools, because the service holds two. The freeze runs as the tenant-scoped role under
	// its policy; beginning and retiring run as the provider role, which is the only one that can
	// read the sponsoring Organization at all.
	providerPool, err := fdb.Open(ctx, fdb.Config{
		Name:     "offboarding-provider",
		DSN:      dsnFor(t, "organization_provider_app", os.Getenv("TEST_PROVIDER_PASSWORD")),
		MaxConns: 4,
	})
	if err != nil {
		t.Fatalf("open provider pool: %v", err)
	}
	t.Cleanup(providerPool.Close)

	runtimePool, err := fdb.Open(ctx, fdb.Config{
		Name:     "offboarding-runtime",
		DSN:      dsnFor(t, "organization_app", os.Getenv("TEST_RUNTIME_PASSWORD")),
		MaxConns: 4,
	})
	if err != nil {
		t.Fatalf("open runtime pool: %v", err)
	}
	t.Cleanup(runtimePool.Close)

	provider, err := db.NewProviderPool(providerPool, &recorder{})
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}
	tenantPool, err := db.NewTenantPool(runtimePool)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}

	fixed, err := time.Parse(time.RFC3339, fixedNowText)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tenants, err := tenant.New(provider)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	memberships, err := membership.New(tenantPool)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	service, err := New(provider, tenantPool, tenants, memberships)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	service.now = func() time.Time { return fixed }

	actor, correlation := mustID(t), mustID(t)
	providerScope, err := db.ProviderScope(actor, correlation)
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	return &fixture{
		service:     service,
		provider:    provider,
		providerCtx: db.WithScope(ctx, providerScope),
		tenantCtxFor: func(tenantID id.UUID) context.Context {
			scope, err := db.TenantScope(tenantID, actor, correlation)
			if err != nil {
				t.Fatalf("TenantScope: %v", err)
			}
			return db.WithScope(ctx, scope)
		},
		fixed: fixed,
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
	if err := db.WithProviderScope(f.providerCtx, f.provider, "offboarding suite fixture",
		func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, statement, args...)
			return err
		}); err != nil {
		t.Fatalf("fixture statement: %v", err)
	}
}

// seed creates an active Tenant with memberCount active Memberships.
func (f *fixture) seed(t *testing.T, memberCount int) (id.UUID, []id.UUID) {
	t.Helper()
	organizationID, tenantID := mustID(t), mustID(t)
	f.exec(t, `INSERT INTO organization.organization (organization_id, display_name, classification, status)
	    VALUES ($1, 'offboarding suite', 'customer', 'active')`, organizationID.String())
	f.exec(t, `INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
	    VALUES ($1, $2, 'offboarding suite', 'active', 'pooled')`, tenantID.String(), organizationID.String())

	var members []id.UUID
	for i := 0; i < memberCount; i++ {
		membershipID, principalID := mustID(t), mustID(t)
		f.exec(t, `INSERT INTO membership.membership
		    (membership_id, principal_id, tenant_id, subject_type, status, membership_version, valid_from, provenance)
		    VALUES ($1, $2, $3, 'human', 'active', 1, now(), 'offboarding suite')`,
			membershipID.String(), principalID.String(), tenantID.String())
		members = append(members, membershipID)
	}

	t.Cleanup(func() {
		f.exec(t, `DELETE FROM platform.outbox WHERE aggregate_id IN (
		    SELECT membership_id FROM membership.membership WHERE tenant_id = $1)`, tenantID.String())
		f.exec(t, `DELETE FROM platform.outbox WHERE aggregate_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM operation.offboarding_obligation WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM platform.outbox WHERE aggregate_id IN (
		    SELECT offboarding_id FROM operation.offboarding WHERE tenant_id = $1)`, tenantID.String())
		f.exec(t, `DELETE FROM operation.offboarding WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM membership.membership WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM organization.organization WHERE organization_id = $1`, organizationID.String())
	})
	return tenantID, members
}

func (f *fixture) tenantRow(t *testing.T, tenantID id.UUID) (string, int64, int64) {
	t.Helper()
	var status string
	var version, securityVersion int64
	if err := db.WithProviderScope(f.providerCtx, f.provider, "offboarding suite read",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT status, version, tenant_security_version
			    FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String()).
				Scan(&status, &version, &securityVersion)
		}); err != nil {
		t.Fatalf("read tenant: %v", err)
	}
	return status, version, securityVersion
}

func (f *fixture) activeMembers(t *testing.T, tenantID id.UUID) int {
	t.Helper()
	var count int
	if err := db.WithProviderScope(f.providerCtx, f.provider, "offboarding suite read",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM membership.membership
			    WHERE tenant_id = $1 AND status = 'active'`, tenantID.String()).Scan(&count)
		}); err != nil {
		t.Fatalf("count active: %v", err)
	}
	return count
}

func (f *fixture) eventCount(t *testing.T, eventType string, aggregate id.UUID) int {
	t.Helper()
	var count int
	if err := db.WithProviderScope(f.providerCtx, f.provider, "offboarding suite read",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM platform.outbox
			    WHERE event_type = $1 AND aggregate_id = $2`, eventType, aggregate.String()).Scan(&count)
		}); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return count
}

// begin drives Begin with the Tenant's current version.
func (f *fixture) begin(t *testing.T, tenantID id.UUID, hold bool) Offboarding {
	t.Helper()
	_, version, _ := f.tenantRow(t, tenantID)
	record, err := f.service.Begin(f.providerCtx, BeginRequest{
		TenantID: tenantID, ExpectedVersion: version,
		Reason: "asserted by the offboarding suite", LegalHold: hold,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return record
}

// freezeAll drains the freeze in batches, the way a worker does.
func (f *fixture) freezeAll(t *testing.T, tenantID id.UUID, size int) int {
	t.Helper()
	ctx := f.tenantCtxFor(tenantID)
	total := 0
	for round := 0; round < 100; round++ {
		frozen, err := f.service.FreezeBatch(ctx, tenantID, size)
		if err != nil {
			t.Fatalf("FreezeBatch round %d: %v", round, err)
		}
		if frozen == 0 {
			return total
		}
		total += frozen
	}
	t.Fatal("the freeze did not drain within 100 batches")
	return total
}

// TestAccessStopsBeforeAnythingIsReleased is the ordering that makes offboarding safe to start.
//
// Beginning an offboarding stops Tenant-wide access immediately and destroys nothing. An operator
// who began one by mistake has taken a reversible action; if release happened in the same step,
// every mistaken start would be irreversible.
func TestAccessStopsBeforeAnythingIsReleased(t *testing.T) {
	f := newFixture(t)
	tenantID, _ := f.seed(t, 3)
	_, _, securityBefore := f.tenantRow(t, tenantID)

	record := f.begin(t, tenantID, false)

	if record.Stage != StageFreeze {
		t.Errorf("stage = %s, want freeze", record.Stage)
	}
	status, _, securityAfter := f.tenantRow(t, tenantID)
	if status != string(tenant.StateOffboarding) {
		t.Errorf("tenant status = %s, want offboarding", status)
	}
	// Tenant-wide enforcement does not wait for the Membership batches: the priority security
	// event and the version bump commit when offboarding begins.
	if securityAfter != securityBefore+1 {
		t.Errorf("security version = %d, want %d", securityAfter, securityBefore+1)
	}
	if got := f.eventCount(t, "com.scnehaux.organization.tenant.security.suspended", tenantID); got != 1 {
		t.Errorf("tenant.security.suspended count = %d, want 1", got)
	}
	if got := f.eventCount(t, "com.scnehaux.organization.tenant.offboarding.started", record.OffboardingID); got != 1 {
		t.Errorf("offboarding.started count = %d, want 1", got)
	}
	// Nothing has been released and the Memberships are still active — the freeze has not run.
	if got := f.activeMembers(t, tenantID); got != 3 {
		t.Errorf("active Memberships = %d, want 3 before the freeze", got)
	}
	if record.RetiredAt != nil {
		t.Error("beginning an offboarding stamped a retirement")
	}
}

// TestTheFreezeIsResumableAndSuspendsEveryMembership.
//
// Batched, and the predicate is the resume token: only `active` rows are selected, so a batch that
// already committed is simply not seen again. Nothing has to remember a cursor across a restart.
func TestTheFreezeIsResumableAndSuspendsEveryMembership(t *testing.T) {
	f := newFixture(t)
	tenantID, members := f.seed(t, 5)
	f.begin(t, tenantID, false)

	// One batch of two, simulating a worker that stopped after the first batch.
	first, err := f.service.FreezeBatch(f.tenantCtxFor(tenantID), tenantID, 2)
	if err != nil {
		t.Fatalf("FreezeBatch: %v", err)
	}
	if first != 2 {
		t.Fatalf("first batch froze %d, want 2", first)
	}
	if got := f.activeMembers(t, tenantID); got != 3 {
		t.Errorf("active after one batch = %d, want 3", got)
	}

	// A fresh drain continues from what is left rather than repeating.
	rest := f.freezeAll(t, tenantID, 2)
	if rest != 3 {
		t.Errorf("the resumed freeze changed %d, want the remaining 3", rest)
	}
	if got := f.activeMembers(t, tenantID); got != 0 {
		t.Errorf("active after the freeze = %d, want 0", got)
	}

	// One priority event per Membership, and exactly one — a Membership suspended twice would mean
	// the batch predicate was not the resume token.
	for _, membershipID := range members {
		if got := f.eventCount(t, "com.scnehaux.organization.membership.security.suspended", membershipID); got != 1 {
			t.Errorf("membership %s produced %d suspension events, want 1", membershipID, got)
		}
	}
}

// TestCompleteFreezeCountsRatherThanTrustingTheCaller.
//
// A caller that stopped batching early — a crash, a cancelled context, a miscounted loop — would
// otherwise advance a Tenant into obligations with Memberships still active, and the projection
// would keep serving them.
func TestCompleteFreezeCountsRatherThanTrustingTheCaller(t *testing.T) {
	f := newFixture(t)
	tenantID, _ := f.seed(t, 3)
	record := f.begin(t, tenantID, false)

	if _, err := f.service.FreezeBatch(f.tenantCtxFor(tenantID), tenantID, 1); err != nil {
		t.Fatalf("FreezeBatch: %v", err)
	}
	if _, err := f.service.CompleteFreeze(f.providerCtx, record.OffboardingID); err == nil {
		t.Fatal("the freeze completed with active Memberships remaining")
	}

	f.freezeAll(t, tenantID, 10)
	advanced, err := f.service.CompleteFreeze(f.providerCtx, record.OffboardingID)
	if err != nil {
		t.Fatalf("CompleteFreeze: %v", err)
	}
	if advanced.Stage != StageObligations {
		t.Errorf("stage = %s, want obligations", advanced.Stage)
	}
	if advanced.FrozenAt == nil || !advanced.FrozenAt.Equal(f.fixed) {
		t.Errorf("frozen_at = %v, want %s", advanced.FrozenAt, f.fixed)
	}
	if got := f.eventCount(t, "com.scnehaux.organization.tenant.offboarding.frozen", record.OffboardingID); got != 1 {
		t.Errorf("offboarding.frozen count = %d, want 1", got)
	}
}

// TestARestartMidAdvanceResumesFromTheRecordedStage is the resumability half of the exit criterion.
//
// The stage column is the process's memory. A failure injected after a stage's work and before the
// column moves must leave the recorded stage unchanged, so a restart repeats a stage rather than
// skipping one — and the recorded stage, not the caller's expectation, is what the next attempt
// reads.
func TestARestartMidAdvanceResumesFromTheRecordedStage(t *testing.T) {
	f := newFixture(t)
	tenantID, _ := f.seed(t, 2)
	record := f.begin(t, tenantID, false)
	f.freezeAll(t, tenantID, 10)

	injected := errors.New("crash between the work and the record of the work")
	f.service.beforeAdvance = func(context.Context) error { return injected }

	if _, err := f.service.CompleteFreeze(f.providerCtx, record.OffboardingID); !errors.Is(err, injected) {
		t.Fatalf("error = %v, want the injected failure", err)
	}

	// The recorded stage did not move, and neither did the event.
	resumed, err := f.service.Get(f.providerCtx, record.OffboardingID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resumed.Stage != StageFreeze {
		t.Errorf("stage = %s after a rolled-back advance, want freeze", resumed.Stage)
	}
	if resumed.FrozenAt != nil {
		t.Errorf("frozen_at = %v after a rolled-back advance, want NULL", resumed.FrozenAt)
	}
	if got := f.eventCount(t, "com.scnehaux.organization.tenant.offboarding.frozen", record.OffboardingID); got != 0 {
		t.Errorf("a rolled-back advance published %d frozen events", got)
	}

	// The restart continues from the recorded stage and succeeds.
	f.service.beforeAdvance = nil
	advanced, err := f.service.CompleteFreeze(f.providerCtx, record.OffboardingID)
	if err != nil {
		t.Fatalf("the resumed advance failed: %v", err)
	}
	if advanced.Stage != StageObligations {
		t.Errorf("stage = %s, want obligations", advanced.Stage)
	}

	// And advancing twice from a stage that has moved on is refused, which is what stops a process
	// that crashed after advancing and before reporting from advancing again.
	if _, err := f.service.CompleteFreeze(f.providerCtx, record.OffboardingID); !errors.Is(err, ErrStageRefused) {
		t.Errorf("a second advance returned %v, want ErrStageRefused", err)
	}
}

// TestReleaseIsRefusedWhileAnyObligationIsOutstanding is the "no single response" half.
//
// Completion comes from the obligation registry, not from a deprovisioning call that returned
// success. A `failed` obligation holds exactly as `open` does: a failure is not a resolution, and
// folding it into one would release data whose obligations are known to be unmet.
func TestReleaseIsRefusedWhileAnyObligationIsOutstanding(t *testing.T) {
	f := newFixture(t)
	tenantID, _ := f.seed(t, 1)
	record := f.begin(t, tenantID, false)
	f.freezeAll(t, tenantID, 10)
	if _, err := f.service.CompleteFreeze(f.providerCtx, record.OffboardingID); err != nil {
		t.Fatalf("CompleteFreeze: %v", err)
	}

	exportObligation, err := f.service.Raise(f.providerCtx, RaiseRequest{
		OffboardingID: record.OffboardingID, Domain: "product", Type: "data-export",
	})
	if err != nil {
		t.Fatalf("Raise: %v", err)
	}
	retention, err := f.service.Raise(f.providerCtx, RaiseRequest{
		OffboardingID: record.OffboardingID, Domain: "audit", Type: "retention-hold",
	})
	if err != nil {
		t.Fatalf("Raise: %v", err)
	}

	// Open holds, and the refusal names them rather than counting them.
	_, err = f.service.Release(f.providerCtx, record.OffboardingID)
	if !errors.Is(err, ErrObligationsOutstanding) {
		t.Fatalf("error = %v, want ErrObligationsOutstanding", err)
	}
	for _, want := range []string{"product/data-export", "audit/retention-hold"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}

	// A failure is recorded and still holds.
	if _, err := f.service.Resolve(f.providerCtx, Resolution{
		ObligationID: exportObligation.ObligationID, Domain: "product",
		State: ObligationFailed, Detail: "the export target rejected the archive",
	}); err != nil {
		t.Fatalf("Resolve as failed: %v", err)
	}
	if _, err := f.service.Release(f.providerCtx, record.OffboardingID); !errors.Is(err, ErrObligationsOutstanding) {
		t.Fatalf("a failed obligation did not hold release: %v", err)
	}

	// Completed and waived both resolve, and the record keeps them distinguishable.
	if _, err := f.service.Resolve(f.providerCtx, Resolution{
		ObligationID: exportObligation.ObligationID, Domain: "product",
		State: ObligationCompleted,
	}); err != nil {
		t.Fatalf("Resolve as completed: %v", err)
	}
	waived, err := f.service.Resolve(f.providerCtx, Resolution{
		ObligationID: retention.ObligationID, Domain: "audit",
		State: ObligationWaived, Detail: "counsel released the hold on 2026-08-24",
	})
	if err != nil {
		t.Fatalf("Resolve as waived: %v", err)
	}
	if waived.State != ObligationWaived {
		t.Errorf("state = %s, want waived — recording it as completed would make an audit read as satisfied", waived.State)
	}
	if waived.Detail == "" {
		t.Error("a waiver was recorded without the reason a person gave")
	}

	released, err := f.service.Release(f.providerCtx, record.OffboardingID)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if released.Stage != StageRelease {
		t.Errorf("stage = %s, want release", released.Stage)
	}
}

// TestAnObligationCannotBeResolvedByAnotherDomain, and cannot be resolved twice.
func TestAnObligationCannotBeResolvedByAnotherDomain(t *testing.T) {
	f := newFixture(t)
	tenantID, _ := f.seed(t, 0)
	record := f.begin(t, tenantID, false)
	if _, err := f.service.CompleteFreeze(f.providerCtx, record.OffboardingID); err != nil {
		t.Fatalf("CompleteFreeze: %v", err)
	}

	obligation, err := f.service.Raise(f.providerCtx, RaiseRequest{
		OffboardingID: record.OffboardingID, Domain: "billing", Type: "final-invoice",
	})
	if err != nil {
		t.Fatalf("Raise: %v", err)
	}

	if _, err := f.service.Resolve(f.providerCtx, Resolution{
		ObligationID: obligation.ObligationID, Domain: "hcm", State: ObligationCompleted,
	}); !errors.Is(err, ErrWrongDomain) {
		t.Fatalf("error = %v, want ErrWrongDomain", err)
	}

	if _, err := f.service.Resolve(f.providerCtx, Resolution{
		ObligationID: obligation.ObligationID, Domain: "billing", State: ObligationCompleted,
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := f.service.Resolve(f.providerCtx, Resolution{
		ObligationID: obligation.ObligationID, Domain: "billing",
		State: ObligationWaived, Detail: "second attempt",
	}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("a second resolution returned %v, want ErrAlreadyResolved", err)
	}

	// A waiver and a failure must carry the reason a person gave; a completion needs none.
	other, err := f.service.Raise(f.providerCtx, RaiseRequest{
		OffboardingID: record.OffboardingID, Domain: "audit", Type: "log-archive",
	})
	if err != nil {
		t.Fatalf("Raise: %v", err)
	}
	if _, err := f.service.Resolve(f.providerCtx, Resolution{
		ObligationID: other.ObligationID, Domain: "audit", State: ObligationWaived,
	}); err == nil {
		t.Error("a waiver with no detail was accepted")
	}
}

// TestLegalHoldBlocksDestructionAndNothingElse is SAD-004 §6.5.
//
// A hold that also blocked the freeze would keep access open on a Tenant that is leaving, which is
// the opposite of what a hold is for.
func TestLegalHoldBlocksDestructionAndNothingElse(t *testing.T) {
	f := newFixture(t)
	tenantID, _ := f.seed(t, 2)
	record := f.begin(t, tenantID, true)

	// Freeze proceeds under a hold.
	if got := f.freezeAll(t, tenantID, 10); got != 2 {
		t.Errorf("the freeze changed %d Memberships under a legal hold, want 2", got)
	}
	advanced, err := f.service.CompleteFreeze(f.providerCtx, record.OffboardingID)
	if err != nil {
		t.Fatalf("CompleteFreeze under a hold: %v", err)
	}
	if !advanced.LegalHold {
		t.Error("the hold was lost while advancing")
	}

	// Obligations proceed under a hold.
	obligation, err := f.service.Raise(f.providerCtx, RaiseRequest{
		OffboardingID: record.OffboardingID, Domain: "audit", Type: "retention-hold",
	})
	if err != nil {
		t.Fatalf("Raise under a hold: %v", err)
	}
	if _, err := f.service.Resolve(f.providerCtx, Resolution{
		ObligationID: obligation.ObligationID, Domain: "audit", State: ObligationCompleted,
	}); err != nil {
		t.Fatalf("Resolve under a hold: %v", err)
	}

	// Release is refused, with every obligation settled.
	if _, err := f.service.Release(f.providerCtx, record.OffboardingID); !errors.Is(err, ErrLegalHold) {
		t.Fatalf("error = %v, want ErrLegalHold", err)
	}

	// Lifting the hold releases it.
	if _, err := f.service.SetLegalHold(f.providerCtx, record.OffboardingID, false,
		"counsel released the hold"); err != nil {
		t.Fatalf("SetLegalHold: %v", err)
	}
	if _, err := f.service.Release(f.providerCtx, record.OffboardingID); err != nil {
		t.Fatalf("Release after the hold was lifted: %v", err)
	}
}

// TestRetirementRechecksTheGatesAtTheMomentOfTheAct.
//
// Retirement is the irreversible half, and both a hold and an obligation can arrive between release
// and retirement. Checking the state that permitted the previous stage would retire a Tenant whose
// position has changed since.
func TestRetirementRechecksTheGatesAtTheMomentOfTheAct(t *testing.T) {
	f := newFixture(t)
	tenantID, _ := f.seed(t, 1)
	record := f.begin(t, tenantID, false)
	f.freezeAll(t, tenantID, 10)
	if _, err := f.service.CompleteFreeze(f.providerCtx, record.OffboardingID); err != nil {
		t.Fatalf("CompleteFreeze: %v", err)
	}
	if _, err := f.service.Release(f.providerCtx, record.OffboardingID); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// A hold placed after release blocks retirement.
	if _, err := f.service.SetLegalHold(f.providerCtx, record.OffboardingID, true,
		"litigation hold arrived after release"); err != nil {
		t.Fatalf("SetLegalHold: %v", err)
	}
	_, version, _ := f.tenantRow(t, tenantID)
	if _, err := f.service.Retire(f.providerCtx, record.OffboardingID, version); !errors.Is(err, ErrLegalHold) {
		t.Fatalf("error = %v, want ErrLegalHold", err)
	}

	if _, err := f.service.SetLegalHold(f.providerCtx, record.OffboardingID, false, "hold lifted"); err != nil {
		t.Fatalf("SetLegalHold: %v", err)
	}

	_, version, securityBefore := f.tenantRow(t, tenantID)
	retired, err := f.service.Retire(f.providerCtx, record.OffboardingID, version)
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if retired.Stage != StageRetired {
		t.Errorf("stage = %s, want retired", retired.Stage)
	}
	if retired.RetiredAt == nil {
		t.Error("retirement was not stamped")
	}

	status, _, securityAfter := f.tenantRow(t, tenantID)
	if status != string(tenant.StateRetired) {
		t.Errorf("tenant status = %s, want retired", status)
	}
	if securityAfter != securityBefore+1 {
		t.Errorf("security version = %d, want %d", securityAfter, securityBefore+1)
	}
	// The Tenant transition publishes the retirement; the process does not publish a second event
	// for the same fact.
	if got := f.eventCount(t, "com.scnehaux.organization.tenant.lifecycle.retired", tenantID); got != 1 {
		t.Errorf("tenant.lifecycle.retired count = %d, want 1", got)
	}
	if got := f.eventCount(t, "com.scnehaux.organization.tenant.offboarding.released", record.OffboardingID); got != 1 {
		t.Errorf("offboarding.released count = %d, want 1", got)
	}
}

// TestObligationsCanOnlyBeRaisedInTheObligationsStage. Raising during the freeze would let an
// obligation exist against a Tenant whose Memberships are still active; raising after release would
// ask a domain for something whose subject is already deprovisioned.
func TestObligationsCanOnlyBeRaisedInTheObligationsStage(t *testing.T) {
	f := newFixture(t)
	tenantID, _ := f.seed(t, 0)
	record := f.begin(t, tenantID, false)

	if _, err := f.service.Raise(f.providerCtx, RaiseRequest{
		OffboardingID: record.OffboardingID, Domain: "product", Type: "too-early",
	}); !errors.Is(err, ErrStageRefused) {
		t.Fatalf("raising during the freeze returned %v, want ErrStageRefused", err)
	}

	if _, err := f.service.CompleteFreeze(f.providerCtx, record.OffboardingID); err != nil {
		t.Fatalf("CompleteFreeze: %v", err)
	}
	if _, err := f.service.Release(f.providerCtx, record.OffboardingID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := f.service.Raise(f.providerCtx, RaiseRequest{
		OffboardingID: record.OffboardingID, Domain: "product", Type: "too-late",
	}); !errors.Is(err, ErrStageRefused) {
		t.Fatalf("raising after release returned %v, want ErrStageRefused", err)
	}
}

// TestBeginningTwiceIsRefusedByTheTenantStateMachine, so no second record can exist for a Tenant
// that is already leaving.
func TestBeginningTwiceIsRefusedByTheTenantStateMachine(t *testing.T) {
	f := newFixture(t)
	tenantID, _ := f.seed(t, 0)
	f.begin(t, tenantID, false)

	_, version, _ := f.tenantRow(t, tenantID)
	if _, err := f.service.Begin(f.providerCtx, BeginRequest{
		TenantID: tenantID, ExpectedVersion: version, Reason: "second attempt",
	}); !errors.Is(err, tenant.ErrTransitionRefused) {
		t.Fatalf("error = %v, want ErrTransitionRefused", err)
	}

	var records int
	if err := db.WithProviderScope(f.providerCtx, f.provider, "offboarding suite read",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM operation.offboarding WHERE tenant_id = $1`,
				tenantID.String()).Scan(&records)
		}); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if records != 1 {
		t.Errorf("offboarding records = %d, want 1", records)
	}
}

// TestEveryPublishedEventNameIsValid walks the table rather than a list maintained beside it, so an
// event added later cannot carry a name the platform grammar refuses — which would surface when it
// was first published rather than here.
func TestEveryPublishedEventNameIsValid(t *testing.T) {
	for _, name := range EventNames() {
		eventType, err := EventType(name)
		if err != nil {
			t.Errorf("EventType(%s): %v", name, err)
			continue
		}
		if !strings.Contains(string(eventType), ".offboarding.") {
			t.Errorf("%s publishes %q, which is not classified as an offboarding event", name, eventType)
		}
	}
	if _, err := EventType("invented"); err == nil {
		t.Error("an event this process does not publish returned a type")
	}
}

// TestTheStageMachineIsLinearAndForwardOnly. An offboarding that released data cannot return to
// obligations, because the obligation it would return to is one whose subject no longer exists.
func TestTheStageMachineIsLinearAndForwardOnly(t *testing.T) {
	want := map[Stage]Stage{
		StageFreeze:      StageObligations,
		StageObligations: StageRelease,
		StageRelease:     StageRetired,
	}
	for _, stage := range []Stage{StageFreeze, StageObligations, StageRelease, StageRetired} {
		if !stage.Valid() {
			t.Errorf("%s is a stage and Valid() rejects it", stage)
		}
		next, ok := Next(stage)
		expected, shouldAdvance := want[stage]
		if ok != shouldAdvance {
			t.Errorf("Next(%s) advanced = %v, want %v", stage, ok, shouldAdvance)
			continue
		}
		if ok && next != expected {
			t.Errorf("Next(%s) = %s, want %s", stage, next, expected)
		}
	}
}

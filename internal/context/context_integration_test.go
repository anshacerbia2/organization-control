package context

// The Week 4 fresh-check exit criterion, against a real engine:
//
//	`:verify` call rate is measured per consumer.
//
// In-package for the clock seam. The rate assertions read the registry directly, because a signal
// that is only visible through the method that writes it is a signal nobody can alert on.

import (
	stdcontext "context"
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

const fixedNowText = "2026-08-27T09:10:11Z"

type recorder struct{ calls int }

func (r *recorder) RecordProviderAccess(stdcontext.Context, db.ProviderAccess) error {
	r.calls++
	return nil
}

type fixture struct {
	service  *Service
	provider *db.ProviderPool
	recorder *recorder
	ctx      stdcontext.Context
	fixed    time.Time
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

	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := fdb.Open(ctx, fdb.Config{Name: "context-test", DSN: dsn, MaxConns: 4})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	rec := &recorder{}
	provider, err := db.NewProviderPool(pool, rec)
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}
	service, err := New(provider)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fixed, err := time.Parse(time.RFC3339, fixedNowText)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	service.now = func() time.Time { return fixed }

	scope, err := db.ProviderScope(mustID(t), mustID(t))
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	return &fixture{
		service: service, provider: provider, recorder: rec,
		ctx: db.WithScope(ctx, scope), fixed: fixed,
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
	if err := db.WithProviderScope(f.ctx, f.provider, "context suite fixture",
		func(ctx stdcontext.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, statement, args...)
			return err
		}); err != nil {
		t.Fatalf("fixture statement: %v", err)
	}
}

func (f *fixture) register(t *testing.T) string {
	t.Helper()
	consumerID := "ctx-" + mustID(t).String()
	f.exec(t, `INSERT INTO projection.consumer
	    (consumer_id, projection_version, max_accepted_age, stale_behavior)
	    VALUES ($1, 'v1', interval '30 seconds', 'fail_closed')`, consumerID)
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM projection.consumer WHERE consumer_id = $1`, consumerID)
	})
	return consumerID
}

// seed creates an Organization, a Tenant in the given status, and optionally one Membership.
func (f *fixture) seed(t *testing.T, tenantStatus, membershipStatus string) (id.UUID, id.UUID) {
	t.Helper()
	organizationID, tenantID, principalID := mustID(t), mustID(t), mustID(t)
	f.exec(t, `INSERT INTO organization.organization (organization_id, display_name, classification, status)
	    VALUES ($1, 'context suite', 'customer', 'active')`, organizationID.String())
	f.exec(t, `INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
	    VALUES ($1, $2, 'context suite', $3, 'pooled')`,
		tenantID.String(), organizationID.String(), tenantStatus)
	if membershipStatus != "" {
		f.exec(t, `INSERT INTO membership.membership
		    (membership_id, principal_id, tenant_id, subject_type, status, membership_version, valid_from, provenance)
		    VALUES ($1, $2, $3, 'human', $4, 7, now(), 'context suite')`,
			mustID(t).String(), principalID.String(), tenantID.String(), membershipStatus)
	}
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM membership.membership WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM organization.organization WHERE organization_id = $1`, organizationID.String())
	})
	return tenantID, principalID
}

func (f *fixture) counters(t *testing.T, consumerID string) (int64, *int64, *float64) {
	t.Helper()
	var calls int64
	var requests *int64
	var ratio *float64
	if err := db.WithProviderScope(f.ctx, f.provider, "context suite read",
		func(ctx stdcontext.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT verify_calls_since_report, last_reported_requests, last_verify_ratio
			    FROM projection.consumer WHERE consumer_id = $1`, consumerID).
				Scan(&calls, &requests, &ratio)
		}); err != nil {
		t.Fatalf("read counters: %v", err)
	}
	return calls, requests, ratio
}

// TestEveryFreshCheckIsMeteredAgainstItsConsumer is the exit criterion's numerator.
//
// An unmetered fresh-check path is the one that gets used as an ordinary read, because nothing
// reports that it is being used that way.
func TestEveryFreshCheckIsMeteredAgainstItsConsumer(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)
	tenantID, principalID := f.seed(t, "active", "active")

	for i := 0; i < 3; i++ {
		decision, err := f.service.Verify(f.ctx, VerifyRequest{
			ConsumerID: consumerID, TenantID: tenantID, PrincipalID: principalID,
		})
		if err != nil {
			t.Fatalf("Verify %d: %v", i, err)
		}
		if !decision.Granted {
			t.Fatalf("Verify %d refused an active Membership in an active Tenant: %s", i, decision.Refusal)
		}
	}

	calls, _, _ := f.counters(t, consumerID)
	if calls != 3 {
		t.Errorf("verify calls = %d, want 3", calls)
	}

	// A refused check is metered too. Otherwise a consumer probing contexts it does not hold would
	// be the one consumer the signal could not see.
	other, otherPrincipal := f.seed(t, "active", "")
	if _, err := f.service.Verify(f.ctx, VerifyRequest{
		ConsumerID: consumerID, TenantID: other, PrincipalID: otherPrincipal,
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if calls, _, _ := f.counters(t, consumerID); calls != 4 {
		t.Errorf("verify calls = %d after a refusal, want 4", calls)
	}
}

// TestAnUnregisteredConsumerCannotUseTheFreshCheck. The rate signal is per consumer, so a caller
// that cannot be metered would have an unmetered path to the authoritative read.
func TestAnUnregisteredConsumerCannotUseTheFreshCheck(t *testing.T) {
	f := newFixture(t)
	tenantID, principalID := f.seed(t, "active", "active")

	_, err := f.service.Verify(f.ctx, VerifyRequest{
		ConsumerID: "never-registered", TenantID: tenantID, PrincipalID: principalID,
	})
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("error = %v, want ErrNotRegistered", err)
	}
}

// TestTheRatioIsPerIntervalAndBothCountersResetTogether is the exit criterion.
//
// A lifetime ratio dilutes forever: a consumer that misused the path for a day and then fixed it
// would read as healthy a month later, and one running for years could never trip the threshold.
func TestTheRatioIsPerIntervalAndBothCountersResetTogether(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)
	tenantID, principalID := f.seed(t, "active", "active")

	for i := 0; i < 10; i++ {
		if _, err := f.service.Verify(f.ctx, VerifyRequest{
			ConsumerID: consumerID, TenantID: tenantID, PrincipalID: principalID,
		}); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	}

	// Ten checks against a hundred requests: one in ten, well over the threshold.
	rate, err := f.service.RecordRate(f.ctx, RateReport{ConsumerID: consumerID, Requests: 100})
	if err != nil {
		t.Fatalf("RecordRate: %v", err)
	}
	if rate.Calls != 10 || rate.Requests != 100 {
		t.Errorf("rate = %+v, want 10 calls over 100 requests", rate)
	}
	if rate.Ratio != 0.1 {
		t.Errorf("ratio = %v, want 0.1", rate.Ratio)
	}

	// The counter reset, so the next interval measures the next interval.
	calls, requests, ratio := f.counters(t, consumerID)
	if calls != 0 {
		t.Errorf("verify calls = %d after a report, want 0", calls)
	}
	if requests == nil || *requests != 100 {
		t.Errorf("stored requests = %v, want 100", requests)
	}
	if ratio == nil || *ratio != 0.1 {
		t.Errorf("stored ratio = %v, want 0.1", ratio)
	}

	// A well-behaved interval that follows a bad one reads as well behaved.
	if _, err := f.service.Verify(f.ctx, VerifyRequest{
		ConsumerID: consumerID, TenantID: tenantID, PrincipalID: principalID,
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	next, err := f.service.RecordRate(f.ctx, RateReport{ConsumerID: consumerID, Requests: 1000})
	if err != nil {
		t.Fatalf("RecordRate: %v", err)
	}
	if next.Ratio != 0.001 {
		t.Errorf("ratio = %v, want 0.001 — the previous interval leaked into this one", next.Ratio)
	}
}

// TestAnIntervalWithNoRequestsLeavesTheRatioUnset.
//
// A consumer that served nothing and checked nothing is misusing nothing; one that served nothing
// and checked repeatedly has a ratio of infinity, which no threshold comparison handles usefully.
// The calls still clear, so the next interval measures the next interval.
func TestAnIntervalWithNoRequestsLeavesTheRatioUnset(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)
	tenantID, principalID := f.seed(t, "active", "active")

	if _, err := f.service.Verify(f.ctx, VerifyRequest{
		ConsumerID: consumerID, TenantID: tenantID, PrincipalID: principalID,
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	rate, err := f.service.RecordRate(f.ctx, RateReport{ConsumerID: consumerID, Requests: 0})
	if err != nil {
		t.Fatalf("RecordRate: %v", err)
	}
	if rate.Calls != 1 {
		t.Errorf("calls = %d, want 1", rate.Calls)
	}
	if rate.Ratio != 0 {
		t.Errorf("ratio = %v, want the zero value for an unset ratio", rate.Ratio)
	}
	calls, _, ratio := f.counters(t, consumerID)
	if calls != 0 {
		t.Errorf("verify calls = %d, want 0 — an unmeasurable interval still ends", calls)
	}
	if ratio != nil {
		t.Errorf("stored ratio = %v, want NULL", ratio)
	}

	// A negative count is not a low ratio; it is a broken counter.
	if _, err := f.service.RecordRate(f.ctx, RateReport{ConsumerID: consumerID, Requests: -1}); !errors.Is(err, ErrRequestRequired) {
		t.Errorf("a negative request count returned %v, want ErrRequestRequired", err)
	}
}

// TestOverThresholdNamesTheMisusingConsumersWorstFirst.
//
// An operator reading the first line of this list should see the consumer that matters, not
// whichever row a hash bucket happened to hold.
func TestOverThresholdNamesTheMisusingConsumersWorstFirst(t *testing.T) {
	f := newFixture(t)
	tenantID, principalID := f.seed(t, "active", "active")

	// Three consumers: well behaved, over the threshold, and far over it.
	type wanted struct {
		id    string
		calls int
		reqs  int64
		ratio float64
	}
	cases := []wanted{
		{f.register(t), 1, 1000, 0.001},
		{f.register(t), 2, 20, 0.1},
		{f.register(t), 6, 10, 0.6},
	}
	for _, c := range cases {
		for i := 0; i < c.calls; i++ {
			if _, err := f.service.Verify(f.ctx, VerifyRequest{
				ConsumerID: c.id, TenantID: tenantID, PrincipalID: principalID,
			}); err != nil {
				t.Fatalf("Verify: %v", err)
			}
		}
		if _, err := f.service.RecordRate(f.ctx, RateReport{ConsumerID: c.id, Requests: c.reqs}); err != nil {
			t.Fatalf("RecordRate: %v", err)
		}
	}

	over, err := f.service.OverThreshold(f.ctx, DefaultRateAlert)
	if err != nil {
		t.Fatalf("OverThreshold: %v", err)
	}

	found := map[string]float64{}
	var previous = 1.0
	for _, rate := range over {
		found[rate.ConsumerID] = rate.Ratio
		if rate.Ratio > previous {
			t.Errorf("the list is not ordered worst first: %v after %v", rate.Ratio, previous)
		}
		previous = rate.Ratio
	}
	if _, flagged := found[cases[0].id]; flagged {
		t.Error("a consumer at one call per thousand requests was flagged")
	}
	for _, c := range cases[1:] {
		if got, flagged := found[c.id]; !flagged {
			t.Errorf("a consumer at %v was not flagged", c.ratio)
		} else if got != c.ratio {
			t.Errorf("consumer ratio = %v, want %v", got, c.ratio)
		}
	}

	// Ten times the threshold is the critical band. The list carries the ratio so an alert can
	// separate the two without a second query.
	if found[cases[2].id] < DefaultRateAlert*10 {
		t.Errorf("the worst consumer reads %v, below the critical band", found[cases[2].id])
	}
}

// TestTheCheckAnswersAboutNowRatherThanAboutTheProjection walks every refusal.
func TestTheCheckAnswersAboutNowRatherThanAboutTheProjection(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)

	cases := []struct {
		name             string
		tenantStatus     string
		membershipStatus string
		wantGranted      bool
		wantRefusal      Refusal
	}{
		{"active in active", "active", "active", true, RefusalNone},
		{"no membership", "active", "", false, RefusalNoMembership},
		{"suspended membership", "active", "suspended", false, RefusalNoMembership},
		{"revoked membership", "active", "revoked", false, RefusalNoMembership},
		{"active membership in suspended tenant", "suspended", "active", false, RefusalTenantNotActive},
		{"active membership in offboarding tenant", "offboarding", "active", false, RefusalTenantNotActive},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenantID, principalID := f.seed(t, tc.tenantStatus, tc.membershipStatus)
			decision, err := f.service.Verify(f.ctx, VerifyRequest{
				ConsumerID: consumerID, TenantID: tenantID, PrincipalID: principalID,
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if decision.Granted != tc.wantGranted {
				t.Errorf("granted = %v, want %v (refusal %q)", decision.Granted, tc.wantGranted, decision.Refusal)
			}
			if decision.Refusal != tc.wantRefusal {
				t.Errorf("refusal = %q, want %q", decision.Refusal, tc.wantRefusal)
			}
			if !decision.Granted && !decision.MembershipID.IsNil() {
				t.Error("a refusal disclosed a Membership identifier")
			}
			if decision.Granted {
				if decision.MembershipVersion != 7 {
					t.Errorf("membership version = %d, want 7", decision.MembershipVersion)
				}
				if decision.TenantSecurityVersion == 0 {
					t.Error("a granted decision carries no tenant security version")
				}
				if !decision.CheckedAt.Equal(f.fixed) {
					t.Errorf("checked at %s, want the recorded instant %s", decision.CheckedAt, f.fixed)
				}
			}
		})
	}
}

// TestASuspendedMembershipAndAnAbsentOneAnswerIdentically.
//
// Probing the difference would disclose that a Principal once held access in a Tenant, to a caller
// that holds nothing there now. The answer is the same, so the disclosure buys the caller nothing
// it is entitled to.
func TestASuspendedMembershipAndAnAbsentOneAnswerIdentically(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)

	suspendedTenant, suspendedPrincipal := f.seed(t, "active", "suspended")
	absentTenant, absentPrincipal := f.seed(t, "active", "")

	suspended, err := f.service.Verify(f.ctx, VerifyRequest{
		ConsumerID: consumerID, TenantID: suspendedTenant, PrincipalID: suspendedPrincipal,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	absent, err := f.service.Verify(f.ctx, VerifyRequest{
		ConsumerID: consumerID, TenantID: absentTenant, PrincipalID: absentPrincipal,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if suspended.Refusal != absent.Refusal {
		t.Errorf("a suspended Membership answers %q and an absent one %q", suspended.Refusal, absent.Refusal)
	}
	if suspended.Granted || absent.Granted {
		t.Error("a non-active Membership was granted")
	}
}

// TestAnAbsentTenantIsIndistinguishableFromAnAbsentMembership. Naming which would tell an
// unauthorised caller whether a Tenant identifier exists.
func TestAnAbsentTenantIsIndistinguishableFromAnAbsentMembership(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)

	decision, err := f.service.Verify(f.ctx, VerifyRequest{
		ConsumerID: consumerID, TenantID: mustID(t), PrincipalID: mustID(t),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if decision.Refusal != RefusalNoMembership {
		t.Errorf("refusal = %q, want %q for a Tenant that does not exist",
			decision.Refusal, RefusalNoMembership)
	}
}

// TestASwitchIsDecidedAuthoritativelyAndNeverFromAProjection.
//
// A context switch mints authority for a context the caller was not operating in, so it is the one
// operation that must never be decided from a projection: the projection can be inside its declared
// freshness budget and still predate the revocation that matters.
func TestASwitchIsDecidedAuthoritativelyAndNeverFromAProjection(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)
	tenantID, principalID := f.seed(t, "active", "active")

	eligible, err := f.service.SwitchEligible(f.ctx, VerifyRequest{
		ConsumerID: consumerID, TenantID: tenantID, PrincipalID: principalID,
	})
	if err != nil {
		t.Fatalf("SwitchEligible: %v", err)
	}
	if !eligible.Granted {
		t.Fatalf("a switch into a held context was refused: %s", eligible.Refusal)
	}

	// Revoke, and the very next check refuses. No projection, no cache, no budget.
	f.exec(t, `UPDATE membership.membership SET status = 'revoked' WHERE tenant_id = $1`, tenantID.String())

	refused, err := f.service.SwitchEligible(f.ctx, VerifyRequest{
		ConsumerID: consumerID, TenantID: tenantID, PrincipalID: principalID,
	})
	if err != nil {
		t.Fatalf("SwitchEligible: %v", err)
	}
	if refused.Granted {
		t.Error("a switch into a revoked context was permitted")
	}

	// The switch was metered like any other fresh check.
	if calls, _, _ := f.counters(t, consumerID); calls != 2 {
		t.Errorf("verify calls = %d, want 2 — a switch check that is not metered is an unmetered path", calls)
	}
}

// TestEveryCheckRecordsProviderEvidence. A cross-Tenant read of who holds access is exactly the
// access an audit asks about, so it is recorded before the transaction rather than alongside it.
func TestEveryCheckRecordsProviderEvidence(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)
	tenantID, principalID := f.seed(t, "active", "active")
	before := f.recorder.calls

	if _, err := f.service.Verify(f.ctx, VerifyRequest{
		ConsumerID: consumerID, TenantID: tenantID, PrincipalID: principalID,
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if f.recorder.calls != before+1 {
		t.Errorf("the check recorded %d accesses, want 1", f.recorder.calls-before)
	}
}

// TestTheCheckRefusesAnIncompleteRequest.
func TestTheCheckRefusesAnIncompleteRequest(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)

	for name, req := range map[string]VerifyRequest{
		"no consumer":  {TenantID: mustID(t), PrincipalID: mustID(t)},
		"no tenant":    {ConsumerID: consumerID, PrincipalID: mustID(t)},
		"no principal": {ConsumerID: consumerID, TenantID: mustID(t)},
	} {
		if _, err := f.service.Verify(f.ctx, req); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

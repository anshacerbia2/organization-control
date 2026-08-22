package tenant

// Against a real engine, as `organization_provider_app` — a login role inheriting
// `organization_provider_rt`, which owns no table and holds no BYPASSRLS.
//
// An in-package test file, because two seams are unexported. That is deliberate: the clock seam and
// the injection point exist so the accepted timestamp and the atomicity claim can be falsified, and
// exporting either would put a way to skip the outbox append into the production API.

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

const fixedNowText = "2026-08-23T09:10:11Z"

// recorder captures the provider-access evidence instead of writing it.
//
// `db.NewProviderPool` refuses a nil recorder, so every test here proves the evidence path is
// wired as a side effect of running at all — which is the point of making it mandatory.
type recorder struct {
	calls []db.ProviderAccess
	fail  error
}

func (r *recorder) RecordProviderAccess(_ context.Context, access db.ProviderAccess) error {
	r.calls = append(r.calls, access)
	return r.fail
}

type fixture struct {
	service  *Service
	ctx      context.Context
	recorder *recorder
	fixed    time.Time
	actor    id.UUID
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	pool, err := fdb.Open(ctx, fdb.Config{Name: "tenant-test", DSN: dsn, MaxConns: 2})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	rec := &recorder{}
	providerPool, err := db.NewProviderPool(pool, rec)
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}
	service, err := New(providerPool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fixed, err := time.Parse(time.RFC3339, fixedNowText)
	if err != nil {
		t.Fatalf("parse fixed time: %v", err)
	}
	service.now = func() time.Time { return fixed }

	actor, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	correlation, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	scope, err := db.ProviderScope(actor, correlation)
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	return &fixture{
		service:  service,
		ctx:      db.WithScope(ctx, scope),
		recorder: rec,
		fixed:    fixed,
		actor:    actor,
	}
}

// seed creates an Organization and a Tenant of its own rather than reusing the two the suite seeds.
//
// These tests move a Tenant through states, and `go test ./...` runs packages concurrently against
// one database. Mutating a shared Tenant would make the Membership suite fail for a reason that has
// nothing to do with Membership.
func (f *fixture) seed(t *testing.T, status State, sponsorStatus string) Tenant {
	t.Helper()

	organizationID, tenantID := mustID(t), mustID(t)

	f.exec(t, `INSERT INTO organization.organization
	    (organization_id, display_name, classification, status)
	VALUES ($1, 'tenant-suite sponsor', 'customer', $2)`, organizationID.String(), sponsorStatus)

	f.exec(t, `INSERT INTO tenant.tenant
	    (tenant_id, organization_id, display_name, status, isolation_profile)
	VALUES ($1, $2, 'tenant-suite subject', $3, 'pooled')`,
		tenantID.String(), organizationID.String(), string(status))

	t.Cleanup(func() {
		f.exec(t, `DELETE FROM platform.outbox WHERE aggregate_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM tenant.provisioning_request WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM organization.organization WHERE organization_id = $1`, organizationID.String())
	})

	return Tenant{TenantID: tenantID, OrganizationID: organizationID, Status: status, Version: 1, SecurityVersion: 1}
}

// requestProvisioning records one provisioning attempt. requestedAt is explicit so a retry ordering
// can be constructed rather than raced.
func (f *fixture) requestProvisioning(t *testing.T, tenantID id.UUID, state string, requestedAt time.Time) {
	t.Helper()
	f.exec(t, `INSERT INTO tenant.provisioning_request
	    (request_id, tenant_id, desired_profile, state, correlation_id, requested_at)
	VALUES ($1, $2, '{}'::jsonb, $3, $4, $5)`,
		mustID(t).String(), tenantID.String(), state, mustID(t).String(), requestedAt)
}

func (f *fixture) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	if err := db.WithProviderScope(f.ctx, f.service.pool, "tenant suite fixture",
		func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, statement, args...)
			return err
		}); err != nil {
		t.Fatalf("fixture statement: %v", err)
	}
}

type row struct {
	status          State
	version         int64
	securityVersion int64
	activatedAt     *time.Time
	suspendedAt     *time.Time
}

func (f *fixture) read(t *testing.T, tenantID id.UUID) row {
	t.Helper()
	var got row
	var status string
	if err := db.WithProviderScope(f.ctx, f.service.pool, "tenant suite read",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT status, version, tenant_security_version, activated_at, suspended_at
	        FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String()).
				Scan(&status, &got.version, &got.securityVersion, &got.activatedAt, &got.suspendedAt)
		}); err != nil {
		t.Fatalf("read tenant: %v", err)
	}
	got.status = State(status)
	return got
}

type published struct {
	eventType string
	priority  int16
	occurred  time.Time
}

func (f *fixture) events(t *testing.T, tenantID id.UUID) []published {
	t.Helper()
	var all []published
	if err := db.WithProviderScope(f.ctx, f.service.pool, "tenant suite read",
		func(ctx context.Context, tx db.Tx) error {
			result, err := tx.Query(ctx, `SELECT event_type, priority, (envelope->>'time')::timestamptz
	        FROM platform.outbox WHERE aggregate_id = $1 ORDER BY sequence`, tenantID.String())
			if err != nil {
				return err
			}
			defer result.Close()
			for result.Next() {
				var next published
				if err := result.Scan(&next.eventType, &next.priority, &next.occurred); err != nil {
					return err
				}
				all = append(all, next)
			}
			return result.Err()
		}); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return all
}

func mustID(t *testing.T) id.UUID {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	return value
}

func command(tenantID id.UUID, version int64) Command {
	return Command{TenantID: tenantID, Reason: "asserted by the tenant suite", ExpectedVersion: version}
}

// TestSuspensionIncrementsTheSecurityVersionAndSoDoesRestore is the Week 2 item.
//
// STD-IAM-002 §3.5 rule 8 rejects a token whose version is below the local projection's, which is
// what lets a suspension take effect before the token expires. Restore increments as well, and that
// is the one that is easy to omit: a consumer holding "suspended" with no version change keeps
// denying a Tenant that has been restored, and the symptom arrives as a support ticket rather than
// as a projection failure.
func TestSuspensionIncrementsTheSecurityVersionAndSoDoesRestore(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateActive, "active")

	suspended, err := f.service.Suspend(f.ctx, command(seeded.TenantID, 1))
	if err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if suspended.Tenant.Status != StateSuspended {
		t.Errorf("status = %s, want suspended", suspended.Tenant.Status)
	}
	if suspended.Tenant.SecurityVersion != 2 || suspended.Tenant.Version != 2 {
		t.Errorf("versions = (%d, %d), want (2, 2)",
			suspended.Tenant.Version, suspended.Tenant.SecurityVersion)
	}
	if got := f.read(t, seeded.TenantID); got.suspendedAt == nil || !got.suspendedAt.Equal(f.fixed) {
		t.Errorf("suspended_at = %v, want the accepted instant %s", got.suspendedAt, f.fixed)
	}

	restored, err := f.service.Restore(f.ctx, command(seeded.TenantID, 2))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.Tenant.SecurityVersion != 3 {
		t.Errorf("security version = %d after restore, want 3", restored.Tenant.SecurityVersion)
	}

	// suspended_at records when the *current* suspension began. Left populated after a restore it
	// would make a restored Tenant indistinguishable from a suspended one to every report that
	// filters on the column; the history of past suspensions is in the event stream, which is the
	// record that is supposed to be append-only.
	if got := f.read(t, seeded.TenantID); got.suspendedAt != nil {
		t.Errorf("suspended_at = %v after a restore, want NULL", got.suspendedAt)
	}
}

// TestBothWithdrawalDirectionsTakeThePriorityLane. Suspension removes cached context and
// restoration makes a cached denial wrong; both are urgent for the same reason, which is that a
// consumer acting on the stale answer is wrong in a way it cannot detect locally.
func TestBothWithdrawalDirectionsTakeThePriorityLane(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateActive, "active")

	if _, err := f.service.Suspend(f.ctx, command(seeded.TenantID, 1)); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if _, err := f.service.Restore(f.ctx, command(seeded.TenantID, 2)); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	events := f.events(t, seeded.TenantID)
	if len(events) != 2 {
		t.Fatalf("the outbox holds %d events, want 2", len(events))
	}
	for i, want := range []string{
		"com.scnehaux.organization.tenant.security.suspended",
		"com.scnehaux.organization.tenant.security.restored",
	} {
		if events[i].eventType != want {
			t.Errorf("event %d = %q, want %q", i, events[i].eventType, want)
		}
		// The named constant rather than its value: the dispatcher claims with `ORDER BY priority
		// ASC`, so the reserved lane is the lower number and a literal here would read backwards.
		if events[i].priority != outbox.PriorityHigh {
			t.Errorf("event %d took priority %d, want the reserved lane", i, events[i].priority)
		}
		if !events[i].occurred.Equal(f.fixed) {
			t.Errorf("event %d carries %s, want the accepted instant %s", i, events[i].occurred, f.fixed)
		}
	}
}

// TestTheStatusChangeAndTheEventCommitTogether is the atomicity claim.
//
// A suspension that committed without its event is unreachable by every consumer: authority says
// suspended, every projection says active, and the security version has moved with nothing telling
// anyone to re-read it. That is the exact failure the transactional outbox exists to prevent, so it
// is asserted by failing inside the window it protects.
func TestTheStatusChangeAndTheEventCommitTogether(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateActive, "active")
	before := f.read(t, seeded.TenantID)

	injected := errors.New("failure between the status change and the append")
	f.service.beforeAppend = func(context.Context) error { return injected }

	if _, err := f.service.Suspend(f.ctx, command(seeded.TenantID, 1)); !errors.Is(err, injected) {
		t.Fatalf("error = %v, want the injected failure", err)
	}

	after := f.read(t, seeded.TenantID)
	if after.status != before.status {
		t.Errorf("status = %s after a rolled-back suspension, want %s", after.status, before.status)
	}
	if after.version != before.version {
		t.Errorf("version = %d, want %d", after.version, before.version)
	}
	// The one that would be silent. A security version that survived the rollback would make every
	// consumer re-read a Tenant nothing had happened to, and nothing would ever report the
	// mismatch.
	if after.securityVersion != before.securityVersion {
		t.Errorf("security version = %d, want %d", after.securityVersion, before.securityVersion)
	}
	if events := f.events(t, seeded.TenantID); len(events) != 0 {
		t.Errorf("the outbox holds %d events after a rolled-back suspension", len(events))
	}

	// The same suspension succeeds once the injection is removed, so the rollback left the row
	// usable rather than merely unchanged.
	f.service.beforeAppend = nil
	if _, err := f.service.Suspend(f.ctx, command(seeded.TenantID, before.version)); err != nil {
		t.Fatalf("Suspend after the rollback: %v", err)
	}
	if got := f.read(t, seeded.TenantID); got.securityVersion != before.securityVersion+1 {
		t.Errorf("security version = %d, want %d", got.securityVersion, before.securityVersion+1)
	}
}

// TestAStaleExpectedVersionIsRefused. Two operators looking at the same Tenant page decide
// independently, and without this the second write wins silently — having been decided from a state
// that no longer held.
func TestAStaleExpectedVersionIsRefused(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateActive, "active")

	if _, err := f.service.Suspend(f.ctx, command(seeded.TenantID, 1)); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// A second operator still holding version 1 tries to restore.
	_, err := f.service.Restore(f.ctx, command(seeded.TenantID, 1))
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("error = %v, want ErrVersionMismatch", err)
	}
	if got := f.read(t, seeded.TenantID); got.status != StateSuspended || got.securityVersion != 2 {
		t.Errorf("a refused restore changed the row: %+v", got)
	}
	if events := f.events(t, seeded.TenantID); len(events) != 1 {
		t.Errorf("the outbox holds %d events, want 1", len(events))
	}
}

// TestARefusedTransitionSaysWhichRuleRefusedIt. A caller acting on a stale view usually gets both
// the state and the version wrong, and the state is the one that tells an operator what happened.
func TestARefusedTransitionSaysWhichRuleRefusedIt(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateActive, "active")

	// Restore from active is not in the machine, and the version supplied is also wrong.
	_, err := f.service.Restore(f.ctx, command(seeded.TenantID, 99))
	if !errors.Is(err, ErrTransitionRefused) {
		t.Fatalf("error = %v, want ErrTransitionRefused", err)
	}
}

// TestActivationWaitsForProvisioningToBeRealized is SAD-004 §5.1.
//
// Activating on request would mean Memberships could be granted into a Tenant whose isolation
// boundary does not exist yet — and the Membership service has no way to detect that, because from
// its side the Tenant reads as active.
func TestActivationWaitsForProvisioningToBeRealized(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateProvisioning, "active")

	// No request at all.
	if _, err := f.service.Activate(f.ctx, command(seeded.TenantID, 1)); !errors.Is(err, ErrProvisioningNotRealized) {
		t.Fatalf("error with no provisioning request = %v, want ErrProvisioningNotRealized", err)
	}

	f.requestProvisioning(t, seeded.TenantID, "requested", f.fixed.Add(-time.Hour))
	if _, err := f.service.Activate(f.ctx, command(seeded.TenantID, 1)); !errors.Is(err, ErrProvisioningNotRealized) {
		t.Fatalf("error with an unrealized request = %v, want ErrProvisioningNotRealized", err)
	}

	f.requestProvisioning(t, seeded.TenantID, "realized", f.fixed.Add(-time.Minute))
	activated, err := f.service.Activate(f.ctx, command(seeded.TenantID, 1))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if activated.Tenant.Status != StateActive {
		t.Errorf("status = %s, want active", activated.Tenant.Status)
	}
	// Activation invalidates nothing, because no context existed inside a Tenant that has never
	// been active. A version bump here would make every consumer re-read for no reason.
	if activated.Tenant.SecurityVersion != 1 {
		t.Errorf("security version = %d after activation, want 1", activated.Tenant.SecurityVersion)
	}
	if got := f.read(t, seeded.TenantID); got.activatedAt == nil || !got.activatedAt.Equal(f.fixed) {
		t.Errorf("activated_at = %v, want the accepted instant %s", got.activatedAt, f.fixed)
	}

	events := f.events(t, seeded.TenantID)
	if len(events) != 1 || events[0].eventType != "com.scnehaux.organization.tenant.lifecycle.activated" {
		t.Fatalf("events = %+v, want one lifecycle.activated", events)
	}
	if events[0].priority != outbox.PriorityStandard {
		t.Errorf("activation took priority %d, want the standard lane", events[0].priority)
	}
}

// TestActivationReadsTheMostRecentProvisioningAttempt. A realized attempt followed by a later
// failure must not activate, and `EXISTS ... AND state = 'realized'` would get exactly that case
// wrong in the permissive direction.
func TestActivationReadsTheMostRecentProvisioningAttempt(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateProvisioning, "active")

	f.requestProvisioning(t, seeded.TenantID, "realized", f.fixed.Add(-2*time.Hour))
	f.requestProvisioning(t, seeded.TenantID, "failed", f.fixed.Add(-time.Hour))

	if _, err := f.service.Activate(f.ctx, command(seeded.TenantID, 1)); !errors.Is(err, ErrProvisioningNotRealized) {
		t.Fatalf("error = %v, want ErrProvisioningNotRealized", err)
	}

	// And the reverse order activates, so the ordering is what decided it rather than the presence
	// of a failure.
	f.requestProvisioning(t, seeded.TenantID, "realized", f.fixed.Add(-time.Minute))
	if _, err := f.service.Activate(f.ctx, command(seeded.TenantID, 1)); err != nil {
		t.Fatalf("Activate after a successful retry: %v", err)
	}
}

// TestActivationRefusesUnderASponsorThatIsNotActive. Checked at activation rather than at creation:
// a Tenant may legitimately be requested while its Organization is still being onboarded, and may
// not become active under a sponsor that has since been suspended.
func TestActivationRefusesUnderASponsorThatIsNotActive(t *testing.T) {
	for _, sponsor := range []string{"suspended", "retired"} {
		t.Run(sponsor, func(t *testing.T) {
			f := newFixture(t)
			seeded := f.seed(t, StateProvisioning, sponsor)
			f.requestProvisioning(t, seeded.TenantID, "realized", f.fixed.Add(-time.Minute))

			_, err := f.service.Activate(f.ctx, command(seeded.TenantID, 1))
			if !errors.Is(err, ErrSponsorNotActive) {
				t.Fatalf("error = %v, want ErrSponsorNotActive", err)
			}
			if got := f.read(t, seeded.TenantID); got.status != StateProvisioning {
				t.Errorf("status = %s after a refused activation", got.status)
			}
		})
	}
}

// TestEveryTransitionCarriesRecordedEvidence is PAD-PLT-002 §3.3 invariant 22.
//
// Every Tenant transition is cross-Tenant administration, so every one records an actor, a
// correlation, and a reason before the transaction runs. Recorded first rather than afterwards:
// evidence written on the way out is missing for exactly the cases an investigation asks about,
// because a transaction that panics or is killed mid-flight never reaches its own epilogue.
func TestEveryTransitionCarriesRecordedEvidence(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateActive, "active")
	before := len(f.recorder.calls)

	if _, err := f.service.Suspend(f.ctx, command(seeded.TenantID, 1)); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if len(f.recorder.calls) != before+1 {
		t.Fatalf("the suspension recorded %d accesses, want 1", len(f.recorder.calls)-before)
	}
	access := f.recorder.calls[len(f.recorder.calls)-1]
	if access.Actor != f.actor {
		t.Errorf("actor = %s, want %s", access.Actor, f.actor)
	}
	if access.Correlation.IsNil() {
		t.Error("the recorded access carries no correlation identifier")
	}
	if access.Reason != "asserted by the tenant suite" {
		t.Errorf("reason = %q", access.Reason)
	}
}

// TestATransitionWithoutAReasonNeverReachesTheDatabase. A blank reason is evidence naming nobody's
// intent, so it is refused before the transaction opens rather than recorded as empty.
func TestATransitionWithoutAReasonNeverReachesTheDatabase(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateActive, "active")
	before := len(f.recorder.calls)

	_, err := f.service.Suspend(f.ctx, Command{TenantID: seeded.TenantID, ExpectedVersion: 1})
	if err == nil {
		t.Fatal("a suspension with no reason was accepted")
	}
	if len(f.recorder.calls) != before {
		t.Error("a refused command still recorded provider access")
	}
	if got := f.read(t, seeded.TenantID); got.status != StateActive {
		t.Errorf("status = %s after a refused command", got.status)
	}
}

// TestAFailureToRecordEvidenceStopsTheTransition. Proceeding without evidence would make the access
// unattributable, which is the one property the invariant does not treat as best-effort.
func TestAFailureToRecordEvidenceStopsTheTransition(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateActive, "active")

	f.recorder.fail = errors.New("the evidence store is unavailable")
	_, err := f.service.Suspend(f.ctx, command(seeded.TenantID, 1))
	f.recorder.fail = nil

	if err == nil {
		t.Fatal("the suspension proceeded without recorded evidence")
	}
	if got := f.read(t, seeded.TenantID); got.status != StateActive || got.securityVersion != 1 {
		t.Errorf("the row moved despite unrecorded evidence: %+v", got)
	}
}

// TestATenantScopeCannotDriveTenantTransitions. A Tenant does not suspend itself, and the pool
// types are distinct so that handing this service a tenant-scoped context fails rather than
// binding one Tenant onto a cross-Tenant policy.
func TestATenantScopeCannotDriveTenantTransitions(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateActive, "active")

	scope, err := db.TenantScope(seeded.TenantID, f.actor, mustID(t))
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}
	_, err = f.service.Suspend(db.WithScope(f.ctx, scope), command(seeded.TenantID, 1))
	if !errors.Is(err, db.ErrWrongScope) {
		t.Fatalf("error = %v, want ErrWrongScope", err)
	}
}

// TestAnAbsentTenantIsNotFound, and the error does not attempt to be helpful about it. There is no
// cross-Tenant disclosure to make here — the provider scope may read every Tenant — but the same
// sentinel keeps the surface's mapping to 404 independent of which scope asked.
func TestAnAbsentTenantIsNotFound(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Suspend(f.ctx, command(mustID(t), 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

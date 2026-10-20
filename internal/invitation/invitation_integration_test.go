package invitation

// The invitation half of Week 4, against a real engine.
//
// The property under test is SAD-004 §5.5: Membership activates on the join of two independent
// facts, and neither alone activates anything. From ADR-ORG-007 the two facts are an unexpired
// invitation and the signed-in caller holding the invited address as a proven address, both checked
// when the caller accepts. Most of what follows is that sentence, taken apart.
//
// In-package, because the clock, identifier, and token seams are unexported — and the token seam
// must be, since a test that read the token back out of the database would disprove the property
// that the token is never stored.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
	"github.com/anshacerbia2/organization-control/internal/membership"
)

const fixedNowText = "2026-08-28T08:09:10Z"

type recorder struct{}

func (recorder) RecordProviderAccess(context.Context, db.ProviderAccess) error { return nil }

// matcher stands in for the Identity Control API's identifiers:match: it answers from a table of the
// addresses each Principal holds as proven, and records every question.
type matcher struct {
	mu      sync.Mutex
	proven  map[id.UUID][]string
	err     error
	matched []id.UUID
}

func (m *matcher) Match(_ context.Context, principal id.UUID, address string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.matched = append(m.matched, principal)
	if m.err != nil {
		return false, m.err
	}
	for _, held := range m.proven[principal] {
		if strings.EqualFold(held, address) {
			return true, nil
		}
	}
	return false, nil
}

func (m *matcher) prove(principal id.UUID, address string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proven[principal] = append(m.proven[principal], address)
}

func (m *matcher) asked() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.matched)
}

type fixture struct {
	setup *fdb.Pool

	service     *Service
	matcher     *matcher
	tenantPool  *db.TenantPool
	selfPool    *db.SelfPool
	memberships *membership.Service
	provider    *db.ProviderPool
	providerCtx context.Context
	scopeFor    func(id.UUID) context.Context
	fixed       time.Time
	actor       id.UUID

	// raw is the provider connections with no scope wrapper, as the scheduled expiry holds them.
	raw *fdb.Pool
}

func hostFrom(t *testing.T) string {
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
	return rest
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	host := hostFrom(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	runtimePool, err := fdb.Open(ctx, fdb.Config{
		Name:     "invitation-runtime",
		DSN:      fmt.Sprintf("postgres://organization_app:%s@%s", os.Getenv("TEST_RUNTIME_PASSWORD"), host),
		MaxConns: 4,
	})
	if err != nil {
		t.Fatalf("open runtime pool: %v", err)
	}
	t.Cleanup(runtimePool.Close)

	providerPool, err := fdb.Open(ctx, fdb.Config{
		Name:     "invitation-provider",
		DSN:      fmt.Sprintf("postgres://organization_provider_app:%s@%s", os.Getenv("TEST_PROVIDER_PASSWORD"), host),
		MaxConns: 4,
	})
	if err != nil {
		t.Fatalf("open provider pool: %v", err)
	}
	t.Cleanup(providerPool.Close)

	tenantPool, err := db.NewTenantPool(runtimePool)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	provider, err := db.NewProviderPool(providerPool, recorder{})
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}
	memberships, err := membership.New(tenantPool)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	selfPool, err := db.NewSelfPool(runtimePool)
	if err != nil {
		t.Fatalf("NewSelfPool: %v", err)
	}
	match := &matcher{proven: map[id.UUID][]string{}}
	service, err := New(tenantPool, provider, selfPool, memberships, match)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fixed, err := time.Parse(time.RFC3339, fixedNowText)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	service.now = func() time.Time { return fixed }

	actor, correlation := mustID(t), mustID(t)
	providerScope, err := db.ProviderScope(actor, correlation, db.EmergencyAuthority())
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	// TEST_DATABASE_URL unmodified: the owner, not a runtime role.
	setup, err := fdb.Open(ctx, fdb.Config{Name: "invitation-test-setup", DSN: os.Getenv("TEST_DATABASE_URL"), MaxConns: 2})
	if err != nil {
		t.Fatalf("open setup pool: %v", err)
	}
	t.Cleanup(setup.Close)

	return &fixture{
		setup:   setup,
		service: service, provider: provider, matcher: match,
		tenantPool: tenantPool, selfPool: selfPool, memberships: memberships,
		providerCtx: db.WithScope(ctx, providerScope),
		scopeFor: func(tenantID id.UUID) context.Context {
			scope, err := db.TenantScope(tenantID, actor, mustID(t))
			if err != nil {
				t.Fatalf("TenantScope: %v", err)
			}
			return db.WithScope(ctx, scope)
		},
		fixed: fixed, actor: actor,
		raw: providerPool,
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

// exec runs seeding and teardown on the owner connection, not on the pool the service uses.
//
// Seeding is not the system under test. While the two shared a credential, this suite could
// not tell "the code needs that privilege" apart from "my cleanup needs it".
func (f *fixture) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	if err := f.setup.InTx(f.providerCtx, func(ctx context.Context, tx fdb.Tx) error {
		_, err := tx.Exec(ctx, statement, args...)
		return err
	}); err != nil {
		t.Fatalf("fixture statement: %v", err)
	}
}

func (f *fixture) seedTenant(t *testing.T, status string) id.UUID {
	t.Helper()
	organizationID, tenantID := mustID(t), mustID(t)
	f.exec(t, `INSERT INTO organization.organization (organization_id, display_name, classification, status)
	    VALUES ($1, 'invitation suite', 'customer', 'active')`, organizationID.String())
	f.exec(t, `INSERT INTO tenant.tenant (tenant_id, organization_id, display_name, status, isolation_profile)
	    VALUES ($1, $2, 'invitation suite', $3, 'pooled')`, tenantID.String(), organizationID.String(), status)
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM platform.outbox WHERE aggregate_id IN (
		    SELECT invitation_id FROM invitation.invitation WHERE tenant_id = $1)`, tenantID.String())
		f.exec(t, `DELETE FROM platform.outbox WHERE aggregate_id IN (
		    SELECT membership_id FROM membership.membership WHERE tenant_id = $1)`, tenantID.String())
		f.exec(t, `DELETE FROM invitation.invitation WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM membership.membership_event WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM membership.membership WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM tenant.tenant_event WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM tenant.tenant WHERE tenant_id = $1`, tenantID.String())
		f.exec(t, `DELETE FROM organization.organization WHERE organization_id = $1`, organizationID.String())
	})
	return tenantID
}

// issue invites a fresh identifier and returns the issue result.
func (f *fixture) issue(t *testing.T, tenantID id.UUID, identifier string) Issued {
	t.Helper()
	issued, err := f.service.Issue(f.scopeFor(tenantID), IssueRequest{
		TargetIdentifier: identifier, SubjectType: "human", Reason: "asserted by the invitation suite",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return issued
}

// selfFor is the context of a person signed in as themselves, with no Tenant: the self caller
// acceptance admits (ADR-ORG-007 §5.1).
func (f *fixture) selfFor(t *testing.T, principal id.UUID) context.Context {
	t.Helper()
	scope, err := db.SelfScope(principal, mustID(t))
	if err != nil {
		t.Fatalf("SelfScope: %v", err)
	}
	return db.WithScope(f.providerCtx, scope)
}

// invitee is a Principal holding identifier as a proven address, and its signed-in context.
func (f *fixture) invitee(t *testing.T, identifier string) (id.UUID, context.Context) {
	t.Helper()
	principal := mustID(t)
	f.matcher.prove(principal, identifier)
	return principal, f.selfFor(t, principal)
}

// stored reads an invitation as its Tenant's administrator.
func (f *fixture) stored(t *testing.T, tenantID, invitationID id.UUID) Invitation {
	t.Helper()
	current, err := f.service.Get(f.scopeFor(tenantID), invitationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return current
}

func (f *fixture) activeMemberships(t *testing.T, tenantID id.UUID) int {
	t.Helper()
	var count int
	if err := db.WithProviderScope(f.providerCtx, f.provider, "invitation suite read",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM membership.membership
			    WHERE tenant_id = $1 AND status = 'active'`, tenantID.String()).Scan(&count)
		}); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	return count
}

func (f *fixture) events(t *testing.T, aggregate id.UUID) []string {
	t.Helper()
	var types []string
	if err := db.WithProviderScope(f.providerCtx, f.provider, "invitation suite read",
		func(ctx context.Context, tx db.Tx) error {
			rows, err := tx.Query(ctx, `SELECT event_type FROM platform.outbox
			    WHERE aggregate_id = $1 ORDER BY sequence`, aggregate.String())
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var e string
				if err := rows.Scan(&e); err != nil {
					return err
				}
				types = append(types, e)
			}
			return rows.Err()
		}); err != nil {
		t.Fatalf("read events: %v", err)
	}
	return types
}

// TestNeitherFactAloneCreatesMembership is SAD-004 §5.5, as ADR-ORG-007 builds it.
//
// An invitation that granted Membership on token presentation would have authenticated an email
// inbox, and an inbox is not a Principal. A proven address with no invitation behind it grants
// nothing either: acceptance starts from the token.
func TestNeitherFactAloneCreatesMembership(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	identifier := "invitee@example.test"

	issued := f.issue(t, tenantID, identifier)
	if issued.Invitation.State != StatePending {
		t.Fatalf("state = %s, want pending", issued.Invitation.State)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Fatal("issuing an invitation created a Membership")
	}

	// Fact one alone: a valid token, presented by a signed-in person who does not hold the address.
	stranger := mustID(t)
	if _, _, err := f.service.Accept(f.selfFor(t, stranger), issued.Token); !errors.Is(err, ErrNotAcceptable) {
		t.Fatalf("accepting without the invited address returned %v, want ErrNotAcceptable", err)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Fatal("token possession alone created a Membership")
	}

	// Fact two alone: a person holding the address, presenting a token that is not the invitation's.
	invitee, inviteeCtx := f.invitee(t, identifier)
	other, _, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if _, _, err := f.service.Accept(inviteeCtx, other); !errors.Is(err, ErrNotAcceptable) {
		t.Fatalf("accepting an unknown token returned %v, want ErrNotAcceptable", err)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Fatal("a proven address alone created a Membership")
	}

	// Both facts: now it works, for the caller and nobody else.
	accepted, granted, err := f.service.Accept(inviteeCtx, issued.Token)
	if err != nil {
		t.Fatalf("Accept with both facts: %v", err)
	}
	if accepted.State != StateAccepted {
		t.Errorf("state = %s, want accepted", accepted.State)
	}
	if accepted.AcceptedAt == nil || !accepted.AcceptedAt.Equal(f.fixed) {
		t.Errorf("accepted_at = %v, want %s", accepted.AcceptedAt, f.fixed)
	}
	if accepted.PrincipalID == nil || *accepted.PrincipalID != invitee {
		t.Errorf("the invitation records principal %v, want the caller %s", accepted.PrincipalID, invitee)
	}
	if granted.Membership.PrincipalID != invitee || granted.Membership.TenantID != tenantID {
		t.Errorf("Membership for %s in %s, want the caller %s in %s",
			granted.Membership.PrincipalID, granted.Membership.TenantID, invitee, tenantID)
	}
	if f.activeMemberships(t, tenantID) != 1 {
		t.Errorf("active Memberships = %d, want 1", f.activeMemberships(t, tenantID))
	}
	// The provenance answers the first question an access review asks.
	if !strings.Contains(granted.Membership.Provenance, issued.Invitation.InvitationID.String()) {
		t.Errorf("provenance = %q, want it to name the invitation", granted.Membership.Provenance)
	}
	stored := f.stored(t, tenantID, issued.Invitation.InvitationID)
	if stored.State != StateAccepted || stored.PrincipalID == nil || *stored.PrincipalID != invitee {
		t.Errorf("stored %s for %v, want accepted for the caller", stored.State, stored.PrincipalID)
	}
	events := f.events(t, issued.Invitation.InvitationID)
	if len(events) != 2 || !strings.HasSuffix(events[1], ".invitation.accepted") {
		t.Errorf("the invitation's events are %v, want requested then accepted", events)
	}
	if got := f.events(t, granted.Membership.MembershipID); len(got) != 1 || !strings.HasSuffix(got[0], ".membership.lifecycle.granted") {
		t.Errorf("the Membership's events are %v, want one grant", got)
	}
}

// TestAnotherMemberHoldingTheTokenIsRefused is the defect ADR-ORG-007 closes: before it, acceptance
// took the token alone, and any member of the Tenant who held it could accept. A member who does not
// hold the invited address is refused like a stranger, and nothing changes: the invitee can still
// accept afterwards.
func TestAnotherMemberHoldingTheTokenIsRefused(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	identifier := "invited@example.test"
	issued := f.issue(t, tenantID, identifier)

	member := mustID(t)
	f.matcher.prove(member, "member@example.test")
	f.exec(t, `INSERT INTO membership.membership
	    (membership_id, principal_id, tenant_id, subject_type, status, membership_version, valid_from, provenance)
	    VALUES ($1, $2, $3, 'human', 'active', 1, now(), 'seeded')`,
		mustID(t).String(), member.String(), tenantID.String())
	// The administrator who issued it holds the token too.
	for name, who := range map[string]id.UUID{"another member": member, "the inviter": f.actor} {
		if _, _, err := f.service.Accept(f.selfFor(t, who), issued.Token); !errors.Is(err, ErrNotAcceptable) {
			t.Fatalf("%s accepting returned %v, want ErrNotAcceptable", name, err)
		}
	}
	if current := f.stored(t, tenantID, issued.Invitation.InvitationID); current.State != StatePending || current.PrincipalID != nil {
		t.Fatalf("after the refusals the invitation is %s for %v, want pending for nobody", current.State, current.PrincipalID)
	}
	if f.activeMemberships(t, tenantID) != 1 {
		t.Fatalf("active Memberships = %d, want the member's one", f.activeMemberships(t, tenantID))
	}

	invitee, inviteeCtx := f.invitee(t, identifier)
	_, granted, err := f.service.Accept(inviteeCtx, issued.Token)
	if err != nil || granted.Membership.PrincipalID != invitee {
		t.Fatalf("the invitee's acceptance: %v, for %s", err, granted.Membership.PrincipalID)
	}
}

// TestEveryFailedLookupIsTheSameRefusal is ADR-ORG-007 §5.4: unknown, expired, revoked, accepted and
// mismatched answer the same sentinel and the same message, so no answer tells a token holder which
// tokens are live. Only a pending, unexpired invitation reaches the match.
func TestEveryFailedLookupIsTheSameRefusal(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	ctx := f.scopeFor(tenantID)

	expired := f.issue(t, tenantID, "lapsed@example.test")
	f.exec(t, `UPDATE invitation.invitation SET expires_at = $2 WHERE invitation_id = $1`,
		expired.Invitation.InvitationID.String(), f.fixed.Add(-time.Minute))
	revoked := f.issue(t, tenantID, "withdrawn@example.test")
	if _, err := f.service.Revoke(ctx, revoked.Invitation.InvitationID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	accepted := f.issue(t, tenantID, "done@example.test")
	_, doneCtx := f.invitee(t, "done@example.test")
	if _, _, err := f.service.Accept(doneCtx, accepted.Token); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	unknown, _, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	mismatched := f.issue(t, tenantID, "someone@example.test")

	// The caller holds every address, so only the invitation's own state can refuse it.
	caller := mustID(t)
	for _, address := range []string{"lapsed@example.test", "withdrawn@example.test", "done@example.test"} {
		f.matcher.prove(caller, address)
	}
	callerCtx := f.selfFor(t, caller)
	before := f.matcher.asked()
	var messages []string
	for name, token := range map[string]Token{
		"unknown": unknown, "expired": expired.Token, "revoked": revoked.Token, "accepted": accepted.Token,
	} {
		_, _, err := f.service.Accept(callerCtx, token)
		if !errors.Is(err, ErrNotAcceptable) {
			t.Errorf("%s returned %v, want ErrNotAcceptable", name, err)
			continue
		}
		messages = append(messages, err.Error())
	}
	if f.matcher.asked() != before {
		t.Errorf("the match was asked %d times for invitations that were not pending", f.matcher.asked()-before)
	}
	_, _, err = f.service.Accept(callerCtx, mismatched.Token)
	if !errors.Is(err, ErrNotAcceptable) {
		t.Fatalf("a mismatched address returned %v, want ErrNotAcceptable", err)
	}
	messages = append(messages, err.Error())
	for _, message := range messages {
		if message != ErrNotAcceptable.Error() {
			t.Errorf("a refusal reads %q, want the one message %q", message, ErrNotAcceptable.Error())
		}
	}
	// And the second acceptance of the accepted one: used once.
	if _, _, err := f.service.Accept(doneCtx, accepted.Token); !errors.Is(err, ErrNotAcceptable) {
		t.Errorf("a second acceptance returned %v, want ErrNotAcceptable", err)
	}
}

// TestUnknownIsNotNo: a match that cannot be asked, or no client configured, refuses with
// ErrIdentityUnavailable and changes nothing (ADR-ORG-007 §5.3).
func TestUnknownIsNotNo(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	identifier := "unknown@example.test"
	issued := f.issue(t, tenantID, identifier)
	_, inviteeCtx := f.invitee(t, identifier)

	f.matcher.err = errors.New("connection refused")
	if _, _, err := f.service.Accept(inviteeCtx, issued.Token); !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("an unreachable match returned %v, want ErrIdentityUnavailable", err)
	}
	f.matcher.err = nil

	unconfigured, err := New(f.tenantPool, f.provider, f.selfPool, f.memberships, nil)
	if err != nil {
		t.Fatalf("New without a matcher: %v", err)
	}
	unconfigured.now = f.service.now
	if _, _, err := unconfigured.Accept(inviteeCtx, issued.Token); !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("no matcher returned %v, want ErrIdentityUnavailable", err)
	}
	if current := f.stored(t, tenantID, issued.Invitation.InvitationID); current.State != StatePending {
		t.Fatalf("state = %s, want pending: an unknown answer changed the invitation", current.State)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Fatal("an unknown answer created a Membership")
	}
}

// TestOnlyASelfScopeAccepts: a Tenant administrator's or a provider's scope reaching Accept is a
// wiring defect, refused before anything is read, so the Membership cannot go to whoever that scope
// names.
func TestOnlyASelfScopeAccepts(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	issued := f.issue(t, tenantID, "scoped@example.test")
	f.matcher.prove(f.actor, "scoped@example.test")
	for name, ctx := range map[string]context.Context{"tenant": f.scopeFor(tenantID), "provider": f.providerCtx} {
		if _, _, err := f.service.Accept(ctx, issued.Token); !errors.Is(err, db.ErrWrongScope) {
			t.Errorf("a %s scope returned %v, want ErrWrongScope", name, err)
		}
	}
	if f.matcher.asked() != 0 {
		t.Error("a refused scope reached the match")
	}
}

// TestTheInvitationAndTheMembershipCommitTogether.
//
// An accepted invitation with no Membership is an intent recorded as fulfilled that granted
// nothing, and nothing downstream would report it: the invitation reads as closed and the person
// has no access.
func TestTheInvitationAndTheMembershipCommitTogether(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	identifier := "atomic@example.test"

	issued := f.issue(t, tenantID, identifier)
	_, inviteeCtx := f.invitee(t, identifier)

	injected := errors.New("failure between the state change and the grant")
	f.service.beforeGrant = func(context.Context) error { return injected }

	if _, _, err := f.service.Accept(inviteeCtx, issued.Token); !errors.Is(err, injected) {
		t.Fatalf("error = %v, want the injected failure", err)
	}

	current := f.stored(t, tenantID, issued.Invitation.InvitationID)
	if current.State != StatePending {
		t.Errorf("state = %s after a rolled-back acceptance, want pending", current.State)
	}
	if current.AcceptedAt != nil || current.PrincipalID != nil {
		t.Errorf("accepted_at %v, principal %v after a rolled-back acceptance", current.AcceptedAt, current.PrincipalID)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Error("a rolled-back acceptance left a Membership")
	}

	// The same acceptance succeeds once the injection is removed, so the rollback left the row
	// usable rather than merely unchanged.
	f.service.beforeGrant = nil
	if _, _, err := f.service.Accept(inviteeCtx, issued.Token); err != nil {
		t.Fatalf("Accept after the rollback: %v", err)
	}
	if f.activeMemberships(t, tenantID) != 1 {
		t.Error("the resumed acceptance did not create the Membership")
	}
}

// TestAcceptanceIntoASuspendedTenantIsRefused is the recheck the long-lived invitation makes
// necessary. An invitation issued while a Tenant was active may be presented after it was
// suspended, and that window is exactly what an expiry measured in days creates.
func TestAcceptanceIntoASuspendedTenantIsRefused(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	identifier := "suspended@example.test"

	issued := f.issue(t, tenantID, identifier)
	_, inviteeCtx := f.invitee(t, identifier)

	f.exec(t, `UPDATE tenant.tenant SET status = 'suspended' WHERE tenant_id = $1`, tenantID.String())

	if _, _, err := f.service.Accept(inviteeCtx, issued.Token); !errors.Is(err, ErrTenantNotActive) {
		t.Fatalf("error = %v, want ErrTenantNotActive", err)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Error("acceptance into a suspended Tenant created a Membership")
	}

	// Restoring the Tenant lets the same invitation complete: the refusal was about the Tenant's
	// state at that moment, not about the invitation.
	f.exec(t, `UPDATE tenant.tenant SET status = 'active' WHERE tenant_id = $1`, tenantID.String())
	if _, _, err := f.service.Accept(inviteeCtx, issued.Token); err != nil {
		t.Fatalf("Accept after the Tenant was restored: %v", err)
	}
}

// TestAnExpiredInvitationCannotBeAccepted, including in the race with the sweep.
//
// Expiry is checked against the clock rather than the state, so whichever of the two arrives first
// the answer is the same. Checking the state alone would let an acceptance land in the window
// between lapsing and the sweep materialising it.
func TestAnExpiredInvitationCannotBeAccepted(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	identifier := "expired@example.test"

	issued := f.issue(t, tenantID, identifier)
	_, inviteeCtx := f.invitee(t, identifier)

	// Lapse it without running the sweep: the state is still pending.
	f.exec(t, `UPDATE invitation.invitation SET expires_at = $2 WHERE invitation_id = $1`,
		issued.Invitation.InvitationID.String(), f.fixed.Add(-time.Minute))

	if before := f.stored(t, tenantID, issued.Invitation.InvitationID); before.State != StatePending {
		t.Fatalf("state = %s; the sweep should not have run yet", before.State)
	}
	if _, _, err := f.service.Accept(inviteeCtx, issued.Token); !errors.Is(err, ErrNotAcceptable) {
		t.Fatalf("error = %v, want ErrNotAcceptable", err)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Error("an expired invitation created a Membership")
	}
}

// TestTheSweepMaterialisesExpiryAndFreesTheSlot.
//
// Materialised rather than computed at read time, so an expired invitation reads as expired in
// every listing — and so the slot the partial unique index guards is released, letting the same
// person be invited again.
func TestTheSweepMaterialisesExpiryAndFreesTheSlot(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	ctx := f.scopeFor(tenantID)
	identifier := "sweep@example.test"

	issued := f.issue(t, tenantID, identifier)

	// A second invitation for the same person and context is refused while the first is
	// outstanding: two pending invitations from one intent would produce two Memberships.
	if _, err := f.service.Issue(ctx, IssueRequest{
		TargetIdentifier: identifier, SubjectType: "human", Reason: "second attempt",
	}); err == nil {
		t.Fatal("a second outstanding invitation for the same person and context was accepted")
	}

	f.exec(t, `UPDATE invitation.invitation SET expires_at = $2 WHERE invitation_id = $1`,
		issued.Invitation.InvitationID.String(), f.fixed.Add(-time.Hour))

	swept, err := f.service.ExpireLapsed(f.providerCtx, 10)
	if err != nil {
		t.Fatalf("ExpireLapsed: %v", err)
	}
	if swept < 1 {
		t.Fatalf("the sweep materialised %d invitations, want at least the lapsed one", swept)
	}

	current, err := f.service.Get(ctx, issued.Invitation.InvitationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if current.State != StateExpired {
		t.Errorf("state = %s, want expired", current.State)
	}
	if got := f.events(t, issued.Invitation.InvitationID); len(got) != 2 ||
		!strings.HasSuffix(got[0], "invitation.requested") ||
		!strings.HasSuffix(got[1], "invitation.expired") {
		t.Errorf("events = %v, want requested then expired", got)
	}

	// The slot is free, so the same person can be invited again.
	if _, err := f.service.Issue(ctx, IssueRequest{
		TargetIdentifier: identifier, SubjectType: "human", Reason: "reissued after expiry",
	}); err != nil {
		t.Fatalf("reissuing after expiry was refused: %v", err)
	}

	// The sweep is idempotent: a second run finds nothing already-expired to expire again.
	if again, err := f.service.ExpireLapsed(f.providerCtx, 10); err != nil {
		t.Fatalf("second ExpireLapsed: %v", err)
	} else if again != 0 {
		t.Errorf("a second sweep changed %d invitations, want 0", again)
	}
}

// TestRevokeWithdrawsTheIntentAndIsTerminal.
func TestRevokeWithdrawsTheIntentAndIsTerminal(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	ctx := f.scopeFor(tenantID)
	identifier := "revoked@example.test"

	issued := f.issue(t, tenantID, identifier)
	revoked, err := f.service.Revoke(ctx, issued.Invitation.InvitationID)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked.State != StateRevoked {
		t.Errorf("state = %s, want revoked", revoked.State)
	}
	if revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(f.fixed) {
		t.Errorf("revoked_at = %v, want %s", revoked.RevokedAt, f.fixed)
	}

	// Acceptance refuses it as it refuses every failed lookup (ADR-ORG-007 §5.4), even for the
	// person holding the address.
	_, inviteeCtx := f.invitee(t, identifier)
	if _, _, err := f.service.Accept(inviteeCtx, issued.Token); !errors.Is(err, ErrNotAcceptable) {
		t.Errorf("accepting a revoked invitation returned %v, want ErrNotAcceptable", err)
	}
	if _, err := f.service.Revoke(ctx, issued.Invitation.InvitationID); !errors.Is(err, ErrSettled) {
		t.Errorf("revoking twice returned %v, want ErrSettled", err)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Error("a revoked invitation produced a Membership")
	}
}

// TestTheAnonymousLookupDisclosesNothingAndReadsNothing.
//
// Every outcome renders identically, so no part of the answer derives from the row. That is what
// keeps an unauthenticated endpoint off the provider-scoped pool: the table is Row-Level Security
// protected, and an anonymous caller can bind neither a Tenant nor an actor.
func TestTheAnonymousLookupDisclosesNothingAndReadsNothing(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	ctx := f.scopeFor(tenantID)

	issued := f.issue(t, tenantID, "lookup@example.test")
	revoked := f.issue(t, tenantID, "lookup-revoked@example.test")
	if _, err := f.service.Revoke(ctx, revoked.Invitation.InvitationID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	neverIssued, _, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	// Valid, revoked, and never-issued all answer identically.
	for name, token := range map[string]Token{
		"valid":        issued.Token,
		"revoked":      revoked.Token,
		"never issued": neverIssued,
	} {
		if !f.service.Lookup(token) {
			t.Errorf("%s: the lookup distinguished this token from the others", name)
		}
	}

	// Only shape is rejected, which is what lets a malformed request be refused as malformed.
	for name, token := range map[string]Token{
		"empty":      "",
		"too short":  "abc",
		"not base64": Token(strings.Repeat("!", 43)),
	} {
		if f.service.Lookup(token) {
			t.Errorf("%s was accepted as well formed", name)
		}
	}

	// No provider access is required to answer, because nothing is read.
	if f.service.Lookup(issued.Token) != f.service.Lookup(neverIssued) {
		t.Error("the lookup is not uniform")
	}
}

// TestTheTokenIsNeverStored. A token that can be read back out of storage is a token every
// operator with read access holds.
func TestTheTokenIsNeverStored(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	issued := f.issue(t, tenantID, "token@example.test")

	// Read on the owner connection: from ADR-ORG-007 the provider role holds nothing on the table.
	var stored string
	if err := f.setup.InTx(f.providerCtx,
		func(ctx context.Context, tx fdb.Tx) error {
			return tx.QueryRow(ctx, `SELECT token_hash FROM invitation.invitation
			    WHERE invitation_id = $1`, issued.Invitation.InvitationID.String()).Scan(&stored)
		}); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}

	if stored == string(issued.Token) {
		t.Fatal("the token itself is in the database")
	}
	if stored != issued.Token.Hash() {
		t.Errorf("stored hash does not match the token's hash")
	}
	// And the whole row carries the token nowhere else.
	var row string
	if err := f.setup.InTx(f.providerCtx,
		func(ctx context.Context, tx fdb.Tx) error {
			return tx.QueryRow(ctx, `SELECT to_jsonb(i)::text FROM invitation.invitation i
			    WHERE invitation_id = $1`, issued.Invitation.InvitationID.String()).Scan(&row)
		}); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if strings.Contains(row, string(issued.Token)) {
		t.Error("the token appears somewhere in the stored row")
	}
}

// TestAnInvitationInAnotherTenantIsNotFound.
func TestAnInvitationInAnotherTenantIsNotFound(t *testing.T) {
	f := newFixture(t)
	owner := f.seedTenant(t, "active")
	other := f.seedTenant(t, "active")

	issued := f.issue(t, owner, "crosstenant@example.test")
	otherCtx := f.scopeFor(other)

	if _, err := f.service.Get(otherCtx, issued.Invitation.InvitationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get from another Tenant returned %v, want ErrNotFound", err)
	}
	if _, err := f.service.Revoke(otherCtx, issued.Invitation.InvitationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Revoke from another Tenant returned %v, want ErrNotFound", err)
	}
}

// TestAcceptanceIsRefusedWhenTheSubjectAlreadyHoldsMembership. The partial unique index would
// refuse the insert; refusing here names the reason instead of surfacing a constraint violation.
func TestAcceptanceIsRefusedWhenTheSubjectAlreadyHoldsMembership(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	identifier := "duplicate@example.test"

	issued := f.issue(t, tenantID, identifier)
	principalID, ctx := f.invitee(t, identifier)

	// The same Principal already holds an active Membership in this context.
	f.exec(t, `INSERT INTO membership.membership
	    (membership_id, principal_id, tenant_id, subject_type, status, membership_version, valid_from, provenance)
	    VALUES ($1, $2, $3, 'human', 'active', 1, now(), 'seeded')`,
		mustID(t).String(), principalID.String(), tenantID.String())

	if _, _, err := f.service.Accept(ctx, issued.Token); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("error = %v, want ErrAlreadyMember", err)
	}
	if f.activeMemberships(t, tenantID) != 1 {
		t.Errorf("active Memberships = %d, want the one that already existed", f.activeMemberships(t, tenantID))
	}
}

// TestIssueBoundsTheLifetime. An invitation without an effective bound is a standing grant nobody
// revokes, and a request above the ceiling is refused rather than clamped — an inviter who asked
// for a year and silently got a month would believe it is live long after it lapsed.
func TestIssueBoundsTheLifetime(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	ctx := f.scopeFor(tenantID)

	issued := f.issue(t, tenantID, "ttl-default@example.test")
	if want := f.fixed.Add(DefaultTTL); !issued.Invitation.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %s, want the default %s", issued.Invitation.ExpiresAt, want)
	}

	if _, err := f.service.Issue(ctx, IssueRequest{
		TargetIdentifier: "ttl-over@example.test", SubjectType: "human", TTL: MaxTTL + time.Hour,
	}); !errors.Is(err, ErrTTL) {
		t.Errorf("a lifetime above the ceiling returned %v, want ErrTTL", err)
	}

	bounded, err := f.service.Issue(ctx, IssueRequest{
		TargetIdentifier: "ttl-ok@example.test", SubjectType: "human", TTL: MaxTTL,
	})
	if err != nil {
		t.Fatalf("a lifetime at the ceiling was refused: %v", err)
	}
	if want := f.fixed.Add(MaxTTL); !bounded.Invitation.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %s, want %s", bounded.Invitation.ExpiresAt, want)
	}

	for name, req := range map[string]IssueRequest{
		"no identifier":   {SubjectType: "human"},
		"unknown subject": {TargetIdentifier: "x@example.test", SubjectType: "service"},
	} {
		if _, err := f.service.Issue(ctx, req); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestAnIdentifierDifferingOnlyInCaseIsTheSamePerson. Two outstanding invitations for one person in
// one context is what the partial unique index refuses, and case is not a difference in person.
func TestAnIdentifierDifferingOnlyInCaseIsTheSamePerson(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	ctx := f.scopeFor(tenantID)

	f.issue(t, tenantID, "Case@Example.test")
	if _, err := f.service.Issue(ctx, IssueRequest{
		TargetIdentifier: "case@example.TEST", SubjectType: "human",
	}); err == nil {
		t.Fatal("the same identifier in different case was accepted as a second invitation")
	}

	if HashIdentifier("  Case@Example.test ") != HashIdentifier("case@example.test") {
		t.Error("the correlation hash is sensitive to case or surrounding space")
	}
}

// TestTheMachineIsWalkedWholeAndRefusesEverythingElse.
func TestTheMachineIsWalkedWholeAndRefusesEverythingElse(t *testing.T) {
	states := []State{StatePending, StateAccepted, StateExpired, StateRevoked}
	permitted := map[string]State{
		"accept|pending": StateAccepted,
		"revoke|pending": StateRevoked,
		"expire|pending": StateExpired,
	}

	covered := map[string]bool{}
	for _, action := range Actions() {
		for _, from := range states {
			key := string(action) + "|" + string(from)
			next, err := Resolve(action, from)
			want, allowed := permitted[key]
			if allowed {
				covered[key] = true
				if err != nil {
					t.Errorf("%s from %s was refused: %v", action, from, err)
				}
				if next != want {
					t.Errorf("%s from %s went to %s, want %s", action, from, next, want)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s from %s was permitted and went to %s", action, from, next)
			}
		}
	}
	for key := range permitted {
		if !covered[key] {
			t.Errorf("%q is expected to be permitted and is not in the machine", key)
		}
	}

	// Every terminal state says settled rather than refused.
	for _, terminal := range []State{StateAccepted, StateExpired, StateRevoked} {
		for _, action := range Actions() {
			if _, err := Resolve(action, terminal); !errors.Is(err, ErrSettled) {
				t.Errorf("%s from %s returned %v, want ErrSettled", action, terminal, err)
			}
		}
		if terminal.Outstanding() {
			t.Errorf("%s reports as outstanding", terminal)
		}
	}
	for _, state := range states {
		if !state.Valid() {
			t.Errorf("%s is in the machine and Valid() rejects it", state)
		}
	}
	if _, err := Resolve(Action("resend"), StatePending); !errors.Is(err, ErrUnknownAction) {
		t.Errorf("an action outside the machine returned %v", err)
	}
	// The half-join is gone (ADR-ORG-007 §5.4): neither the state nor its action is in the machine.
	if State("identity_verified").Valid() {
		t.Error("identity_verified is still a valid state")
	}
	if _, err := Resolve(Action("verify-identity"), StatePending); !errors.Is(err, ErrUnknownAction) {
		t.Errorf("verify-identity returned %v, want ErrUnknownAction", err)
	}
}

// TestEveryActionEitherPublishesOrIsDeclaredSilent. Since `verify-identity` was removed, the one
// silent action, every action publishes.
func TestEveryActionEitherPublishesOrIsDeclaredSilent(t *testing.T) {
	silent := map[Action]bool{}

	for _, action := range Actions() {
		eventType, publishes, err := EventType(action)
		if err != nil {
			t.Errorf("EventType(%s): %v", action, err)
			continue
		}
		if publishes == silent[action] {
			t.Errorf("%s publishes = %v and is declared silent = %v; exactly one must hold",
				action, publishes, silent[action])
		}
		if publishes && !strings.Contains(string(eventType), ".invitation.") {
			t.Errorf("%s publishes %q, which is not classified as an invitation event", action, eventType)
		}
	}
	if _, _, err := EventType(Action("resend")); !errors.Is(err, ErrUnknownAction) {
		t.Errorf("EventType for an unknown action returned %v", err)
	}
}

// TestThePayloadCarriesNoIdentifier. STD-GLB-007 makes the target identifier Tier-2 PII, and an
// event stream is read by consumers with no business knowing who was invited.
func TestThePayloadCarriesNoIdentifier(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	identifier := "pii@example.test"
	issued := f.issue(t, tenantID, identifier)

	var envelope string
	if err := db.WithProviderScope(f.providerCtx, f.provider, "invitation suite read",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT envelope::text FROM platform.outbox
			    WHERE aggregate_id = $1`, issued.Invitation.InvitationID.String()).Scan(&envelope)
		}); err != nil {
		t.Fatalf("read envelope: %v", err)
	}

	for _, leaked := range []string{identifier, "pii@", issued.Invitation.TargetHash} {
		if strings.Contains(envelope, leaked) {
			t.Errorf("the envelope carries %q", leaked)
		}
	}
	if !strings.Contains(envelope, issued.Invitation.CorrelationID.String()) {
		t.Error("the envelope omits the correlation a consumer needs")
	}
}

// TestTheListIsAKeysetPageOfTheBoundTenant is STD-GLB-001 1.3.0 §Pagination over invitations: key
// order, `next` on a full page and null on the last, a state filter, and Row-Level Security
// confining the list to the caller's Tenant.
func TestTheListIsAKeysetPageOfTheBoundTenant(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	otherTenant := f.seedTenant(t, "active")
	ctx := f.scopeFor(tenantID)

	first := f.issue(t, tenantID, "first@list.example")
	second := f.issue(t, tenantID, "second@list.example")
	third := f.issue(t, tenantID, "third@list.example")
	f.issue(t, otherTenant, "elsewhere@list.example")
	if _, err := f.service.Revoke(ctx, second.Invitation.InvitationID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	page, err := f.service.List(ctx, ListQuery{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Invitations) != 2 || page.Invitations[0].InvitationID != first.Invitation.InvitationID ||
		page.Invitations[1].InvitationID != second.Invitation.InvitationID {
		t.Fatalf("first page is not the first two invitations in creation order: %+v", page.Invitations)
	}
	if page.Next == nil || *page.Next != second.Invitation.InvitationID {
		t.Fatalf("next = %v, want the last item of the page", page.Next)
	}

	last, err := f.service.List(ctx, ListQuery{Limit: 2, After: *page.Next})
	if err != nil {
		t.Fatalf("List after: %v", err)
	}
	if len(last.Invitations) != 1 || last.Invitations[0].InvitationID != third.Invitation.InvitationID || last.Next != nil {
		t.Fatalf("last page = %+v, next %v; want the third invitation and no cursor", last.Invitations, last.Next)
	}

	revoked, err := f.service.List(ctx, ListQuery{State: StateRevoked})
	if err != nil {
		t.Fatalf("List revoked: %v", err)
	}
	if len(revoked.Invitations) != 1 || revoked.Invitations[0].InvitationID != second.Invitation.InvitationID {
		t.Errorf("the revoked filter returned %+v", revoked.Invitations)
	}

	if _, err := f.service.List(ctx, ListQuery{State: "sent"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown state: error = %v, want ErrInvalid", err)
	}
	if _, err := f.service.List(ctx, ListQuery{Limit: -1}); !errors.Is(err, ErrInvalid) {
		t.Errorf("limit -1: error = %v, want ErrInvalid", err)
	}
}

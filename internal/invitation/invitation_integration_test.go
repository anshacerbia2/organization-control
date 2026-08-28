package invitation

// The invitation half of Week 4, against a real engine.
//
// The property under test is SAD-004 §5.5: Membership activates on the join of two independent
// facts, and neither alone activates anything. Most of what follows is that sentence, taken apart.
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

type fixture struct {
	service     *Service
	provider    *db.ProviderPool
	providerCtx context.Context
	scopeFor    func(id.UUID) context.Context
	fixed       time.Time
	actor       id.UUID
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
	service, err := New(tenantPool, provider, memberships)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fixed, err := time.Parse(time.RFC3339, fixedNowText)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	service.now = func() time.Time { return fixed }

	actor, correlation := mustID(t), mustID(t)
	providerScope, err := db.ProviderScope(actor, correlation)
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}

	return &fixture{
		service: service, provider: provider,
		providerCtx: db.WithScope(ctx, providerScope),
		scopeFor: func(tenantID id.UUID) context.Context {
			scope, err := db.TenantScope(tenantID, actor, mustID(t))
			if err != nil {
				t.Fatalf("TenantScope: %v", err)
			}
			return db.WithScope(ctx, scope)
		},
		fixed: fixed, actor: actor,
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
	if err := db.WithProviderScope(f.providerCtx, f.provider, "invitation suite fixture",
		func(ctx context.Context, tx db.Tx) error {
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
		f.exec(t, `DELETE FROM membership.membership WHERE tenant_id = $1`, tenantID.String())
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

// verify delivers the identity fact for an invitation.
func (f *fixture) verify(t *testing.T, issued Issued, identifier string) (Invitation, id.UUID) {
	t.Helper()
	principalID := mustID(t)
	record, err := f.service.RecordVerifiedIdentity(f.providerCtx, VerifiedIdentity{
		CorrelationID: issued.Invitation.CorrelationID,
		Identifier:    identifier,
		PrincipalID:   principalID,
	})
	if err != nil {
		t.Fatalf("RecordVerifiedIdentity: %v", err)
	}
	return record, principalID
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

// TestNeitherFactAloneCreatesMembership is SAD-004 §5.5.
//
// An invitation that granted Membership on token presentation would have authenticated an email
// inbox, and an inbox is not a Principal. An identity verification that granted Membership without
// an invitation would admit anyone the kernel happened to verify.
func TestNeitherFactAloneCreatesMembership(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	ctx := f.scopeFor(tenantID)
	identifier := "invitee@example.test"

	issued := f.issue(t, tenantID, identifier)
	if issued.Invitation.State != StatePending {
		t.Fatalf("state = %s, want pending", issued.Invitation.State)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Fatal("issuing an invitation created a Membership")
	}

	// Fact one alone: a valid token, presented before any identity was verified.
	if _, _, err := f.service.Accept(ctx, issued.Token); !errors.Is(err, ErrTransitionRefused) {
		t.Fatalf("accepting a pending invitation returned %v, want ErrTransitionRefused", err)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Fatal("token possession alone created a Membership")
	}

	// Fact two alone: a verification whose identifier is not the invited one.
	if _, err := f.service.RecordVerifiedIdentity(f.providerCtx, VerifiedIdentity{
		CorrelationID: issued.Invitation.CorrelationID,
		Identifier:    "someone-else@example.test",
		PrincipalID:   mustID(t),
	}); !errors.Is(err, ErrTransitionRefused) {
		t.Fatalf("a verification for another identifier returned %v, want ErrTransitionRefused", err)
	}

	// A verification with no invitation behind it correlates to nothing.
	if _, err := f.service.RecordVerifiedIdentity(f.providerCtx, VerifiedIdentity{
		CorrelationID: mustID(t), Identifier: identifier, PrincipalID: mustID(t),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an uncorrelated verification returned %v, want ErrNotFound", err)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Fatal("a verification alone created a Membership")
	}

	// Both facts: now it works.
	verified, principalID := f.verify(t, issued, identifier)
	if verified.State != StateIdentityVerified {
		t.Fatalf("state = %s, want identity_verified", verified.State)
	}
	if verified.PrincipalID == nil || *verified.PrincipalID != principalID {
		t.Fatalf("principal = %v, want %s", verified.PrincipalID, principalID)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Fatal("verification created a Membership before acceptance")
	}

	accepted, granted, err := f.service.Accept(ctx, issued.Token)
	if err != nil {
		t.Fatalf("Accept with both facts: %v", err)
	}
	if accepted.State != StateAccepted {
		t.Errorf("state = %s, want accepted", accepted.State)
	}
	if accepted.AcceptedAt == nil || !accepted.AcceptedAt.Equal(f.fixed) {
		t.Errorf("accepted_at = %v, want %s", accepted.AcceptedAt, f.fixed)
	}
	if granted.Membership.PrincipalID != principalID {
		t.Errorf("Membership principal = %s, want %s", granted.Membership.PrincipalID, principalID)
	}
	if f.activeMemberships(t, tenantID) != 1 {
		t.Errorf("active Memberships = %d, want 1", f.activeMemberships(t, tenantID))
	}
	// The provenance answers the first question an access review asks.
	if !strings.Contains(granted.Membership.Provenance, issued.Invitation.InvitationID.String()) {
		t.Errorf("provenance = %q, want it to name the invitation", granted.Membership.Provenance)
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
	ctx := f.scopeFor(tenantID)
	identifier := "atomic@example.test"

	issued := f.issue(t, tenantID, identifier)
	f.verify(t, issued, identifier)

	injected := errors.New("failure between the state change and the grant")
	f.service.beforeGrant = func(context.Context) error { return injected }

	if _, _, err := f.service.Accept(ctx, issued.Token); !errors.Is(err, injected) {
		t.Fatalf("error = %v, want the injected failure", err)
	}

	current, err := f.service.Get(ctx, issued.Invitation.InvitationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if current.State != StateIdentityVerified {
		t.Errorf("state = %s after a rolled-back acceptance, want identity_verified", current.State)
	}
	if current.AcceptedAt != nil {
		t.Errorf("accepted_at = %v after a rolled-back acceptance", current.AcceptedAt)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Error("a rolled-back acceptance left a Membership")
	}

	// The same acceptance succeeds once the injection is removed, so the rollback left the row
	// usable rather than merely unchanged.
	f.service.beforeGrant = nil
	if _, _, err := f.service.Accept(ctx, issued.Token); err != nil {
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
	ctx := f.scopeFor(tenantID)
	identifier := "suspended@example.test"

	issued := f.issue(t, tenantID, identifier)
	f.verify(t, issued, identifier)

	f.exec(t, `UPDATE tenant.tenant SET status = 'suspended' WHERE tenant_id = $1`, tenantID.String())

	if _, _, err := f.service.Accept(ctx, issued.Token); !errors.Is(err, ErrTenantNotActive) {
		t.Fatalf("error = %v, want ErrTenantNotActive", err)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Error("acceptance into a suspended Tenant created a Membership")
	}

	// Restoring the Tenant lets the same invitation complete: the refusal was about the Tenant's
	// state at that moment, not about the invitation.
	f.exec(t, `UPDATE tenant.tenant SET status = 'active' WHERE tenant_id = $1`, tenantID.String())
	if _, _, err := f.service.Accept(ctx, issued.Token); err != nil {
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
	ctx := f.scopeFor(tenantID)
	identifier := "expired@example.test"

	issued := f.issue(t, tenantID, identifier)
	f.verify(t, issued, identifier)

	// Lapse it without running the sweep: the state is still identity_verified.
	f.exec(t, `UPDATE invitation.invitation SET expires_at = $2 WHERE invitation_id = $1`,
		issued.Invitation.InvitationID.String(), f.fixed.Add(-time.Minute))

	before, err := f.service.Get(ctx, issued.Invitation.InvitationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if before.State != StateIdentityVerified {
		t.Fatalf("state = %s; the sweep should not have run yet", before.State)
	}
	if _, _, err := f.service.Accept(ctx, issued.Token); !errors.Is(err, ErrExpired) {
		t.Fatalf("error = %v, want ErrExpired", err)
	}
	if f.activeMemberships(t, tenantID) != 0 {
		t.Error("an expired invitation created a Membership")
	}

	// And a verification arriving after expiry is refused for the same reason.
	second := f.issue(t, tenantID, "expired2@example.test")
	f.exec(t, `UPDATE invitation.invitation SET expires_at = $2 WHERE invitation_id = $1`,
		second.Invitation.InvitationID.String(), f.fixed.Add(-time.Minute))
	if _, err := f.service.RecordVerifiedIdentity(f.providerCtx, VerifiedIdentity{
		CorrelationID: second.Invitation.CorrelationID,
		Identifier:    "expired2@example.test",
		PrincipalID:   mustID(t),
	}); !errors.Is(err, ErrExpired) {
		t.Fatalf("verification after expiry returned %v, want ErrExpired", err)
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

	// Terminal, and it says so rather than reporting a refused transition: a client retrying
	// acceptance on a revoked invitation has a different problem from one retrying too early.
	if _, _, err := f.service.Accept(ctx, issued.Token); !errors.Is(err, ErrSettled) {
		t.Errorf("accepting a revoked invitation returned %v, want ErrSettled", err)
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

	var stored string
	if err := db.WithProviderScope(f.providerCtx, f.provider, "invitation suite read",
		func(ctx context.Context, tx db.Tx) error {
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
	if err := db.WithProviderScope(f.providerCtx, f.provider, "invitation suite read",
		func(ctx context.Context, tx db.Tx) error {
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
	// Even holding the token, which is the point: the token is not authority.
	if _, _, err := f.service.Accept(otherCtx, issued.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Accept from another Tenant returned %v, want ErrNotFound", err)
	}
}

// TestAcceptanceIsRefusedWhenTheSubjectAlreadyHoldsMembership. The partial unique index would
// refuse the insert; refusing here names the reason instead of surfacing a constraint violation.
func TestAcceptanceIsRefusedWhenTheSubjectAlreadyHoldsMembership(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	ctx := f.scopeFor(tenantID)
	identifier := "duplicate@example.test"

	issued := f.issue(t, tenantID, identifier)
	_, principalID := f.verify(t, issued, identifier)

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
	states := []State{StatePending, StateIdentityVerified, StateAccepted, StateExpired, StateRevoked}
	permitted := map[string]State{
		"verify-identity|pending":  StateIdentityVerified,
		"accept|identity_verified": StateAccepted,
		"revoke|pending":           StateRevoked,
		"revoke|identity_verified": StateRevoked,
		"expire|pending":           StateExpired,
		"expire|identity_verified": StateExpired,
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
}

// TestEveryActionEitherPublishesOrIsDeclaredSilent. `verify-identity` records that one of two
// facts arrived, and no consumer outside this service can act on half a join.
func TestEveryActionEitherPublishesOrIsDeclaredSilent(t *testing.T) {
	silent := map[Action]bool{ActionVerifyIdentity: true}

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

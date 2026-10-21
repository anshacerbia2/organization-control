package invitation

// identity-control's resolve of an invitation token, against a real engine (ADR-IAM-011 §5.7;
// TDD-organization-control-004 1.18.0). The property under test is §5.7 (b) and (c): the database
// returns the one pending, unexpired invitation a token names or nothing, every other outcome is one
// refusal, and every call writes one access record, the same whatever it finds.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// capturingRecorder keeps every access record the resolver writes.
type capturingRecorder struct {
	mu      sync.Mutex
	records []db.ProviderAccess
}

func (c *capturingRecorder) RecordProviderAccess(_ context.Context, access db.ProviderAccess) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, access)
	return nil
}

func (c *capturingRecorder) taken() []db.ProviderAccess {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]db.ProviderAccess(nil), c.records...)
}

// newResolver opens the tenant login, as production hands the resolver pool, with a recorder that
// keeps what it is given.
func newResolver(t *testing.T) (*Resolver, *capturingRecorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	runtime, err := fdb.Open(ctx, fdb.Config{
		Name:     "invitation-resolver",
		DSN:      fmt.Sprintf("postgres://organization_app:%s@%s", os.Getenv("TEST_RUNTIME_PASSWORD"), hostFrom(t)),
		MaxConns: 2,
	})
	if err != nil {
		t.Fatalf("open runtime pool: %v", err)
	}
	t.Cleanup(runtime.Close)
	recorder := &capturingRecorder{}
	pool, err := db.NewInvitationResolverPool(runtime, recorder)
	if err != nil {
		t.Fatalf("NewInvitationResolverPool: %v", err)
	}
	resolver, err := NewResolver(pool)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return resolver, recorder
}

func resolverCtx(t *testing.T, workload id.UUID) context.Context {
	t.Helper()
	scope, err := db.InvitationResolverScope(workload, mustID(t))
	if err != nil {
		t.Fatalf("InvitationResolverScope: %v", err)
	}
	ctx := db.WithAccessRoute(context.Background(), "POST /v1/invitations/resolve", id.UUID{})
	return db.WithScope(ctx, scope)
}

func TestTheResolveAnswersOnlyAPendingUnexpiredInvitation(t *testing.T) {
	f := newFixture(t)
	resolver, recorder := newResolver(t)
	tenantID := f.seedTenant(t, "active")
	ctx := f.scopeFor(tenantID)

	// The fixture's service clock is fixed in the past, and the policy reads the database's clock, so
	// each expiry is set against the database's now.
	expiresIn := func(issued Issued, interval string) {
		f.exec(t, `UPDATE invitation.invitation SET expires_at = now() + $2::interval WHERE invitation_id = $1`,
			issued.Invitation.InvitationID.String(), interval)
	}
	pending := f.issue(t, tenantID, "Newcomer@Example.test")
	expiresIn(pending, "1 day")
	expired := f.issue(t, tenantID, "lapsed@example.test")
	expiresIn(expired, "-1 minute")
	revoked := f.issue(t, tenantID, "withdrawn@example.test")
	expiresIn(revoked, "1 day")
	if _, err := f.service.Revoke(ctx, revoked.Invitation.InvitationID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	accepted := f.issue(t, tenantID, "done@example.test")
	_, doneCtx := f.invitee(t, "done@example.test")
	if _, _, err := f.service.Accept(doneCtx, accepted.Token); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	f.exec(t, `UPDATE invitation.invitation SET expires_at = now() + interval '1 day' WHERE invitation_id = $1`,
		accepted.Invitation.InvitationID.String())
	unknown, _, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	workload := mustID(t)
	resolved, err := resolver.Resolve(resolverCtx(t, workload), pending.Token)
	if err != nil {
		t.Fatalf("Resolve of a pending invitation: %v", err)
	}
	if resolved.InvitationID != pending.Invitation.InvitationID || resolved.TargetIdentifier != "Newcomer@Example.test" {
		t.Errorf("resolved %+v, want the invitation's identifier and its address as stored", resolved)
	}

	for name, token := range map[string]Token{
		"unknown": unknown, "expired": expired.Token, "revoked": revoked.Token, "accepted": accepted.Token,
	} {
		got, err := resolver.Resolve(resolverCtx(t, workload), token)
		if !errors.Is(err, ErrNotAcceptable) || err.Error() != ErrNotAcceptable.Error() || got != (Resolved{}) {
			t.Errorf("%s resolved %+v, %v; want the one refusal and nothing", name, got, err)
		}
	}

	// One record per call, all alike: the registered workload, the reporter authority, the purpose,
	// the route, and no Tenant, whether the token named an invitation or nothing.
	records := recorder.taken()
	if len(records) != 5 {
		t.Fatalf("recorded %d accesses for 5 resolves", len(records))
	}
	for _, record := range records {
		if record.Actor != workload || record.Authority != db.AuthorityReporter || record.Reason != "invitation:resolve" ||
			!record.Tenant.IsNil() || !record.Activation.IsNil() || record.Operation != "POST /v1/invitations/resolve" {
			t.Errorf("recorded %+v", record)
		}
	}

	// The resolve changed nothing.
	if got := f.stored(t, tenantID, pending.Invitation.InvitationID); got.State != StatePending {
		t.Errorf("the resolved invitation is %s, want pending", got.State)
	}
}

func TestTheResolveRefusesAMalformedTokenBeforeAnyRead(t *testing.T) {
	resolver, recorder := newResolver(t)
	if _, err := resolver.Resolve(resolverCtx(t, mustID(t)), Token("not a token")); !errors.Is(err, ErrToken) {
		t.Errorf("a malformed token returned %v, want ErrToken", err)
	}
	if n := len(recorder.taken()); n != 0 {
		t.Errorf("a malformed token wrote %d access records", n)
	}
}

func TestOnlyTheResolverScopeResolves(t *testing.T) {
	resolver, recorder := newResolver(t)
	token, _, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	self, _ := db.SelfScope(mustID(t), mustID(t))
	provider, _ := db.ProviderScope(mustID(t), mustID(t), db.ReporterAuthority())
	consumer, _ := db.ConsumerScope(mustID(t), mustID(t))
	tenant, _ := db.TenantScope(mustID(t), mustID(t), mustID(t))
	for name, scope := range map[string]db.Scope{"self": self, "provider": provider, "consumer": consumer, "tenant": tenant} {
		if _, err := resolver.Resolve(db.WithScope(context.Background(), scope), token); !errors.Is(err, db.ErrWrongScope) {
			t.Errorf("a %s scope resolved: %v", name, err)
		}
	}
	if n := len(recorder.taken()); n != 0 {
		t.Errorf("a refused scope wrote %d access records", n)
	}
}

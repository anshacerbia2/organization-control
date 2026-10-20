package membership

// A provider suspends or revokes one Membership inside the Tenant it names (ADR-ORG-003 §5.6, decided
// 2026-10-20), as the tenant runtime role its act runs as.

import (
	"errors"
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"
)

func TestAProviderSuspendsAndRevokesOneMembershipInsideItsTenant(t *testing.T) {
	service, ctx, pool := newFixture(t)
	granted := grantOne(t, service, ctx)
	membershipID := granted.Membership.MembershipID
	sink, providerCtx := providerIn(t, service, ctx, pool)
	tenant, err := id.Parse(tenantA)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	other, err := id.Parse(tenantB)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Inside another Tenant the Membership is not found, and nothing moves.
	if _, err := service.ProviderSuspend(providerCtx, other,
		Command{MembershipID: membershipID, ExpectedVersion: granted.Membership.Version, Reason: "INC-2"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a suspension inside another Tenant answered %v, want ErrNotFound", err)
	}
	// A stale version is refused.
	if _, err := service.ProviderSuspend(providerCtx, tenant,
		Command{MembershipID: membershipID, ExpectedVersion: granted.Membership.Version + 5, Reason: "INC-2"}); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("a stale suspension answered %v, want ErrVersionMismatch", err)
	}
	// A reason is required.
	if _, err := service.ProviderSuspend(providerCtx, tenant,
		Command{MembershipID: membershipID, ExpectedVersion: granted.Membership.Version}); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("a suspension without a reason answered %v, want ErrReasonRequired", err)
	}

	sink.accesses = nil
	suspended, err := service.ProviderSuspend(providerCtx, tenant,
		Command{MembershipID: membershipID, ExpectedVersion: granted.Membership.Version, Reason: "INC-2 a compromised member"})
	if err != nil || suspended.Membership.Status != StateSuspended || suspended.EventID.IsNil() {
		t.Fatalf("ProviderSuspend answered %+v, %v", suspended, err)
	}
	if len(sink.accesses) != 1 || sink.accesses[0].Tenant != tenant || sink.accesses[0].Reason != "INC-2 a compromised member" {
		t.Errorf("the act recorded %+v; want one access naming the Tenant, with the reason", sink.accesses)
	}
	if status, _ := statusAndVersion(t, service, ctx, membershipID); status != StateSuspended {
		t.Errorf("the Membership is %s, want suspended", status)
	}

	revoked, err := service.ProviderRevoke(providerCtx, tenant,
		Command{MembershipID: membershipID, ExpectedVersion: suspended.Membership.Version, Reason: "INC-2 confirmed"})
	if err != nil || revoked.Membership.Status != StateRevoked {
		t.Fatalf("ProviderRevoke answered %+v, %v", revoked, err)
	}
	if count := outboxCount(t, service, ctx, membershipID); count != 3 {
		t.Errorf("the outbox holds %d events for the Membership, want grant, suspension and revocation", count)
	}
}

// TestAProviderNeverRestoresOrGrants: the containment is the task, and giving access back stays the
// Tenant administrator's act (ADR-ORG-003 §5.6).
func TestAProviderNeverRestoresOrGrants(t *testing.T) {
	service, ctx, pool := newFixture(t)
	_, providerCtx := providerIn(t, service, ctx, pool)
	tenant, err := id.Parse(tenantA)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, action := range []Action{ActionRestore, ActionGrant} {
		if _, err := service.providerTransition(providerCtx, tenant, action,
			Command{MembershipID: tenant, ExpectedVersion: 1, Reason: "no"}); !errors.Is(err, ErrInvalid) {
			t.Errorf("a provider %s answered %v, want ErrInvalid", action, err)
		}
	}
	// A Tenant administrator's scope is no provider's.
	if _, err := service.ProviderSuspend(ctx, tenant,
		Command{MembershipID: tenant, ExpectedVersion: 1, Reason: "no"}); err == nil {
		t.Error("a Tenant scope suspended through the provider's route")
	}
}

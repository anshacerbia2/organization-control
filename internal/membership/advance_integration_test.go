package membership

// A provider inside one Tenant: the enforcement read and the version advance after a restore to an
// older point (TDD-organization-control-002 1.15.0), as the tenant runtime role the provider's act
// runs as.

import (
	"context"
	"errors"
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

type accessSink struct{ accesses []db.ProviderAccess }

func (s *accessSink) RecordProviderAccess(_ context.Context, access db.ProviderAccess) error {
	s.accesses = append(s.accesses, access)
	return nil
}

// providerIn equips the service with a provider pool on the same connections, its access records
// captured, and returns a provider's context.
func providerIn(t *testing.T, service *Service, ctx context.Context, conns db.Transactor) (*accessSink, context.Context) {
	t.Helper()
	sink := &accessSink{}
	provider, err := db.NewProviderPool(conns, sink)
	if err != nil {
		t.Fatalf("NewProviderPool: %v", err)
	}
	service.provider = provider
	actor, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	scope, err := db.ProviderScope(actor, actor, db.EmergencyAuthority())
	if err != nil {
		t.Fatalf("ProviderScope: %v", err)
	}
	return sink, db.WithScope(ctx, scope)
}

func TestAProviderReadsEnforcementInsideTheTenantItNames(t *testing.T) {
	service, ctx, pool := newFixture(t)
	granted := grantOne(t, service, ctx)
	sink, providerCtx := providerIn(t, service, ctx, pool)
	tenant, err := id.Parse(tenantA)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	report, err := service.EnforcementInTenant(providerCtx, tenant, granted.Membership.MembershipID, "INC-1 over budget")
	if err != nil {
		t.Fatalf("EnforcementInTenant: %v", err)
	}
	if report.EventID != granted.EventID {
		t.Errorf("the provider read event %s, the grant published %s", report.EventID, granted.EventID)
	}
	if len(sink.accesses) != 1 || sink.accesses[0].Reason != "INC-1 over budget" {
		t.Errorf("the read recorded %+v, want one access with the reason", sink.accesses)
	}

	other, err := id.Parse(tenantB)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := service.EnforcementInTenant(providerCtx, other, granted.Membership.MembershipID, "INC-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a Membership read inside another Tenant answered %v, want ErrNotFound", err)
	}
}

// A Membership a consumer holds at a higher version moves past it, keeps its state, and publishes it;
// one already past is left alone, so the same report twice changes nothing the second time.
func TestAnAdvanceMovesAMembershipPastTheConsumersVersion(t *testing.T) {
	service, ctx, pool := newFixture(t)
	granted := grantOne(t, service, ctx)
	if _, err := service.Suspend(ctx, at(t, service, ctx, granted.Membership.MembershipID)); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	_, before := statusAndVersion(t, service, ctx, granted.Membership.MembershipID)
	events := outboxCount(t, service, ctx, granted.Membership.MembershipID)
	_, providerCtx := providerIn(t, service, ctx, pool)
	tenant, err := id.Parse(tenantA)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	advance := []Advance{{MembershipID: granted.Membership.MembershipID, TenantID: tenant, Above: before + 5}}
	done, err := service.AdvanceVersions(providerCtx, advance, "INC-1 restored from the 2026-10-08 backup")
	if err != nil {
		t.Fatalf("AdvanceVersions: %v", err)
	}
	if len(done) != 1 || done[0].ToVersion != before+6 || done[0].EventID == nil || done[0].Status != StateSuspended {
		t.Fatalf("advanced %+v, want version %d, suspended, with an event", done, before+6)
	}
	status, after := statusAndVersion(t, service, ctx, granted.Membership.MembershipID)
	if status != StateSuspended || after != before+6 {
		t.Errorf("the Membership is %s at %d, want suspended at %d", status, after, before+6)
	}
	if got := outboxCount(t, service, ctx, granted.Membership.MembershipID); got != events+1 {
		t.Errorf("%d events, want one more than %d", got, events)
	}

	again, err := service.AdvanceVersions(providerCtx, advance, "INC-1 restored from the 2026-10-08 backup")
	if err != nil {
		t.Fatalf("AdvanceVersions again: %v", err)
	}
	if len(again) != 1 || again[0].EventID != nil || again[0].ToVersion != before+6 {
		t.Errorf("a second advance did %+v, want nothing", again)
	}

	if _, err := service.AdvanceVersions(providerCtx, advance, " "); !errors.Is(err, ErrAdvanceReasonRequired) {
		t.Errorf("an advance without a reason answered %v", err)
	}
}

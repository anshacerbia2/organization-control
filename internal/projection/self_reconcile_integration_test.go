package projection

// A consumer reconciles itself, on an interval it declares (ADR-ORG-001 §5.7, amended 2026-10-20).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anshacerbia2/organization-control/internal/db"
)

func consumerCtx(t *testing.T, f *fixture, consumerID string) context.Context {
	t.Helper()
	scope, err := db.ConsumerScope(principalOf(consumerID), mustID(t))
	if err != nil {
		t.Fatalf("ConsumerScope: %v", err)
	}
	return db.WithScope(context.Background(), scope)
}

func TestAConsumerReconcilesItsOwnCopyAndClearsARequestedRun(t *testing.T) {
	f := newFixture(t)
	consumerID := f.register(t)
	tenantID := f.seedTenant(t)
	held := f.seedMembership(t, tenantID, 4)

	// A restore asked every consumer to reconcile (ADR-ORG-003 §5.5).
	f.exec(t, `UPDATE projection.consumer SET reconciliation_requested_at = now() WHERE consumer_id = $1`, consumerID)
	before, err := f.registry.Get(f.ctx, consumerID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if before.ReconciliationRequestedAt == nil || !before.ReconciliationOverdue(f.fixed) ||
		before.ReconciliationInterval != DefaultReconciliationInterval {
		t.Fatalf("before: %+v; want the run requested, overdue, and the default interval", before)
	}

	f.recorder.calls = 0
	ctx := consumerCtx(t, f, consumerID)
	result, err := f.reconciler.Reconcile(ctx, Report{ConsumerID: consumerID, Mark: 10,
		Rows: []ReportedRow{{MembershipID: held, MembershipVersion: 3}}})
	if err != nil {
		t.Fatalf("Reconcile as the consumer: %v", err)
	}
	if err := f.reconciler.PublishReconciled(ctx, result); err != nil {
		t.Fatalf("PublishReconciled as the consumer: %v", err)
	}
	if f.recorder.calls != 2 {
		t.Errorf("the consumer's run recorded %d accesses, want the comparison and the publication", f.recorder.calls)
	}
	if f.reconciledEvents(t, consumerID) != 1 {
		t.Error("the consumer's run published no repair")
	}
	after, err := f.registry.Get(f.ctx, consumerID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.ReconciliationRequestedAt != nil || after.LastReconciledAt == nil || after.ReconciliationOverdue(f.fixed) {
		t.Errorf("after: %+v; want the request cleared and the run recorded", after)
	}
}

func TestAConsumerReconcilesNoOtherConsumer(t *testing.T) {
	f := newFixture(t)
	mine, theirs := f.register(t), f.register(t)
	_, err := f.reconciler.Reconcile(consumerCtx(t, f, mine), Report{ConsumerID: theirs, Mark: 1})
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("reconciling another consumer answered %v, want ErrNotRegistered", err)
	}
}

func TestTheReconciliationIntervalIsDeclaredWithinBounds(t *testing.T) {
	f := newFixture(t)
	base := Registration{ConsumerID: "test-" + mustID(t).String(), ProjectionVersion: "v1",
		MaxAcceptedAge: time.Minute, StaleBehavior: StaleFailClosed, EventTypes: SubscribableEventTypes}
	base.PrincipalID = principalOf(base.ConsumerID)
	t.Cleanup(func() { f.forget(t, base.ConsumerID) })

	for _, bad := range []time.Duration{time.Second, 8 * 24 * time.Hour, -time.Hour} {
		reg := base
		reg.ReconciliationInterval = bad
		if _, err := f.registry.Register(f.ctx, reg); !errors.Is(err, ErrInvalid) {
			t.Errorf("an interval of %s answered %v, want ErrInvalid", bad, err)
		}
	}
	reg := base
	reg.ReconciliationInterval = 6 * time.Hour
	if _, err := f.registry.Register(f.ctx, reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := f.registry.Get(f.ctx, base.ConsumerID)
	if err != nil || got.ReconciliationInterval != 6*time.Hour {
		t.Fatalf("Get answered %+v, %v; want the declared six hours", got, err)
	}
	if !got.ReconciliationOverdue(got.RegisteredAt.Add(7 * time.Hour)) {
		t.Error("a consumer never reconciled is not overdue past its interval")
	}
}

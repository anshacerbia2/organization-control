package membership

import (
	"context"
	"errors"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
)

// TestTheEnforcementStateFollowsTheEvidence is ADR-ORG-004 §5.2 as the tenant runtime role: the
// state comes from the deliveries, receipts and dead letters recorded for the latest transition,
// never from the response that accepted it.
func TestTheEnforcementStateFollowsTheEvidence(t *testing.T) {
	service, ctx, _ := newFixture(t)
	fixed := service.now()
	granted := grantOne(t, service, ctx)

	report, err := service.Enforcement(ctx, granted.Membership.MembershipID)
	if err != nil {
		t.Fatalf("Enforcement after grant: %v", err)
	}
	if report.Transition != ActionGrant || report.EventID != granted.EventID || report.EventID.IsNil() {
		t.Errorf("after grant: transition %s, event %s; the grant published %s", report.Transition, report.EventID, granted.EventID)
	}
	// Owed to no consumer: no projection holds the Membership, and the empty list says so.
	if report.State != EnforcementEnforced || len(report.Consumers) != 0 || report.Budget != PropagationBudget {
		t.Errorf("an event owed to no consumer: %+v", report)
	}

	revoked, err := service.Revoke(ctx, at(t, service, ctx, granted.Membership.MembershipID))
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	event := revoked.EventID
	owner := ownerPool(t, ctx)
	exec := func(statement string, args ...any) {
		t.Helper()
		if err := owner.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
			_, err := tx.Exec(ctx, statement, args...)
			return err
		}); err != nil {
			t.Fatalf("fixture statement: %v", err)
		}
	}
	t.Cleanup(func() {
		exec(`DELETE FROM platform.outbox_delivery WHERE event_id = $1`, event.String())
		exec(`DELETE FROM platform.delivery_receipt WHERE event_id = $1`, event.String())
		exec(`DELETE FROM platform.dead_letter WHERE event_id = $1`, event.String())
	})
	const revokedType = "com.scnehaux.organization.membership.security.revoked"
	owe := func(consumer string, failureClass any) {
		exec(`INSERT INTO platform.outbox_delivery (created_at, event_id, consumer, sequence, event_type, priority, failure_class)
		      VALUES (now(), $1, $2, 1, $3, 10, $4)`, event.String(), consumer, revokedType, failureClass)
	}
	owe("enforcement-suite-a", nil)
	owe("enforcement-suite-b", nil)
	owe("enforcement-suite-retired", "abandoned")

	state := func(want EnforcementState) Enforcement {
		t.Helper()
		report, err := service.Enforcement(ctx, granted.Membership.MembershipID)
		if err != nil {
			t.Fatalf("Enforcement: %v", err)
		}
		if report.State != want {
			t.Errorf("state = %s, want %s: %+v", report.State, want, report)
		}
		return report
	}

	accepted := state(EnforcementAccepted)
	if accepted.EventID != event || accepted.Transition != ActionRevoke || !accepted.AcceptedAt.Equal(fixed) ||
		accepted.PublishedAt != nil {
		t.Errorf("accepted: %+v; want the revocation, accepted at %s, unpublished", accepted, fixed)
	}
	if len(accepted.Consumers) != 2 || accepted.Consumers[0].Evidence != EvidencePending {
		t.Errorf("the abandoned delivery counts, or a consumer is not pending: %+v", accepted.Consumers)
	}

	exec(`UPDATE platform.outbox_delivery SET published = TRUE, published_at = $2
	      WHERE event_id = $1 AND consumer = 'enforcement-suite-a'`, event.String(), fixed.Add(time.Second))
	exec(`INSERT INTO platform.delivery_receipt (event_id, consumer, event_type, evidence)
	      VALUES ($1, 'enforcement-suite-a', $2, 'transport_accepted')`, event.String(), revokedType)
	propagating := state(EnforcementPropagating)
	if propagating.PublishedAt == nil || !propagating.PublishedAt.Equal(fixed.Add(time.Second)) ||
		propagating.Consumers[0].Evidence != EvidenceTransportAccepted || propagating.Consumers[0].RecordedAt == nil {
		t.Errorf("transport acceptance is propagation: %+v", propagating)
	}

	// Past the budget and still not applied.
	service.now = func() time.Time { return fixed.Add(PropagationBudget + time.Second) }
	state(EnforcementOverBudget)
	service.now = func() time.Time { return fixed }

	// A dead letter is over budget whatever the clock says.
	exec(`INSERT INTO platform.dead_letter (event_id, event_type, consumer, failure_class, failure_detail, attempts, first_failed_at)
	      VALUES ($1, $2, 'enforcement-suite-b', 'poison', 'refused by the suite', 5, now())`, event.String(), revokedType)
	dead := state(EnforcementOverBudget)
	if dead.Consumers[1].Evidence != EvidenceDeadLettered {
		t.Errorf("the dead-lettered consumer reads %+v", dead.Consumers[1])
	}

	exec(`UPDATE platform.delivery_receipt SET evidence = 'consumer_applied' WHERE event_id = $1`, event.String())
	exec(`INSERT INTO platform.delivery_receipt (event_id, consumer, event_type, evidence)
	      VALUES ($1, 'enforcement-suite-b', $2, 'consumer_applied')`, event.String(), revokedType)
	state(EnforcementEnforced)

	if _, err := service.Enforcement(boundTo(t, context.Background(), tenantB), granted.Membership.MembershipID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another Tenant's Membership: error = %v, want ErrNotFound", err)
	}
	absent, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	if _, err := service.Enforcement(ctx, absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("an absent Membership: error = %v, want ErrNotFound", err)
	}
}

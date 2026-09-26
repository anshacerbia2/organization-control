package projection

// SUPERSEDED, against the real resolution role and the real history table.
//
// The case this file exists for is the one a position-based predicate gets wrong: an older event
// replayed after a newer revocation carries the higher stream position, and its receipt must not
// close the revocation's incident. Everything else here is the predicate's ordinary edges.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
)

const (
	typeGranted   = "com.scnehaux.organization.membership.lifecycle.granted"
	typeRestored  = "com.scnehaux.organization.membership.lifecycle.restored"
	typeSuspended = "com.scnehaux.organization.membership.security.suspended"
	typeRevoked   = "com.scnehaux.organization.membership.security.revoked"
)

// history writes the row the Membership service writes beside every event it publishes.
//
// Through the owner, because no runtime role may write history after the fact -- the service writes
// it in the publishing transaction, and this suite is not that transaction.
func history(t *testing.T, f *fixture, eventID, membershipID, tenantID id.UUID, version int64, eventType string) {
	t.Helper()
	f.exec(t, `INSERT INTO membership.membership_event
	    (event_id, membership_id, tenant_id, membership_version, event_type)
	    VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5)`,
		eventID.String(), membershipID.String(), tenantID.String(), version, eventType)
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM membership.membership_event WHERE event_id = $1`, eventID.String())
	})
}

// deadLettered files an incident for a Membership event that also has its history row.
func deadLettered(t *testing.T, f *fixture, membershipID, tenantID id.UUID, version int64, eventType string) id.UUID {
	t.Helper()
	eventID := mustID(t)
	f.exec(t, `INSERT INTO platform.dead_letter
	    (event_id, event_type, envelope, payload, aggregate_id, failure_class, failure_detail,
	     attempts, first_failed_at)
	    VALUES ($1::uuid, $2, '{}'::jsonb, '{}'::jsonb, $3::uuid, 'poison',
	            'the consumer refused the envelope', 3, clock_timestamp())`,
		eventID.String(), eventType, membershipID.String())
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM platform.dead_letter WHERE event_id = $1`, eventID.String())
	})
	history(t, f, eventID, membershipID, tenantID, version, eventType)
	return eventID
}

// applied records a newer event of the same Membership as history plus the receipt the dispatcher
// writes when the consumer accepted it.
func applied(t *testing.T, f *fixture, membershipID, tenantID id.UUID, version int64,
	eventType, consumer, evidence string) id.UUID {
	t.Helper()
	eventID := mustID(t)
	history(t, f, eventID, membershipID, tenantID, version, eventType)
	receipt(t, f, eventID, consumer, evidence)
	return eventID
}

func deadLetterState(t *testing.T, f *fixture, eventID id.UUID) (bool, string, string) {
	t.Helper()
	var (
		resolved  bool
		kind      string
		reference string
	)
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `SELECT resolved_at IS NOT NULL, coalesce(resolution_type, ''),
		                                coalesce(resolution_reference, '')
		                           FROM platform.dead_letter WHERE event_id = $1`,
			eventID.String()).Scan(&resolved, &kind, &reference)
	}); err != nil {
		t.Fatalf("reading the incident: %v", err)
	}
	return resolved, kind, reference
}

// The ordinary case: a suspension was dead-lettered, the Membership was then restored, and the
// consumer applied the restoration. The consumer holds version 4; the suspension at version 3 can
// never take effect, and the incident closes on the restoration's receipt.
func TestAnIncidentClosesAsSupersededOnANewerAppliedEvent(t *testing.T) {
	f := newFixture(t)
	clearDeadLetters(t, f.ctx, f.setup)
	consumer := f.register(t)
	tenantID := f.seedTenant(t)
	membershipID := f.seedMembership(t, tenantID, 4)

	failed := deadLettered(t, f, membershipID, tenantID, 3, typeSuspended)
	newer := applied(t, f, membershipID, tenantID, 4, typeRestored, consumer, "consumer_applied")

	reader, err := NewFrontierReader(f.pool)
	if err != nil {
		t.Fatalf("NewFrontierReader: %v", err)
	}
	if before, err := reader.Frontier(f.ctx); err != nil || !before.SecurityDebt {
		t.Fatalf("the seeded incident is not reported as security debt (err %v), so this case "+
			"would pass without the resolution doing anything", err)
	}

	r, _ := resolver(t, f)
	resolution, err := r.Supersede(f.ctx, failed)
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	want := fmt.Sprintf("platform.delivery_receipt:%s:%s", newer, consumer)
	if resolution.Type != ResolutionTypeSuperseded || resolution.Reference != want {
		t.Errorf("resolution = %+v, want type %s on %s", resolution, ResolutionTypeSuperseded, want)
	}
	resolved, kind, reference := deadLetterState(t, f, failed)
	if !resolved || kind != ResolutionTypeSuperseded || reference != want {
		t.Errorf("the incident is recorded as resolved=%v type=%q reference=%q", resolved, kind, reference)
	}

	// The account names both versions, so an investigation can see why the failed event was moot
	// without reconstructing the Membership's history.
	count, reason, _ := closureRecords(t, f, failed)
	if count != 1 || !strings.Contains(reason, "version 3 superseded by version 4") {
		t.Errorf("closure records = %d, reason %q; want one naming both versions", count, reason)
	}

	if after, err := reader.Frontier(f.ctx); err != nil || after.SecurityDebt {
		t.Errorf("the incident is closed and the frontier still reports debt (err %v)", err)
	}
}

// The security case. A revocation at version 5 is dead-lettered. An operator then replays an older
// grant at version 4, the consumer applies it, and the grant's receipt is now the most recent one
// the dispatcher wrote -- with a higher stream position than the revocation ever had.
//
// The consumer holds an ACTIVE row. Closing the revocation's incident here would clear the security
// debt while the withdrawal it records has reached nobody.
func TestAReplayedOlderEventDoesNotSupersedeANewerRevocation(t *testing.T) {
	f := newFixture(t)
	consumer := f.register(t)
	tenantID := f.seedTenant(t)
	membershipID := f.seedMembership(t, tenantID, 5)

	revocation := deadLettered(t, f, membershipID, tenantID, 5, typeRevoked)
	applied(t, f, membershipID, tenantID, 4, typeGranted, consumer, "consumer_applied")

	r, _ := resolver(t, f)
	if _, err := r.Supersede(f.ctx, revocation); !errors.Is(err, ErrNotSuperseded) {
		t.Fatalf("Supersede returned %v, want ErrNotSuperseded: an older event's receipt closed "+
			"a revocation the consumer never applied", err)
	}
	if resolved, _, _ := deadLetterState(t, f, revocation); resolved {
		t.Error("the revocation's incident was closed on an older event's receipt")
	}
	if count, reason, _ := closureRecords(t, f, revocation); count != 0 {
		t.Errorf("%d closure records for an incident that was never closed: %q", count, reason)
	}
}

// Newer, but not evidence: the receipt is weak, it belongs to another consumer, or it is about
// another Membership. Each must leave the incident open.
func TestANewerEventThatIsNotEvidenceSupersedesNothing(t *testing.T) {
	f := newFixture(t)
	consumer := f.register(t)
	tenantID := f.seedTenant(t)
	membershipID := f.seedMembership(t, tenantID, 3)
	otherMembership := f.seedMembership(t, tenantID, 9)

	failed := deadLettered(t, f, membershipID, tenantID, 2, typeSuspended)
	applied(t, f, membershipID, tenantID, 3, typeRestored, consumer, "transport_accepted")
	applied(t, f, membershipID, tenantID, 4, typeSuspended, consumer+"-typo", "consumer_applied")
	applied(t, f, otherMembership, tenantID, 9, typeRestored, consumer, "consumer_applied")

	r, _ := resolver(t, f)
	_, err := r.Supersede(f.ctx, failed)
	if !errors.Is(err, ErrNotSuperseded) {
		t.Fatalf("Supersede returned %v, want ErrNotSuperseded", err)
	}
	if !strings.Contains(err.Error(), consumer) || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("the refusal does not name the consumer and the version it compared: %v", err)
	}
	if resolved, _, _ := deadLetterState(t, f, failed); resolved {
		t.Error("the incident was closed on evidence that is not evidence")
	}
}

// An incident whose event has no history row: published before the history existed, or not a
// Membership event. There is no version to compare, and the envelope is not a substitute.
func TestAnIncidentWithNoRecordedVersionCannotBeSuperseded(t *testing.T) {
	f := newFixture(t)
	f.register(t)
	eventID, _ := abandoned(t, f, outbox.PriorityHigh, true)

	r, _ := resolver(t, f)
	_, err := r.Supersede(f.ctx, eventID)
	if !errors.Is(err, ErrNotSuperseded) {
		t.Fatalf("Supersede returned %v, want ErrNotSuperseded", err)
	}
	if !strings.Contains(err.Error(), "replay it instead") {
		t.Errorf("the refusal does not say what to do next: %v", err)
	}
}

// REPLAYED is not loosened by SUPERSEDED existing: a newer applied event is not evidence that THIS
// event was applied, and a REPLAYED closure recorded on it would state something false.
func TestANewerAppliedEventDoesNotCloseAnIncidentAsReplayed(t *testing.T) {
	f := newFixture(t)
	consumer := f.register(t)
	tenantID := f.seedTenant(t)
	membershipID := f.seedMembership(t, tenantID, 4)

	failed := deadLettered(t, f, membershipID, tenantID, 3, typeSuspended)
	applied(t, f, membershipID, tenantID, 4, typeRestored, consumer, "consumer_applied")

	r, _ := resolver(t, f)
	if _, err := r.Resolve(f.ctx, failed); !errors.Is(err, ErrNoAppliedEvidence) {
		t.Fatalf("Resolve returned %v, want ErrNoAppliedEvidence", err)
	}
}

// The history is read by the resolver and written by nobody after the fact.
//
// A resolution role able to insert a history row could invent a newer version and close any
// incident with a receipt for an unrelated event; one able to update could renumber an old event
// above a revocation. Either is the security case above, reached through a grant instead of a bug.
func TestTheResolutionRoleCannotRewriteMembershipHistory(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t)
	membershipID := f.seedMembership(t, tenantID, 2)
	eventID := mustID(t)
	history(t, f, eventID, membershipID, tenantID, 1, typeGranted)
	pool, _ := resolutionPool(t, f)

	for _, attempt := range []struct {
		what      string
		statement string
		args      []any
	}{
		{"invent a newer version", `INSERT INTO membership.membership_event
		     (event_id, membership_id, tenant_id, membership_version, event_type)
		     VALUES ($1::uuid, $2::uuid, $3::uuid, 99, 'x')`,
			[]any{mustID(t).String(), membershipID.String(), tenantID.String()}},
		{"renumber an event", `UPDATE membership.membership_event SET membership_version = 99 WHERE event_id = $1`,
			[]any{eventID.String()}},
		{"erase an event", `DELETE FROM membership.membership_event WHERE event_id = $1`,
			[]any{eventID.String()}},
	} {
		err := db.WithResolutionScope(f.ctx, pool, "attempt to "+attempt.what,
			func(ctx context.Context, tx db.Tx) error {
				_, err := tx.Exec(ctx, attempt.statement, attempt.args...)
				return err
			})
		if err == nil {
			t.Errorf("the resolution role can %s", attempt.what)
		}
	}
}

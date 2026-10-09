package invitation

// The scheduled expiry (TDD-organization-control-004 §Expiry Sweep), against a real engine as
// `organization_provider_app` with no scope bound, as the serving process runs it.

import (
	"context"
	"strings"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
)

// unscoped is a context with no resolved scope. A provider-scoped path refuses it, so a run that
// succeeds on it took no provider scope and filed no privileged-access record.
func unscoped(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (f *fixture) state(t *testing.T, tenantID, invitationID string) State {
	t.Helper()
	var state string
	if err := f.setup.InTx(unscoped(t), func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM invitation.invitation WHERE tenant_id = $1 AND invitation_id = $2`,
			tenantID, invitationID).Scan(&state)
	}); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return State(state)
}

// TestTheScheduledExpiryExpiresAndPublishes is the schedule's path: no scope, the view, and the
// same event the route publishes.
func TestTheScheduledExpiryExpiresAndPublishes(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")
	issued := f.issue(t, tenantID, "scheduled@example.test")
	f.exec(t, `UPDATE invitation.invitation SET expires_at = $2 WHERE invitation_id = $1`,
		issued.Invitation.InvitationID.String(), f.fixed.Add(-time.Hour))

	scheduled, err := f.service.Scheduled(f.raw)
	if err != nil {
		t.Fatalf("Scheduled: %v", err)
	}
	expired, err := scheduled.ExpireLapsed(unscoped(t), 10)
	if err != nil {
		t.Fatalf("ExpireLapsed with no scope: %v", err)
	}
	if expired < 1 {
		t.Fatalf("the scheduled expiry expired %d invitations, want at least the lapsed one", expired)
	}
	if got := f.state(t, tenantID.String(), issued.Invitation.InvitationID.String()); got != StateExpired {
		t.Errorf("state = %s, want expired", got)
	}
	if got := f.events(t, issued.Invitation.InvitationID); len(got) != 2 ||
		!strings.HasSuffix(got[0], "invitation.requested") || !strings.HasSuffix(got[1], "invitation.expired") {
		t.Errorf("events = %v, want requested then expired", got)
	}

	if again, err := scheduled.ExpireLapsed(unscoped(t), 10); err != nil {
		t.Fatalf("second ExpireLapsed: %v", err)
	} else if again != 0 {
		t.Errorf("a second scheduled run expired %d invitations, want 0", again)
	}
	if _, err := f.service.Scheduled(nil); err == nil {
		t.Error("Scheduled accepted no connections")
	}
}

// TestTheExpiryViewAdmitsOneTransition: the database clock bounds what the view returns, whatever
// the service's clock says, and a lapsed invitation can become expired and nothing else.
func TestTheExpiryViewAdmitsOneTransition(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t, "active")

	live := f.issue(t, tenantID, "live@example.test")
	f.exec(t, `UPDATE invitation.invitation SET expires_at = now() + interval '1 day' WHERE invitation_id = $1`,
		live.Invitation.InvitationID.String())
	lapsed := f.issue(t, tenantID, "lapsed@example.test")
	f.exec(t, `UPDATE invitation.invitation SET expires_at = now() - interval '1 day' WHERE invitation_id = $1`,
		lapsed.Invitation.InvitationID.String())

	run := func(statement, invitationID string) (int64, error) {
		var affected int64
		err := f.raw.InTx(unscoped(t), func(ctx context.Context, tx fdb.Tx) error {
			tag, err := tx.Exec(ctx, statement, invitationID)
			if err != nil {
				return err
			}
			affected = tag.RowsAffected()
			return nil
		})
		return affected, err
	}

	// An invitation still inside its lifetime is not in the view.
	affected, err := run(`UPDATE operation.invitation_expiry SET state = 'expired' WHERE invitation_id = $1::uuid`,
		live.Invitation.InvitationID.String())
	if err != nil {
		t.Fatalf("an UPDATE through the view: %v", err)
	}
	if affected != 0 {
		t.Error("the view expired an invitation still inside its lifetime")
	}

	// A lapsed one cannot be revoked or accepted through the view.
	for _, state := range []State{StateRevoked, StateAccepted} {
		if _, err := run(`UPDATE operation.invitation_expiry SET state = '`+string(state)+`' WHERE invitation_id = $1::uuid`,
			lapsed.Invitation.InvitationID.String()); err == nil {
			t.Errorf("the view let a lapsed invitation become %s", state)
		}
	}

	// A service clock a year ahead does not reach past the database's.
	scheduled, err := f.service.Scheduled(f.raw)
	if err != nil {
		t.Fatalf("Scheduled: %v", err)
	}
	f.service.now = func() time.Time { return time.Now().AddDate(1, 0, 0) }
	if _, err := scheduled.ExpireLapsed(unscoped(t), 100); err != nil {
		t.Fatalf("ExpireLapsed: %v", err)
	}
	if got := f.state(t, tenantID.String(), live.Invitation.InvitationID.String()); got != StatePending {
		t.Errorf("an invitation inside its lifetime by the database clock is %s, want pending", got)
	}
	if got := f.state(t, tenantID.String(), lapsed.Invitation.InvitationID.String()); got != StateExpired {
		t.Errorf("the lapsed invitation is %s, want expired", got)
	}
}

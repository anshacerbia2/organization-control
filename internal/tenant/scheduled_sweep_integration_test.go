package tenant

// The scheduled provisioning sweep (TDD-organization-control-003 §Scheduled Sweeps), against a real
// engine as `organization_provider_app` with no scope bound, as the serving process runs it.

import (
	"context"
	"errors"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
)

func (f *fixture) unscoped(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestTheScheduledSweepAgesARequestAndRecordsNoAccess is the schedule's path: the raw provider
// connections, no scope, and no privileged-access record, because a timer is not a provider.
func TestTheScheduledSweepAgesARequestAndRecordsNoAccess(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, _ := f.withCorrelation(t)

	requested := f.request(t, ctx, sponsor)
	tenantID := requested.Tenant.TenantID

	coordinator := f.coordinator(t, 30*time.Minute, f.fixed.Add(time.Hour))
	scheduled, err := coordinator.Scheduled(f.raw)
	if err != nil {
		t.Fatalf("Scheduled: %v", err)
	}
	before := len(f.recorder.calls)

	affected, err := scheduled.SweepUnresolved(f.unscoped(t), 100)
	if err != nil {
		t.Fatalf("SweepUnresolved: %v", err)
	}
	if affected < 1 {
		t.Fatalf("the scheduled sweep aged %d requests, want at least this Tenant's", affected)
	}
	if got := len(f.recorder.calls); got != before {
		t.Errorf("the scheduled sweep filed %d privileged-access records, want none", got-before)
	}

	rows := f.requestRows(t, tenantID)
	if len(rows) != 1 || rows[0].state != RequestUnresolved || rows[0].resolvedAt == nil || rows[0].detail == "" {
		t.Fatalf("the request is %+v, want one unresolved with resolved_at and detail", rows)
	}
	if stored := f.read(t, tenantID); stored.status != StateRequested {
		t.Errorf("the scheduled sweep moved the Tenant to %s, want it left in %s", stored.status, StateRequested)
	}

	// A second run takes nothing of this Tenant's again, and the one aged stays as it was.
	if _, err := scheduled.SweepUnresolved(f.unscoped(t), 100); err != nil {
		t.Fatalf("second SweepUnresolved: %v", err)
	}
	again := f.requestRows(t, tenantID)
	if len(again) != 1 || !again[0].resolvedAt.Equal(*rows[0].resolvedAt) {
		t.Errorf("a second run changed the aged request: %+v, was %+v", again, rows)
	}
}

// TestTheScheduledSweepLeavesARequestInsideTheTimeout: the service clock decides the age.
func TestTheScheduledSweepLeavesARequestInsideTheTimeout(t *testing.T) {
	f := newFixture(t)
	sponsor := f.seedSponsor(t, "active")
	ctx, _ := f.withCorrelation(t)

	requested := f.request(t, ctx, sponsor)
	coordinator := f.coordinator(t, 30*time.Minute, f.fixed.Add(10*time.Minute))
	scheduled, err := coordinator.Scheduled(f.raw)
	if err != nil {
		t.Fatalf("Scheduled: %v", err)
	}
	if _, err := scheduled.SweepUnresolved(f.unscoped(t), 100); err != nil {
		t.Fatalf("SweepUnresolved: %v", err)
	}
	if rows := f.requestRows(t, requested.Tenant.TenantID); len(rows) != 1 || rows[0].state != RequestRequested {
		t.Errorf("a request ten minutes old was aged: %+v", rows)
	}
	if _, err := scheduled.SweepUnresolved(f.unscoped(t), 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("a zero batch size returned %v, want ErrInvalid", err)
	}
	if _, err := coordinator.Scheduled(nil); err == nil {
		t.Error("Scheduled accepted no connections")
	}
}

// TestTheSweepViewAdmitsOneTransition holds the view's policy to its one transition, as the
// provider role with no scope: a request already answered is not in it, and a request still
// requested can become unresolved and nothing else.
func TestTheSweepViewAdmitsOneTransition(t *testing.T) {
	f := newFixture(t)
	seeded := f.seed(t, StateProvisioning, "active")
	f.requestProvisioning(t, seeded.TenantID, string(RequestRealized), f.fixed.Add(-2*time.Hour))
	f.requestProvisioning(t, seeded.TenantID, string(RequestRequested), f.fixed.Add(-time.Hour))

	byState := map[RequestState]string{}
	for _, r := range f.requestRows(t, seeded.TenantID) {
		byState[r.state] = r.requestID.String()
	}
	if len(byState) != 2 {
		t.Fatalf("seeded %v, want one realized and one requested request", byState)
	}

	run := func(statement, requestID string) (int64, error) {
		var affected int64
		err := f.raw.InTx(f.unscoped(t), func(ctx context.Context, tx fdb.Tx) error {
			tag, err := tx.Exec(ctx, statement, requestID)
			if err != nil {
				return err
			}
			affected = tag.RowsAffected()
			return nil
		})
		return affected, err
	}

	// The realized request is not in the view, so the sweep's own write cannot reach it.
	affected, err := run(`UPDATE operation.provisioning_sweep SET state = 'unresolved', resolved_at = now()
	    WHERE request_id = $1::uuid`, byState[RequestRealized])
	if err != nil {
		t.Fatalf("an UPDATE through the view: %v", err)
	}
	if affected != 0 {
		t.Errorf("an UPDATE through the view reached a realized request")
	}

	// The requested one cannot be declared realized, or failed, through the view: WITH CHECK refuses.
	for _, state := range []RequestState{RequestRealized, RequestFailed} {
		if _, err := run(`UPDATE operation.provisioning_sweep SET state = '`+string(state)+`', resolved_at = now()
		    WHERE request_id = $1::uuid`, byState[RequestRequested]); err == nil {
			t.Errorf("the view let a requested request become %s", state)
		}
	}

	for _, r := range f.requestRows(t, seeded.TenantID) {
		if r.state != RequestRealized && r.state != RequestRequested {
			t.Errorf("a refused write left a request %s", r.state)
		}
	}
}

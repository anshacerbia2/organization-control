package projection

// SUPERSEDED for Tenant events, against the real resolution role.
//
// Tenant events count as security debt, so a dead-lettered Tenant suspension that a later
// restoration overtook must be closable: replaying it is discarded by the consumer's ordering rule,
// so REPLAYED never gets a receipt, and without this the incident would block every check forever.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

const (
	typeTenantActivated = "com.scnehaux.organization.tenant.lifecycle.activated"
	typeTenantSuspended = "com.scnehaux.organization.tenant.security.suspended"
	typeTenantRestored  = "com.scnehaux.organization.tenant.security.restored"
)

// tenantHistory writes the row the tenant service writes beside every Tenant event it publishes.
func tenantHistory(t *testing.T, f *fixture, eventID, tenantID id.UUID, securityVersion int64, eventType string) {
	t.Helper()
	f.exec(t, `INSERT INTO tenant.tenant_event (event_id, tenant_id, tenant_security_version, event_type)
	    VALUES ($1::uuid, $2::uuid, $3, $4)`, eventID.String(), tenantID.String(), securityVersion, eventType)
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM tenant.tenant_event WHERE event_id = $1`, eventID.String())
	})
}

func deadLetteredTenantEvent(t *testing.T, f *fixture, tenantID id.UUID, securityVersion int64, eventType string) id.UUID {
	t.Helper()
	eventID := mustID(t)
	f.exec(t, `INSERT INTO platform.dead_letter
	    (event_id, event_type, envelope, payload, aggregate_id, failure_class, failure_detail,
	     attempts, first_failed_at)
	    VALUES ($1::uuid, $2, '{}'::jsonb, '{}'::jsonb, $3::uuid, 'poison',
	            'the consumer refused the envelope', 3, clock_timestamp())`,
		eventID.String(), eventType, tenantID.String())
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM platform.dead_letter WHERE event_id = $1`, eventID.String())
	})
	tenantHistory(t, f, eventID, tenantID, securityVersion, eventType)
	return eventID
}

func appliedTenantEvent(t *testing.T, f *fixture, tenantID id.UUID, securityVersion int64, eventType, consumer string) id.UUID {
	t.Helper()
	eventID := mustID(t)
	tenantHistory(t, f, eventID, tenantID, securityVersion, eventType)
	receipt(t, f, eventID, consumer, "consumer_applied")
	return eventID
}

// A suspension dead-lettered, then the Tenant restored and the restoration applied: the consumer
// holds a newer state, the suspension can never take effect, and the incident closes on the
// restoration's receipt.
func TestATenantEventClosesAsSupersededOnANewerAppliedOne(t *testing.T) {
	f := newFixture(t)
	clearDeadLetters(t, f.ctx, f.setup)
	consumer := f.register(t)
	tenantID := f.seedTenant(t)

	failed := deadLetteredTenantEvent(t, f, tenantID, 2, typeTenantSuspended)
	newer := appliedTenantEvent(t, f, tenantID, 3, typeTenantRestored, consumer)

	r, _ := resolver(t, f)
	resolution, err := r.Supersede(f.ctx, failed)
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if want := fmt.Sprintf("platform.delivery_receipt:%s:%s", newer, consumer); resolution.Reference != want {
		t.Errorf("reference %q, want %q", resolution.Reference, want)
	}
	count, reason, _ := closureRecords(t, f, failed)
	if count != 1 || !strings.Contains(reason, "Tenant "+tenantID.String()+" version 2 superseded by version 3") {
		t.Errorf("closure records = %d, reason %q; want one naming the Tenant and both versions", count, reason)
	}
}

// The ordering is the security version, not the stream. An activation applied after a suspension
// dead-lettered is older than it and supersedes nothing.
func TestAnOlderTenantEventDoesNotSupersedeANewerOne(t *testing.T) {
	f := newFixture(t)
	consumer := f.register(t)
	tenantID := f.seedTenant(t)

	failed := deadLetteredTenantEvent(t, f, tenantID, 2, typeTenantSuspended)
	appliedTenantEvent(t, f, tenantID, 1, typeTenantActivated, consumer)

	r, _ := resolver(t, f)
	if _, err := r.Supersede(f.ctx, failed); !errors.Is(err, ErrNotSuperseded) {
		t.Fatalf("Supersede returned %v, want ErrNotSuperseded: an older Tenant event closed a "+
			"suspension the consumer never applied", err)
	}
	if resolved, _, _ := deadLetterState(t, f, failed); resolved {
		t.Error("the suspension's incident was closed on an older event's receipt")
	}
}

// The resolution role reads Tenant history and cannot write it.
func TestTheResolutionRoleCannotRewriteTenantHistory(t *testing.T) {
	f := newFixture(t)
	tenantID := f.seedTenant(t)
	eventID := mustID(t)
	tenantHistory(t, f, eventID, tenantID, 1, typeTenantActivated)
	pool, _ := resolutionPool(t, f)

	for _, attempt := range []struct {
		what      string
		statement string
		args      []any
	}{
		{"invent a newer version", `INSERT INTO tenant.tenant_event (event_id, tenant_id, tenant_security_version, event_type)
		     VALUES ($1::uuid, $2::uuid, 99, 'x')`, []any{mustID(t).String(), tenantID.String()}},
		{"renumber an event", `UPDATE tenant.tenant_event SET tenant_security_version = 99 WHERE event_id = $1`,
			[]any{eventID.String()}},
		{"erase an event", `DELETE FROM tenant.tenant_event WHERE event_id = $1`, []any{eventID.String()}},
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

	// And it can read it, which the predicate depends on.
	var seen int
	if err := db.WithResolutionScope(f.ctx, pool, "read tenant history", func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant.tenant_event WHERE event_id = $1`, eventID.String()).Scan(&seen)
	}); err != nil || seen != 1 {
		t.Errorf("the resolution role read %d rows (err %v), want 1", seen, err)
	}
}

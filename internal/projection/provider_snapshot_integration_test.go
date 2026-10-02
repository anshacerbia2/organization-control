package projection

// The provider authority snapshot and the subscription rule on both snapshots, against the real
// engine (TDD-organization-control-001 §Provider Authority Projection, TDD-organization-control-002
// §Consumer Registry).

import (
	"context"
	"errors"
	"fmt"
	"testing"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
)

const (
	identityScope     = "provider:identity-control"
	organizationScope = "provider:organization-control"
	providerRevoked   = "com.scnehaux.organization.provider.security.revoked"
	providerActivated = "com.scnehaux.organization.provider.lifecycle.activated"
)

// providerGrant seeds a grant through the owner and removes it, with what names it, afterwards.
func (f *fixture) providerGrant(t *testing.T, scope string, revoked bool, version int64) id.UUID {
	t.Helper()
	grantID := mustID(t)
	f.exec(t, `INSERT INTO organization.provider_grant
	    (grant_id, principal_id, scope, granted_by, reason, kind, grant_version, revoked_at, revoked_by, revoke_reason)
	    VALUES ($1::uuid, gen_random_uuid(), $2, gen_random_uuid(), 'suite', 'eligible', $3,
	            CASE WHEN $4 THEN now() END, CASE WHEN $4 THEN gen_random_uuid() END, CASE WHEN $4 THEN 'suite' END)`,
		grantID.String(), scope, version, revoked)
	t.Cleanup(func() {
		_ = f.setup.InTx(context.Background(), func(ctx context.Context, tx fdb.Tx) error {
			for _, statement := range []string{
				`DELETE FROM organization.provider_grant_event WHERE grant_id = $1::uuid`,
				`DELETE FROM organization.provider_activation WHERE grant_id = $1::uuid`,
				`DELETE FROM organization.provider_grant WHERE grant_id = $1::uuid`,
			} {
				if _, err := tx.Exec(ctx, statement, grantID.String()); err != nil {
					return err
				}
			}
			return nil
		})
	})
	return grantID
}

// activate records an approved activation of the grant ending after the interval.
func (f *fixture) activate(t *testing.T, grantID id.UUID, endsIn string) id.UUID {
	t.Helper()
	activationID := mustID(t)
	f.exec(t, `INSERT INTO organization.provider_activation
	    (activation_id, grant_id, principal_id, scope, reason, duration_seconds, approval_required,
	     decided_by, decision, decision_reason, decided_at, ends_at)
	    SELECT $1::uuid, grant_id, principal_id, scope, 'suite', 3600, FALSE,
	           principal_id, 'approved', 'suite', now(), now() + $2::interval
	      FROM organization.provider_grant WHERE grant_id = $3::uuid`,
		activationID.String(), endsIn, grantID.String())
	return activationID
}

func (f *fixture) providerPage(t *testing.T, consumer string) (ProviderPage, error) {
	t.Helper()
	return f.publisher.ProviderSnapshot(f.ctx, SnapshotRequest{ConsumerID: consumer, PageSize: MaxPageSize})
}

func TestTheProviderSnapshotReturnsWhatPublishedGrantsConfer(t *testing.T) {
	f := newFixture(t)
	consumer := "provider-snapshot-" + mustID(t).String()
	if err := f.registerNamed(t, consumer, ProviderEventTypes...); err != nil {
		t.Fatalf("Register: %v", err)
	}

	active := f.providerGrant(t, identityScope, false, 2)
	activation := f.activate(t, active, "1 hour")
	expired := f.providerGrant(t, identityScope, false, 2)
	f.activate(t, expired, "-1 minute")
	revoked := f.providerGrant(t, identityScope, true, 3)
	ownScope := f.providerGrant(t, organizationScope, false, 0)

	page, err := f.providerPage(t, consumer)
	if err != nil {
		t.Fatalf("ProviderSnapshot: %v", err)
	}
	got := map[id.UUID]ProviderGrantRow{}
	for _, row := range page.Grants {
		got[row.GrantID] = row
	}
	if row, ok := got[active]; !ok || row.Activation == nil || row.Activation.ActivationID != activation ||
		row.GrantVersion != 2 || row.GrantStatus != "active" || row.Scope != identityScope {
		t.Errorf("the activated grant reads as %+v", row)
	}
	if row, ok := got[expired]; !ok || row.Activation != nil {
		t.Errorf("a grant whose activation ended reads as %+v; want it present with no activation", row)
	}
	if _, ok := got[revoked]; ok {
		t.Error("a revoked grant is in the snapshot")
	}
	if _, ok := got[ownScope]; ok {
		t.Error("a provider:organization-control grant is in the snapshot; that scope is never published")
	}
	if page.HighWaterMark < 0 {
		t.Errorf("the mark is %d", page.HighWaterMark)
	}
}

// A consumer reads only the snapshot of what it subscribes to.
func TestASnapshotAnswersOnlyAConsumerSubscribedToIt(t *testing.T) {
	f := newFixture(t)
	organization := "snapshot-organization-" + mustID(t).String()
	provider := "snapshot-provider-" + mustID(t).String()
	if err := f.registerNamed(t, organization, revoked); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := f.registerNamed(t, provider, providerRevoked); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := f.providerPage(t, organization); !errors.Is(err, ErrNotSubscribed) {
		t.Errorf("a Membership subscriber read the provider authority snapshot: %v", err)
	}
	if _, err := f.publisher.Snapshot(f.ctx, SnapshotRequest{ConsumerID: provider}); !errors.Is(err, ErrNotSubscribed) {
		t.Errorf("a provider subscriber read the Organization snapshot: %v", err)
	}
	if _, err := f.providerPage(t, provider); err != nil {
		t.Errorf("the provider subscriber was refused its own snapshot: %v", err)
	}
	if _, err := f.publisher.Snapshot(f.ctx, SnapshotRequest{ConsumerID: organization}); err != nil {
		t.Errorf("the Membership subscriber was refused its own snapshot: %v", err)
	}
}

// An older provider grant event closes as SUPERSEDED once the consumer applied a newer version of
// the same grant: each event carries the grant's whole state (TDD-organization-control-005).
func TestAProviderGrantEventClosesAsSupersededOnANewerAppliedOne(t *testing.T) {
	f := newFixture(t)
	clearDeadLetters(t, f.ctx, f.setup)
	consumer := f.register(t)
	grantID := f.providerGrant(t, identityScope, false, 3)

	failed := mustID(t)
	f.exec(t, `INSERT INTO organization.provider_grant_event (event_id, grant_id, grant_version, event_type)
	    VALUES ($1::uuid, $2::uuid, 2, $3)`, failed.String(), grantID.String(), providerActivated)
	f.exec(t, `INSERT INTO platform.dead_letter
	    (event_id, event_type, envelope, payload, aggregate_id, priority, failure_class, failure_detail,
	     attempts, first_failed_at, consumer)
	    VALUES ($1::uuid, $2, '{}'::jsonb, '{}'::jsonb, $3::uuid, 100, 'poison', 'refused', 1, clock_timestamp(), $4)`,
		failed.String(), providerActivated, grantID.String(), consumer)
	t.Cleanup(func() { f.exec(t, `DELETE FROM platform.dead_letter WHERE event_id = $1::uuid`, failed.String()) })

	newer := mustID(t)
	f.exec(t, `INSERT INTO organization.provider_grant_event (event_id, grant_id, grant_version, event_type)
	    VALUES ($1::uuid, $2::uuid, 3, $3)`, newer.String(), grantID.String(), providerRevoked)
	receipt(t, f, newer, consumer, "consumer_applied")

	r, _ := resolver(t, f)
	resolution, err := r.Supersede(f.ctx, failed, consumer)
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if want := fmt.Sprintf("platform.delivery_receipt:%s:%s", newer, consumer); resolution.Reference != want {
		t.Errorf("the closure cites %s, want %s", resolution.Reference, want)
	}
}

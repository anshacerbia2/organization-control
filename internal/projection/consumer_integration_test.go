package projection

// The consumer registry, against the real engine as the provider role (TDD-organization-control-002
// §Consumer Registry).
//
// Several consumers are active at once (ADR-GLB-018). A registration is the consumer's subscription,
// so each test here also asserts what the registry wrote to platform.subscription, and what a
// retirement did to the deliveries the consumer was still owed.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// principalOf is the workload principal_id a test consumer is registered with: derived from its name,
// so re-registering the same consumer names the same Principal, as a real consumer would.
func principalOf(consumerID string) id.UUID {
	sum := sha256.Sum256([]byte(consumerID))
	sum[6] = sum[6]&0x0f | 0x80 // version 8, name-derived
	sum[8] = sum[8]&0x3f | 0x80 // RFC 9562 variant
	return id.MustParse(fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16]))
}

const (
	revoked   = "com.scnehaux.organization.membership.security.revoked"
	suspended = "com.scnehaux.organization.tenant.security.suspended"
)

func (f *fixture) registerNamed(t *testing.T, consumerID string, types ...string) error {
	t.Helper()
	if len(types) == 0 {
		types = SubscribableEventTypes
	}
	_, err := f.registry.Register(f.ctx, Registration{
		ConsumerID:        consumerID,
		PrincipalID:       principalOf(consumerID),
		ProjectionVersion: "v1",
		MaxAcceptedAge:    30 * time.Second,
		StaleBehavior:     StaleFailClosed,
		EventTypes:        types,
	})
	t.Cleanup(func() { f.forget(t, consumerID) })
	return err
}

// subscription reads the consumer's active subscription, sorted, or nil when it has none.
func (f *fixture) subscription(t *testing.T, consumerID string) []string {
	t.Helper()
	var types []string
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		rows, err := tx.Query(ctx, `SELECT unnest(event_types) FROM platform.subscription
			WHERE consumer = $1 AND retired_at IS NULL`, consumerID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				return err
			}
			types = append(types, t)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("reading the subscription of %s: %v", consumerID, err)
	}
	sort.Strings(types)
	return types
}

// TestSeveralConsumersAreActiveAtOnce: the limit consumer_single_active held is gone, and each
// consumer is subscribed to its own types.
func TestSeveralConsumersAreActiveAtOnce(t *testing.T) {
	f := newFixture(t)

	first := "multi-first-" + mustID(t).String()
	second := "multi-second-" + mustID(t).String()
	if err := f.registerNamed(t, first); err != nil {
		t.Fatalf("registering %s: %v", first, err)
	}
	if err := f.registerNamed(t, second, revoked, revoked); err != nil {
		t.Fatalf("registering a second active consumer: %v", err)
	}
	for _, consumer := range []string{first, second} {
		if _, err := f.registry.Get(f.ctx, consumer); err != nil {
			t.Errorf("%s does not read as registered: %v", consumer, err)
		}
	}

	all := append([]string(nil), SubscribableEventTypes...)
	sort.Strings(all)
	if got := f.subscription(t, first); strings.Join(got, ",") != strings.Join(all, ",") {
		t.Errorf("%s subscribes to %v, want every type the registry offers", first, got)
	}
	record, err := f.registry.Get(f.ctx, second)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := f.subscription(t, second); len(got) != 1 || got[0] != revoked {
		t.Errorf("%s subscribes to %v, want %s once", second, got, revoked)
	}
	if len(record.EventTypes) != 1 || record.EventTypes[0] != revoked {
		t.Errorf("%s reads its event types as %v, want [%s]", second, record.EventTypes, revoked)
	}
}

// A registration names what it applies, and only what this registry offers. A misspelt type is an
// answer here rather than a subscription nothing ever matches.
func TestRegistrationRefusesATypeThisRegistryDoesNotOffer(t *testing.T) {
	f := newFixture(t)
	for name, types := range map[string][]string{
		"no event types":    {},
		"an unoffered type": {"com.scnehaux.organization.workspace.lifecycle.renamed"},
		"a misspelt type":   {"com.scnehaux.organization.membership.security.revokd"},
	} {
		consumer := "multi-refused-" + mustID(t).String()
		_, err := f.registry.Register(f.ctx, Registration{
			ConsumerID: consumer, PrincipalID: principalOf(consumer), ProjectionVersion: "v1",
			MaxAcceptedAge: 30 * time.Second, StaleBehavior: StaleFailClosed, EventTypes: types,
		})
		t.Cleanup(func() { f.forget(t, consumer) })
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s answered %v, want ErrInvalid", name, err)
		}
		if got := f.subscription(t, consumer); got != nil {
			t.Errorf("%s left a subscription %v behind", name, got)
		}
	}
}

// Re-registering with the same types changes the declared terms only. With other types it replaces
// the subscription and clears the snapshot mark: events of a newly added type that committed before
// the change were never delivered, so the consumer bootstraps again (ADR-GLB-018 §5.1).
func TestASubscriptionChangeRequiresABootstrap(t *testing.T) {
	f := newFixture(t)
	consumer := "multi-change-" + mustID(t).String()
	if err := f.registerNamed(t, consumer, revoked); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := f.publisher.Bootstrap(f.ctx, consumer, 1); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if err := f.registerNamed(t, consumer, revoked); err != nil {
		t.Fatalf("re-registering with the same types: %v", err)
	}
	if record, err := f.registry.Get(f.ctx, consumer); err != nil || record.SnapshotMark == nil {
		t.Fatalf("re-registering with the same types reads as %+v, %v; the snapshot mark must stay", record, err)
	}

	if err := f.registerNamed(t, consumer, revoked, suspended); err != nil {
		t.Fatalf("re-registering with another type: %v", err)
	}
	record, err := f.registry.Get(f.ctx, consumer)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if record.SnapshotMark != nil {
		t.Error("the subscription changed and the snapshot mark stayed; a progress report would vouch for a model missing the new type")
	}
	if got := f.subscription(t, consumer); len(got) != 2 {
		t.Errorf("the subscription is %v, want both types", got)
	}
	if _, err := f.registry.RecordProgress(f.ctx, Progress{ConsumerID: consumer, AppliedMark: 2}); !errors.Is(err, ErrNoSnapshotMark) {
		t.Errorf("progress after a subscription change answered %v, want ErrNoSnapshotMark", err)
	}
}

// ADR-GLB-018 §5.5: a retirement closes what the consumer was still owed, and no other consumer's.
func TestRetiringAConsumerAbandonsWhatItWasOwed(t *testing.T) {
	f := newFixture(t)
	retiring := "multi-retiring-" + mustID(t).String()
	staying := "multi-staying-" + mustID(t).String()
	if err := f.registerNamed(t, retiring); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := f.registerNamed(t, staying); err != nil {
		t.Fatalf("Register: %v", err)
	}
	eventID := f.owed(t, retiring, staying)

	if err := f.registry.Retire(f.ctx, retiring); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if got := f.subscription(t, retiring); got != nil {
		t.Errorf("the retired consumer still subscribes to %v", got)
	}
	for consumer, want := range map[string]struct {
		published bool
		class     string
	}{
		retiring: {true, "abandoned"},
		staying:  {false, ""},
	} {
		var published bool
		var class *string
		if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
			return tx.QueryRow(ctx, `SELECT published, failure_class FROM platform.outbox_delivery
				WHERE event_id = $1 AND consumer = $2`, eventID.String(), consumer).Scan(&published, &class)
		}); err != nil {
			t.Fatalf("reading %s's delivery: %v", consumer, err)
		}
		got := ""
		if class != nil {
			got = *class
		}
		if published != want.published || got != want.class {
			t.Errorf("%s's delivery reads published=%v class=%q, want %v %q", consumer, published, got, want.published, want.class)
		}
	}
}

// owed seeds one unpublished delivery of an event to each consumer, as an append would.
func (f *fixture) owed(t *testing.T, consumers ...string) id.UUID {
	t.Helper()
	eventID := mustID(t)
	f.exec(t, `INSERT INTO platform.outbox (event_id, aggregate_id, event_type, payload, envelope)
		VALUES ($1::uuid, gen_random_uuid(), $2, '{}'::jsonb, '{}'::jsonb)`, eventID.String(), revoked)
	for _, consumer := range consumers {
		f.exec(t, `INSERT INTO platform.outbox_delivery (created_at, event_id, consumer, sequence, event_type, priority)
			SELECT created_at, event_id, $2, sequence, event_type, priority FROM platform.outbox WHERE event_id = $1::uuid`,
			eventID.String(), consumer)
	}
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM platform.outbox_delivery WHERE event_id = $1::uuid`, eventID.String())
		f.exec(t, `DELETE FROM platform.outbox WHERE event_id = $1::uuid`, eventID.String())
	})
	return eventID
}

// Retirement withdraws authority and keeps the record. The row is what an investigation into
// a stale enforcement decision reads after the consumer is gone.
func TestARetiredConsumerHoldsNoAuthorityAndKeepsItsRecord(t *testing.T) {
	f := newFixture(t)

	consumerID := "multi-retired-" + mustID(t).String()
	if err := f.registerNamed(t, consumerID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := f.registry.Retire(f.ctx, consumerID); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	// Every runtime path -- snapshot, progress, frontier -- reads through this, so one
	// refusal here withdraws all of them.
	if _, err := f.registry.Get(f.ctx, consumerID); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("Get on a retired consumer returned %v, want ErrNotRegistered", err)
	}
	if _, err := f.registry.RecordProgress(f.ctx, Progress{ConsumerID: consumerID, AppliedMark: 1}); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("RecordProgress for a retired consumer returned %v, want ErrNotRegistered", err)
	}

	var kept int
	if err := db.WithProviderScope(f.ctx, f.provider, "projection scope suite",
		func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM projection.consumer WHERE consumer_id = $1 AND retired_at IS NOT NULL`,
				consumerID).Scan(&kept)
		}); err != nil {
		t.Fatalf("reading the retired row: %v", err)
	}
	if kept != 1 {
		t.Error("the row was removed rather than stamped, so what this consumer was told is gone")
	}
}

func TestRetiringTwiceIsNotAnErrorAndRetiringNothingIs(t *testing.T) {
	f := newFixture(t)

	consumerID := "multi-twice-" + mustID(t).String()
	if err := f.registerNamed(t, consumerID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := f.registry.Retire(f.ctx, consumerID); err != nil {
		t.Fatalf("first Retire: %v", err)
	}
	// The caller asked for a state that already holds, so there is nothing to report.
	if err := f.registry.Retire(f.ctx, consumerID); err != nil {
		t.Errorf("retiring an already-retired consumer returned %v, want nil", err)
	}
	// A mistyped name, on the other hand, must not be answered as success.
	if err := f.registry.Retire(f.ctx, "multi-never-registered"); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("retiring an unknown consumer returned %v, want ErrNotRegistered", err)
	}
	if err := f.registry.Retire(f.ctx, "  "); !errors.Is(err, ErrInvalid) {
		t.Errorf("retiring a blank identifier returned %v, want ErrInvalid", err)
	}
}

// A consumer returning under an identity it previously held is a legitimate act. It is subscribed
// again and bootstraps again: nothing was delivered to it while it was retired.
func TestRegisteringARetiredIdentityRevivesIt(t *testing.T) {
	f := newFixture(t)

	consumerID := "multi-revived-" + mustID(t).String()
	if err := f.registerNamed(t, consumerID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := f.publisher.Bootstrap(f.ctx, consumerID, 1); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := f.registry.Retire(f.ctx, consumerID); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	if _, err := f.registry.Register(f.ctx, Registration{
		ConsumerID:        consumerID,
		PrincipalID:       principalOf(consumerID),
		ProjectionVersion: "v1",
		MaxAcceptedAge:    30 * time.Second,
		StaleBehavior:     StaleFailClosed,
		EventTypes:        SubscribableEventTypes,
	}); err != nil {
		t.Fatalf("re-registering a retired identity: %v", err)
	}
	record, err := f.registry.Get(f.ctx, consumerID)
	if err != nil {
		t.Fatalf("the revived consumer does not read as registered: %v", err)
	}
	if record.SnapshotMark != nil {
		t.Error("the revived consumer kept its snapshot mark, and nothing was delivered to it while it was retired")
	}
	if got := f.subscription(t, consumerID); len(got) != len(SubscribableEventTypes) {
		t.Errorf("the revived consumer subscribes to %v", got)
	}
}

// A consumer is the workload it was registered with (ADR-ORG-001 §5.11). Re-registering it under
// another principal_id would hand its records and its authority to a different workload, and a
// workload naming two consumers would make its token ambiguous; both are refused.
func TestAConsumerKeepsItsWorkload(t *testing.T) {
	f := newFixture(t)

	consumerID := "workload-kept-" + mustID(t).String()
	if err := f.registerNamed(t, consumerID); err != nil {
		t.Fatalf("registering the consumer: %v", err)
	}

	_, err := f.registry.Register(f.ctx, Registration{
		ConsumerID: consumerID, PrincipalID: mustID(t), ProjectionVersion: "v1",
		MaxAcceptedAge: 30 * time.Second, StaleBehavior: StaleFailClosed, EventTypes: SubscribableEventTypes,
	})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "another principal_id") {
		t.Errorf("re-registering under another principal_id answered %v, want ErrInvalid", err)
	}
	record, err := f.registry.Get(f.ctx, consumerID)
	if err != nil || record.PrincipalID != principalOf(consumerID) {
		t.Errorf("the consumer reads as %+v, %v; its principal_id must be unchanged", record, err)
	}

	if err := f.registry.Retire(f.ctx, consumerID); err != nil {
		t.Fatalf("retire: %v", err)
	}
	other := "workload-other-" + mustID(t).String()
	_, err = f.registry.Register(f.ctx, Registration{
		ConsumerID: other, PrincipalID: principalOf(consumerID), ProjectionVersion: "v1",
		MaxAcceptedAge: 30 * time.Second, StaleBehavior: StaleFailClosed, EventTypes: SubscribableEventTypes,
	})
	t.Cleanup(func() { f.forget(t, other) })
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), consumerID) {
		t.Errorf("a second consumer under a retired consumer's principal_id answered %v, want ErrInvalid naming %s",
			err, consumerID)
	}
}

// TestTheConsumerListIsAKeysetPageWithStaleComputed is TDD-organization-control-002 1.11.0 §The
// Consumer List: consumer_id order, `next` on a full page, the state filter on every page, each page
// recorded with the caller's reason, and `stale` from Consumer.Age at the registry's clock -- true for
// a consumer that never reported or reported past its budget, false within it and for a retired one.
func TestTheConsumerListIsAKeysetPageWithStaleComputed(t *testing.T) {
	f := newFixture(t)

	// A prefix no other row shares, so the page after it begins with these four. Only begins: rows
	// other suites leave behind may sort after them.
	prefix := "list-" + mustID(t).String() + "-"
	fresh, late, never, retired := prefix+"a", prefix+"b", prefix+"c", prefix+"d"
	for _, name := range []string{fresh, late, never, retired} {
		if err := f.registerNamed(t, name, revoked); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	// The budget registerNamed declares is 30 s, against the registry's fixed clock.
	f.exec(t, `UPDATE projection.consumer SET snapshot_mark = 1, last_reported_mark = 1,
		last_reported_at = $2 WHERE consumer_id = $1`, fresh, f.fixed.Add(-10*time.Second))
	f.exec(t, `UPDATE projection.consumer SET snapshot_mark = 1, last_reported_mark = 1,
		last_reported_at = $2 WHERE consumer_id = $1`, late, f.fixed.Add(-31*time.Second))
	if err := f.registry.Retire(f.ctx, retired); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	before := len(f.recorder.reasons)
	page, err := f.registry.List(f.ctx, ConsumerListQuery{After: prefix, Limit: 2}, "the projection health screen")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Consumers) != 2 || page.Consumers[0].ConsumerID != fresh || page.Consumers[1].ConsumerID != late {
		t.Fatalf("the first page is not the first two consumers in key order: %+v", page.Consumers)
	}
	if page.Next == nil || *page.Next != late {
		t.Fatalf("next = %v, want %s", page.Next, late)
	}
	if got := f.recorder.reasons[before:]; len(got) != 1 || got[0] != "the projection health screen" {
		t.Errorf("the page recorded %q; want one access with the caller's reason", got)
	}
	if first := page.Consumers[0]; first.Stale || first.State != ConsumerActive || first.RetiredAt != nil ||
		first.MaxAcceptedAge != 30*time.Second || first.StaleBehavior != StaleFailClosed ||
		len(first.EventTypes) != 1 || first.EventTypes[0] != revoked {
		t.Errorf("a consumer within its budget reads %+v", first)
	}
	if !page.Consumers[1].Stale {
		t.Errorf("a consumer that reported 31 s ago against a 30 s budget is not stale")
	}

	rest, err := f.registry.List(f.ctx, ConsumerListQuery{After: *page.Next, Limit: 2}, "x")
	if err != nil {
		t.Fatalf("List after: %v", err)
	}
	if len(rest.Consumers) != 2 || rest.Consumers[0].ConsumerID != never || rest.Consumers[1].ConsumerID != retired {
		t.Fatalf("the second page = %+v; want %s then %s", rest.Consumers, never, retired)
	}
	if !rest.Consumers[0].Stale {
		t.Errorf("a consumer that never reported is not stale")
	}
	if gone := rest.Consumers[1]; gone.State != ConsumerRetired || gone.RetiredAt == nil || gone.Stale ||
		len(gone.EventTypes) != 0 {
		t.Errorf("a retired consumer reads %+v; want retired, retired_at set, not stale, no event types", gone)
	}

	active, err := f.registry.List(f.ctx, ConsumerListQuery{After: late, State: ConsumerActive}, "x")
	if err != nil {
		t.Fatalf("List active: %v", err)
	}
	for _, item := range active.Consumers {
		if item.ConsumerID == retired || item.State != ConsumerActive {
			t.Errorf("state=active returned %s in state %s", item.ConsumerID, item.State)
		}
	}
	if len(active.Consumers) == 0 || active.Consumers[0].ConsumerID != never {
		t.Errorf("state=active after %s = %+v; want it to start at %s", late, active.Consumers, never)
	}
	gone, err := f.registry.List(f.ctx, ConsumerListQuery{After: prefix, State: ConsumerRetired}, "x")
	if err != nil {
		t.Fatalf("List retired: %v", err)
	}
	if len(gone.Consumers) == 0 || gone.Consumers[0].ConsumerID != retired {
		t.Errorf("state=retired after the prefix = %+v; want it to start at %s", gone.Consumers, retired)
	}

	if _, err := f.registry.List(f.ctx, ConsumerListQuery{State: "stale"}, "x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown state: error = %v, want ErrInvalid", err)
	}
	if _, err := f.registry.List(f.ctx, ConsumerListQuery{Limit: 101}, "x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a limit above the bound: error = %v, want ErrInvalid", err)
	}
	if _, err := f.registry.List(f.ctx, ConsumerListQuery{}, ""); !errors.Is(err, db.ErrReasonRequired) {
		t.Errorf("a list without a reason: error = %v, want db.ErrReasonRequired", err)
	}
}

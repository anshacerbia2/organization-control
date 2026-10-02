package authority

// Publishing provider:identity-control grants, against the real engine as the provider role
// (TDD-organization-control-001 §Provider Authority Projection).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"
)

type publishedEvent struct {
	eventType string
	priority  int16
	state     GrantState
	history   int64
}

// publishedFor reads every event published for the grant, oldest first, with the version its
// history row records.
func publishedFor(t *testing.T, ctx context.Context, owner *fdb.Pool, grantID id.UUID) []publishedEvent {
	t.Helper()
	var out []publishedEvent
	if err := owner.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT o.event_type, o.priority, o.payload, coalesce(h.grant_version, -1)
			  FROM platform.outbox o
			  LEFT JOIN organization.provider_grant_event h ON h.event_id = o.event_id
			 WHERE o.aggregate_id = $1::uuid
			 ORDER BY o.sequence`, grantID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e   publishedEvent
				raw []byte
			)
			if err := rows.Scan(&e.eventType, &e.priority, &raw, &e.history); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &e.state); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("reading the published events: %v", err)
	}
	t.Cleanup(func() {
		exec(t, context.Background(), owner, `DELETE FROM platform.outbox_delivery d USING platform.outbox o
		    WHERE d.created_at = o.created_at AND d.event_id = o.event_id AND o.aggregate_id = $1::uuid`, grantID.String())
		exec(t, context.Background(), owner, `DELETE FROM platform.outbox WHERE aggregate_id = $1::uuid`, grantID.String())
	})
	return out
}

func TestAnIdentityControlGrantPublishesEachTransition(t *testing.T) {
	service, admin, _, asFirst, _ := activations(t, true)
	_, owner, ctx := pools(t)
	holder := newID(t)

	grant, err := admin.Grant(asFirst, holder, ScopeIdentityControl, KindEligible, "identity on-call")
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	// An approver holds a grant for the same scope.
	approver := newID(t)
	if _, err := admin.Grant(asFirst, approver, ScopeIdentityControl, KindEmergency, "identity break-glass"); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	asHolder := actingAs(t, context.Background(), holder)
	requested, err := service.Request(asHolder, grant.ID, time.Hour, "rotate a client key")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, err := service.Decide(actingAs(t, context.Background(), approver), requested.ID, DecisionApproved, "go"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := service.End(asHolder, requested.ID, false, "done"); err != nil {
		t.Fatalf("End: %v", err)
	}
	if _, err := admin.Revoke(asFirst, grant.ID, "left the team"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	events := publishedFor(t, ctx, owner, grant.ID)
	want := []struct {
		eventType  string
		priority   int16
		status     string
		activation bool
	}{
		{string(EventGranted), outbox.PriorityStandard, GrantActive, false},
		{string(EventActivated), outbox.PriorityStandard, GrantActive, true},
		{string(EventEnded), outbox.PriorityHigh, GrantActive, false},
		{string(EventRevoked), outbox.PriorityHigh, GrantRevoked, false},
	}
	if len(events) != len(want) {
		t.Fatalf("published %d events for the grant, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		e := events[i]
		switch {
		case e.eventType != w.eventType:
			t.Errorf("event %d is %s, want %s", i, e.eventType, w.eventType)
		case e.priority != w.priority:
			t.Errorf("%s travelled in lane %d, want %d", e.eventType, e.priority, w.priority)
		case e.state.GrantVersion != int64(i+1) || e.history != e.state.GrantVersion:
			t.Errorf("%s carries version %d and its history %d, want %d", e.eventType, e.state.GrantVersion, e.history, i+1)
		case e.state.GrantStatus != w.status || (e.state.Activation != nil) != w.activation:
			t.Errorf("%s carries status %q and activation %+v", e.eventType, e.state.GrantStatus, e.state.Activation)
		case e.state.PrincipalID != holder || e.state.Scope != ScopeIdentityControl || e.state.Kind != KindEligible:
			t.Errorf("%s describes %+v", e.eventType, e.state)
		}
	}
	if a := events[1].state.Activation; a != nil && (a.ActivationID != requested.ID || !a.EndsAt.After(time.Now())) {
		t.Errorf("the activation event carries %+v", a)
	}
}

// This service enforces its own scope from its records, so nothing about those grants is published,
// and nothing about a request that confers nothing.
func TestAnOrganizationControlGrantPublishesNothing(t *testing.T) {
	service, admin, _, asFirst, _ := activations(t, true)
	_, owner, ctx := pools(t)
	holder := newID(t)
	grant, err := admin.Grant(asFirst, holder, Scope, KindEligible, "orders on-call")
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	requested, err := service.Request(actingAs(t, context.Background(), holder), grant.ID, time.Hour, "work")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, err := service.Decide(asFirst, requested.ID, DecisionApproved, "go"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if events := publishedFor(t, ctx, owner, grant.ID); len(events) != 0 {
		t.Errorf("a provider:organization-control grant published %+v", events)
	}

	identity, err := admin.Grant(asFirst, newID(t), ScopeIdentityControl, KindEligible, "identity on-call")
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	pending, err := service.Request(actingAs(t, context.Background(), identity.Principal), identity.ID, time.Hour, "work")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	// asFirst holds no provider:identity-control grant, so it cannot decide this scope's requests.
	if _, err := service.Decide(asFirst, pending.ID, DecisionApproved, "go"); !errors.Is(err, ErrNotHolder) {
		t.Errorf("approving without a grant for the scope answered %v, want ErrNotHolder", err)
	}
	if events := publishedFor(t, ctx, owner, identity.ID); len(events) != 1 || events[0].eventType != string(EventGranted) {
		t.Errorf("a pending request published %+v; only the grant is an event", events)
	}
}

func TestAGrantNamesARegisteredScope(t *testing.T) {
	admin, _, asFirst, _ := administration(t)
	for _, scope := range []string{"", "provider:billing", "provider:organization-control "} {
		if _, err := admin.Grant(asFirst, newID(t), scope, KindEligible, "x"); !errors.Is(err, ErrInvalid) {
			t.Errorf("a grant of scope %q answered %v, want ErrInvalid", scope, err)
		}
	}
}

// The last provider:organization-control grant stays, because only it can grant again. The last
// provider:identity-control grant confers nothing here, so it may go.
func TestOnlyTheLastOrganizationControlGrantIsKept(t *testing.T) {
	admin, _, asFirst, first := administration(t)
	identity, err := admin.Grant(asFirst, newID(t), ScopeIdentityControl, KindEmergency, "identity break-glass")
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := admin.Revoke(asFirst, identity.ID, "replaced"); err != nil {
		t.Errorf("revoking the last identity-control grant answered %v", err)
	}
	list, err := admin.List(asFirst, "find the bootstrap grant")
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range list {
		if record.Principal == first && record.Scope == Scope {
			if _, err := admin.Revoke(asFirst, record.ID, "last"); !errors.Is(err, ErrLastGrant) {
				t.Errorf("revoking the last organization-control grant answered %v, want ErrLastGrant", err)
			}
		}
	}
}

// A holder of a grant for another scope is eligible here: the activation routes are where a grant of
// any scope is activated. It holds no authority over this service.
func TestAnIdentityControlHolderIsEligibleAndNotInForceHere(t *testing.T) {
	admin, records, asFirst, _ := administration(t)
	holder := newID(t)
	if _, err := admin.Grant(asFirst, holder, ScopeIdentityControl, KindEmergency, "identity break-glass"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	standing, err := records.ProviderStanding(context.Background(), holder)
	if err != nil {
		t.Fatal(err)
	}
	if !standing.Holder || standing.InForce || standing.Emergency {
		t.Errorf("an identity-control emergency holder reads as %+v here; want a holder with no authority over this service", standing)
	}
}

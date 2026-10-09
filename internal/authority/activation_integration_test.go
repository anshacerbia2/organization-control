package authority

// Provider activations against the real engine as the provider role (ADR-ORG-002,
// TDD-organization-control-001 §Provider Activation).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

func activations(t *testing.T, approval bool) (*Activations, *Administration, *Reader, context.Context, id.UUID) {
	t.Helper()
	admin, records, ctx, first := administration(t)
	provider, _, _ := pools(t)
	pool, err := db.NewProviderPool(provider, noEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewActivations(pool, ActivationPolicy{Max: 8 * time.Hour, ApprovalRequired: approval})
	if err != nil {
		t.Fatal(err)
	}
	return service, admin, records, ctx, first
}

func TestAnEligibleGrantConfersAuthorityOnlyWhileAnApprovedActivationLasts(t *testing.T) {
	service, admin, records, asFirst, first := activations(t, true)
	second := newID(t)
	grant, err := admin.Grant(asFirst, second, Scope, KindEligible, "orders on-call")
	if err != nil {
		t.Fatal(err)
	}
	asSecond := actingAs(t, context.Background(), second)

	standing, err := records.ProviderStanding(asSecond, second)
	if err != nil || !standing.Holder || standing.InForce {
		t.Fatalf("an eligible grant reads as %+v, %v; want a holder not in force", standing, err)
	}

	requested, err := service.Request(asSecond, grant.ID, time.Hour, "rotate the tenant keys")
	if err != nil || requested.Decision != "" || !requested.ApprovalRequired {
		t.Fatalf("request: %+v, %v", requested, err)
	}
	if _, err := service.Request(asSecond, grant.ID, time.Hour, "again"); !errors.Is(err, ErrActivationPending) {
		t.Errorf("a second pending request answered %v, want ErrActivationPending", err)
	}
	if standing, _ := records.ProviderStanding(asSecond, second); standing.InForce {
		t.Error("a pending request confers authority")
	}

	// Its holder approves nothing; another holder, in force or not, approves.
	if _, err := service.Decide(asSecond, requested.ID, DecisionApproved, "mine"); !errors.Is(err, ErrSelfApproval) {
		t.Errorf("the holder's own approval answered %v, want ErrSelfApproval", err)
	}
	approved, err := service.Decide(asFirst, requested.ID, DecisionApproved, "the rotation is scheduled")
	if err != nil || approved.Decision != DecisionApproved || approved.EndsAt == nil || approved.DecidedBy == nil || *approved.DecidedBy != first {
		t.Fatalf("approve: %+v, %v", approved, err)
	}
	if standing, _ := records.ProviderStanding(asSecond, second); !standing.InForce || standing.Emergency ||
		standing.Activation != requested.ID {
		t.Errorf("an approved activation reads as %+v; want in force by activation %s, not emergency",
			standing, requested.ID)
	}
	if _, err := service.Request(asSecond, grant.ID, time.Hour, "more"); !errors.Is(err, ErrActivationInForce) {
		t.Errorf("a request while in force answered %v, want ErrActivationInForce", err)
	}

	ended, err := service.End(asSecond, requested.ID, false, "done early")
	if err != nil || ended.EndedAt == nil || ended.EndedBy == nil || *ended.EndedBy != second {
		t.Fatalf("end: %+v, %v", ended, err)
	}
	if standing, _ := records.ProviderStanding(asSecond, second); standing.InForce {
		t.Error("an ended activation still confers authority")
	}
	if _, err := service.End(asSecond, requested.ID, false, "again"); !errors.Is(err, ErrActivationNotInForce) {
		t.Errorf("ending twice answered %v, want ErrActivationNotInForce", err)
	}
}

func TestTheDatabaseRefusesASelfApprovedActivation(t *testing.T) {
	service, admin, _, asFirst, _ := activations(t, true)
	second := newID(t)
	grant, err := admin.Grant(asFirst, second, Scope, KindEligible, "on-call")
	if err != nil {
		t.Fatal(err)
	}
	requested, err := service.Request(actingAs(t, context.Background(), second), grant.ID, time.Hour, "work")
	if err != nil {
		t.Fatal(err)
	}
	_, owner, ctx := pools(t)
	err = owner.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, decideActivationStatement, requested.ID.String(), second.String(), DecisionApproved, "mine")
		return err
	})
	if err == nil {
		t.Fatal("the database recorded an activation approved by its own holder")
	}
}

func TestARevokedGrantEndsItsActivationsAuthority(t *testing.T) {
	service, admin, records, asFirst, _ := activations(t, true)
	second := newID(t)
	grant, err := admin.Grant(asFirst, second, Scope, KindEligible, "on-call")
	if err != nil {
		t.Fatal(err)
	}
	asSecond := actingAs(t, context.Background(), second)
	requested, err := service.Request(asSecond, grant.ID, time.Hour, "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Decide(asFirst, requested.ID, DecisionApproved, "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Revoke(asFirst, grant.ID, "left the team"); err != nil {
		t.Fatal(err)
	}
	if standing, _ := records.ProviderStanding(asSecond, second); standing.InForce || standing.Holder {
		t.Errorf("a revoked grant's activation reads as %+v", standing)
	}
}

func TestARequestIsRefusedWhenItBreaksARule(t *testing.T) {
	service, admin, _, asFirst, first := activations(t, true)
	second, stranger := newID(t), newID(t)
	grant, err := admin.Grant(asFirst, second, Scope, KindEligible, "on-call")
	if err != nil {
		t.Fatal(err)
	}
	asSecond := actingAs(t, context.Background(), second)
	list, err := admin.List(asFirst, "find the bootstrap grant")
	if err != nil {
		t.Fatal(err)
	}
	var emergency id.UUID
	for _, record := range list {
		if record.Principal == first {
			emergency = record.ID
		}
	}

	for name, c := range map[string]struct {
		ctx      context.Context
		grant    id.UUID
		duration time.Duration
		reason   string
		want     error
	}{
		"too long":         {asSecond, grant.ID, 9 * time.Hour, "work", ErrActivationTooLong},
		"no reason":        {asSecond, grant.ID, time.Hour, " ", ErrActivationInvalid},
		"someone else's":   {actingAs(t, context.Background(), stranger), grant.ID, time.Hour, "work", ErrNotHolder},
		"unknown grant":    {asSecond, newID(t), time.Hour, "work", ErrNotHolder},
		"emergency grant":  {asFirst, emergency, time.Hour, "work", ErrEmergencyGrant},
		"under one second": {asSecond, grant.ID, time.Millisecond, "work", ErrActivationInvalid},
	} {
		if _, err := service.Request(c.ctx, c.grant, c.duration, c.reason); !errors.Is(err, c.want) {
			t.Errorf("%s answered %v, want %v", name, err, c.want)
		}
	}

	requested, err := service.Request(asSecond, grant.ID, time.Hour, "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Decide(actingAs(t, context.Background(), stranger), requested.ID, DecisionApproved, "x"); !errors.Is(err, ErrNotHolder) {
		t.Errorf("a decision by a non-holder answered %v, want ErrNotHolder", err)
	}
	denied, err := service.Decide(asFirst, requested.ID, DecisionDenied, "not today")
	if err != nil || denied.Decision != DecisionDenied || denied.EndsAt != nil {
		t.Fatalf("deny: %+v, %v", denied, err)
	}
	if _, err := service.Decide(asFirst, requested.ID, DecisionApproved, "changed my mind"); !errors.Is(err, ErrActivationDecided) {
		t.Errorf("approving a denied request answered %v, want ErrActivationDecided", err)
	}
	if _, err := service.Decide(asFirst, newID(t), DecisionApproved, "x"); !errors.Is(err, ErrActivationNotFound) {
		t.Errorf("deciding an unknown activation answered %v, want ErrActivationNotFound", err)
	}
}

func TestAPendingRequestLapsesAfterADay(t *testing.T) {
	service, admin, _, asFirst, _ := activations(t, true)
	second := newID(t)
	grant, err := admin.Grant(asFirst, second, Scope, KindEligible, "on-call")
	if err != nil {
		t.Fatal(err)
	}
	asSecond := actingAs(t, context.Background(), second)
	old, err := service.Request(asSecond, grant.ID, time.Hour, "yesterday")
	if err != nil {
		t.Fatal(err)
	}
	_, owner, ctx := pools(t)
	exec(t, ctx, owner, `UPDATE organization.provider_activation SET requested_at = now() - interval '25 hours'
	    WHERE activation_id = $1`, old.ID.String())

	if _, err := service.Decide(asFirst, old.ID, DecisionApproved, "late"); !errors.Is(err, ErrActivationDecided) {
		t.Errorf("approving a lapsed request answered %v, want ErrActivationDecided", err)
	}
	fresh, err := service.Request(asSecond, grant.ID, time.Hour, "today")
	if err != nil || fresh.ID == old.ID {
		t.Fatalf("a request after the lapse answered %+v, %v", fresh, err)
	}
	list, err := service.List(asFirst, "review")
	if err != nil {
		t.Fatal(err)
	}
	for _, activation := range list {
		if activation.ID == old.ID && activation.Decision != DecisionLapsed {
			t.Errorf("the old request is %q, want lapsed", activation.Decision)
		}
	}
}

func TestWithApprovalOptionalAHolderActivatesWithAReason(t *testing.T) {
	service, admin, records, asFirst, _ := activations(t, false)
	second := newID(t)
	grant, err := admin.Grant(asFirst, second, Scope, KindEligible, "dev")
	if err != nil {
		t.Fatal(err)
	}
	asSecond := actingAs(t, context.Background(), second)
	activated, err := service.Request(asSecond, grant.ID, time.Hour, "local work")
	if err != nil || activated.Decision != DecisionApproved || activated.ApprovalRequired || *activated.DecidedBy != second {
		t.Fatalf("a self-activation answered %+v, %v", activated, err)
	}
	if standing, _ := records.ProviderStanding(asSecond, second); !standing.InForce {
		t.Error("a self-activation is not in force")
	}
}

// A holder reads its own unrevoked grants, of either kind, and nobody else's: how an eligible holder
// learns the grant_id it can activate (TDD-organization-control-001 §Provider Activation).
func TestAHolderReadsOnlyItsOwnUnrevokedGrants(t *testing.T) {
	service, admin, _, asFirst, first := activations(t, true)
	second, third := newID(t), newID(t)
	kept, err := admin.Grant(asFirst, second, Scope, KindEligible, "on-call")
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := admin.Grant(asFirst, second, ScopeIdentityControl, KindEligible, "identity on-call")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Revoke(asFirst, revoked.ID, "no longer on the identity rota"); err != nil {
		t.Fatal(err)
	}
	others, err := admin.Grant(asFirst, third, Scope, KindEligible, "another on-call")
	if err != nil {
		t.Fatal(err)
	}

	held, err := service.Grants(actingAs(t, context.Background(), second), "find what I can activate")
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].ID != kept.ID || held[0].Principal != second || held[0].Kind != KindEligible ||
		held[0].Scope != Scope || held[0].RevokedAt != nil {
		t.Fatalf("the second holder read %+v; want only its unrevoked grant %s", held, kept.ID)
	}
	for _, record := range held {
		if record.ID == others.ID || record.ID == revoked.ID {
			t.Errorf("the second holder read grant %s, which is not its own unrevoked grant", record.ID)
		}
	}

	// The bootstrap provider holds the emergency grant, and reads it as its own.
	mine, err := service.Grants(asFirst, "find what I hold")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Principal != first || mine[0].Kind != KindEmergency {
		t.Errorf("the bootstrap provider read %+v; want its one emergency grant", mine)
	}

	if _, err := service.Grants(context.Background(), "no scope"); !errors.Is(err, db.ErrNoScope) {
		t.Errorf("an unscoped read answered %v, want db.ErrNoScope", err)
	}
	if _, err := service.Grants(actingAs(t, context.Background(), second), ""); err == nil {
		t.Error("a read without a reason was accepted")
	}
}

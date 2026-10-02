package authority

// Granting and revoking through the provider scope, against the real engine as the provider role.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// noEvidence satisfies the provider pool's mandatory recorder. The evidence row is internal/access's
// to write and test; what is asserted here is the grant table.
type noEvidence struct{}

func (noEvidence) RecordProviderAccess(context.Context, db.ProviderAccess) error { return nil }

// administration builds the grant administration on the provider role, with the grant table emptied
// and one bootstrap provider granted, and returns a context acting as that provider.
func administration(t *testing.T) (*Administration, *Reader, context.Context, id.UUID) {
	t.Helper()
	provider, owner, ctx := pools(t)
	emptyGrants(t, ctx, owner)
	grants, err := NewGrants(provider)
	if err != nil {
		t.Fatal(err)
	}
	first := newID(t)
	if _, err := grants.Bootstrap(ctx, Bootstrap{Principal: first, Operator: "Ada Operator", Reason: "first provider"}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	pool, err := db.NewProviderPool(provider, noEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := NewAdministration(pool)
	if err != nil {
		t.Fatal(err)
	}
	records, err := NewReader(provider)
	if err != nil {
		t.Fatal(err)
	}
	return admin, records, actingAs(t, ctx, first), first
}

func actingAs(t *testing.T, ctx context.Context, actor id.UUID) context.Context {
	t.Helper()
	scope, err := db.ProviderScope(actor, newID(t))
	if err != nil {
		t.Fatal(err)
	}
	return db.WithScope(ctx, scope)
}

func TestAProviderGrantsAndRevokesAnother(t *testing.T) {
	admin, records, ctx, first := administration(t)
	second := newID(t)

	granted, err := admin.Grant(ctx, second, KindEligible, "a second operator")
	if err != nil || granted.Principal != second || granted.GrantedBy == nil || *granted.GrantedBy != first {
		t.Fatalf("the grant answered %+v, %v; want granted_by the calling provider", granted, err)
	}
	if ok, err := holderOf(ctx, records, second); err != nil || !ok {
		t.Errorf("the granted Principal reads as granted=%t, %v", ok, err)
	}
	if _, err := admin.Grant(ctx, second, KindEligible, "again"); !errors.Is(err, ErrAlreadyGranted) {
		t.Errorf("a second active grant answered %v, want ErrAlreadyGranted", err)
	}

	revoked, err := admin.Revoke(ctx, granted.ID, "the operator left")
	if err != nil || revoked.RevokedAt == nil || revoked.RevokedBy == nil || *revoked.RevokedBy != first ||
		revoked.RevokeReason != "the operator left" {
		t.Fatalf("the revocation answered %+v, %v", revoked, err)
	}
	if ok, err := holderOf(ctx, records, second); err != nil || ok {
		t.Errorf("a revoked Principal reads as granted=%t, %v", ok, err)
	}
	if _, err := admin.Revoke(ctx, granted.ID, "again"); !errors.Is(err, ErrAlreadyRevoked) {
		t.Errorf("revoking twice answered %v, want ErrAlreadyRevoked", err)
	}
	if _, err := admin.Revoke(ctx, newID(t), "nothing"); !errors.Is(err, ErrGrantNotFound) {
		t.Errorf("revoking an unknown grant answered %v, want ErrGrantNotFound", err)
	}

	// Granted again, as a new row beside the revoked one, which stays listed.
	again, err := admin.Grant(ctx, second, KindEligible, "back")
	if err != nil || again.ID == granted.ID {
		t.Fatalf("granting again answered %+v, %v; want a new grant", again, err)
	}
	list, err := admin.List(ctx, "review")
	if err != nil {
		t.Fatal(err)
	}
	var revokedListed bool
	for _, record := range list {
		revokedListed = revokedListed || (record.ID == granted.ID && record.RevokedAt != nil)
	}
	if !revokedListed || len(list) != 3 {
		t.Errorf("the list holds %d grants, revoked one listed=%t; want the bootstrap, the revoked and the new", len(list), revokedListed)
	}
}

func TestTheLastActiveGrantIsNotRevoked(t *testing.T) {
	admin, records, ctx, first := administration(t)
	list, err := admin.List(ctx, "review")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v, %v", list, err)
	}
	if _, err := admin.Revoke(ctx, list[0].ID, "leaving"); !errors.Is(err, ErrLastGrant) {
		t.Errorf("revoking the last grant answered %v, want ErrLastGrant", err)
	}
	if ok, err := holderOf(ctx, records, first); err != nil || !ok {
		t.Errorf("the last provider reads as granted=%t, %v", ok, err)
	}
}

// Two revocations of the last two grants at once: the lock makes the second count one, so one
// provider remains.
func TestConcurrentRevocationsLeaveAProvider(t *testing.T) {
	admin, _, ctx, _ := administration(t)
	second, err := admin.Grant(ctx, newID(t), KindEligible, "a second operator")
	if err != nil {
		t.Fatal(err)
	}
	list, err := admin.List(ctx, "review")
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v, %v", list, err)
	}

	var wg sync.WaitGroup
	results := make([]error, len(list))
	for i, record := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = admin.Revoke(ctx, record.ID, "both at once")
		}()
	}
	wg.Wait()

	succeeded, refused := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrLastGrant):
			refused++
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Errorf("two concurrent revocations gave %v; want one revoked and one ErrLastGrant (second grant %s)", results, second.ID)
	}
}

func TestTheProviderRoleCannotRewriteAGrant(t *testing.T) {
	provider, owner, ctx := pools(t)
	emptyGrants(t, ctx, owner)
	grants, err := NewGrants(provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Bootstrap(ctx, Bootstrap{Principal: newID(t), Operator: "Ada", Reason: "first"}); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"change the grantee": `UPDATE organization.provider_grant SET principal_id = gen_random_uuid()`,
		"change the reason":  `UPDATE organization.provider_grant SET reason = 'rewritten'`,
	} {
		err := provider.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, statement)
			return err
		})
		if err == nil {
			t.Errorf("the provider role could %s", name)
		}
	}
}

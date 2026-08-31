package db_test

// The idempotency claim, against the real engine as the real runtime roles.
//
// The property under test is the one that decided where the claim lives: it commits with the effect
// it guards. A middleware claiming in a transaction of its own would commit separately, and a key
// held by a mutation that then rolled back refuses every retry of a request that never happened.
// That failure is silent — the caller is told "already in progress" about work nothing did — so it is
// asserted here by rolling a mutation back and showing the key came back with it.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/idempotency"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// claimFixture is a tenant-scoped pool, a bound context, and a claim nobody else is using.
type claimFixture struct {
	pool  *db.TenantPool
	store *db.ClaimStore
	ctx   context.Context
	claim db.Claim
	t     *testing.T
}

func newClaimFixture(t *testing.T) *claimFixture {
	t.Helper()

	conns, ctx := poolAs(t, "organization_app", "TEST_RUNTIME_PASSWORD")
	pool, err := db.NewTenantPool(conns)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	store, err := db.NewClaimStore(conns)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}

	scope, err := db.TenantScope(mustParse(t, tenantA), mustUUIDValue(t), mustUUIDValue(t))
	if err != nil {
		t.Fatalf("TenantScope: %v", err)
	}

	// The key is minted per test. A fixed one would make two runs of the same test collide, and the
	// second would fail as "already in progress" — which is the correct behaviour reported as a
	// broken test.
	claim := db.Claim{
		Scope:  "tenant:" + tenantA + ":subject:" + mustUUIDValue(t).String(),
		Key:    mustUUIDValue(t).String(),
		Digest: db.Digest([]byte("POST"), []byte("/v1/memberships"), []byte(`{"a":1}`)),
	}

	f := &claimFixture{pool: pool, store: store, ctx: db.WithScope(ctx, scope), claim: claim, t: t}
	t.Cleanup(func() { f.forgetKey(claim) })
	return f
}

func mustUUIDValue(t *testing.T) id.UUID {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	return value
}

// keyState reports whether the key exists and whether it carries a completed response.
func (f *claimFixture) keyState(claim db.Claim) (exists bool, completed bool) {
	f.t.Helper()
	if err := db.WithTenantScope(f.ctx, f.pool, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) > 0,
		       coalesce(bool_or(completed_at IS NOT NULL), false)
		FROM platform.idempotency_key WHERE scope = $1 AND key = $2`,
			claim.Scope, claim.Key).Scan(&exists, &completed)
	}); err != nil {
		f.t.Fatalf("read the key: %v", err)
	}
	return exists, completed
}

func (f *claimFixture) forgetKey(claim db.Claim) {
	_ = db.WithTenantScope(f.ctx, f.pool, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM platform.idempotency_key WHERE scope = $1 AND key = $2`,
			claim.Scope, claim.Key)
		return err
	})
}

// grantMembership is a real mutation in a Row-Level-Security-protected table, so the claim is
// committing alongside something that matters rather than alongside nothing.
func (f *claimFixture) grantMembership(ctx context.Context, tx db.Tx, membershipID id.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO membership.membership
	    (membership_id, principal_id, tenant_id, subject_type, status, valid_from, provenance)
	VALUES ($1, $2, $3, 'human', 'active', now(), 'idempotency suite')`,
		membershipID.String(), mustUUIDValue(f.t).String(), tenantA)
	return err
}

func (f *claimFixture) membershipExists(membershipID id.UUID) bool {
	f.t.Helper()
	var exists bool
	if err := db.WithTenantScope(f.ctx, f.pool, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) > 0 FROM membership.membership WHERE membership_id = $1`,
			membershipID.String()).Scan(&exists)
	}); err != nil {
		f.t.Fatalf("read the membership: %v", err)
	}
	return exists
}

func (f *claimFixture) forgetMembership(membershipID id.UUID) {
	_ = db.WithTenantScope(f.ctx, f.pool, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM membership.membership WHERE membership_id = $1`,
			membershipID.String())
		return err
	})
}

// TestAClaimCommitsWithTheEffectItGuards is the whole reason the claim is made here.
func TestAClaimCommitsWithTheEffectItGuards(t *testing.T) {
	f := newClaimFixture(t)

	membershipID := mustUUIDValue(t)
	t.Cleanup(func() { f.forgetMembership(membershipID) })

	if err := db.WithTenantScope(db.WithClaim(f.ctx, f.claim), f.pool,
		func(ctx context.Context, tx db.Tx) error {
			return f.grantMembership(ctx, tx, membershipID)
		}); err != nil {
		t.Fatalf("the claimed mutation failed: %v", err)
	}

	if !f.membershipExists(membershipID) {
		t.Error("the mutation did not commit")
	}
	if exists, _ := f.keyState(f.claim); !exists {
		t.Error("the mutation committed and its key did not, so a retry would run it again")
	}
}

// TestAFailedMutationReleasesItsKey is the failure direction, and the one a separately committed
// claim gets wrong.
//
// A key held by work that never happened refuses every retry of it, and the refusal says "already in
// progress" — which sends whoever is debugging to look for a concurrent request that does not exist.
func TestAFailedMutationReleasesItsKey(t *testing.T) {
	f := newClaimFixture(t)

	membershipID := mustUUIDValue(t)
	sentinel := errors.New("the mutation failed after the claim")

	err := db.WithTenantScope(db.WithClaim(f.ctx, f.claim), f.pool,
		func(ctx context.Context, tx db.Tx) error {
			if err := f.grantMembership(ctx, tx, membershipID); err != nil {
				return err
			}
			return sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("the scope returned %v, want the injected failure", err)
	}

	if f.membershipExists(membershipID) {
		t.Error("the failed mutation committed")
	}
	if exists, _ := f.keyState(f.claim); exists {
		t.Fatal("the key survived a rolled-back mutation, so every retry would be refused as " +
			"already in progress for work that never happened")
	}

	// And the retry is therefore free to run, which is the point.
	retryID := mustUUIDValue(t)
	t.Cleanup(func() { f.forgetMembership(retryID) })
	if err := db.WithTenantScope(db.WithClaim(f.ctx, f.claim), f.pool,
		func(ctx context.Context, tx db.Tx) error {
			return f.grantMembership(ctx, tx, retryID)
		}); err != nil {
		t.Fatalf("the retry after a released key was refused: %v", err)
	}
	if !f.membershipExists(retryID) {
		t.Error("the retry did not commit")
	}
}

// TestASecondUseOfAnUncompletedKeyIsRefused covers the window the chosen design leaves open.
//
// The response is recorded after the domain transaction, so between the two a key is claimed and
// uncompleted. A retry arriving there is refused rather than replayed and rather than re-executed.
// The mutation still happened exactly once, which is the half that matters.
func TestASecondUseOfAnUncompletedKeyIsRefused(t *testing.T) {
	f := newClaimFixture(t)

	membershipID := mustUUIDValue(t)
	t.Cleanup(func() { f.forgetMembership(membershipID) })
	if err := db.WithTenantScope(db.WithClaim(f.ctx, f.claim), f.pool,
		func(ctx context.Context, tx db.Tx) error {
			return f.grantMembership(ctx, tx, membershipID)
		}); err != nil {
		t.Fatalf("the first use failed: %v", err)
	}

	ran := false
	err := db.WithTenantScope(db.WithClaim(f.ctx, f.claim), f.pool,
		func(context.Context, db.Tx) error {
			ran = true
			return nil
		})
	if !errors.Is(err, idempotency.ErrInProgress) {
		t.Fatalf("the second use returned %v, want ErrInProgress", err)
	}
	if ran {
		t.Error("the body ran for a key whose first use had not completed")
	}
}

// TestACompletedKeyReplaysItsResponseWithoutRunningTheBody is the payoff.
func TestACompletedKeyReplaysItsResponseWithoutRunningTheBody(t *testing.T) {
	f := newClaimFixture(t)

	membershipID := mustUUIDValue(t)
	t.Cleanup(func() { f.forgetMembership(membershipID) })
	if err := db.WithTenantScope(db.WithClaim(f.ctx, f.claim), f.pool,
		func(ctx context.Context, tx db.Tx) error {
			return f.grantMembership(ctx, tx, membershipID)
		}); err != nil {
		t.Fatalf("the first use failed: %v", err)
	}

	stored := json.RawMessage(`{"membership_id":"` + membershipID.String() + `","status":"active"}`)
	if err := f.store.Complete(f.ctx, f.claim, 201, stored); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, completed := f.keyState(f.claim); !completed {
		t.Fatal("Complete did not record the response")
	}

	ran := false
	err := db.WithTenantScope(db.WithClaim(f.ctx, f.claim), f.pool,
		func(context.Context, db.Tx) error {
			ran = true
			return nil
		})

	var replayed *db.Replayed
	if !errors.As(err, &replayed) {
		t.Fatalf("the retry returned %v, want a *db.Replayed", err)
	}
	if ran {
		t.Error("the body ran on a replay, so the mutation would have happened twice")
	}
	if replayed.Status != 201 {
		t.Errorf("the replay carries status %d, want 201", replayed.Status)
	}

	// Semantically equal, not byte-identical, and the difference is a property of the store rather
	// than of this code: `platform.idempotency_key.response_body` is `jsonb`, so PostgreSQL
	// normalises the document — object keys come back sorted and insignificant whitespace is gone.
	//
	// Asserted this way deliberately. A byte comparison would fail for a correct replay, and
	// changing the column to `text` to make one pass would trade a real benefit — the store
	// rejecting a malformed document — for a guarantee no client should be relying on. What a caller
	// is owed is the same response, not the same bytes; anything hashing or signing the body has to
	// canonicalise first, which is true of every JSON API and worth stating rather than discovering.
	var replayedDoc, storedDoc any
	if err := json.Unmarshal(replayed.Body, &replayedDoc); err != nil {
		t.Fatalf("the replayed body is not JSON: %v", err)
	}
	if err := json.Unmarshal(stored, &storedDoc); err != nil {
		t.Fatalf("the stored body is not JSON: %v", err)
	}
	replayedCanonical, err := json.Marshal(replayedDoc)
	if err != nil {
		t.Fatalf("canonicalise the replay: %v", err)
	}
	storedCanonical, err := json.Marshal(storedDoc)
	if err != nil {
		t.Fatalf("canonicalise the stored body: %v", err)
	}
	if string(replayedCanonical) != string(storedCanonical) {
		t.Errorf("the replay carries body %s, want %s", replayedCanonical, storedCanonical)
	}
}

// TestAKeyReusedForADifferentRequestIsRefused stops a caller being told the wrong thing happened.
func TestAKeyReusedForADifferentRequestIsRefused(t *testing.T) {
	f := newClaimFixture(t)

	membershipID := mustUUIDValue(t)
	t.Cleanup(func() { f.forgetMembership(membershipID) })
	if err := db.WithTenantScope(db.WithClaim(f.ctx, f.claim), f.pool,
		func(ctx context.Context, tx db.Tx) error {
			return f.grantMembership(ctx, tx, membershipID)
		}); err != nil {
		t.Fatalf("the first use failed: %v", err)
	}
	if err := f.store.Complete(f.ctx, f.claim, 201, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	different := f.claim
	different.Digest = db.Digest([]byte("POST"), []byte("/v1/memberships"), []byte(`{"a":2}`))

	ran := false
	err := db.WithTenantScope(db.WithClaim(f.ctx, different), f.pool,
		func(context.Context, db.Tx) error {
			ran = true
			return nil
		})
	if !errors.Is(err, idempotency.ErrConflict) {
		t.Fatalf("a key reused for a different request returned %v, want ErrConflict", err)
	}
	if ran {
		t.Error("the body ran for a conflicting key")
	}
}

// TestOneRequestOpeningTwoScopesClaimsOnce is why the claim carries a consumption flag.
//
// `internal/invitation` binds a tenant pool and a provider pool within one call. Without the flag the
// second transaction would read its own uncommitted first claim and refuse the request as already in
// progress — a request failing on its own success.
func TestOneRequestOpeningTwoScopesClaimsOnce(t *testing.T) {
	f := newClaimFixture(t)

	// One context, carrying one claim, used for two scoped transactions in sequence.
	ctx := db.WithClaim(f.ctx, f.claim)

	first := mustUUIDValue(t)
	t.Cleanup(func() { f.forgetMembership(first) })
	if err := db.WithTenantScope(ctx, f.pool, func(ctx context.Context, tx db.Tx) error {
		return f.grantMembership(ctx, tx, first)
	}); err != nil {
		t.Fatalf("the first scope failed: %v", err)
	}

	second := mustUUIDValue(t)
	t.Cleanup(func() { f.forgetMembership(second) })
	if err := db.WithTenantScope(ctx, f.pool, func(ctx context.Context, tx db.Tx) error {
		return f.grantMembership(ctx, tx, second)
	}); err != nil {
		t.Fatalf("the second scope of the same request was refused: %v", err)
	}

	if !f.membershipExists(second) {
		t.Error("the second scope's work did not commit")
	}
}

// TestARequestWithNoClaimIsUnaffected keeps the seam invisible to everything that does not use it.
func TestARequestWithNoClaimIsUnaffected(t *testing.T) {
	f := newClaimFixture(t)

	membershipID := mustUUIDValue(t)
	t.Cleanup(func() { f.forgetMembership(membershipID) })

	if err := db.WithTenantScope(f.ctx, f.pool, func(ctx context.Context, tx db.Tx) error {
		return f.grantMembership(ctx, tx, membershipID)
	}); err != nil {
		t.Fatalf("an unclaimed mutation failed: %v", err)
	}
	if exists, _ := f.keyState(f.claim); exists {
		t.Error("a request carrying no claim wrote a key anyway")
	}
}

// TestAnIncompleteClaimIsIgnoredRatherThanHalfApplied refuses to guess.
//
// A claim missing its scope, key, or digest is not attached at all. The alternative — attaching it
// and letting `idempotency.Claim` refuse — would turn a caller's malformed header into a failed
// mutation, and the mutation is not what was wrong.
func TestAnIncompleteClaimIsIgnoredRatherThanHalfApplied(t *testing.T) {
	f := newClaimFixture(t)

	membershipID := mustUUIDValue(t)
	t.Cleanup(func() { f.forgetMembership(membershipID) })

	partial := f.claim
	partial.Digest = ""

	if err := db.WithTenantScope(db.WithClaim(f.ctx, partial), f.pool,
		func(ctx context.Context, tx db.Tx) error {
			return f.grantMembership(ctx, tx, membershipID)
		}); err != nil {
		t.Fatalf("a mutation carrying an incomplete claim failed: %v", err)
	}
	if exists, _ := f.keyState(f.claim); exists {
		t.Error("an incomplete claim was written anyway")
	}
	if _, ok := db.ClaimFrom(db.WithClaim(f.ctx, partial)); ok {
		t.Error("an incomplete claim was carried in the context")
	}
}

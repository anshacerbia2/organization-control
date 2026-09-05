package db

// Idempotency, carried through the scope binding rather than bolted onto every service.
//
// # Why it lives here
//
// foundation-platform's `idempotency.Claim` takes a `db.Tx` because the claim has to commit with
// the effect it guards. A middleware could only claim in a transaction of its own, which would
// commit separately — and a key claimed by a mutation that then rolled back is a key that refuses
// every retry of a request that never happened.
//
// This package already opens the transaction for every service in the repository, so the claim goes
// where the transaction is. The alternative was a claim parameter on some thirty service methods
// across eight packages, which is the same property bought at the price of changing every
// signature — and of letting one method be written without it.
//
// # What is and is not guaranteed
//
// The claim commits with the effect: a mutation that fails releases its key, and a mutation that
// succeeds consumes it. That is the property that stops a retry from executing twice.
//
// The stored *response* is written afterwards, by the HTTP surface, in its own transaction —
// `Complete` needs the status and body, and neither exists until the handler has rendered them. So
// there is a window: a process that dies between the domain commit and the completion leaves a key
// claimed and uncompleted, and every later retry of it is refused as in progress rather than
// replayed. The mutation still happened exactly once, which is the half that matters; what is lost
// is the convenience of being told what it returned. Recorded here rather than discovered later.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/anshacerbia2/foundation-platform/idempotency"
)

// Claim is one caller's reservation of one mutation key.
type Claim struct {
	// Scope is the authenticated caller. It is part of the key's identity so two callers cannot
	// collide on a value one of them chose, and so a leaked key is useless to anybody else.
	Scope string

	// Key is the `Idempotency-Key` the caller supplied.
	Key string

	// Digest is a hash of the request the key was first used for. A second request presenting the
	// same key with a different digest is refused rather than served the first one's answer: the
	// caller has reused a key for a different intention, and replaying the earlier response would
	// tell it the wrong thing happened.
	Digest string
}

func (c Claim) valid() bool {
	return strings.TrimSpace(c.Scope) != "" &&
		strings.TrimSpace(c.Key) != "" &&
		strings.TrimSpace(c.Digest) != ""
}

// The two refusals a claim can produce are `idempotency.ErrConflict` — a key reused for a different
// request — and `idempotency.ErrInProgress` — a key whose first use has not completed. They are
// deliberately not re-exported here.
//
// Re-exporting them would put two sentinels in this package that no code path can yet produce,
// because the HTTP surface does not attach a claim yet. `TestSentinelRegistryCoversSource` walks the
// source for exactly that shape and would demand a problem mapping for a refusal that cannot
// happen — a mapping nothing could test. They get mapped from the foundation package at the point
// the surface starts attaching claims, which is when the mapping becomes assertable.
//
// `ErrInProgress` covers two situations the caller cannot tell apart and does not need to: a
// genuinely concurrent retry, and a first attempt whose response was never recorded. Both answers
// are the same — do not retry, re-read the resource — and both are the safe direction.

// A replay returns the same response, not the same bytes.
//
// `platform.idempotency_key.response_body` is `jsonb`, so PostgreSQL normalises what is stored:
// object keys come back sorted and insignificant whitespace is gone. The document is equal, the
// encoding is not. That is worth stating rather than discovering, because anything hashing or
// signing a response body has to canonicalise first — and a client comparing bytes to decide whether
// a retry returned the same thing would conclude it did not.
//
// The column type is foundation-platform's and is the right choice: `jsonb` rejects a malformed
// document at the store, which `text` would accept and replay verbatim.

// Replayed is returned when a claim matched a completed request.
//
// An error rather than a result, because it has to travel out through `Body`, whose signature is
// `error` and whose services know nothing about idempotency. The scoped transaction rolls back on
// it, which is exactly right: a replay must not re-execute the work, and there is nothing to keep
// because the claim read is all that ran.
type Replayed struct {
	Status int
	Body   json.RawMessage
}

func (r *Replayed) Error() string {
	return fmt.Sprintf("db: the request was already completed with status %d", r.Status)
}

// pending is the claim as it travels in a context, with the flag that keeps it from being made
// twice.
//
// A pointer with an atomic flag rather than a plain value, because one request can open more than
// one scoped transaction — `internal/invitation` binds a tenant pool and a provider pool — and the
// second claim of the same key would read its own uncommitted first claim and refuse the request as
// in progress. The first transaction to reach the claim makes it; the rest skip.
//
// Atomic rather than a bare bool: a request is one goroutine today, and a claim silently corrupted
// by a future one that fanned out would be a race the detector could only find if a test happened to
// fan out the same way.
type pending struct {
	claim    Claim
	consumed atomic.Bool

	// made records that the claim was newly reserved, as opposed to skipped or replayed.
	//
	// It exists so the HTTP surface knows whether it owes the store a completion. Without it the
	// surface would call `Complete` on every claimed request including replays, where the key is
	// already complete and the update matches no row — an error on a correct request, logged on
	// every retry a well-behaved client makes.
	made atomic.Bool
}

type claimKey struct{}

// WithClaim carries a claim to be made inside the next scoped transaction.
//
// The HTTP surface puts it here and the services never see it. That is the point: a service cannot
// forget to honour an idempotency key it does not know exists, and cannot honour it in a transaction
// other than its own.
func WithClaim(ctx context.Context, claim Claim) context.Context {
	if !claim.valid() {
		return ctx
	}
	return context.WithValue(ctx, claimKey{}, &pending{claim: claim})
}

// ClaimFrom returns the claim carried by the context, if any.
func ClaimFrom(ctx context.Context) (Claim, bool) {
	held, ok := ctx.Value(claimKey{}).(*pending)
	if !ok {
		return Claim{}, false
	}
	return held.claim, true
}

// ClaimMade reports whether the claim in this context was newly reserved by a scoped transaction
// that then committed.
//
// It is how the caller knows it owes the store a completion. False for a request carrying no claim,
// for one whose claim was replayed, and for one whose transaction rolled back — the last because a
// rollback takes the key with it, so there is nothing to complete.
//
// It is read after the transaction returns, and it reports what the claim attempt decided rather
// than whether the commit succeeded. A transaction that claimed and then failed to commit reports
// true here; the completion that follows finds no claimed row and reports `ErrNotClaimed`, which is
// the correct outcome and the reason completion failures are logged rather than surfaced.
func ClaimMade(ctx context.Context) bool {
	held, ok := ctx.Value(claimKey{}).(*pending)
	return ok && held.made.Load()
}

// claimWithin makes the pending claim inside the caller's transaction.
//
// Called after the scope is bound and before the body runs, so a replay costs one SELECT and does
// none of the work. A request carrying no claim reaches this and returns immediately, which is why
// every read path and every unkeyed mutation is unaffected.
func claimWithin(ctx context.Context, tx Tx) error {
	held, ok := ctx.Value(claimKey{}).(*pending)
	if !ok {
		return nil
	}
	// CompareAndSwap rather than a load-then-store: two transactions racing here would otherwise
	// both see false and both claim, and the second would refuse a request that is proceeding
	// normally.
	if !held.consumed.CompareAndSwap(false, true) {
		return nil
	}

	result, err := idempotency.Claim(ctx, tx, held.claim.Scope, held.claim.Key, held.claim.Digest)
	if err != nil {
		// The flag is released so a caller that retries within the same request — there is no such
		// caller today — does not silently skip the claim it failed to make.
		held.consumed.Store(false)
		return err
	}

	switch result.State {
	case idempotency.StateReplay:
		return &Replayed{Status: result.Status, Body: result.Body}
	case idempotency.StateClaimed:
		held.made.Store(true)
		return nil
	default:
		// StateInProgress arrives as ErrInProgress above, so reaching here means the foundation
		// package grew a state this repository has not been taught. Failing closed rather than
		// proceeding: an unrecognised idempotency decision is not one to guess at.
		return fmt.Errorf("db: unrecognised idempotency state %d for key %q",
			result.State, held.claim.Key)
	}
}

// ClaimStore records the response a completed mutation returned, so a retry can be answered without
// re-executing it.
//
// It holds the raw transactor rather than a scoped pool, and that is deliberate rather than lax.
// `platform.idempotency_key` carries no `tenant_id` and no Row-Level Security policy — its `scope`
// column *is* its isolation, which is why the claim identity includes the caller — so there is no
// binding for this write to need. Routing it through `WithProviderScope` would have bound a scope it
// does not use and written a privileged-access record for a bookkeeping update, filling the evidence
// table an investigation reads with rows about nothing.
type ClaimStore struct{ tx Transactor }

// NewClaimStore constructs the store.
func NewClaimStore(tx Transactor) (*ClaimStore, error) {
	if tx == nil {
		return nil, errors.New("db: a transactor is required")
	}
	return &ClaimStore{tx: tx}, nil
}

// Complete stores the response for future replay.
//
// Its own transaction, and its own failure. A completion that fails leaves the key claimed and the
// mutation committed, which refuses later retries rather than repeating the work — so the caller of
// this is expected to log and carry on rather than turn a successful mutation into an error
// response. Reporting failure to the client would be the worst of both: the work is done and the
// client is told it is not.
func (s *ClaimStore) Complete(ctx context.Context, claim Claim, status int, body json.RawMessage) error {
	if !claim.valid() {
		return errors.New("db: a complete claim is required")
	}
	return s.tx.InTx(ctx, func(ctx context.Context, tx Tx) error {
		return idempotency.Complete(ctx, tx, claim.Scope, claim.Key, claim.Digest, status, body)
	})
}

// Digest hashes the parts of a request that decide whether two uses of one key are the same
// request.
//
// Exposed here so the HTTP surface does not import the foundation package directly for one call, and
// so what goes into a digest is decided in one place.
func Digest(parts ...[]byte) string { return idempotency.Digest(parts...) }

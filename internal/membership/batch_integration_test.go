package membership

// ADR-ORG-004 against a real engine, as the tenant runtime role: a preview is the single command's
// own validation and writes nothing but the batch; execution is held to the version the preview
// read, item by item; and the enforcement state follows the recorded evidence.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// testClassifier stands in for the HTTP surface's translation table, with the same statuses.
func testClassifier(err error) Problem {
	switch {
	case errors.Is(err, ErrNotFound):
		return Problem{Type: "not-found", Status: 404, Detail: err.Error()}
	case errors.Is(err, ErrVersionMismatch):
		return Problem{Type: "version-conflict", Status: 409, Detail: err.Error()}
	case errors.Is(err, ErrRevoked), errors.Is(err, ErrTransitionRefused):
		return Problem{Type: "state-transition-refused", Status: 409, Detail: err.Error()}
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrReasonRequired):
		return Problem{Type: "validation-failed", Status: 400, Detail: err.Error()}
	}
	return Problem{Type: "internal", Status: 500}
}

// cleanupBatches removes the batches a test made in its Tenant. Registered after the Memberships'
// own cleanups, so it runs before them.
func cleanupBatches(t *testing.T, ctx context.Context, batchIDs ...*id.UUID) {
	t.Helper()
	t.Cleanup(func() {
		_ = ownerPool(t, ctx).InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
			for _, batchID := range batchIDs {
				if batchID.IsNil() {
					continue
				}
				_, _ = tx.Exec(ctx, `DELETE FROM membership.membership_batch_item WHERE batch_id = $1`, batchID.String())
				_, _ = tx.Exec(ctx, `DELETE FROM membership.membership_batch WHERE batch_id = $1`, batchID.String())
			}
			return nil
		})
	})
}

func TestAPreviewIsTheSingleCommandsValidationAndWritesOnlyTheBatch(t *testing.T) {
	service, ctx, _ := newFixture(t)
	active := grantOne(t, service, ctx).Membership
	suspended := grantOne(t, service, ctx).Membership
	if _, err := service.Suspend(ctx, at(t, service, ctx, suspended.MembershipID)); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	revoked := grantOne(t, service, ctx).Membership
	if _, err := service.Revoke(ctx, at(t, service, ctx, revoked.MembershipID)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	elsewhere := grantOne(t, service, boundTo(t, context.Background(), tenantB)).Membership
	absent := mustNewID(t)

	before := map[id.UUID]int{}
	for _, m := range []id.UUID{active.MembershipID, suspended.MembershipID, revoked.MembershipID} {
		before[m] = outboxCount(t, service, ctx, m)
	}

	var batchID id.UUID
	cleanupBatches(t, ctx, &batchID)
	ids := []id.UUID{active.MembershipID, suspended.MembershipID, revoked.MembershipID, elsewhere.MembershipID, absent}
	batch, err := service.PreviewBatch(ctx, BatchRequest{Action: ActionRestore, MembershipIDs: ids}, testClassifier)
	if err != nil {
		t.Fatalf("PreviewBatch: %v", err)
	}
	batchID = batch.BatchID

	if batch.WouldChange != 1 || batch.WouldNotChange != 4 || len(batch.Items) != 5 {
		t.Fatalf("counts %d/%d over %d items, want 1 would change and 4 not", batch.WouldChange, batch.WouldNotChange, len(batch.Items))
	}
	// Each refusal is the problem the single command returns for the same Membership.
	for i, membershipID := range ids {
		item := batch.Items[i]
		if item.MembershipID != membershipID || item.Position != i {
			t.Fatalf("item %d is %s at %d", i, item.MembershipID, item.Position)
		}
		var want *Problem
		_, single := service.Restore(ctx, Command{MembershipID: membershipID, ExpectedVersion: versionOr(item.VersionRead), Reason: "x"})
		if single != nil {
			p := testClassifier(single)
			want = &p
		}
		if membershipID == suspended.MembershipID {
			// The single command just restored it; the preview said it would.
			if item.Refusal != nil || item.ResultingStatus == nil || *item.ResultingStatus != StateActive {
				t.Errorf("the suspended Membership: refusal %+v, resulting %v", item.Refusal, item.ResultingStatus)
			}
			continue
		}
		if item.Refusal == nil || want == nil || item.Refusal.Type != want.Type || item.Refusal.Status != want.Status {
			t.Errorf("item %d refusal = %+v, the single command answered %+v", i, item.Refusal, want)
		}
	}
	if batch.Items[3].PrincipalID != nil || batch.Items[4].VersionRead != nil {
		t.Error("a Membership the Tenant cannot read was described")
	}

	// The preview wrote nothing but the batch: no event for any Membership it named.
	for m, count := range before {
		if m == suspended.MembershipID {
			continue // restored above by the single command
		}
		if got := outboxCount(t, service, ctx, m); got != count {
			t.Errorf("%s gained %d outbox rows from a preview", m, got-count)
		}
	}

	read, err := service.GetBatch(ctx, batch.BatchID)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if read.State != BatchPreviewed || len(read.Items) != 5 || read.Items[1].Refusal != nil ||
		read.Items[0].Refusal == nil || read.Items[0].Refusal.Type != "state-transition-refused" {
		t.Errorf("the stored batch reads back as %+v", read)
	}
	if _, err := service.GetBatch(boundTo(t, context.Background(), tenantB), batch.BatchID); !errors.Is(err, ErrBatchNotFound) {
		t.Errorf("another Tenant's batch: error = %v, want ErrBatchNotFound", err)
	}

	// More items than a batch carries is its own refusal, so the surface answers it 413 naming the
	// bound (RFC 7644 §3.7.4), and it is refused before an item is read.
	_, err = service.PreviewBatch(ctx, BatchRequest{Action: ActionSuspend,
		MembershipIDs: make([]id.UUID, MaxBatchItems+1)}, testClassifier)
	if !errors.Is(err, ErrBatchTooLarge) || !strings.Contains(err.Error(), "at most 500 items") {
		t.Errorf("too many items: error = %v, want ErrBatchTooLarge naming the bound", err)
	}

	// The request itself is refused whole when it is malformed.
	for name, req := range map[string]BatchRequest{
		"no items":                      {Action: ActionSuspend},
		"a repeated item":               {Action: ActionSuspend, MembershipIDs: []id.UUID{active.MembershipID, active.MembershipID}},
		"too many items":                {Action: ActionSuspend, MembershipIDs: make([]id.UUID, MaxBatchItems+1)},
		"a grant":                       {Action: ActionGrant, MembershipIDs: []id.UUID{active.MembershipID}},
		"a revocation without a reason": {Action: ActionRevoke, MembershipIDs: []id.UUID{active.MembershipID}},
	} {
		if _, err := service.PreviewBatch(ctx, req, testClassifier); !errors.Is(err, ErrInvalid) &&
			!errors.Is(err, ErrReasonRequired) && !errors.Is(err, ErrBatchTooLarge) {
			t.Errorf("%s: error = %v, want a refusal of the request", name, err)
		}
	}
}

func versionOr(v *int64) int64 {
	if v == nil {
		return 1
	}
	return *v
}

func mustNewID(t *testing.T) id.UUID {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	return value
}

func TestExecutionIsHeldToThePreviewedVersion(t *testing.T) {
	service, ctx, _ := newFixture(t)
	first := grantOne(t, service, ctx).Membership
	changed := grantOne(t, service, ctx).Membership
	third := grantOne(t, service, ctx).Membership
	revoked := grantOne(t, service, ctx).Membership
	if _, err := service.Revoke(ctx, at(t, service, ctx, revoked.MembershipID)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	var batchID id.UUID
	cleanupBatches(t, ctx, &batchID)
	batch, err := service.PreviewBatch(ctx, BatchRequest{
		Action: ActionRevoke, Reason: "the contract ended",
		MembershipIDs: []id.UUID{first.MembershipID, changed.MembershipID, revoked.MembershipID, third.MembershipID},
	}, testClassifier)
	if err != nil {
		t.Fatalf("PreviewBatch: %v", err)
	}
	batchID = batch.BatchID

	// Changed after the preview: suspended and restored, so the status is what the preview saw and
	// the version is not.
	if _, err := service.Suspend(ctx, at(t, service, ctx, changed.MembershipID)); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if _, err := service.Restore(ctx, at(t, service, ctx, changed.MembershipID)); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	executed, err := service.ExecuteBatch(ctx, batch.BatchID, nil, testClassifier)
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	if executed.State != BatchExecuted || executed.CompletedAt == nil || executed.ExecutedAt == nil {
		t.Errorf("batch is %s, completed %v", executed.State, executed.CompletedAt)
	}
	outcome := func(i int) ItemOutcome {
		t.Helper()
		if executed.Items[i].Outcome == nil {
			t.Fatalf("item %d has no outcome", i)
		}
		return *executed.Items[i].Outcome
	}
	for _, i := range []int{0, 3} {
		o := outcome(i)
		if o.Status != OutcomeSucceeded || o.EventID == nil || o.AcceptedAt == nil || o.Version == nil {
			t.Errorf("item %d: %+v, want succeeded with its event", i, o)
		}
		if status, _ := statusAndVersion(t, service, ctx, executed.Items[i].MembershipID); status != StateRevoked {
			t.Errorf("item %d is %s, want revoked", i, status)
		}
	}
	if o := outcome(1); o.Status != OutcomeFailed || o.Problem == nil || o.Problem.Type != "version-conflict" {
		t.Errorf("the changed item: %+v, want failed with version-conflict", o)
	}
	if status, _ := statusAndVersion(t, service, ctx, changed.MembershipID); status != StateActive {
		t.Errorf("the changed Membership is %s; a stale item was applied", status)
	}
	if o := outcome(2); o.Status != OutcomeNotAttempted || o.Reason != ReasonRefusedAtPreview {
		t.Errorf("the item refused at preview: %+v", o)
	}
	if s, f, n := executed.Counts(); s != 2 || f != 1 || n != 1 {
		t.Errorf("counts %d/%d/%d, want 2 succeeded, 1 failed, 1 not attempted", s, f, n)
	}

	// The event row records the batch's reason, as the single revocation's does.
	var reason string
	if err := ownerPool(t, ctx).InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `SELECT reason FROM membership.membership_event WHERE event_id = $1`,
			outcome(0).EventID.String()).Scan(&reason)
	}); err != nil || reason != "the contract ended" {
		t.Errorf("the event row's reason = %q (%v)", reason, err)
	}

	if _, err := service.ExecuteBatch(ctx, batch.BatchID, nil, testClassifier); !errors.Is(err, ErrBatchNotPreviewed) {
		t.Errorf("a second execution: error = %v, want ErrBatchNotPreviewed", err)
	}

	// The failed item, resubmitted as a continuation, keeps the correlation.
	var continuationID id.UUID
	cleanupBatches(t, ctx, &continuationID)
	continuation, err := service.PreviewBatch(ctx, BatchRequest{
		Action: ActionRevoke, Reason: "the contract ended", Continues: batch.BatchID,
		MembershipIDs: []id.UUID{changed.MembershipID},
	}, testClassifier)
	if err != nil {
		t.Fatalf("PreviewBatch continuation: %v", err)
	}
	continuationID = continuation.BatchID
	if continuation.CorrelationID != batch.CorrelationID || continuation.Continues == nil || *continuation.Continues != batch.BatchID {
		t.Errorf("the continuation carries correlation %s and continues %v", continuation.CorrelationID, continuation.Continues)
	}
	if continuation.WouldChange != 1 {
		t.Errorf("the resubmitted item would not change: %+v", continuation.Items)
	}
	if _, err := service.PreviewBatch(ctx, BatchRequest{
		Action: ActionRevoke, Reason: "x", Continues: continuation.BatchID,
		MembershipIDs: []id.UUID{changed.MembershipID},
	}, testClassifier); !errors.Is(err, ErrInvalid) {
		t.Errorf("continuing a batch that never executed: error = %v, want ErrInvalid", err)
	}
}

func TestPastTheErrorAllowanceTheRestIsNotAttempted(t *testing.T) {
	service, ctx, _ := newFixture(t)
	var members []Membership
	for i := 0; i < 4; i++ {
		members = append(members, grantOne(t, service, ctx).Membership)
	}
	var batchID id.UUID
	cleanupBatches(t, ctx, &batchID)
	ids := []id.UUID{members[0].MembershipID, members[1].MembershipID, members[2].MembershipID, members[3].MembershipID}
	batch, err := service.PreviewBatch(ctx, BatchRequest{Action: ActionSuspend, MembershipIDs: ids}, testClassifier)
	if err != nil {
		t.Fatalf("PreviewBatch: %v", err)
	}
	batchID = batch.BatchID

	// The first two change under the preview, so both fail; an allowance of one stops at the second.
	for _, m := range members[:2] {
		if _, err := service.Suspend(ctx, at(t, service, ctx, m.MembershipID)); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
	}
	allowance := 1
	executed, err := service.ExecuteBatch(ctx, batch.BatchID, &allowance, testClassifier)
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	want := []struct{ status, reason string }{
		{OutcomeFailed, ""}, {OutcomeFailed, ""},
		{OutcomeNotAttempted, ReasonErrorAllowance}, {OutcomeNotAttempted, ReasonErrorAllowance},
	}
	for i, w := range want {
		o := executed.Items[i].Outcome
		if o == nil || o.Status != w.status || o.Reason != w.reason {
			t.Errorf("item %d: %+v, want %s %s", i, o, w.status, w.reason)
		}
	}
	for _, m := range members[2:] {
		if status, _ := statusAndVersion(t, service, ctx, m.MembershipID); status != StateActive {
			t.Errorf("%s past the allowance is %s, want untouched", m.MembershipID, status)
		}
	}
	if executed.FailOnErrors == nil || *executed.FailOnErrors != 1 {
		t.Errorf("fail_on_errors = %v", executed.FailOnErrors)
	}
}

func TestAnExpiredPreviewIsRefused(t *testing.T) {
	service, ctx, _ := newFixture(t)
	member := grantOne(t, service, ctx).Membership
	var batchID id.UUID
	cleanupBatches(t, ctx, &batchID)
	batch, err := service.PreviewBatch(ctx, BatchRequest{Action: ActionSuspend, MembershipIDs: []id.UUID{member.MembershipID}}, testClassifier)
	if err != nil {
		t.Fatalf("PreviewBatch: %v", err)
	}
	batchID = batch.BatchID

	fixed := service.now()
	service.now = func() time.Time { return fixed.Add(BatchTTL + time.Second) }
	if _, err := service.ExecuteBatch(ctx, batch.BatchID, nil, testClassifier); !errors.Is(err, ErrBatchExpired) {
		t.Fatalf("an expired preview: error = %v, want ErrBatchExpired", err)
	}
	if status, _ := statusAndVersion(t, service, ctx, member.MembershipID); status != StateActive {
		t.Errorf("an expired preview changed the Membership to %s", status)
	}
	read, err := service.GetBatch(ctx, batch.BatchID)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if read.State != BatchExpired || read.Items[0].Outcome == nil || read.Items[0].Outcome.Reason != ReasonExpired {
		t.Errorf("the expired batch reads as %s with %+v", read.State, read.Items[0].Outcome)
	}
}

func TestAnExecutionReplaysItsIdempotencyKey(t *testing.T) {
	service, ctx, pool := newFixture(t)
	member := grantOne(t, service, ctx).Membership
	var batchID id.UUID
	cleanupBatches(t, ctx, &batchID)
	batch, err := service.PreviewBatch(ctx, BatchRequest{Action: ActionSuspend, MembershipIDs: []id.UUID{member.MembershipID}}, testClassifier)
	if err != nil {
		t.Fatalf("PreviewBatch: %v", err)
	}
	batchID = batch.BatchID

	claim := db.Claim{Scope: "tenant:" + tenantA + ":batch-suite", Key: "execute-" + batch.BatchID.String(), Digest: "digest-1"}
	t.Cleanup(func() {
		_ = ownerPool(t, ctx).InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM platform.idempotency_key WHERE scope = $1 AND key = $2`, claim.Scope, claim.Key)
			return err
		})
	})
	first := db.WithClaim(ctx, claim)
	if _, err := service.ExecuteBatch(first, batch.BatchID, nil, testClassifier); err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	if !db.ClaimMade(first) {
		t.Fatal("the execution did not make its idempotency claim")
	}
	store, err := db.NewClaimStore(pool)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}
	if err := store.Complete(ctx, claim, 200, []byte(`{"replayed":true}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	_, err = service.ExecuteBatch(db.WithClaim(ctx, claim), batch.BatchID, nil, testClassifier)
	var replayed *db.Replayed
	if !errors.As(err, &replayed) || replayed.Status != 200 {
		t.Fatalf("the same key again: error = %v, want the stored response replayed", err)
	}
}

// TestACrashedExecutionResumesWithoutReapplying is TDD-organization-control-002 §Resuming an
// execution against a real engine. A request dies part way through an execution, mid-item, holding
// an Idempotency-Key it never completed. Sent again while its lease is live, the execute is refused;
// once the lease is stale, the same key adopts its own claim and finishes the batch. Every item
// ends applied exactly once: the two finished before the crash keep their outcome and event, the one
// in flight was rolled back and is applied now, and nothing is applied twice.
func TestACrashedExecutionResumesWithoutReapplying(t *testing.T) {
	service, ctx, pool := newFixture(t)
	var members []Membership
	for i := 0; i < 4; i++ {
		members = append(members, grantOne(t, service, ctx).Membership)
	}
	var batchID id.UUID
	cleanupBatches(t, ctx, &batchID)
	ids := []id.UUID{members[0].MembershipID, members[1].MembershipID, members[2].MembershipID, members[3].MembershipID}
	before := map[id.UUID]int{}
	for _, m := range ids {
		before[m] = outboxCount(t, service, ctx, m)
	}
	batch, err := service.PreviewBatch(ctx, BatchRequest{Action: ActionSuspend, MembershipIDs: ids}, testClassifier)
	if err != nil {
		t.Fatalf("PreviewBatch: %v", err)
	}
	batchID = batch.BatchID

	claim := db.Claim{Scope: "tenant:" + tenantA + ":resume-suite", Key: "execute-" + batch.BatchID.String(), Digest: "digest-1"}
	t.Cleanup(func() {
		_ = ownerPool(t, ctx).InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM platform.idempotency_key WHERE scope = $1 AND key = $2`, claim.Scope, claim.Key)
			return err
		})
	})

	base := service.now().UTC()
	service.now = func() time.Time { return base }
	crash := errors.New("the process died while applying the third item")
	service.halt = func(_ context.Context, position int) error {
		if position == 2 {
			return crash
		}
		return nil
	}
	if _, err := service.ExecuteBatch(db.WithClaim(ctx, claim), batch.BatchID, nil, testClassifier); !errors.Is(err, crash) {
		t.Fatalf("the crashed execution: error = %v, want the injected crash", err)
	}
	service.halt = nil

	crashed, err := service.GetBatch(ctx, batch.BatchID)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if crashed.State != BatchExecuting || crashed.HeartbeatAt == nil {
		t.Fatalf("after the crash the batch is %s with heartbeat %v, want executing", crashed.State, crashed.HeartbeatAt)
	}
	finished := map[int]id.UUID{}
	for i, item := range crashed.Items {
		switch {
		case i < 2 && (item.Outcome == nil || item.Outcome.Status != OutcomeSucceeded):
			t.Errorf("item %d finished before the crash and has outcome %+v", i, item.Outcome)
		case i < 2:
			finished[i] = *item.Outcome.EventID
		case item.Outcome != nil:
			t.Errorf("item %d was not finished before the crash and has outcome %+v", i, item.Outcome)
		}
	}
	if status, _ := statusAndVersion(t, service, ctx, members[2].MembershipID); status != StateActive {
		t.Errorf("the item in flight at the crash is %s; its transaction was not rolled back", status)
	}

	// The same key while the lease is live: the earlier request may still be running.
	if _, err := service.ExecuteBatch(db.WithClaim(ctx, claim), batch.BatchID, nil, testClassifier); !errors.Is(err, ErrBatchExecuting) {
		t.Fatalf("an execute inside the lease: error = %v, want ErrBatchExecuting", err)
	}
	if _, err := service.ExecuteBatch(ctx, batch.BatchID, nil, testClassifier); !errors.Is(err, ErrBatchExecuting) {
		t.Fatalf("an execute without the key inside the lease: error = %v, want ErrBatchExecuting", err)
	}
	two := 2
	service.now = func() time.Time { return base.Add(BatchLease + time.Second) }
	if _, err := service.ExecuteBatch(ctx, batch.BatchID, &two, testClassifier); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a resume naming another allowance: error = %v, want ErrInvalid", err)
	}

	// Past the lease, the same key resumes, adopting the claim the crashed request left.
	resuming := db.WithClaim(ctx, claim)
	resumed, err := service.ExecuteBatch(resuming, batch.BatchID, nil, testClassifier)
	if err != nil {
		t.Fatalf("the resume: %v", err)
	}
	if !db.ClaimAdopted(resuming) || !db.ClaimMade(resuming) {
		t.Error("the resume did not adopt the crashed request's claim, so its response cannot be recorded")
	}
	if resumed.State != BatchExecuted || resumed.ResumedAt == nil || resumed.ResumedBy == nil {
		t.Errorf("the resumed batch is %s, resumed at %v by %v", resumed.State, resumed.ResumedAt, resumed.ResumedBy)
	}
	for i, item := range resumed.Items {
		if item.Outcome == nil || item.Outcome.Status != OutcomeSucceeded {
			t.Errorf("item %d after the resume: %+v, want succeeded", i, item.Outcome)
			continue
		}
		if event, ok := finished[i]; ok && *item.Outcome.EventID != event {
			t.Errorf("item %d finished before the crash and was applied again: event %s, then %s",
				i, event, *item.Outcome.EventID)
		}
		status, version := statusAndVersion(t, service, ctx, item.MembershipID)
		if status != StateSuspended || version != *item.VersionRead+1 {
			t.Errorf("item %d is %s at version %d, want suspended at %d", i, status, version, *item.VersionRead+1)
		}
		if got := outboxCount(t, service, ctx, item.MembershipID) - before[item.MembershipID]; got != 1 {
			t.Errorf("item %d published %d events, want exactly one", i, got)
		}
	}

	// The key whose response was never recorded answers with the batch as it ended, and its
	// completion now succeeds, so later retries replay.
	again := db.WithClaim(ctx, claim)
	if answered, err := service.ExecuteBatch(again, batch.BatchID, nil, testClassifier); err != nil || answered.State != BatchExecuted {
		t.Fatalf("the key again after the resume: %s, %v; want the executed batch", answered.State, err)
	}
	store, err := db.NewClaimStore(pool)
	if err != nil {
		t.Fatalf("NewClaimStore: %v", err)
	}
	if err := store.Complete(ctx, claim, 200, []byte(`{"state":"executed"}`)); err != nil {
		t.Fatalf("completing the adopted claim: %v", err)
	}
	if _, err := service.ExecuteBatch(db.WithClaim(ctx, claim), batch.BatchID, nil, testClassifier); err == nil {
		t.Error("the completed key executed again instead of replaying")
	}
	// A different key on an executed batch is a second execution, refused as before.
	if _, err := service.ExecuteBatch(ctx, batch.BatchID, nil, testClassifier); !errors.Is(err, ErrBatchNotPreviewed) {
		t.Errorf("a new execution of an executed batch: error = %v, want ErrBatchNotPreviewed", err)
	}
}

// TestAnExecutionWhoseLeaseWasTakenOverCommitsNothing: the fence. A request that went silent past
// the lease and comes back after another took the batch over cannot apply an item: its transaction
// finds its lease gone and rolls back.
func TestAnExecutionWhoseLeaseWasTakenOverCommitsNothing(t *testing.T) {
	service, ctx, _ := newFixture(t)
	first := grantOne(t, service, ctx).Membership
	second := grantOne(t, service, ctx).Membership
	var batchID id.UUID
	cleanupBatches(t, ctx, &batchID)
	batch, err := service.PreviewBatch(ctx, BatchRequest{Action: ActionSuspend,
		MembershipIDs: []id.UUID{first.MembershipID, second.MembershipID}}, testClassifier)
	if err != nil {
		t.Fatalf("PreviewBatch: %v", err)
	}
	batchID = batch.BatchID

	readLease := func() id.UUID {
		t.Helper()
		var raw string
		if err := ownerPool(t, ctx).InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
			return tx.QueryRow(ctx, `SELECT lease_id::text FROM membership.membership_batch WHERE batch_id = $1`,
				batch.BatchID.String()).Scan(&raw)
		}); err != nil {
			t.Fatalf("reading the lease: %v", err)
		}
		lease, err := id.Parse(raw)
		if err != nil {
			t.Fatalf("lease %q: %v", raw, err)
		}
		return lease
	}

	base := service.now().UTC()
	service.now = func() time.Time { return base }
	stop := errors.New("stop")
	service.halt = func(context.Context, int) error { return stop }
	if _, err := service.ExecuteBatch(ctx, batch.BatchID, nil, testClassifier); !errors.Is(err, stop) {
		t.Fatalf("the first execution: error = %v, want the injected stop", err)
	}
	stale := readLease()

	service.now = func() time.Time { return base.Add(BatchLease + time.Second) }
	service.halt = func(_ context.Context, position int) error {
		if position == 1 {
			return stop
		}
		return nil
	}
	if _, err := service.ExecuteBatch(ctx, batch.BatchID, nil, testClassifier); !errors.Is(err, stop) {
		t.Fatalf("the resume: error = %v, want the injected stop at the second item", err)
	}
	service.halt = nil
	if readLease() == stale {
		t.Fatal("the resume did not take the lease over")
	}

	current, err := service.GetBatch(ctx, batch.BatchID)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if err := service.executeItem(ctx, current, stale, current.Items[1], ""); !errors.Is(err, ErrBatchLeaseLost) {
		t.Fatalf("the silent request coming back: error = %v, want ErrBatchLeaseLost", err)
	}
	if status, _ := statusAndVersion(t, service, ctx, second.MembershipID); status != StateActive {
		t.Errorf("a request without the lease applied an item: the Membership is %s", status)
	}
	if status, _ := statusAndVersion(t, service, ctx, first.MembershipID); status != StateSuspended {
		t.Errorf("the item the resume applied is %s, want suspended", status)
	}
}

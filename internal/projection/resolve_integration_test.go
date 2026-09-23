package projection

// Closing a dead letter, against the real role and the real evidence table.
//
// Every case here runs the resolver on organization_resolution_app rather than on the owner
// connection, because half of what is under test is not in Go: the resolution role holds
// column-level UPDATE on four columns of platform.dead_letter and nothing else in the estate. A
// suite that drove this through the owner would assert the predicate and prove nothing about the
// blast radius of the credential the predicate runs under.
//
// The receipts are seeded through the owner for the mirror-image reason: the resolution role must
// not be able to write the evidence it then reads, so seeding through it would be asserting the
// opposite of the contract. TestTheResolutionRoleCannotManufactureItsOwnEvidence is that rule
// stated directly.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
	"github.com/anshacerbia2/foundation-platform/outbox"

	"github.com/anshacerbia2/organization-control/internal/db"
)

// roleDSN rewrites the owner DSN this suite is handed into one for a runtime login role.
//
// The host, port and database name come from TEST_DATABASE_URL so that a suite pointed at a
// different engine moves every role with it; only the credential is substituted.
func roleDSN(base, user, password string) string {
	rest := base
	if index := strings.Index(base, "://"); index >= 0 {
		rest = base[index+3:]
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return fmt.Sprintf("postgres://%s:%s@%s", user, password, rest)
}

// resolutionPool opens the resolver's own credential.
//
// The password is named before connecting. Without that, an unexported variable surfaces as a SASL
// failure for organization_resolution_app, which reads as a broken role rather than an unset
// environment -- which is exactly how the dispatch role presented the first time it ran in CI.
func resolutionPool(t *testing.T, f *fixture) (*db.ResolutionPool, *recorder) {
	t.Helper()

	password := os.Getenv("TEST_RESOLUTION_PASSWORD")
	if password == "" {
		t.Fatal("TEST_RESOLUTION_PASSWORD is empty: the resolution login role exists but its " +
			"password was never exported to the test environment, so this capability cannot be checked")
	}

	pool, err := fdb.Open(f.ctx, fdb.Config{
		Name:     "projection-test-resolution",
		DSN:      roleDSN(os.Getenv("TEST_DATABASE_URL"), "organization_resolution_app", password),
		MaxConns: 2,
	})
	if err != nil {
		t.Fatalf("open resolution pool: %v", err)
	}
	t.Cleanup(pool.Close)

	sink := &recorder{}
	resolution, err := db.NewResolutionPool(pool, sink)
	if err != nil {
		t.Fatalf("NewResolutionPool: %v", err)
	}
	return resolution, sink
}

func resolver(t *testing.T, f *fixture) (*Resolver, *recorder) {
	t.Helper()
	pool, sink := resolutionPool(t, f)
	r, err := NewResolver(pool)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r, sink
}

// receipt writes what the dispatcher writes when a consumer accepted an event.
//
// evidence is a parameter rather than a constant, because the whole reason the column exists is
// that a transport acknowledgement is not an application. A test that could only write the strong
// class could not check that the weak one is refused.
func receipt(t *testing.T, f *fixture, eventID id.UUID, consumer, evidence string) {
	t.Helper()
	f.exec(t, `INSERT INTO platform.delivery_receipt (event_id, consumer, event_type, evidence)
	           VALUES ($1::uuid, $2, 'com.scnehaux.organization.membership.security.revoked', $3)`,
		eventID.String(), consumer, evidence)
	t.Cleanup(func() {
		_ = f.setup.InTx(context.Background(), func(ctx context.Context, tx fdb.Tx) error {
			_, _ = tx.Exec(ctx, `DELETE FROM platform.delivery_receipt WHERE event_id = $1`, eventID.String())
			return nil
		})
	})
}

func incident(t *testing.T, f *fixture) (id.UUID, string) {
	t.Helper()
	consumer := f.register(t)
	eventID, _ := abandoned(t, f, outbox.PriorityHigh, true)
	return eventID, consumer
}

// closureRecords reads the outcome evidence for one incident.
//
// Read from audit.privileged_access itself rather than from the fixture's recorder stub, because
// this is the record the resolver writes through its own connection inside its own transaction --
// the stub only sees the attempt the scope wrapper files before that transaction opens.
func closureRecords(t *testing.T, f *fixture, eventID id.UUID) (int, string, string) {
	t.Helper()

	var (
		count  int
		reason string
		actor  string
	)
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `
			-- max(actor_id::text) rather than max(actor_id): PostgreSQL 15, which this suite runs
			-- against locally, has no max() for uuid while the CI engine does. Casting first keeps
			-- the two engines answering the same question.
			SELECT count(*), coalesce(max(reason), ''), coalesce(max(actor_id::text), '')
			  FROM audit.privileged_access
			 WHERE reason LIKE 'closed dead-lettered event ' || $1::text || '%'`,
			eventID.String()).Scan(&count, &reason, &actor)
	}); err != nil {
		t.Fatalf("reading the closure records: %v", err)
	}
	return count, reason, actor
}

// A closure accounts for itself, and the account is enrolled in the transaction that made it.
//
// The attempt record the scope wrapper files first cannot do this job: at that point nothing has
// happened yet, so it can name an intention but not an outcome. This one names the incident and
// the receipt the closure rested on.
func TestAClosureRecordsItselfInsideTheTransactionThatMadeIt(t *testing.T) {
	f := newFixture(t)
	eventID, consumer := incident(t, f)
	receipt(t, f, eventID, consumer, "consumer_applied")

	r, sink := resolver(t, f)
	before := sink.calls
	resolution, err := r.Resolve(f.ctx, eventID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sink.calls <= before {
		t.Error("the resolution filed no attempt record before its transaction opened")
	}

	count, reason, actor := closureRecords(t, f, eventID)
	if count != 1 {
		t.Fatalf("%d closure records for %s, want exactly 1", count, eventID)
	}
	// The reference, not just the verdict. A record saying an incident was closed, without saying
	// on what, leaves an investigation exactly where it started.
	if !strings.Contains(reason, resolution.Reference) {
		t.Errorf("the closure record does not name the evidence it rested on: %q", reason)
	}
	scope, _ := db.ScopeFrom(f.ctx)
	if actor != scope.Actor().String() {
		t.Errorf("the closure record names actor %s, want the bound %s", actor, scope.Actor())
	}
}

// And a refused closure leaves no account of a closure.
//
// This is the half that makes the record worth reading. If the outcome were written outside the
// transaction the way the attempt is, a refusal -- or a crash between the UPDATE and the commit --
// would leave a row stating that an incident was closed when it is still open, and nothing in the
// table would distinguish it from a true one.
func TestARefusedClosureLeavesNoAccountOfAClosure(t *testing.T) {
	f := newFixture(t)
	eventID, _ := incident(t, f)

	r, sink := resolver(t, f)
	before := sink.calls
	if _, err := r.Resolve(f.ctx, eventID); !errors.Is(err, ErrNoAppliedEvidence) {
		t.Fatalf("Resolve returned %v, want ErrNoAppliedEvidence", err)
	}

	if sink.calls <= before {
		t.Error("the refused resolution filed no attempt record; the attempt is unattributable")
	}
	if count, reason, _ := closureRecords(t, f, eventID); count != 0 {
		t.Errorf("%d closure records for an incident that was never closed: %q", count, reason)
	}
}

// The whole property, end to end: evidence exists, the incident closes, and the frontier stops
// reporting the debt.
//
// The last assertion is the one worth having. Everything upstream of it is bookkeeping; the reason
// this resolver exists at all is that an unresolved authority dead letter makes every
// projection-backed check in the estate refuse, and a resolution that closed the row without
// clearing the debt would leave the estate exactly as stuck as before.
func TestAnIncidentClosesOnAppliedEvidence(t *testing.T) {
	f := newFixture(t)
	clearDeadLetters(t, f.ctx, f.setup)

	eventID, consumer := incident(t, f)
	receipt(t, f, eventID, consumer, "consumer_applied")

	reader, err := NewFrontierReader(f.pool)
	if err != nil {
		t.Fatalf("NewFrontierReader: %v", err)
	}
	before, err := reader.Frontier(f.ctx)
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}
	if !before.SecurityDebt {
		t.Fatal("the seeded incident is not reported as security debt, so this case would pass " +
			"without the resolution doing anything")
	}

	r, _ := resolver(t, f)
	resolution, err := r.Resolve(f.ctx, eventID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolution.Type != ResolutionTypeReplayed {
		t.Errorf("resolution type is %q, want %q", resolution.Type, ResolutionTypeReplayed)
	}
	if resolution.Consumer != consumer {
		t.Errorf("resolved against consumer %q, want the registered %q", resolution.Consumer, consumer)
	}

	scope, _ := db.ScopeFrom(f.ctx)
	var (
		resolvedAt time.Time
		kind       string
		by         string
		reference  string
	)
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `SELECT resolved_at, resolution_type, resolved_by, resolution_reference
		                           FROM platform.dead_letter WHERE event_id = $1`,
			eventID.String()).Scan(&resolvedAt, &kind, &by, &reference)
	}); err != nil {
		t.Fatalf("reading the resolved incident: %v", err)
	}
	if resolvedAt.IsZero() || kind != ResolutionTypeReplayed {
		t.Errorf("the incident is recorded as resolved_at=%v type=%q", resolvedAt, kind)
	}
	// From the bound scope, never from a request body. The field exists so an investigation can
	// ask who decided this incident was over, and an author a caller can name answers nobody.
	if by != scope.Actor().String() {
		t.Errorf("resolved_by is %q, want the bound actor %q", by, scope.Actor())
	}
	// A key, not a sentence: whoever reads this later has to be able to go and look at the row.
	want := fmt.Sprintf("platform.delivery_receipt:%s:%s", eventID, consumer)
	if reference != want {
		t.Errorf("resolution_reference is %q, want %q", reference, want)
	}

	after, err := reader.Frontier(f.ctx)
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}
	if after.SecurityDebt {
		t.Errorf("the incident is closed and the frontier still reports %d unresolved authority "+
			"events: resolution has no effect and every consumer stays refused",
			after.SecurityDeadLettered)
	}
}

// No receipt at all, which is the honest case: the event has not been delivered since it was
// abandoned. The refusal has to leave the incident exactly as it found it.
func TestAnIncidentWithNoEvidenceStaysOpen(t *testing.T) {
	f := newFixture(t)
	eventID, _ := incident(t, f)

	r, sink := resolver(t, f)
	before := sink.calls
	_, err := r.Resolve(f.ctx, eventID)
	if !errors.Is(err, ErrNoAppliedEvidence) {
		t.Fatalf("Resolve returned %v, want ErrNoAppliedEvidence", err)
	}
	if !strings.Contains(err.Error(), "replay it first") {
		t.Errorf("the refusal does not say what to do next: %v", err)
	}

	var resolved bool
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `SELECT resolved_at IS NOT NULL FROM platform.dead_letter WHERE event_id = $1`,
			eventID.String()).Scan(&resolved)
	}); err != nil {
		t.Fatalf("reading the incident: %v", err)
	}
	if resolved {
		t.Error("a refused resolution closed the incident anyway")
	}
	// Recorded before the transaction, so an attempt that fails is still attributable. Asserted
	// against the stub the pool was built with; nothing from this suite reaches
	// audit.privileged_access.
	if sink.calls <= before {
		t.Error("a refused resolution filed no privileged-access record; the attempt is unattributable")
	}
}

// A receipt exists, for this event, carrying applied evidence -- under a consumer name nothing
// resolves against.
//
// This is the case the diagnostic exists for. DISPATCH_CONSUMER_NAME and the registered consumer
// are configured in two repositories with nothing linking them, so one character of disagreement
// produces a working delivery path whose receipts no resolution can read. Reported as bare
// "no evidence" it sends an operator to investigate a delivery that is not broken.
func TestAReceiptUnderAnotherConsumerNameResolvesNothing(t *testing.T) {
	f := newFixture(t)
	eventID, consumer := incident(t, f)
	receipt(t, f, eventID, consumer+"-typo", "consumer_applied")

	r, _ := resolver(t, f)
	_, err := r.Resolve(f.ctx, eventID)
	if !errors.Is(err, ErrNoAppliedEvidence) {
		t.Fatalf("Resolve returned %v, want ErrNoAppliedEvidence", err)
	}
	if !strings.Contains(err.Error(), consumer+"-typo") {
		t.Errorf("the refusal does not name the consumer that did acknowledge the event, so it is "+
			"indistinguishable from a missing delivery: %v", err)
	}
	if !strings.Contains(err.Error(), "DISPATCH_CONSUMER_NAME") {
		t.Errorf("the refusal does not name the likeliest cause: %v", err)
	}
}

// Transport acceptance is not application, and the column that records the difference is worth
// nothing if the predicate reads past it.
//
// A broker acknowledgement means the message was handed on; the consumer may still be hours
// behind. If this case ever passes a resolution, introducing a broker silently weakens every
// closed incident in the estate.
func TestTransportAcceptanceIsNotResolutionEvidence(t *testing.T) {
	f := newFixture(t)
	eventID, consumer := incident(t, f)
	receipt(t, f, eventID, consumer, "transport_accepted")

	r, _ := resolver(t, f)
	if _, err := r.Resolve(f.ctx, eventID); !errors.Is(err, ErrNoAppliedEvidence) {
		t.Fatalf("Resolve returned %v, want ErrNoAppliedEvidence; a transport acknowledgement was "+
			"accepted as proof the consumer applied the event", err)
	}
}

func TestResolvingAClosedIncidentIsRefused(t *testing.T) {
	f := newFixture(t)
	eventID, consumer := incident(t, f)
	receipt(t, f, eventID, consumer, "consumer_applied")

	r, _ := resolver(t, f)
	if _, err := r.Resolve(f.ctx, eventID); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The second one is not idempotent-and-fine: it would overwrite resolved_by and
	// resolution_reference, and the record of who closed the incident is the point of the columns.
	if _, err := r.Resolve(f.ctx, eventID); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("the second Resolve returned %v, want ErrAlreadyResolved", err)
	}
}

func TestResolvingAnUnknownEventIsRefused(t *testing.T) {
	f := newFixture(t)
	r, _ := resolver(t, f)

	if _, err := r.Resolve(f.ctx, mustID(t)); !errors.Is(err, ErrDeadLetterNotFound) {
		t.Fatalf("Resolve returned %v, want ErrDeadLetterNotFound", err)
	}
}

// Nothing registered to enforce means the predicate has no subject.
//
// Resolving against whichever consumer happens to hold a row would close an incident on evidence
// about a consumer that is enforcing nothing -- which is the same as closing it on no evidence,
// with a paper trail that looks convincing.
func TestTheResolverRefusesWhenNothingIsRegisteredToEnforce(t *testing.T) {
	f := newFixture(t)
	eventID, _ := abandoned(t, f, outbox.PriorityHigh, true)

	// Deliberate rather than assumed: another case's leftovers would otherwise decide whether
	// this one is testing anything. Retired and restored, because the rows belong to the shared
	// database rather than to this test.
	var active []string
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		rows, err := tx.Query(ctx,
			`UPDATE projection.consumer SET retired_at = now() WHERE retired_at IS NULL RETURNING consumer_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			active = append(active, id)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("retiring the active consumers: %v", err)
	}
	t.Cleanup(func() {
		f.exec(t, `UPDATE projection.consumer SET retired_at = NULL WHERE consumer_id = ANY ($1::text[])`, active)
	})

	r, _ := resolver(t, f)
	if _, err := r.Resolve(f.ctx, eventID); !errors.Is(err, ErrNoActiveConsumer) {
		t.Fatalf("Resolve returned %v, want ErrNoActiveConsumer", err)
	}
}

// The credential's blast radius, asserted by the database rather than by this package remembering.
//
// Column-level UPDATE is what keeps a resolver from rewriting the incident it is closing: the
// failure class, the envelope and the attempt count are the record of what happened, and a role
// that could edit them could close an incident by making it look like a different one.
func TestTheResolutionRoleCannotRewriteTheIncident(t *testing.T) {
	f := newFixture(t)
	eventID, _ := abandoned(t, f, outbox.PriorityHigh, true)
	pool, _ := resolutionPool(t, f)

	for _, attempt := range []struct {
		what      string
		statement string
	}{
		{"rewrite the failure class", `UPDATE platform.dead_letter SET failure_class = 'transient' WHERE event_id = $1`},
		{"rewrite the envelope", `UPDATE platform.dead_letter SET envelope = '{}'::jsonb WHERE event_id = $1`},
		{"delete the incident", `DELETE FROM platform.dead_letter WHERE event_id = $1`},
		{"file a fresh incident", `INSERT INTO platform.dead_letter
		     (event_id, event_type, envelope, payload, failure_class, failure_detail, attempts, first_failed_at)
		     VALUES ($1::uuid, 'x', '{}'::jsonb, '{}'::jsonb, 'poison', 'x', 1, now())`},
	} {
		err := db.WithResolutionScope(f.ctx, pool, "attempt to "+attempt.what,
			func(ctx context.Context, tx db.Tx) error {
				_, err := tx.Exec(ctx, attempt.statement, eventID.String())
				return err
			})
		if err == nil {
			t.Errorf("the resolution role can %s", attempt.what)
		}
	}
}

// And it cannot write the evidence it reads.
//
// A resolver holding INSERT on platform.delivery_receipt could manufacture the proof it then
// consumes, and every resolution in the estate would rest on an assertion the resolver made about
// itself. The receipts are the dispatcher's witness or they are nothing.
func TestTheResolutionRoleCannotManufactureItsOwnEvidence(t *testing.T) {
	f := newFixture(t)
	eventID, consumer := incident(t, f)
	pool, _ := resolutionPool(t, f)

	err := db.WithResolutionScope(f.ctx, pool, "attempt to write a delivery receipt",
		func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO platform.delivery_receipt (event_id, consumer, event_type, evidence)
			     VALUES ($1::uuid, $2, 'com.scnehaux.organization.membership.security.revoked', 'consumer_applied')`,
				eventID.String(), consumer)
			return err
		})
	if err == nil {
		t.Fatal("the resolution role can write a delivery receipt: it can manufacture the evidence " +
			"it then reads, and every closed incident rests on its own assertion")
	}
}

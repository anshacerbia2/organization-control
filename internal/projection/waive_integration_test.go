package projection

// Waivers, against the real resolution role.
//
// The property under test is the negative one: a waiver is never a closure. It leaves resolved_at
// NULL, the estate frontier still reports the debt, and it is refused wherever it would silence a
// live outage. The positive case is here to show those refusals are not simply a waiver that never
// works.

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

// deadLetterFor files an unresolved authority-bearing incident refused by consumer, or by nobody
// when consumer is nil.
func deadLetterFor(t *testing.T, f *fixture, consumer any) id.UUID {
	t.Helper()
	eventID := mustID(t)
	f.exec(t, `INSERT INTO platform.dead_letter
	    (event_id, event_type, envelope, payload, consumer, failure_class, failure_detail, attempts, first_failed_at)
	    VALUES ($1::uuid, $2, '{}'::jsonb, '{}'::jsonb, $3, 'poison', 'refused', 3, clock_timestamp())`,
		eventID.String(), typeRevoked, consumer)
	t.Cleanup(func() {
		f.exec(t, `DELETE FROM platform.dead_letter WHERE event_id = $1`, eventID.String())
	})
	return eventID
}

type waiverState struct {
	resolved bool
	waived   bool
	by       string
}

func readWaiver(t *testing.T, f *fixture, eventID id.UUID) waiverState {
	t.Helper()
	var s waiverState
	if err := f.setup.InTx(f.ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `SELECT resolved_at IS NOT NULL, waived_until IS NOT NULL, coalesce(waived_by, '')
		                           FROM platform.dead_letter WHERE event_id = $1`, eventID.String()).
			Scan(&s.resolved, &s.waived, &s.by)
	}); err != nil {
		t.Fatalf("reading the incident: %v", err)
	}
	return s
}

// A retired consumer's incident can be waived, and the waiver leaves it open: still debt for the
// estate, and never a closure record.
func TestARetiredConsumersIncidentIsWaivedAndStaysOpen(t *testing.T) {
	f := newFixture(t)
	clearDeadLetters(t, f.ctx, f.setup)
	active := f.register(t)
	eventID := deadLetterFor(t, f, "retired-consumer")

	r, sink := resolver(t, f)
	before := sink.calls
	waiver, err := r.Waive(f.ctx, eventID, "the consumer was decommissioned", time.Now().Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("Waive: %v", err)
	}
	if sink.calls <= before {
		t.Error("the waiver filed no attempt record before its transaction opened")
	}
	if waiver.Consumer != "retired-consumer" {
		t.Errorf("waiver names consumer %q", waiver.Consumer)
	}

	state := readWaiver(t, f, eventID)
	bound, _ := db.ScopeFrom(f.ctx)
	scope := bound.Actor().String()
	if !state.waived || state.resolved {
		t.Fatalf("after a waiver the incident is waived=%v resolved=%v; a waiver is not a closure",
			state.waived, state.resolved)
	}
	if state.by != scope {
		t.Errorf("waived_by is %q, want the bound actor %q", state.by, scope)
	}

	reader, err := NewFrontierReader(f.pool)
	if err != nil {
		t.Fatalf("NewFrontierReader: %v", err)
	}
	estate, err := reader.Frontier(f.ctx)
	if err != nil {
		t.Fatalf("Frontier: %v", err)
	}
	if !estate.SecurityDebt {
		t.Error("the estate frontier stopped reporting a waived incident as debt; the waiver closed it")
	}
	own, err := reader.FrontierFor(f.ctx, active)
	if err != nil {
		t.Fatalf("FrontierFor: %v", err)
	}
	if own.SecurityDebt {
		t.Error("the active consumer is charged with a retired consumer's incident")
	}

	// The outcome record says "waived", and no record says the incident was closed.
	if count, reason, _ := closureRecords(t, f, eventID); count != 0 {
		t.Errorf("a waiver filed a closure record: %q", reason)
	}
}

// The waivers that would hide a live outage, and the malformed ones.
func TestAWaiverIsRefusedWhereItWouldHideALiveOutage(t *testing.T) {
	f := newFixture(t)
	clearDeadLetters(t, f.ctx, f.setup)
	active := f.register(t)
	r, _ := resolver(t, f)
	later := time.Now().Add(7 * 24 * time.Hour)

	cases := []struct {
		what    string
		eventID id.UUID
		until   time.Time
		reason  string
		want    error
	}{
		{"the active consumer's own incident", deadLetterFor(t, f, active), later, "x", ErrNotWaivable},
		{"an incident naming no consumer", deadLetterFor(t, f, nil), later, "x", ErrNotWaivable},
		{"an expiry beyond the bound", deadLetterFor(t, f, "gone"), time.Now().Add(MaxWaiver + 24*time.Hour), "x", ErrInvalid},
		{"an expiry already past", deadLetterFor(t, f, "gone"), time.Now().Add(-time.Hour), "x", ErrInvalid},
		{"no reason", deadLetterFor(t, f, "gone"), later, "   ", ErrInvalid},
		{"an unknown incident", mustID(t), later, "x", ErrDeadLetterNotFound},
	}
	for _, c := range cases {
		_, err := r.Waive(f.ctx, c.eventID, c.reason, c.until)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: Waive returned %v, want %v", c.what, err, c.want)
			continue
		}
		if c.want != ErrDeadLetterNotFound && readWaiver(t, f, c.eventID).waived {
			t.Errorf("%s: the refusal wrote a waiver anyway", c.what)
		}
	}

	// The refusal for the active consumer says what to do instead.
	_, err := r.Waive(f.ctx, cases[0].eventID, "x", later)
	if err == nil || !strings.Contains(err.Error(), "replay or supersede") {
		t.Errorf("the refusal does not name the corrective path: %v", err)
	}
}

// A standing waiver is not renewed in place; a resolved incident is not waived at all.
func TestAWaiverIsNotStackedOrAppliedToAClosedIncident(t *testing.T) {
	f := newFixture(t)
	clearDeadLetters(t, f.ctx, f.setup)
	f.register(t)
	r, _ := resolver(t, f)

	eventID := deadLetterFor(t, f, "gone")
	if _, err := r.Waive(f.ctx, eventID, "decommissioned", time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("Waive: %v", err)
	}
	if _, err := r.Waive(f.ctx, eventID, "again", time.Now().Add(48*time.Hour)); !errors.Is(err, ErrAlreadyWaived) {
		t.Errorf("a second waiver while the first stands returned %v, want ErrAlreadyWaived", err)
	}

	closed := deadLetterFor(t, f, "gone")
	f.exec(t, `UPDATE platform.dead_letter
	              SET resolved_at = now(), resolution_type = 'REPLAYED', resolved_by = 'suite', resolution_reference = 'x'
	            WHERE event_id = $1`, closed.String())
	if _, err := r.Waive(f.ctx, closed, "x", time.Now().Add(24*time.Hour)); !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("waiving a resolved incident returned %v, want ErrAlreadyResolved", err)
	}
}

package projection

// The signals and the dead-letter list added in TDD-organization-control-002 1.15.0, -004 1.11.0 and
// -005 2.5.0, read as the provider role the production wiring reads them as.

import (
	"errors"
	"testing"
	"time"
)

// A security event owed and not applied is reported by its age, per consumer, and a consumer owed
// nothing reports zero rather than going absent.
func TestTheUnappliedSignalReportsTheOldestSecurityEventNotApplied(t *testing.T) {
	f := newFixture(t)
	reader, err := NewSignalsReader(f.pool)
	if err != nil {
		t.Fatalf("NewSignalsReader: %v", err)
	}
	consumer, bystander := f.register(t), f.register(t)
	insertOutboxRow(t, f.ctx, f.setup, consumer, true, 40*time.Second) // published, never receipted

	s, err := reader.Read(f.ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got := map[string]UnappliedSignal{}
	for _, u := range s.Unapplied {
		got[u.Consumer] = u
	}
	if u, ok := got[consumer]; !ok || u.OldestAge < 35 {
		t.Errorf("%s reports %+v, and a 40s-old security event is unapplied there", consumer, u)
	}
	if u, ok := got[bystander]; !ok || u.OldestAge != 0 {
		t.Errorf("%s, owed nothing, reports %+v", bystander, u)
	}
}

// The lifecycle view exists, the provider role may read it on a raw connection with no scope bound,
// and it answers one row: what the gauges need on every collection.
func TestTheLifecycleSignalsAreReadableWithoutAScope(t *testing.T) {
	f := newFixture(t)
	reader, err := NewSignalsReader(f.pool)
	if err != nil {
		t.Fatalf("NewSignalsReader: %v", err)
	}
	s, err := reader.ReadLifecycle(f.ctx)
	if err != nil {
		t.Fatalf("ReadLifecycle: %v", err)
	}
	if len(s.Requests) != 4 {
		t.Errorf("the lifecycle signals carry %d request series, want both directions in both states", len(s.Requests))
	}
	if s.ObligationsOverdue < 0 || s.OffboardingsInProgress < 0 {
		t.Errorf("the lifecycle signals read %+v", s)
	}
}

// One consumer's dead letters, filtered by state and paged by event_id, each page an access recorded
// with the caller's reason.
func TestADeadLetterListShowsOneConsumersIncidents(t *testing.T) {
	f := newFixture(t)
	reader, err := NewDeadLetterReader(f.provider)
	if err != nil {
		t.Fatalf("NewDeadLetterReader: %v", err)
	}
	consumer, other := f.register(t), f.register(t)
	insertDeadLetter(t, f.ctx, f.setup, consumer, AuthorityEventTypes[0], time.Hour, false)
	insertDeadLetter(t, f.ctx, f.setup, consumer, AuthorityEventTypes[0], 2*time.Hour, false)
	insertDeadLetter(t, f.ctx, f.setup, consumer, AuthorityEventTypes[0], 3*time.Hour, true)
	insertDeadLetter(t, f.ctx, f.setup, other, AuthorityEventTypes[0], time.Hour, false)

	open, err := reader.List(f.ctx, DeadLetterQuery{Consumer: consumer, State: DeadLetterOpen}, "INC-1 diagnosis")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(open.DeadLetters) != 2 || open.Next != nil {
		t.Fatalf("the open list has %d items and next %v, want the two open incidents of %s",
			len(open.DeadLetters), open.Next, consumer)
	}
	for _, d := range open.DeadLetters {
		if d.Consumer != consumer || d.ResolvedAt != nil || !d.AuthorityBearing {
			t.Errorf("the open list holds %+v", d)
		}
	}

	all, err := reader.List(f.ctx, DeadLetterQuery{Consumer: consumer, Limit: 2}, "INC-1 diagnosis")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all.DeadLetters) != 2 || all.Next == nil {
		t.Fatalf("a page of 2 of 3 has %d items and next %v", len(all.DeadLetters), all.Next)
	}
	rest, err := reader.List(f.ctx, DeadLetterQuery{Consumer: consumer, After: *all.Next, Limit: 2}, "INC-1 diagnosis")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rest.DeadLetters) != 1 || rest.Next != nil {
		t.Errorf("the second page has %d items and next %v, want the last one", len(rest.DeadLetters), rest.Next)
	}

	if _, err := reader.List(f.ctx, DeadLetterQuery{Consumer: "never-registered-" + mustID(t).String()},
		"INC-1 diagnosis"); !errors.Is(err, ErrConsumerNotFound) {
		t.Errorf("an unknown consumer answered %v, want ErrConsumerNotFound", err)
	}
	if _, err := reader.List(f.ctx, DeadLetterQuery{Consumer: consumer, State: "waived"}, "INC-1"); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown state answered %v, want ErrInvalid", err)
	}
}

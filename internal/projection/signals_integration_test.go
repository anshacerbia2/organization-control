package projection

// The enforcement signals, against a real outbox and dead-letter table, read as the provider role
// the production wiring reads them as.

import (
	"testing"
	"time"
)

func TestTheSignalsReportLagDebtAndStaleness(t *testing.T) {
	f := newFixture(t)
	clearDeadLetters(t, f.ctx, f.setup)
	reader, err := NewSignalsReader(f.pool)
	if err != nil {
		t.Fatalf("NewSignalsReader: %v", err)
	}
	consumer, bystander := f.register(t), f.register(t)

	insertOutboxRow(t, f.ctx, f.setup, consumer, false, 90*time.Second) // a priority delivery, owed
	insertDeadLetter(t, f.ctx, f.setup, consumer, AuthorityEventTypes[0], 2*time.Hour, false)
	insertDeadLetter(t, f.ctx, f.setup, consumer, AuthorityEventTypes[0], 3*time.Hour, true) // resolved: no debt

	s, err := reader.Read(f.ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	lanes := map[string]map[string]LaneSignal{}
	for _, lane := range s.Lanes {
		if lanes[lane.Consumer] == nil {
			lanes[lane.Consumer] = map[string]LaneSignal{}
		}
		lanes[lane.Consumer][lane.Lane] = lane
	}
	priority, ok := lanes[consumer]["priority"]
	switch {
	case !ok || len(lanes[consumer]) != 2:
		t.Errorf("%s's lanes = %+v, want priority and standard", consumer, lanes[consumer])
	case priority.Count < 1:
		t.Errorf("the priority lane reports %+v while a priority delivery is owed", priority)
	case priority.OldestAge < 80:
		t.Errorf("the priority lane's oldest age is %.0fs, and the owed delivery is 90s old", priority.OldestAge)
	}
	// An active consumer owed nothing reports zeros rather than going absent.
	if idle, ok := lanes[bystander]["priority"]; !ok || idle.Count != 0 {
		t.Errorf("%s, owed nothing, reports %+v", bystander, lanes[bystander])
	}

	debt := map[string]DebtSignal{}
	for _, d := range s.Debt {
		debt[d.Consumer] = d
	}
	if debt[consumer].Count != 1 {
		t.Errorf("%s's security debt = %d, want the one unresolved authority dead letter", consumer, debt[consumer].Count)
	}
	if debt[consumer].OldestAge < 7000 {
		t.Errorf("the oldest debt is %.0fs old, and the unresolved dead letter is 2h old", debt[consumer].OldestAge)
	}
	if d, ok := debt[bystander]; !ok || d.Count != 0 {
		t.Errorf("%s is charged %+v for another consumer's dead letter", bystander, d)
	}
	if s.Stale < 1 {
		t.Errorf("stale = %d, and an unresolved, unwaived dead letter exists", s.Stale)
	}
}

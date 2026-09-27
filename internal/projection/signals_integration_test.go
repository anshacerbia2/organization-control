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

	insertOutboxRow(t, f.ctx, f.setup, false, 90*time.Second) // a priority row, owed
	insertDeadLetter(t, f.ctx, f.setup, AuthorityEventTypes[0], 2*time.Hour, false)
	insertDeadLetter(t, f.ctx, f.setup, AuthorityEventTypes[0], 3*time.Hour, true) // resolved: no debt

	s, err := reader.Read(f.ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	var priority *LaneSignal
	for i := range s.Lanes {
		if s.Lanes[i].Lane == "priority" {
			priority = &s.Lanes[i]
		}
	}
	switch {
	case len(s.Lanes) != 2:
		t.Errorf("lanes = %+v, want priority and standard", s.Lanes)
	case priority == nil || priority.Count < 1:
		t.Errorf("the priority lane reports %+v while a priority row is owed", priority)
	case priority.OldestAge < 80:
		t.Errorf("the priority lane's oldest age is %.0fs, and the owed row is 90s old", priority.OldestAge)
	}
	if s.SecurityDebt != 1 {
		t.Errorf("security debt = %d, want the one unresolved authority dead letter", s.SecurityDebt)
	}
	if s.SecurityDebtOldestAge < 7000 {
		t.Errorf("the oldest debt is %.0fs old, and the unresolved dead letter is 2h old", s.SecurityDebtOldestAge)
	}
	if s.Stale < 1 {
		t.Errorf("stale = %d, and an unresolved, unwaived dead letter exists", s.Stale)
	}
}

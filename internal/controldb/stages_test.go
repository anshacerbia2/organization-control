package controldb_test

// The stage SQL and the posture check agree. The posture check lives in internal/posture, which the
// serving process imports; the SQL it verifies lives here.

import (
	"strings"
	"testing"

	"github.com/anshacerbia2/organization-control/internal/controldb"
	"github.com/anshacerbia2/organization-control/internal/posture"
)

// TestRLSSchemasMatchesTheGrantedSet keeps the Go constant and the SQL from drifting apart.
//
// AssertIsolation reads RLSSchemas and rls.sql hardcodes the same list. Two copies of a set is
// two chances to add a schema to one of them: a schema added to rls.sql and not here would be
// protected and unverified, and the reverse would fail every deploy for a schema with no tables.
func TestRLSSchemasMatchesTheGrantedSet(t *testing.T) {
	statements, err := controldb.SQL(controldb.StageRLS)
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, schema := range posture.RLSSchemas {
		if !strings.Contains(statements, "'"+schema+"'") {
			t.Errorf("rls.sql does not mention schema %q, which AssertIsolation verifies", schema)
		}
	}
	// The two schemas deliberately outside the set. Their absence is a decision
	// TDD-organization-control-001 states, so it is asserted rather than left to a reader
	// noticing they are missing.
	for _, outside := range []string{"organization", "projection"} {
		for _, inside := range posture.RLSSchemas {
			if inside == outside {
				t.Errorf("%q is in RLSSchemas; it is deliberately not tenant-scoped", outside)
			}
		}
	}
}

func TestSQLRejectsAnUnknownStage(t *testing.T) {
	if _, err := controldb.SQL(controldb.Stage("nonexistent.sql")); err == nil {
		t.Fatal("SQL accepted a stage that is not embedded")
	}
	for _, stage := range append([]controldb.Stage{controldb.StageRoles}, controldb.PostStages...) {
		body, err := controldb.SQL(stage)
		if err != nil {
			t.Errorf("SQL(%s): %v", stage, err)
		}
		if strings.TrimSpace(body) == "" {
			t.Errorf("SQL(%s) is empty", stage)
		}
	}
}

func TestPostStagesRunRLSBeforeGrants(t *testing.T) {
	// Both orders work, and this one means a window where privileges exist without policies
	// never opens: if the run fails between them, the runtime roles cannot reach the tables yet.
	if len(controldb.PostStages) != 2 {
		t.Fatalf("PostStages has %d entries, want 2", len(controldb.PostStages))
	}
	if controldb.PostStages[0] != controldb.StageRLS || controldb.PostStages[1] != controldb.StageGrants {
		t.Errorf("PostStages = %v, want [rls.sql grants.sql]", controldb.PostStages)
	}
}

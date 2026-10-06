package authority

// Emergency grant validation (ADR-ORG-002 §5.2), against the real engine as the provider login role:
// each use is recorded on the grant it was made by, the report lists every emergency grant of this
// service's scope with its last use, and one unused for 90 days is overdue.

import (
	"context"
	"testing"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
)

func TestAnEmergencyGrantIsValidatedByItsUse(t *testing.T) {
	provider, owner, ctx := pools(t)
	emptyGrants(t, ctx, owner)
	grants, err := NewGrants(provider)
	if err != nil {
		t.Fatal(err)
	}
	records, err := NewReader(provider)
	if err != nil {
		t.Fatal(err)
	}
	used, unused := newID(t), newID(t)
	first, err := grants.Bootstrap(ctx, Bootstrap{Principal: used, Operator: "Ada Operator", Reason: "first provider"})
	if err != nil {
		t.Fatal(err)
	}
	// A second emergency grant, and an eligible one that the report leaves out.
	exec(t, ctx, owner, `INSERT INTO organization.provider_grant (grant_id, principal_id, scope, reason, kind, granted_by, granted_at)
	    VALUES (gen_random_uuid(), $1, $2, 'second break glass', 'emergency', $3, now() - interval '100 days')`,
		unused.String(), Scope, used.String())
	exec(t, ctx, owner, `INSERT INTO organization.provider_grant (grant_id, principal_id, scope, reason, kind, granted_by)
	    VALUES (gen_random_uuid(), $1, $2, 'an eligible grant', 'eligible', $3)`, newID(t).String(), Scope, used.String())

	for range 2 {
		if err := records.RecordEmergencyUse(ctx, used); err != nil {
			t.Fatalf("recording a use: %v", err)
		}
	}
	// A Principal with no emergency grant records nothing, and is no error: the standing read decided.
	if err := records.RecordEmergencyUse(ctx, newID(t)); err != nil {
		t.Errorf("a Principal with no emergency grant: %v", err)
	}

	now := time.Now()
	report, err := records.EmergencyValidation(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 2 {
		t.Fatalf("the report lists %d grants, want the two emergency grants: %+v", len(report), report)
	}
	// The oldest due first: the grant made 100 days ago and never used.
	stale, fresh := report[0], report[1]
	if stale.Principal != unused || stale.LastUsedAt != nil || stale.Uses != 0 || !stale.Overdue {
		t.Errorf("the unused grant reads %+v, want never used and overdue", stale)
	}
	if fresh.GrantID != first.ID || fresh.LastUsedAt == nil || fresh.Uses != 2 || fresh.Overdue {
		t.Errorf("the used grant reads %+v, want two uses and not overdue", fresh)
	}
	if fresh.LastUsedAt != nil && !fresh.DueAt.Equal(fresh.LastUsedAt.Add(ValidationPeriod)) {
		t.Errorf("due at %s, want 90 days after the last use %s", fresh.DueAt, fresh.LastUsedAt)
	}
	// Ninety days on, the used grant is overdue as well.
	if later, err := records.EmergencyValidation(ctx, now.Add(ValidationPeriod+time.Hour)); err != nil || !later[1].Overdue {
		t.Errorf("ninety days after its last use the grant reads %+v, %v", later, err)
	}

	// The provider role cannot erase the evidence.
	if err := provider.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM organization.emergency_grant_use`)
		return err
	}); err == nil {
		t.Error("the provider role deleted the record of a grant's use")
	}
}

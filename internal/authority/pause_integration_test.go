package authority

// The pause of Tenant administration, as the provider role (TDD-organization-control-001 1.22.0).

import (
	"context"
	"errors"
	"testing"

	fdb "github.com/anshacerbia2/foundation-platform/db"
)

func emptyPauses(t *testing.T, ctx context.Context, owner *fdb.Pool) {
	t.Helper()
	wipe := func() {
		if err := owner.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM organization.tenant_administration_pause`)
			return err
		}); err != nil {
			t.Fatalf("clearing pauses: %v", err)
		}
	}
	wipe()
	t.Cleanup(wipe)
}

// No decision is not paused; a pause is read by the caller records at once; lifting it is a second
// decision, and the first stays as the record of who paused and why.
func TestAPauseIsReadAtOnceAndLiftingItKeepsTheRecord(t *testing.T) {
	admin, records, ctx, first := administration(t)
	_, owner, _ := pools(t)
	emptyPauses(t, ctx, owner)

	if paused, err := records.TenantAdministrationPaused(ctx); err != nil || paused {
		t.Fatalf("with no decision recorded the pause reads %t, %v", paused, err)
	}
	current, err := admin.CurrentPause(ctx, "INC-1 status")
	if err != nil || current.Paused || !current.ID.IsNil() {
		t.Fatalf("CurrentPause with no decision answered %+v, %v", current, err)
	}

	paused, err := admin.SetPause(ctx, true, "INC-1 restored from the 2026-10-08 backup")
	if err != nil || !paused.Paused || paused.ActorID == nil || *paused.ActorID != first {
		t.Fatalf("SetPause answered %+v, %v; want paused by the calling provider", paused, err)
	}
	if on, err := records.TenantAdministrationPaused(ctx); err != nil || !on {
		t.Fatalf("after the pause the caller records read %t, %v", on, err)
	}

	if _, err := admin.SetPause(ctx, false, "INC-1 reconciled"); err != nil {
		t.Fatalf("lifting: %v", err)
	}
	if on, err := records.TenantAdministrationPaused(ctx); err != nil || on {
		t.Errorf("after lifting the caller records read %t, %v", on, err)
	}
	var decisions int
	if err := owner.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM organization.tenant_administration_pause`).Scan(&decisions)
	}); err != nil {
		t.Fatal(err)
	}
	if decisions != 2 {
		t.Errorf("%d decisions recorded, want the pause and its lifting", decisions)
	}
	if _, err := admin.SetPause(ctx, true, "  "); !errors.Is(err, ErrPauseInvalid) {
		t.Errorf("a pause with no reason answered %v", err)
	}
}

// A decision is never rewritten, and only a person lifts a pause: the provider role holds no UPDATE,
// and a lifting that names no actor is refused by the table.
func TestAPauseDecisionIsNeverRewritten(t *testing.T) {
	provider, owner, ctx := pools(t)
	emptyPauses(t, ctx, owner)
	err := provider.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE organization.tenant_administration_pause SET paused = false`)
		return err
	})
	if err == nil {
		t.Error("the provider role rewrote a pause decision")
	}
	err = owner.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO organization.tenant_administration_pause (pause_id, paused, reason)
			VALUES (gen_random_uuid(), false, 'lifted by nobody')`)
		return err
	})
	if err == nil {
		t.Error("a lifting that names no provider was recorded")
	}
}

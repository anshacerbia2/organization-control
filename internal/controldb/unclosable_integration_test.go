package controldb_test

// The post stage's refusal of dead letters no resolution can close (ROADMAP.md item 12).
//
// Each row below lacks some of what a closure needs, and the check must name exactly the rows that
// lack all of it. The event types are this test's own, passed in as the authority list, so the case
// does not depend on which types the service publishes.

import (
	"context"
	"strings"
	"testing"

	"github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/controldb"
)

const (
	testAuthorityType = "com.scnehaux.test.unclosable.authority"
	testLifecycleType = "com.scnehaux.test.unclosable.lifecycle"
)

func TestThePostStageNamesOnlyDeadLettersNoResolutionCanClose(t *testing.T) {
	pool, ctx := openAdmin(t)

	// One active consumer at most, which the schema enforces: use the one there is, or register one.
	var activeConsumer string
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(max(consumer_id), '') FROM projection.consumer
		    WHERE retired_at IS NULL`).Scan(&activeConsumer)
	}); err != nil {
		t.Fatalf("reading the active consumer: %v", err)
	}
	if activeConsumer == "" {
		activeConsumer = "unclosable-active-" + newID(t)
		adminExec(t, ctx, pool, `INSERT INTO projection.consumer
		    (consumer_id, principal_id, projection_version, max_accepted_age, stale_behavior)
		    VALUES ($1, gen_random_uuid(), 1, interval '1 minute', 'fail_closed')`, activeConsumer)
		t.Cleanup(func() {
			adminExec(t, context.Background(), pool, `DELETE FROM projection.consumer WHERE consumer_id = $1`, activeConsumer)
		})
	}
	retiredConsumer := "unclosable-retired-" + newID(t)
	adminExec(t, ctx, pool, `INSERT INTO projection.consumer
	    (consumer_id, principal_id, projection_version, max_accepted_age, stale_behavior, retired_at)
	    VALUES ($1, gen_random_uuid(), 1, interval '1 minute', 'fail_closed', now())`, retiredConsumer)
	t.Cleanup(func() {
		adminExec(t, context.Background(), pool, `DELETE FROM projection.consumer WHERE consumer_id = $1`, retiredConsumer)
	})

	legacyUnattributed := deadLetter(t, ctx, pool, testAuthorityType, false, nil)
	legacyActive := deadLetter(t, ctx, pool, testAuthorityType, false, &activeConsumer)
	waivable := deadLetter(t, ctx, pool, testAuthorityType, false, &retiredConsumer)
	replayable := deadLetter(t, ctx, pool, testAuthorityType, true, nil)
	notAuthority := deadLetter(t, ctx, pool, testLifecycleType, false, nil)

	// A recorded version makes it supersedable, whatever else it lacks.
	versioned := deadLetter(t, ctx, pool, testAuthorityType, false, nil)
	tenantID := "11111111-1111-4111-8111-11111111111a"
	adminExec(t, ctx, pool, `INSERT INTO tenant.tenant_event (event_id, tenant_id, tenant_security_version, event_type)
	    VALUES ($1::uuid, $2::uuid, 987654321, $3)`, versioned, tenantID, testAuthorityType)
	t.Cleanup(func() {
		adminExec(t, context.Background(), pool, `DELETE FROM tenant.tenant_event WHERE event_id = $1::uuid`, versioned)
	})

	ids, err := controldb.UnclosableDeadLetters(ctx, pool, []string{testAuthorityType})
	if err != nil {
		t.Fatalf("UnclosableDeadLetters: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for name, want := range map[string]struct {
		id         string
		unclosable bool
	}{
		"unreplayable, unversioned, unattributed":          {legacyUnattributed, true},
		"unreplayable, unversioned, the active consumer's": {legacyActive, true},
		"a retired consumer's, so waivable":                {waivable, false},
		"replayable":                                       {replayable, false},
		"versioned, so supersedable":                       {versioned, false},
		"not authority-bearing":                            {notAuthority, false},
	} {
		if got[want.id] != want.unclosable {
			t.Errorf("%s: reported unclosable = %v, want %v", name, got[want.id], want.unclosable)
		}
	}

	if err := controldb.UnclosableError(ids); err == nil || !strings.Contains(err.Error(), legacyUnattributed) {
		t.Errorf("the refusal does not name the row an operator has to deal with: %v", err)
	}
	if _, err := controldb.UnclosableDeadLetters(ctx, pool, nil); err == nil {
		t.Error("an empty authority list was accepted, and the check would pass vacuously")
	}
}

// deadLetter seeds an unresolved dead letter and removes it afterwards.
func deadLetter(t *testing.T, ctx context.Context, pool *db.Pool, eventType string, replayable bool,
	consumer *string) string {
	t.Helper()
	eventID := newID(t)
	var aggregate, priority any
	if replayable {
		aggregate, priority = newID(t), 0
	}
	adminExec(t, ctx, pool, `INSERT INTO platform.dead_letter
	    (event_id, event_type, envelope, payload, aggregate_id, priority, failure_class, failure_detail,
	     attempts, first_failed_at, consumer)
	    VALUES ($1::uuid, $2, '{}'::jsonb, '{}'::jsonb, $3::uuid, $4::smallint, 'poison', 'refused', 1,
	            clock_timestamp(), $5)`, eventID, eventType, aggregate, priority, consumer)
	t.Cleanup(func() {
		adminExec(t, context.Background(), pool, `DELETE FROM platform.dead_letter WHERE event_id = $1::uuid`, eventID)
	})
	return eventID
}

func adminExec(t *testing.T, ctx context.Context, pool *db.Pool, statement string, args ...any) {
	t.Helper()
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, statement, args...)
		return err
	}); err != nil {
		t.Fatalf("%v\n  %s", err, statement)
	}
}

func newID(t *testing.T) string {
	t.Helper()
	value, err := id.NewV7()
	if err != nil {
		t.Fatalf("minting an identifier: %v", err)
	}
	return value.String()
}

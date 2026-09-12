package controldb_test

// The dispatcher's role, asserted from the catalog.
//
// A narrow role is only narrow while nobody widens it, and widening is silent: a schema-wide grant
// added for convenience, or a table added to `platform` that default privileges hand over. Nothing
// fails, nothing logs, and a delivery worker quietly holds privileges on tables it has no business
// reading. These cases read `information_schema` rather than grants.sql, so they answer for what the
// database actually permits.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"
)

const dispatchRole = "organization_dispatch_rt"

// TestTheDispatchRoleTouchesOnlyItsThreeObjects is the assertion the separate role exists for. Run
// under the provider role instead, a delivery worker could mutate every Tenant in the estate — for
// a job whose entire scope is moving rows that are already committed.
func TestTheDispatchRoleTouchesOnlyItsThreeObjects(t *testing.T) {
	pool, ctx := openAdmin(t)

	type grant struct {
		table     string
		privilege string
	}
	var held []grant

	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT table_schema || '.' || table_name, privilege_type
			  FROM information_schema.table_privileges
			 WHERE grantee = $1
			 ORDER BY 1, 2`, dispatchRole)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g grant
			if err := rows.Scan(&g.table, &g.privilege); err != nil {
				return err
			}
			held = append(held, g)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("reading table privileges: %v", err)
	}

	if len(held) == 0 {
		t.Fatal("the dispatch role holds no table privilege at all; grants.sql did not run")
	}

	permitted := map[grant]bool{
		{"platform.outbox", "SELECT"}:      true,
		{"platform.outbox", "UPDATE"}:      true,
		{"platform.dead_letter", "INSERT"}: true,
		// Not for reading incidents. `ON CONFLICT (event_id) DO NOTHING` makes PostgreSQL
		// require SELECT on the table being inserted into; see grants.sql and the capability
		// test below, which measured it.
		{"platform.dead_letter", "SELECT"}: true,
	}

	for _, g := range held {
		if !permitted[g] {
			t.Errorf("the dispatch role holds %s on %s, which is outside the outbox and the dead-letter table",
				g.privilege, g.table)
		}
	}

	// And the three it must have, so a revocation that broke delivery would fail here rather than in
	// production at the moment an event needed sending.
	//
	// The positive half matters as much as the negative one above, and for a failure the negative
	// half cannot see: an allowlist catches a role that gained something, and says nothing about a
	// role that lost something it needs. A table this role must write, added by a later platform
	// version and never granted, is invisible to the check that only looks for excess.
	for _, required := range []grant{
		{"platform.outbox", "SELECT"},
		{"platform.outbox", "UPDATE"},
		{"platform.dead_letter", "INSERT"},
		{"platform.dead_letter", "SELECT"},
	} {
		found := false
		for _, g := range held {
			if g == required {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the dispatch role lacks %s on %s, so it cannot drain the outbox",
				required.privilege, required.table)
		}
	}
}

// TestTheDispatchRoleCannotDeleteFromTheOutbox states why: a dispatched row is marked published,
// never removed. A worker able to delete is a worker whose bug is unrecoverable, because the
// evidence of what it did goes with the row.
func TestTheDispatchRoleCannotDeleteFromTheOutbox(t *testing.T) {
	pool, ctx := openAdmin(t)

	var canDelete bool
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT has_table_privilege($1, 'platform.outbox', 'DELETE')`, dispatchRole).Scan(&canDelete)
	}); err != nil {
		t.Fatalf("reading DELETE privilege: %v", err)
	}
	if canDelete {
		t.Error("the dispatch role can DELETE from platform.outbox; retention is the maintenance job's decision")
	}
}

func TestTheDispatchRoleHoldsNothingDangerous(t *testing.T) {
	pool, ctx := openAdmin(t)

	var superuser, bypassRLS, login, createDB, createRole bool
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT rolsuper, rolbypassrls, rolcanlogin, rolcreatedb, rolcreaterole
			  FROM pg_roles WHERE rolname = $1`, dispatchRole).Scan(
			&superuser, &bypassRLS, &login, &createDB, &createRole)
	}); err != nil {
		t.Fatalf("reading role attributes: %v", err)
	}

	switch {
	case superuser:
		t.Error("the dispatch role is a superuser")
	case bypassRLS:
		// Its tables carry no tenant column and need no policy, so a bypass privilege here would be
		// an unused capability that whatever table is added next silently inherits.
		t.Error("the dispatch role holds BYPASSRLS")
	case login:
		// A group role, like the other two runtimes: the deployable authenticates as a login role
		// that inherits this one, so the credential can be rotated without touching privileges.
		t.Error("the dispatch role can log in; it is a group role and a login role should inherit it")
	case createDB || createRole:
		t.Error("the dispatch role can create databases or roles")
	}
}

// TestTheDispatchRoleOwnsNothing keeps the ownership rule from TDD-organization-control-001 true for
// the fourth role: an owner can ALTER and DROP its own objects whatever the grants say, so a
// runtime that owns something has DDL the grant table does not show.
func TestTheDispatchRoleOwnsNothing(t *testing.T) {
	pool, ctx := openAdmin(t)

	var owned int
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*)
			  FROM pg_class c
			  JOIN pg_roles r ON r.oid = c.relowner
			 WHERE r.rolname = $1`, dispatchRole).Scan(&owned)
	}); err != nil {
		t.Fatalf("reading ownership: %v", err)
	}
	if owned != 0 {
		t.Errorf("the dispatch role owns %d objects, so it holds DDL on them regardless of grants", owned)
	}
}

// TestTheDispatchRoleCanDeadLetterWithInsertAlone answers the question the catalog cannot.
//
// The narrowed grant leaves this role INSERT and nothing else on platform.dead_letter, and the
// dispatcher's dead-letter write is an `INSERT ... SELECT FROM platform.outbox ... ON CONFLICT
// (event_id) DO NOTHING`. Whether that shape needs SELECT or UPDATE on its target is PostgreSQL's
// decision, not one to reason out: DO NOTHING does not read the conflicting row, but the grant was
// narrowed on that belief and a belief is not a test.
//
// It runs the insert twice. The second attempt is the one that matters -- it takes the conflict
// path against a row that already exists, which is the only case where a hidden read requirement
// would surface.
//
// The statement is written out here rather than called, because foundation-platform keeps it
// unexported. That is a drift risk and it is the smaller one: this asserts the SHAPE the privilege
// question turns on, and if the real statement changes shape, the mutation this repository already
// runs against the dispatch role's allowlist is what catches the privilege change.
func TestTheDispatchRoleCanDeadLetterWithInsertAlone(t *testing.T) {
	admin, ctx := openAdmin(t)

	eventID, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	aggregateID, err := id.NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}

	var createdAt time.Time
	if err := admin.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO platform.outbox
			    (event_id, event_type, aggregate_id, payload, envelope)
			VALUES ($1::uuid, 'com.scnehaux.organization.membership.security.revoked', $2::uuid,
			        '{}'::jsonb, jsonb_build_object('id', $3::text))
			RETURNING created_at`,
			eventID.String(), aggregateID.String(), eventID.String()).Scan(&createdAt)
	}); err != nil {
		t.Fatalf("seeding the outbox row: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			_, _ = tx.Exec(ctx, `DELETE FROM platform.dead_letter WHERE event_id = $1`, eventID.String())
			_, _ = tx.Exec(ctx, `DELETE FROM platform.outbox WHERE event_id = $1`, eventID.String())
			return nil
		})
	})

	dispatch, dispatchCtx := openAs(t, "organization_dispatch_app", os.Getenv("TEST_DISPATCH_PASSWORD"))

	deadLetter := `INSERT INTO platform.dead_letter
	    (event_id, event_type, envelope, payload, failure_class, failure_detail, attempts,
	     first_failed_at)
	SELECT event_id, event_type, envelope, payload, $3, $4, $5,
	       COALESCE(first_failed_at, now())
	FROM platform.outbox
	WHERE created_at = $1 AND event_id = $2
	ON CONFLICT (event_id) DO NOTHING`

	for attempt := 1; attempt <= 2; attempt++ {
		if err := dispatch.InTx(dispatchCtx, func(ctx context.Context, tx db.Tx) error {
			_, err := tx.Exec(ctx, deadLetter, createdAt, eventID.String(), "poison", "redacted", 1)
			return err
		}); err != nil {
			t.Fatalf("attempt %d: the dispatch role cannot dead-letter with INSERT alone: %v\n"+
				"The grant on platform.dead_letter was narrowed to INSERT on the understanding that "+
				"ON CONFLICT DO NOTHING reads nothing. Add back only the privilege PostgreSQL names here.",
				attempt, err)
		}
	}

	// And the row is there, read back by the owner: the dispatch role cannot see its own write,
	// which is the point of INSERT-only and is asserted below.
	var rows int
	if err := admin.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM platform.dead_letter WHERE event_id = $1`, eventID.String()).Scan(&rows)
	}); err != nil {
		t.Fatalf("counting dead letters: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d dead-letter rows after two attempts, want 1", rows)
	}

	// The negative half, on the same connection that just succeeded. Without it, a future widening
	// of this grant would make the test above pass for the wrong reason.
	//
	// SELECT is not the thing denied: the conflict clause requires it. What must stay denied is
	// changing an incident after it is written -- resolution is a separate role's decision, and a
	// delivery worker that can edit the record of its own failure is one whose bug erases itself.
	if err := dispatch.InTx(dispatchCtx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE platform.dead_letter SET resolved_at = now() WHERE event_id = $1`, eventID.String())
		return err
	}); err == nil {
		t.Error("the dispatch role can UPDATE platform.dead_letter; resolving an incident is not a delivery worker's decision")
	}
	if err := dispatch.InTx(dispatchCtx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM platform.dead_letter WHERE event_id = $1`, eventID.String())
		return err
	}); err == nil {
		t.Error("the dispatch role can DELETE from platform.dead_letter; the incident record must outlive the worker that wrote it")
	}
}

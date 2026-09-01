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
	"testing"

	"github.com/anshacerbia2/foundation-platform/db"
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
		{"platform.dead_letter", "SELECT"}: true,
		{"platform.dead_letter", "INSERT"}: true,
		{"platform.dead_letter", "UPDATE"}: true,
	}

	for _, g := range held {
		if !permitted[g] {
			t.Errorf("the dispatch role holds %s on %s, which is outside the outbox and the dead-letter table",
				g.privilege, g.table)
		}
	}

	// And the two it must have, so a revocation that broke delivery would fail here rather than in
	// production at the moment an event needed sending.
	for _, required := range []grant{{"platform.outbox", "SELECT"}, {"platform.outbox", "UPDATE"}} {
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

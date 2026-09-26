package controldb_test

// The platform schema's privilege posture, asserted from the catalog.
//
// `platform` is the one schema here that this repository does not own. It arrives from
// foundation-platform on a different release cadence, so a table can appear in this database
// without a line of code here changing — and until this file existed, it arrived carrying full DML
// for both runtime roles, because the grant loop covered the schema and ALTER DEFAULT PRIVILEGES
// handed the same to everything added later.
//
// That is how platform.delivery_receipt, the root of trust for dead-letter resolution, would have
// arrived INSERT-able by the ordinary request path. Nothing would have failed and nothing would
// have logged.
//
// Two layers, and they answer different questions:
//
//	CURRENT OBJECTS   what the roles hold on the tables that exist
//	FUTURE OBJECTS    what the next table foundation-platform adds will be handed
//
// The second is the one that matters more. A per-table revoke fixes one table; the default ACL is
// the reason there was something to fix, and leaving it in place makes every future table a race
// lost one migration at a time.
//
// Neither layer proves the matrix is complete. They detect the database drifting from what is
// declared here, which is a different and narrower claim. The contract's content is derived by
// tools/grantcheck, which reads the code and has PostgreSQL judge each grant.

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/anshacerbia2/foundation-platform/db"
)

// expectedPlatformPrivileges is the declared contract, and every line carries the execution path
// that earns it. A privilege with no path is not here, however harmless it looks.
var expectedPlatformPrivileges = map[string]map[string][]string{
	"organization_rt": {
		// outbox.Append, called from membership, invitation, workspace, organization, tenant and
		// offboarding transitions, inside the caller's own domain transaction.
		"outbox": {"INSERT"},
		// The idempotency claim store runs on the tenant connections by design.
		"idempotency_key": {"INSERT", "SELECT", "UPDATE"},
		// dead_letter: none. No request-path code reads incident evidence. It previously held
		// DELETE here, which let the ordinary request path remove the record of an undelivered
		// security event rather than resolve it.
		// processed_event: none. A consumer's inbox, used by foundation-reference against its own
		// database, and referenced nowhere in this repository's Go code.
	},
	"organization_provider_rt": {
		"outbox":      {"INSERT", "SELECT"},
		"dead_letter": {"SELECT"},
		// claimWithin inside withProviderScope. Completion runs on the tenant connections, so no
		// UPDATE; grantcheck found the one the old schema-wide grant carried unused.
		"idempotency_key": {"INSERT", "SELECT"},
	},
	// The resolver. Column-level UPDATE does not appear in table_privileges as UPDATE unless it is
	// table-wide, so the absence of UPDATE here is the assertion: this role may write four columns
	// and may not write the row.
	"organization_resolution_rt": {
		// The predicate reads the incident before it decides.
		"dead_letter": {"SELECT"},
		// The evidence, read-only. A resolver that could write receipts could manufacture the proof
		// it then consumes.
		"delivery_receipt": {"SELECT"},
		// claimWithin, inside the resolution transaction, for a /resolve carrying an
		// Idempotency-Key. INSERT ... ON CONFLICT needs SELECT. Completion runs elsewhere, so no
		// UPDATE.
		"idempotency_key": {"INSERT", "SELECT"},
	},
	"organization_dispatch_rt": {
		"outbox": {"SELECT", "UPDATE"},
		// SELECT is not for reading incidents. `ON CONFLICT (event_id) DO NOTHING` makes
		// PostgreSQL require SELECT on the table being inserted into; measured, not assumed.
		"dead_letter": {"INSERT", "SELECT"},
		// Measured for this table too rather than carried across from the one above. Assuming it
		// is how foundation-platform v0.2.5 shipped a preflight requiring INSERT alone here.
		"delivery_receipt": {"INSERT", "SELECT"},
	},
}

// TestThePlatformSchemaGrantsExactlyWhatWasDeclared is both halves at once.
//
// The negative half catches a role that gained something — a schema-wide grant added for
// convenience, a table inherited through default privileges. The positive half catches a role that
// LOST something it needs, which the negative half cannot see and which is how the dispatcher broke:
// a table added by a later platform version and never granted is invisible to a check that only
// looks for excess.
func TestThePlatformSchemaGrantsExactlyWhatWasDeclared(t *testing.T) {
	pool, ctx := openAdmin(t)

	held := map[string]map[string]map[string]bool{}
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT grantee, table_name, privilege_type
			  FROM information_schema.table_privileges
			 WHERE table_schema = 'platform'
			   AND grantee IN ('organization_rt', 'organization_provider_rt', 'organization_dispatch_rt',
				                   'organization_resolution_rt')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var role, table, privilege string
			if err := rows.Scan(&role, &table, &privilege); err != nil {
				return err
			}
			if held[role] == nil {
				held[role] = map[string]map[string]bool{}
			}
			if held[role][table] == nil {
				held[role][table] = map[string]bool{}
			}
			held[role][table][privilege] = true
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("reading platform privileges: %v", err)
	}

	for _, role := range sortedKeys(expectedPlatformPrivileges) {
		expected := expectedPlatformPrivileges[role]

		// Positive: everything declared must actually be held.
		for _, table := range sortedKeys(expected) {
			for _, privilege := range expected[table] {
				if !held[role][table][privilege] {
					t.Errorf("%s is missing %s on platform.%s, which grants.sql declares it needs;\n"+
						"the path that needs it is recorded beside the grant, so either the grant was "+
						"dropped or the code that justified it is gone",
						role, privilege, table)
				}
			}
		}

		// Negative: nothing beyond it.
		for _, table := range sortedKeys(held[role]) {
			for _, privilege := range sortedKeys(held[role][table]) {
				if !contains(expected[table], privilege) {
					t.Errorf("%s holds %s on platform.%s and nothing declares why.\n"+
						"If an execution path needs it, record the path in grants.sql beside the grant "+
						"and add it here. If nothing does, it is an over-grant.",
						role, privilege, table)
				}
			}
		}
	}
}

// TestNewPlatformTablesArriveClosed is the root-cause assertion.
//
// It reads pg_default_acl rather than any table, because the defect it guards has no table yet: it
// is about what the NEXT one inherits. While a broad default was in place, every per-table revoke
// above was undone by the following platform release, silently and on schedule.
func TestNewPlatformTablesArriveClosed(t *testing.T) {
	pool, ctx := openAdmin(t)

	var acl []string
	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT unnest(d.defaclacl)::text
			  FROM pg_default_acl d
			  JOIN pg_namespace n ON n.oid = d.defaclnamespace
			 WHERE n.nspname = 'platform'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var entry string
			if err := rows.Scan(&entry); err != nil {
				return err
			}
			acl = append(acl, entry)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("reading default privileges: %v", err)
	}

	for _, entry := range acl {
		grantee := entry
		if index := strings.Index(entry, "="); index >= 0 {
			grantee = entry[:index]
		}
		switch grantee {
		case "organization_rt", "organization_provider_rt", "organization_dispatch_rt", "organization_resolution_rt":
			t.Errorf("platform's default privileges hand %q to a runtime role.\n"+
				"The next table foundation-platform adds would arrive carrying it, with nothing "+
				"failing and nothing logging. Grant platform tables explicitly instead.", entry)
		}
	}
}

// And the schema half, which a table-privilege check cannot see: INSERT on a table inside a schema
// the role cannot enter is not usable, and the failure reads as a privilege problem on the wrong
// object.
func TestRuntimeRolesCanEnterThePlatformSchema(t *testing.T) {
	pool, ctx := openAdmin(t)

	for _, role := range []string{"organization_rt", "organization_provider_rt", "organization_dispatch_rt",
		"organization_resolution_rt"} {
		var permitted bool
		if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT has_schema_privilege($1, 'platform', 'USAGE')`, role).Scan(&permitted)
		}); err != nil {
			t.Fatalf("reading schema privilege for %s: %v", role, err)
		}
		if !permitted {
			t.Errorf("%s cannot USAGE the platform schema, so every table grant it holds is unusable", role)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

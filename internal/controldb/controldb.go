// Package controldb carries the Organization Database statements that Atlas does not own,
// embedded so the migration binary needs no files beside it.
//
// Three things live here and none belongs in schema.hcl:
//
//   - Roles are cluster objects rather than schema objects, and Atlas Community manages
//     schemas, tables, indexes, and constraints. Declaring the role graph as explicit SQL is
//     the honest boundary, not a workaround.
//   - Row-Level Security is not modelled by Atlas OSS at all: neither
//     `ENABLE`/`FORCE ROW LEVEL SECURITY` nor `CREATE POLICY`. That has a consequence worth
//     stating plainly — nothing reconciles a policy the way Atlas reconciles a column, so a
//     policy dropped by hand stays dropped while the schema still matches its declared state.
//     The integration assertions in this package read `pg_class` and `pg_policy` because they
//     are the only thing that would notice.
//   - Privileges must be granted after every object exists, including the `platform` tables
//     foundation-platform ships. A declarative desired state cannot express "after the other
//     owner's migration ran".
//
// ci.yml asserts the resulting privileges and policies against the PostgreSQL catalog, so no
// file here is trusted to be complete on the strength of having been written.
package controldb

import (
	"embed"
	"fmt"
)

//go:embed roles.sql rls.sql grants.sql
var statements embed.FS

// Stage names one ordered step of an Organization Database migration run.
type Stage string

const (
	// StageRoles creates the cluster roles and the schemas. It runs before any table exists.
	StageRoles Stage = "roles.sql"

	// StageRLS enables and forces Row-Level Security and creates the policies. It runs after
	// Atlas has applied the schema, because a policy names a table.
	//
	// Separate from StageGrants so the two failure modes stay separate: a missing grant
	// produces a runtime that cannot read, and a missing policy produces one that reads too
	// much. Collapsing them into one file would make a partial failure ambiguous.
	StageRLS Stage = "rls.sql"

	// StageGrants gives the runtime roles their privileges. It runs last, because a GRANT
	// names objects and an object that does not exist yet cannot be granted on.
	StageGrants Stage = "grants.sql"
)

// PostStages are the stages that run after Atlas, in order.
//
// Ordered deliberately: RLS before grants. Both orders work, and this one means a window
// where privileges exist without policies never opens — if the run fails between them, the
// runtime roles cannot yet reach the tables at all.
var PostStages = []Stage{StageRLS, StageGrants}

// SQL returns the statements for one stage.
func SQL(stage Stage) (string, error) {
	body, err := statements.ReadFile(string(stage))
	if err != nil {
		return "", fmt.Errorf("controldb: read %s: %w", stage, err)
	}
	if len(body) == 0 {
		return "", fmt.Errorf("controldb: %s is empty", stage)
	}
	return string(body), nil
}

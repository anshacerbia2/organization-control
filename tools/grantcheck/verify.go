package main

// Verification: PostgreSQL decides what each derived statement needs.
//
// The alternative -- parsing the SQL here and restating the privilege rules -- would be a second,
// weaker copy of the engine. ON CONFLICT with a target needs SELECT on the table, RETURNING needs
// SELECT on the returned columns, an UPDATE reading a column in its WHERE needs SELECT on it, and
// column-level grants follow their own rules. The engine already knows all of this, and it checks
// privileges when it builds a plan, so EXPLAIN is enough: nothing executes.
//
// Two questions, asked as each runtime role:
//
//	missing   Does every derived statement plan under the role that runs it? A refusal is a grant
//	          the code needs and the database does not give -- on any code path, not only the ones
//	          the test suite happens to execute.
//
//	unused    Is every grant the role holds needed by some derived statement? Each grant is revoked
//	          inside a transaction that is rolled back, and the role's statements are planned again.
//	          If none is refused, nothing in this repository needs the grant.
//
// Both run against a disposable, fully migrated database owned by a superuser -- the CI database.
// The revocations are rolled back, but they take locks, and they need a role able to revoke.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	fdb "github.com/anshacerbia2/foundation-platform/db"
)

// Relation is a table a plan touches.
type Relation struct {
	Schema string
	Name   string
}

func (r Relation) String() string { return r.Schema + "." + r.Name }

// Planned is one derived statement after the engine has seen it.
type Planned struct {
	Role      string
	Statement *Statement
	Relations map[Relation]bool
	Refused   string // the engine's refusal, when planning as the role failed

	// Sequences are the sequences the statement calls a sequence function on, and the privileges
	// each call needs. See sequenceCalls.
	Sequences map[string][]string
}

// Grant is one privilege a runtime role holds.
type Grant struct {
	Role      string
	Kind      string // table, column, schema, sequence
	Schema    string
	Object    string // table or sequence name; empty for a schema
	Column    string
	Privilege string
}

func (g Grant) String() string {
	switch g.Kind {
	case "schema":
		return fmt.Sprintf("%s %s ON SCHEMA %s", g.Role, g.Privilege, g.Schema)
	case "column":
		return fmt.Sprintf("%s %s (%s) ON %s.%s", g.Role, g.Privilege, g.Column, g.Schema, g.Object)
	case "sequence":
		return fmt.Sprintf("%s %s ON SEQUENCE %s.%s", g.Role, g.Privilege, g.Schema, g.Object)
	}
	return fmt.Sprintf("%s %s ON %s.%s", g.Role, g.Privilege, g.Schema, g.Object)
}

func (g Grant) revoke() string {
	role := quoteIdent(g.Role)
	switch g.Kind {
	case "schema":
		return fmt.Sprintf("REVOKE %s ON SCHEMA %s FROM %s", g.Privilege, quoteIdent(g.Schema), role)
	case "column":
		return fmt.Sprintf("REVOKE %s (%s) ON %s.%s FROM %s", g.Privilege, quoteIdent(g.Column),
			quoteIdent(g.Schema), quoteIdent(g.Object), role)
	case "sequence":
		return fmt.Sprintf("REVOKE %s ON SEQUENCE %s.%s FROM %s", g.Privilege,
			quoteIdent(g.Schema), quoteIdent(g.Object), role)
	}
	return fmt.Sprintf("REVOKE %s ON %s.%s FROM %s", g.Privilege, quoteIdent(g.Schema), quoteIdent(g.Object), role)
}

// Verdict is what the database said.
type Verdict struct {
	Planned []*Planned
	Unused  []Grant
}

type verifier struct {
	pool *fdb.Pool
	n    int
}

func verify(ctx context.Context, pool *fdb.Pool, d *Derivation) (*Verdict, error) {
	v := &verifier{pool: pool}
	out := &Verdict{}

	byRole := map[string][]*Planned{}
	for _, role := range Roles {
		var sqls []string
		for sql := range d.ByRole[role] {
			sqls = append(sqls, sql)
		}
		sort.Strings(sqls)
		for _, sql := range sqls {
			p := &Planned{Role: role, Statement: d.ByRole[role][sql]}
			if err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
				relations, refused, err := v.plan(ctx, tx, role, sql)
				p.Relations, p.Refused = relations, refused
				return err
			}); err != nil {
				return nil, err
			}
			p.Sequences = sequenceCalls(sql)
			if p.Refused == "" {
				refused, err := v.sequencePrivileges(ctx, role, p.Sequences)
				if err != nil {
					return nil, err
				}
				p.Refused = refused
			}
			out.Planned = append(out.Planned, p)
			byRole[role] = append(byRole[role], p)
		}
	}

	for _, role := range Roles {
		grants, err := v.grants(ctx, role)
		if err != nil {
			return nil, err
		}
		for _, g := range grants {
			if g.Kind == "sequence" {
				if !sequenceGrantUsed(g, byRole[role]) {
					out.Unused = append(out.Unused, g)
				}
				continue
			}
			used, err := v.needed(ctx, g, byRole[role])
			if err != nil {
				return nil, err
			}
			if !used {
				out.Unused = append(out.Unused, g)
			}
		}
	}
	return out, nil
}

// plan prepares and explains sql as role, inside a savepoint so a refusal leaves the transaction
// usable. It returns the relations the plan touches, or the refusal.
//
// A refusal that is not a privilege refusal is an error: a statement this tool cannot plan is a
// statement whose privileges it cannot judge, and passing it would be the silent gap the tool
// exists to close.
func (v *verifier) plan(ctx context.Context, tx fdb.Tx, role, sql string) (map[Relation]bool, string, error) {
	v.n++
	name := fmt.Sprintf("grantcheck_%d", v.n)

	if _, err := tx.Exec(ctx, "SAVEPOINT grantcheck_plan"); err != nil {
		return nil, "", err
	}
	relations, err := func() (map[Relation]bool, error) {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+quoteIdent(role)); err != nil {
			return nil, err
		}
		// The bindings the scope wrappers set. EXPLAIN evaluates no policy predicate, but a
		// statement reading these settings itself would otherwise fail on an unset parameter.
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', gen_random_uuid()::text, true),
		                                  set_config('app.provider_scope', 'true', true)`); err != nil {
			return nil, err
		}
		// A generic plan, so the relations reported for the matrix are not thinned by folding the
		// NULL arguments below into constants.
		if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "PREPARE "+name+" AS "+sql); err != nil {
			return nil, err
		}
		var params int
		if err := tx.QueryRow(ctx, `SELECT coalesce(cardinality(parameter_types), 0)
		                              FROM pg_prepared_statements WHERE name = $1`, name).Scan(&params); err != nil {
			return nil, err
		}
		nulls := strings.TrimSuffix(strings.Repeat("NULL, ", params), ", ")
		explain := "EXPLAIN (VERBOSE, FORMAT JSON) EXECUTE " + name
		if params > 0 {
			explain += "(" + nulls + ")"
		}
		var raw string
		if err := tx.QueryRow(ctx, explain).Scan(&raw); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "DEALLOCATE "+name); err != nil {
			return nil, err
		}
		return relationsIn(raw)
	}()
	if err != nil {
		if _, rollback := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT grantcheck_plan"); rollback != nil {
			return nil, "", fmt.Errorf("rolling back after %v: %w", err, rollback)
		}
		// PREPARE is not transactional: a statement prepared before the refusal survives it.
		_, _ = tx.Exec(ctx, "DEALLOCATE "+name)
		if isPrivilegeRefusal(err) {
			return nil, err.Error(), nil
		}
		return nil, "", fmt.Errorf("planning as %s: %w\n  %s", role, err, preview(sql))
	}
	if _, err := tx.Exec(ctx, "RESET ROLE"); err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT grantcheck_plan"); err != nil {
		return nil, "", err
	}
	return relations, "", nil
}

// grants lists what role holds directly: table, column, sequence and schema privileges.
const grantsStatement = `
SELECT CASE c.relkind WHEN 'S' THEN 'sequence' ELSE 'table' END, n.nspname, c.relname, '', a.privilege_type
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace,
       LATERAL aclexplode(c.relacl) a
 WHERE a.grantee = $1::text::regrole
   AND c.relkind IN ('r', 'p', 'v', 'S')
UNION ALL
SELECT 'column', n.nspname, c.relname, att.attname, a.privilege_type
  FROM pg_attribute att
  JOIN pg_class c ON c.oid = att.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace,
       LATERAL aclexplode(att.attacl) a
 WHERE a.grantee = $1::text::regrole
   AND att.attnum > 0 AND NOT att.attisdropped
UNION ALL
SELECT 'schema', n.nspname, '', '', a.privilege_type
  FROM pg_namespace n,
       LATERAL aclexplode(n.nspacl) a
 WHERE a.grantee = $1::text::regrole
 ORDER BY 1, 2, 3, 4, 5`

func (v *verifier) grants(ctx context.Context, role string) ([]Grant, error) {
	var out []Grant
	err := v.pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		rows, err := tx.Query(ctx, grantsStatement, role)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			g := Grant{Role: role}
			if err := rows.Scan(&g.Kind, &g.Schema, &g.Object, &g.Column, &g.Privilege); err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("reading the grants of %s: %w", role, err)
	}
	return out, nil
}

// needed revokes g inside a transaction that is always rolled back, and reports whether any of
// the role's statements is then refused.
//
// Every statement of the role is re-planned, not only those whose plan shows the object. The
// engine checks privileges on every relation a statement names, while a plan shows only what
// survived planning: a scan folded away, a join removed, or a sequence reached through a column
// default never appears. Filtering by plan reported the resolver's own evidence read as unused.
func (v *verifier) needed(ctx context.Context, g Grant, planned []*Planned) (bool, error) {
	var candidates []*Planned
	for _, p := range planned {
		if p.Refused == "" {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return false, nil
	}

	used := false
	errRollback := fmt.Errorf("grantcheck: rolled back")
	err := v.pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		if _, err := tx.Exec(ctx, g.revoke()); err != nil {
			return fmt.Errorf("%s: %w", g.revoke(), err)
		}
		for _, p := range candidates {
			_, refused, err := v.plan(ctx, tx, p.Role, p.Statement.SQL)
			if err != nil {
				return err
			}
			if refused != "" {
				used = true
				break
			}
		}
		return errRollback
	})
	if err != nil && err != errRollback && !strings.Contains(err.Error(), errRollback.Error()) {
		return false, err
	}
	return used, nil
}

func isPrivilegeRefusal(err error) bool {
	text := err.Error()
	return strings.Contains(text, "SQLSTATE 42501") || strings.Contains(text, "permission denied")
}

// relationsIn walks an EXPLAIN (FORMAT JSON) plan for every relation it touches.
func relationsIn(raw string) (map[Relation]bool, error) {
	var doc []map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("reading the plan: %w", err)
	}
	out := map[Relation]bool{}
	var visit func(node map[string]any)
	visit = func(node map[string]any) {
		if name, ok := node["Relation Name"].(string); ok {
			schema, _ := node["Schema"].(string)
			out[Relation{schema, name}] = true
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				if m, ok := child.(map[string]any); ok {
					visit(m)
				}
			}
		}
	}
	for _, entry := range doc {
		if plan, ok := entry["Plan"].(map[string]any); ok {
			visit(plan)
		}
	}
	return out, nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Sequence functions check their privilege when they run, inside the function, and EXPLAIN runs
// nothing: a role without USAGE on platform.outbox_sequence plans an INSERT calling nextval() and
// then fails on the first real call. So sequence privileges are read from the statement text and
// asked of the catalog directly, the one place this tool reads SQL instead of the engine.
//
// The privileges are PostgreSQL's own rules for each function: nextval needs USAGE or UPDATE,
// currval and lastval need USAGE or SELECT, setval needs UPDATE.
var sequenceCall = regexp.MustCompile(`(?i)\b(nextval|currval|setval)\s*\(\s*'([^']+)'`)

var sequenceFunctionPrivileges = map[string][]string{
	"nextval": {"USAGE", "UPDATE"},
	"currval": {"USAGE", "SELECT"},
	"setval":  {"UPDATE"},
}

func sequenceCalls(sql string) map[string][]string {
	out := map[string][]string{}
	for _, match := range sequenceCall.FindAllStringSubmatch(sql, -1) {
		function, sequence := strings.ToLower(match[1]), match[2]
		out[sequence] = sequenceFunctionPrivileges[function]
	}
	return out
}

// sequencePrivileges reports a refusal when role holds none of the privileges a sequence call
// accepts.
func (v *verifier) sequencePrivileges(ctx context.Context, role string, calls map[string][]string) (string, error) {
	for sequence, accepted := range calls {
		held := false
		err := v.pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
			for _, privilege := range accepted {
				var ok bool
				if err := tx.QueryRow(ctx, `SELECT has_sequence_privilege($1, $2, $3)`,
					role, sequence, privilege).Scan(&ok); err != nil {
					return err
				}
				if ok {
					held = true
					return nil
				}
			}
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("reading the privileges of %s on %s: %w", role, sequence, err)
		}
		if !held {
			return fmt.Sprintf("permission denied for sequence %s: needs one of %v, and a sequence "+
				"function checks this when it runs rather than when it is planned", sequence, accepted), nil
		}
	}
	return "", nil
}

// sequenceGrantUsed reports whether any statement of the role calls a sequence function the grant
// satisfies.
func sequenceGrantUsed(g Grant, planned []*Planned) bool {
	for _, p := range planned {
		for sequence, accepted := range p.Sequences {
			if sequence != g.Schema+"."+g.Object && sequence != g.Object {
				continue
			}
			for _, privilege := range accepted {
				if privilege == g.Privilege {
					return true
				}
			}
		}
	}
	return false
}

package controldb

// Isolation posture, verified against the running database rather than assumed from the
// migration that was supposed to create it.
//
// # Why this is code and not only a test
//
// Atlas OSS models neither `ENABLE`/`FORCE ROW LEVEL SECURITY` nor `CREATE POLICY` — verified
// against v1.3.2, whose `schema inspect` emits no trace of either and prints "Skipping … advanced
// objects. Upgrade to Pro." So the declarative schema cannot reconcile a policy the way it
// reconciles a column, and three consequences follow. Two are already closed:
//
//	A runtime role cannot remove a policy. Neither holds any DDL privilege, and neither owns a
//	table, so the application cannot weaken its own isolation.
//
//	Drift heals on deploy. rls.sql recreates every policy on every run and discovers its table
//	set from the catalog, so a dropped policy returns and a newly added table is protected
//	without anyone remembering to extend a list.
//
// The third is what this file exists for: **between deploys, in production, nothing checked.**
// CI asserts the posture of a throwaway database, which says nothing about the one serving
// traffic. A superuser action during an incident could drop FORCE from one table and the next
// signal would be a cross-tenant read.
//
// So the process that depends on the control verifies the control. That is not a workaround for
// the vendor gap — a declarative tool would not close it either, because reconciliation happens
// at deploy time and this gap is between deploys.
//
// # Fail closed, deliberately
//
// AssertIsolation is meant to be called at startup and to stop the process, and to back the
// readiness probe so a replica whose database lost a policy leaves the load balancer. Serving
// tenant-scoped traffic with isolation disabled is worse than not serving: EAD-006 §8 requires a
// security-control failure to fail closed, and an unprotected table is that failure.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/anshacerbia2/foundation-platform/db"
)

// RLSSchemas is the tenant-scoped set, per TDD-organization-control-001.
//
// `organization` and `projection` are absent deliberately rather than forgotten. An Organization
// sponsors several Tenants, so scoping it to one would be wrong; the consumer registry carries no
// tenant column at all. Both are protected by grant and by application authorization instead, and
// TestTenantRoleHoldsNothingOnOrganization asserts the grant half.
var RLSSchemas = []string{"tenant", "workspace", "membership", "invitation", "operation"}

// RuntimeRoles are the two roles that carry traffic. Neither may own a table or hold an attribute
// that would make a policy inert.
var RuntimeRoles = []string{"organization_rt", "organization_provider_rt"}

// TableProtection is the posture of one table.
type TableProtection struct {
	Schema   string
	Table    string
	Enabled  bool
	Forced   bool
	Policies int
}

// Qualified returns the schema-qualified name.
func (t TableProtection) Qualified() string { return t.Schema + "." + t.Table }

// IsolationReport is everything AssertIsolation found, so a caller can log the posture it
// verified rather than only whether the check passed.
type IsolationReport struct {
	Tables []TableProtection

	// Problems are stated as complete sentences naming the object and the missing control.
	// A caller logs them verbatim; nothing here needs interpretation at the call site.
	Problems []string
}

// OK reports whether the database is safe to serve tenant-scoped traffic.
func (r IsolationReport) OK() bool { return len(r.Problems) == 0 }

// Err returns a single error naming every problem, or nil.
func (r IsolationReport) Err() error {
	if r.OK() {
		return nil
	}
	return fmt.Errorf("controldb: tenant isolation is not intact: %s", strings.Join(r.Problems, "; "))
}

const protectionQuery = `
SELECT n.nspname,
       c.relname,
       c.relrowsecurity,
       c.relforcerowsecurity,
       (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid),
       EXISTS (
         SELECT 1 FROM pg_attribute a
          WHERE a.attrelid = c.oid AND a.attname = 'tenant_id'
            AND a.attnotnull AND a.attnum > 0 AND NOT a.attisdropped)
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = ANY($1) AND c.relkind = 'r'
 ORDER BY n.nspname, c.relname`

const roleQuery = `
SELECT rolname, rolsuper, rolbypassrls, rolcanlogin,
       (SELECT count(*) FROM pg_tables WHERE tableowner = rolname)
  FROM pg_roles
 WHERE rolname = ANY($1)`

// AssertIsolation reads the catalog and reports every way the isolation posture is not intact.
//
// It reads rather than repairs. Repair belongs to the migration job, which runs under a role that
// holds DDL; a process that could fix its own isolation could also change it, and this one
// deliberately cannot.
func AssertIsolation(ctx context.Context, pool *db.Pool) (IsolationReport, error) {
	var report IsolationReport

	if err := pool.InTx(ctx, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, protectionQuery, RLSSchemas)
		if err != nil {
			return fmt.Errorf("read table protection: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				table       TableProtection
				hasTenantID bool
			)
			if err := rows.Scan(&table.Schema, &table.Table, &table.Enabled,
				&table.Forced, &table.Policies, &hasTenantID); err != nil {
				return fmt.Errorf("scan table protection: %w", err)
			}
			report.Tables = append(report.Tables, table)

			if !table.Enabled {
				report.Problems = append(report.Problems,
					fmt.Sprintf("%s has no row-level security", table.Qualified()))
			}
			// The one that is routinely skipped, and the reason ADR-GLB-002 §5.2 names it
			// explicitly: without FORCE, policies do not apply to the table owner, so the
			// control is inert against exactly the connection most likely to be misused
			// during an incident — while the catalog still reports RLS as enabled.
			if table.Enabled && !table.Forced {
				report.Problems = append(report.Problems,
					fmt.Sprintf("%s has row-level security enabled but not FORCED, so it does not apply to the owner", table.Qualified()))
			}
			// Two policies: one tenant-scoped, one provider-scoped. One policy means either a
			// caller has no access path, or a single permissive policy serves both — which is
			// the conflation the two roles exist to prevent.
			if table.Policies != 2 {
				report.Problems = append(report.Problems,
					fmt.Sprintf("%s carries %d policies, want 2 (tenant scope and provider scope)",
						table.Qualified(), table.Policies))
			}
			// A protected table without the discriminator would make its policy raise at query
			// time on a column that does not exist, turning a schema mistake into an outage.
			if !hasTenantID {
				report.Problems = append(report.Problems,
					fmt.Sprintf("%s is in a tenant-scoped schema and carries no non-nullable tenant_id", table.Qualified()))
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate table protection: %w", err)
		}

		// No tables at all is the failure that would otherwise pass silently: every loop above
		// is vacuous over an empty set, and a report with no problems would read as intact.
		if len(report.Tables) == 0 {
			report.Problems = append(report.Problems,
				"no tables found in the tenant-scoped schemas; the migration did not run")
		}

		roleRows, err := tx.Query(ctx, roleQuery, RuntimeRoles)
		if err != nil {
			return fmt.Errorf("read runtime roles: %w", err)
		}
		defer roleRows.Close()

		seen := map[string]bool{}
		for roleRows.Next() {
			var (
				name                    string
				super, bypass, canLogin bool
				owned                   int
			)
			if err := roleRows.Scan(&name, &super, &bypass, &canLogin, &owned); err != nil {
				return fmt.Errorf("scan runtime role: %w", err)
			}
			seen[name] = true

			// Either attribute makes every policy in the database inert while the catalog still
			// reports RLS as enabled — a control that reads as present and is not.
			if super {
				report.Problems = append(report.Problems, fmt.Sprintf("%s holds SUPERUSER", name))
			}
			if bypass {
				report.Problems = append(report.Problems,
					fmt.Sprintf("%s holds BYPASSRLS, which makes every policy inert", name))
			}
			// Group roles carry privilege and are not authenticated as. A deployable
			// authenticates as a login role that inherits one, so rotating a credential never
			// touches a grant.
			if canLogin {
				report.Problems = append(report.Problems,
					fmt.Sprintf("%s can log in; it is a group role", name))
			}
			// Ownership is what would make the DML grants insufficient: an owner can ALTER and
			// DROP its own tables regardless of which privileges were granted.
			if owned != 0 {
				report.Problems = append(report.Problems,
					fmt.Sprintf("%s owns %d table(s); it must own none", name, owned))
			}
		}
		if err := roleRows.Err(); err != nil {
			return fmt.Errorf("iterate runtime roles: %w", err)
		}

		for _, role := range RuntimeRoles {
			if !seen[role] {
				report.Problems = append(report.Problems,
					fmt.Sprintf("role %s does not exist", role))
			}
		}
		return nil
	}); err != nil {
		return IsolationReport{}, err
	}

	// Sorted so a log line or a test failure reads the same on every run. An unordered list of
	// problems makes two identical failures look like different ones.
	sort.Strings(report.Problems)
	return report, nil
}

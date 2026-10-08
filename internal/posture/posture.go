package posture

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
// AssertIsolation is called in three places. organization-migrate -stage=post calls it as the
// deploy's post-condition. cmd/organization-control calls it at startup, on the tenant connections,
// and refuses to serve on a problem. Probe runs it behind GET /readyz, so a replica whose database
// lost a policy between deploys leaves the load balancer. Serving tenant-scoped traffic with
// isolation disabled is worse than not serving: EAD-006 §8 requires a security-control failure to
// fail closed, and an unprotected table is that failure.
//
// # Why a package of its own
//
// It reads the catalog and holds no statement that changes anything. internal/controldb holds the
// stage SQL and the maintenance steps, which belong to the migrate deployable; the serving process
// imports this package and not that one, so the binary that serves requests carries no DDL.
//
// The runtime role can run it. pg_class, pg_policy, pg_attribute, pg_namespace, pg_roles and
// pg_tables are readable by PUBLIC, and TestTheRuntimeRoleSeesTheWholePosture asserts that the
// tenant login role sees every protected table, so the check at startup is not vacuous.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/db"
)

// RLSSchemas is the tenant-scoped set, per TDD-organization-control-001.
//
// `organization` and `projection` are absent deliberately rather than forgotten. An Organization
// sponsors several Tenants, so scoping it to one would be wrong; the consumer registry carries no
// tenant column at all. Both are protected by grant and by application authorization instead, and
// TestTenantRoleHoldsNothingOnOrganization asserts the grant half.
var RLSSchemas = []string{"tenant", "workspace", "membership", "invitation", "operation"}

// RuntimeRoles are the roles that carry request traffic under Row-Level Security. None may own a
// table or hold an attribute that would make a policy inert.
var RuntimeRoles = []string{"organization_rt", "organization_provider_rt", "organization_consumer_rt",
	"organization_self_rt"}

// AdditionalPolicies are the policies a table may carry beyond its tenant-scope and provider-scope
// pair, by name. Anything else found on a protected table is a problem.
//
// membership.membership_event and tenant.tenant_event are read by the dead-letter resolver, which runs as its own role
// (organization_resolution_rt) so that closing an incident cannot be done with the credential that
// replays one. That role needs a SELECT policy of its own here; the two runtime roles' policies do not
// name it.
//
// membership.membership and tenant.tenant are read by a registered consumer, which runs as its own role
// (organization_consumer_rt) so that a consumer credential is not the control plane's. Its snapshot
// and fresh check read both across Tenants, through a SELECT policy of its own on each.
//
// The three self-read policies serve a person reading their own contexts (ADR-ORG-005), as
// organization_self_rt, which the tenant role may SET ROLE to and does not inherit.
//
// membership.tenant_admin_grant is written by a provider only, as the tenant role inside one Tenant
// (ADR-ORG-003). Two restrictive policies keep the tenant role's writes a provider's.
//
// membership.membership_batch and membership.membership_batch_item are purged of expired previews by
// the maintenance stage, as the migration role that owns them, through one policy each that admits
// expired previews only and no write but a delete.
var AdditionalPolicies = map[string][]string{
	"membership.membership_event": {"membership_event_resolution_read"},
	"membership.tenant_admin_grant": {"tenant_admin_grant_granted_by_provider",
		"tenant_admin_grant_revoked_by_provider", "tenant_admin_grant_self_read"},
	"tenant.tenant_event":              {"tenant_event_resolution_read"},
	"membership.membership":            {"membership_consumer_read", "membership_self_read"},
	"membership.membership_batch":      {"membership_batch_purge"},
	"membership.membership_batch_item": {"membership_batch_item_purge"},
	"tenant.tenant":                    {"tenant_consumer_read", "tenant_self_read"},
}

// TableProtection is the posture of one table.
type TableProtection struct {
	Schema   string
	Table    string
	Enabled  bool
	Forced   bool
	Policies int

	// PolicyNames are the policies found, sorted, so a problem can name what is missing or extra.
	PolicyNames []string
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
	return fmt.Errorf("posture: tenant isolation is not intact: %s", strings.Join(r.Problems, "; "))
}

const protectionQuery = `
SELECT n.nspname,
       c.relname,
       c.relrowsecurity,
       c.relforcerowsecurity,
       ARRAY(SELECT p.polname::text FROM pg_policy p WHERE p.polrelid = c.oid ORDER BY 1),
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
				&table.Forced, &table.PolicyNames, &hasTenantID); err != nil {
				return fmt.Errorf("scan table protection: %w", err)
			}
			table.Policies = len(table.PolicyNames)
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
			// Two policies by name: one tenant-scoped, one provider-scoped. A missing one means a
			// caller has no access path, or a single permissive policy serves both — which is the
			// conflation the two roles exist to prevent. Checked by name rather than by count, so a
			// declared extra policy cannot hide a missing required one, and an undeclared one is
			// reported rather than tolerated.
			if problem := policyProblem(table); problem != "" {
				report.Problems = append(report.Problems, problem)
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

// policyProblem states what is wrong with a table's policies, or returns "".
func policyProblem(table TableProtection) string {
	required := []string{table.Table + "_tenant_scope", table.Table + "_provider_scope"}
	allowed := map[string]bool{}
	for _, name := range append(required, AdditionalPolicies[table.Qualified()]...) {
		allowed[name] = true
	}
	found := map[string]bool{}
	var extra []string
	for _, name := range table.PolicyNames {
		found[name] = true
		if !allowed[name] {
			extra = append(extra, name)
		}
	}
	var missing []string
	for _, name := range append(required, AdditionalPolicies[table.Qualified()]...) {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	return fmt.Sprintf("%s carries %d policies %v; missing %v, undeclared %v (want tenant scope and provider scope, plus any declared in AdditionalPolicies)",
		table.Qualified(), len(table.PolicyNames), table.PolicyNames, missing, extra)
}

// Probe answers readiness for the serving process: the database is reachable and its isolation
// posture is intact.
//
// A failed probe takes the replica out of the load balancer and leaves it running, which is the
// right response to both failures. A restart cannot repair a dropped policy, and liveness touches
// no dependency for that reason (internal/httpapi). The migration job repairs the posture, and the
// next probe after it passes.
//
// It reads the catalog on every probe rather than caching a pass. The read is two catalog queries,
// and a cached pass would keep a replica serving for the cache's lifetime after the posture broke.
type Probe struct {
	pool *db.Pool
}

// NewProbe constructs the readiness probe on the connections ordinary traffic uses.
func NewProbe(pool *db.Pool) (*Probe, error) {
	if pool == nil {
		return nil, errors.New("posture: a pool is required")
	}
	return &Probe{pool: pool}, nil
}

// Ping reports the first reason this replica must not serve, or nil.
func (p *Probe) Ping(ctx context.Context) error {
	if err := p.pool.Ping(ctx); err != nil {
		return err
	}
	report, err := AssertIsolation(ctx, p.pool)
	if err != nil {
		return err
	}
	return report.Err()
}

// Startup is the check the serving process runs before it binds a port. It is bounded, so a
// database that hangs on the catalog read stops the start rather than stalling it.
func Startup(ctx context.Context, pool *db.Pool, timeout time.Duration) (IsolationReport, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	report, err := AssertIsolation(ctx, pool)
	if err != nil {
		return IsolationReport{}, fmt.Errorf("verify tenant isolation: %w", err)
	}
	return report, report.Err()
}

---
doc_meta:
  id: TDD-organization-control-001
  title: Tenant Isolation and Row-Level Security
  owner: Core Platform Team
  version: 1.1.0
  status: approved
  classification: restricted
  review_cycle_days: 90
  created_date: 2026-08-10
  last_reviewed: 2026-08-23
  parent_sad: SAD-004
---

# Tenant Isolation and Row-Level Security

## Purpose

Specify which tables in the Organization Database carry Row-Level Security, what the
isolation predicate binds to, how the runtime supplies that binding, and how
cross-tenant denial is proven by test rather than asserted by review.

STD-GLB-002 makes three of these mandatory and one of them is routinely skipped:

> Isolation tests MUST prove cross-tenant denial using the actual application runtime
> role; a test executed on an administrative or owning connection is not isolation
> evidence.

A policy that has never been exercised by the role that carries production traffic is
an untested control. This design treats the test as the deliverable and the policy as
its implementation.

## Scope

**In scope**

- The set of tables that carry RLS and the reason each does or does not.
- The isolation predicate and the session binding it reads.
- Runtime roles, grants, and the single code path permitted to set tenant scope.
- Provider-scoped administration and how it is separated from tenant-scoped access.
- Cross-tenant denial tests executed as the runtime role.

**Out of scope**

- Application authorization, which remains mandatory and is never replaced by RLS.
- Membership authority, versions, and revocation mechanics.
- Projection publication and consumer freshness.
- The Control Database of the Identity Control Service, which holds no tenant-scoped
  authority table.

## Technical Context

The Organization Database is pooled: every Tenant shares one physical database.
SAD-004 §6.3 accepts that profile and requires compensating controls, of which RLS is
the database-enforced layer.

Isolation here has two distinct callers, and conflating them is the common failure:

| Caller | Scope | Example |
| :-- | :-- | :-- |
| Tenant administration | Exactly one Tenant | A Tenant administrator granting a Membership inside their own Tenant |
| Provider administration | Deliberately cross-Tenant | ATI operations suspending a Tenant, or investigating an incident |

A single role serving both cannot be constrained, because any policy permissive
enough for provider work is permissive enough for a defect in a tenant-scoped path.
The design therefore separates them at the role level, where the separation is
visible in `pg_stat_activity` and in audit output.

RLS is defense in depth. EAD-006 §6.6 keeps authorization accountable to the
application, and STD-GLB-002 prohibits relying on ad-hoc tenant `WHERE` clauses as
the authoritative control. Both layers are required; neither substitutes.

## Component Design

### Packages

| Package | Responsibility |
| :-- | :-- |
| `internal/db` | The only package permitted to bind a transaction's isolation scope. Holds `TenantPool`, `ProviderPool`, and the three entry points below |
| `internal/controldb` | The SQL this design applies — roles, policies, grants — and `AssertIsolation`, which reads the catalog back and refuses a database whose posture has drifted |
| `internal/system` | One constant: the CloudEvents source naming this system in every envelope it publishes |

These three carry no domain authority and appear in no other design's component table, which is
why they are listed here rather than left implicit. A reader who found `TenantPool` in a service
signature and no design that mentions it would have to infer the isolation model from the code
that depends on it.

`internal/system` sits here for a different reason than the other two: it is the published
identity of this service, shared by every package that appends to the outbox, and a constant
declared in each of them would be the same string written six times — whose failure mode is not a
compile error but two sources appearing in a consumer's stream for one system.

### Table Classification

| Schema | RLS | Reason |
| :-- | :-- | :-- |
| `tenant` | Yes | Rows are the Tenants themselves; a tenant-scoped caller sees one row |
| `workspace` | Yes | Directly tenant-scoped through `tenant_id` |
| `membership` | Yes | Directly tenant-scoped through `tenant_id` |
| `invitation` | Yes | Carries a target identifier before acceptance and is tenant-scoped |
| `operation` | Yes | Lifecycle operations, approvals, and offboarding obligations are tenant-scoped |
| `organization` | No | An Organization sponsors Tenants and is not contained by one; access is provider-scoped or resolved through an explicit relationship |
| `projection` | No | Consumer registry and cursors are operational state with no tenant column |
| `platform` | No | Outbox, deduplication, idempotency, and migration state carry no tenant column |

Every table carrying RLS has a non-nullable `tenant_id`. A tenant-scoped table without
that column is a modelling error, and the migration test rejects it rather than
silently leaving the table unprotected.

`organization` is the deliberate exception and the one most likely to be misread. An
Organization may sponsor several Tenants, so scoping it to one Tenant would be wrong.
Its protection is provider-scope plus application authorization, and that is stated
here so a future reviewer does not "fix" it by adding a policy that breaks the model.

### Roles

```sql
-- Owns the tables. Used only by the migration job.
CREATE ROLE organization_migrator;

-- Tenant-scoped runtime. Carries ordinary administrative traffic.
CREATE ROLE organization_rt          NOLOGIN;

-- Provider-scoped runtime. Carries cross-tenant provider operations only.
CREATE ROLE organization_provider_rt NOLOGIN;

-- Neither runtime role owns a table, holds SUPERUSER, holds BYPASSRLS,
-- or holds any DDL privilege.
GRANT USAGE ON SCHEMA organization, tenant, workspace, membership,
                      invitation, operation, projection, platform
  TO organization_rt, organization_provider_rt;
```

The process opens two pools, one per runtime role. Provider traffic is routed to the
provider pool by the authorization layer, never by a request parameter. A defect in a
tenant-scoped handler cannot reach the provider pool, because the handler holds no
reference to it.

## Data Model

### Policy

Applied identically to every table in the RLS set:

```sql
ALTER TABLE membership.membership ENABLE  ROW LEVEL SECURITY;
ALTER TABLE membership.membership FORCE   ROW LEVEL SECURITY;

-- Tenant-scoped access. Reads and writes are confined to the bound Tenant.
CREATE POLICY membership_tenant_scope ON membership.membership
    FOR ALL
    TO organization_rt
    USING      (tenant_id = current_setting('app.tenant_id', false)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', false)::uuid);

-- Provider-scoped access. Deliberately cross-Tenant, and still not BYPASSRLS.
CREATE POLICY membership_provider_scope ON membership.membership
    FOR ALL
    TO organization_provider_rt
    USING      (current_setting('app.provider_scope', false)::boolean)
    WITH CHECK (current_setting('app.provider_scope', false)::boolean);
```

Three choices in that block carry weight.

`FORCE ROW LEVEL SECURITY` is mandated by STD-GLB-002 because without it policies do
not apply to the table owner, and the control is inert against exactly the connection
most likely to be misused during an incident.

`WITH CHECK` mirrors `USING` so a tenant-scoped caller cannot write a row belonging to
another Tenant. A policy carrying only `USING` restricts reads and leaves inserts and
updates open, which is a common and quiet defect.

`current_setting(..., false)` uses `missing_ok = false` deliberately. An unset binding
raises an error instead of returning `NULL`, and a `NULL` predicate would evaluate to
false and silently return zero rows. A query returning nothing looks like an empty
result; a query raising an error looks like the defect it is.

The provider policy still reads a session setting rather than granting unconditional
access, so an unbound provider connection fails closed in the same way.

### Session Binding

The binding is transaction-scoped:

```sql
SET LOCAL app.tenant_id = '019235f2-4d11-7a03-b8c7-1e9f7a2c4b60';
```

`SET LOCAL` reverts at commit or rollback. A pooled connection therefore cannot carry
a previous request's Tenant into the next one, which is the failure mode that makes
connection pooling and RLS interact badly when `SET` is used instead.

## API / Interface

### The Single Binding Path

```go
package db

// Body is the work performed inside a bound transaction.
type Body func(context.Context, Tx) error

// WithTenantScope runs fn inside a transaction bound to exactly one Tenant.
// The tenant identifier is taken from the authenticated administrative context.
// It is never taken from a request path, query parameter, header, or body.
func WithTenantScope(ctx context.Context, pool *TenantPool, fn Body) error

// WithProviderScope runs fn inside a provider-scoped transaction. It requires an
// authenticated provider context, an operation reason, and a correlation identifier,
// and it records privileged access before fn executes.
func WithProviderScope(ctx context.Context, pool *ProviderPool, reason string, fn Body) error
```

These two functions are the only code permitted to bind `app.tenant_id` or
`app.provider_scope`. Both take the scope from the authenticated context rather than from
an argument, so a handler cannot pass a Tenant it was told about by the caller.

`TenantPool` and `ProviderPool` are distinct types rather than one type with a flag, so
handing a tenant-scoped handler the cross-Tenant pool is a compile error rather than a
review finding. A boolean would move the decision to the call site, which is where defects
live: any policy permissive enough for provider work is permissive enough for a mistake in
a tenant-scoped path. `ProviderPool` additionally cannot be constructed without a
privileged-access recorder, because an optional recorder is one a deployment forgets to
supply — after which every cross-tenant access is unattributable and nothing reports it.

**The binding is issued as `set_config('app.tenant_id', $1, true)`, not as literal
`SET LOCAL` text.** The two are the same statement — `is_local = true` is what `SET LOCAL`
means — and the function form is the one that accepts a bind parameter. `SET LOCAL` takes
no parameters, so using it would require building the statement by concatenating the Tenant
identifier into SQL text, which is the one thing this file must never do given that it is
the file establishing what the policy trusts. The architecture test scans for both
spellings, so the single-path rule is enforced regardless of which is written.

Both signatures carry `db.Tx`, an alias for the transaction handle from
`foundation-platform`, rather than naming the driver's `pgx.Tx`. `arch.json` denies this
repository any import of pgx: the shared module is the single place the driver is named, so
replacing it is one module's change rather than every consumer's. The shape is unchanged.

This implements SAD-004 §8.3 directly: a Tenant identifier arriving from a client is
a *requested* scope, and the authoritative scope is resolved from the authenticated
administrative context and current Membership. The requested value is compared against
the resolved value and a mismatch is refused before any query runs.

An architecture test asserts that no package other than `db` binds either setting, in
either spelling — `SET LOCAL app.` or `set_config('app.…', …, true)`. It walks the
repository rather than relying on review, because a second binding path anywhere would be a
second answer to "which Tenant is this", and Row-Level Security would faithfully enforce
whichever one ran last.

## Algorithms / Logic

### Scope Resolution

```text
resolve(request):
    actor    := authenticated principal and administrative context
    requested := tenant identifier carried by the request, if any

    if actor holds provider administrative scope for this operation:
        require reason and correlation identifier
        emit privileged-administration event
        return ProviderScope

    resolved := active organization-administrative assignment for actor
    if requested is present and requested != resolved:
        refuse with 403 before opening a transaction

    return TenantScope(resolved)
```

Refusing before the transaction opens matters: it keeps a cross-tenant attempt out of
the database entirely, so the RLS layer stays a compensating control rather than the
first line of defence.

### Grant and Policy Assertion

Run against the integration database on every build:

```sql
-- Every table with a tenant_id column carries RLS, enabled and forced.
SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
FROM   pg_class c
JOIN   pg_namespace n ON n.oid = c.relnamespace
WHERE  n.nspname IN ('tenant','workspace','membership','invitation','operation')
  AND  c.relkind = 'r';

-- Neither runtime role owns a table or holds a dangerous attribute.
SELECT rolname, rolsuper, rolbypassrls
FROM   pg_roles
WHERE  rolname IN ('organization_rt','organization_provider_rt');
```

The assertion fails when any table in those schemas has `relrowsecurity = false` or
`relforcerowsecurity = false`, when either runtime role owns a table, when either
holds `SUPERUSER` or `BYPASSRLS`, or when either holds a DDL privilege.

Grants and policies drift through migrations. Asserting them on every build is what
keeps the boundary real after the engineer who wrote it has moved on.

## Configuration

| Variable | Default | Purpose |
| :-- | :-- | :-- |
| `ORGANIZATION_DB_DSN` | none, required | Connection string using `organization_rt` |
| `ORGANIZATION_DB_PROVIDER_DSN` | none, required | Connection string using `organization_provider_rt` |
| `ORGANIZATION_DB_MAX_CONNS` | `20` | Tenant-scoped pool ceiling |
| `ORGANIZATION_DB_PROVIDER_MAX_CONNS` | `4` | Provider pool ceiling, held low deliberately |

The provider pool is small on purpose. Provider operations are administrative and
infrequent, and a low ceiling bounds the damage a runaway cross-tenant job can do
before it exhausts its own capacity rather than the Tenant-facing capacity.

Migrations run as `organization_migrator` in a job separate from the application. The
runtime roles hold no DDL privilege, so a defect in application code cannot alter a
policy or disable RLS on a table.

## Testing Strategy

### Isolation, executed as the runtime role

Every test in this group connects as `organization_rt`. A test on an owning or
administrative connection is explicitly not accepted as evidence.

- A `SELECT` bound to Tenant A returns no row belonging to Tenant B.
- An `INSERT` bound to Tenant A carrying Tenant B's `tenant_id` is rejected by
  `WITH CHECK`.
- An `UPDATE` bound to Tenant A cannot move a row into Tenant B.
- A `DELETE` bound to Tenant A affects no row of Tenant B.
- A query issued with `app.tenant_id` unset raises an error rather than returning an
  empty set.
- A transaction that sets scope, commits, and is followed by a second transaction on
  the same pooled connection without setting scope raises an error, proving
  `SET LOCAL` reverted.

### Provider scope

- A provider-scoped transaction reads across Tenants.
- A provider connection with `app.provider_scope` unset is refused.
- `WithProviderScope` emits a privileged-administration event carrying actor, reason,
  and correlation identifier before the transaction body runs.
- A tenant-scoped handler holds no reference to the provider pool, asserted by the
  import and wiring test.

### Structural

- Every table in the RLS schemas has `ENABLE` and `FORCE ROW LEVEL SECURITY`.
- Every table in the RLS schemas has a non-nullable `tenant_id`.
- Neither runtime role owns a table, holds `SUPERUSER`, holds `BYPASSRLS`, or holds a
  DDL privilege.
- `SET LOCAL app.` appears in no package other than `db`.
- A new table added to an RLS schema without a policy fails the migration test.

### Negative

- A request carrying a Tenant identifier that differs from the resolved administrative
  scope is refused with `403` before a transaction opens.
- Disabling RLS on any protected table fails the build.

## Security Notes

RLS is the second layer. Application authorization decides whether an operation is
permitted; RLS bounds the damage when that decision is wrong. Neither is described
here as sufficient alone, and STD-GLB-002 prohibits treating the application layer as
the only control.

The `app.tenant_id` binding is set from the authenticated context inside one function
and never from request input. That property, not the policy text, is what makes the
control trustworthy: a policy is only as strong as the value it reads.

Separating provider access into its own role and its own pool means a cross-tenant
read is attributable at the connection level. Incident review can distinguish a
provider operation from a tenant-scoped defect without reconstructing application
state.

An unset binding fails loudly. The alternative — a `NULL` predicate quietly returning
zero rows — would present a policy failure as an empty result, and empty results are
routinely dismissed as normal.

## Performance Notes

Each policy adds an equality predicate on `tenant_id`. Every RLS table carries
`tenant_id` as the leading column of its primary access index, so the predicate is
satisfied by the index rather than by a filter after the scan.

The provider policy evaluates one boolean session setting and adds no per-row cost.

Two pools cost additional idle connections against the database ceiling. The provider
pool is held small, so the combined ceiling stays close to the single-pool figure.

## Operational Notes

| Signal | Warning | Critical |
| :-- | :-- | :-- |
| Query raising an unset-binding error | any occurrence | 5 in an hour |
| `WITH CHECK` rejection | any occurrence | any occurrence |
| Provider-scoped transactions per hour | above the operational baseline | — |
| Provider transaction without a recorded reason | — | any occurrence |
| RLS assertion failure in CI | — | any occurrence |

A `WITH CHECK` rejection means application code attempted to write a row into a Tenant
it was not bound to. That is a defect or an attack, and it is treated as a security
finding rather than a validation error.

Runbooks required before production: unset-binding investigation, `WITH CHECK`
rejection triage, provider-access review, and suspected cross-tenant exposure.

## Traceability

| Relationship | Target |
| :-- | :-- |
| Parent system | SAD-004 — Scnehaux Organization Control |
| Realizes capability | PAD-PLT-002 — Organization & Tenancy Platform |
| Governed by | ADR-GLB-002 — Enterprise PostgreSQL Row-Level Security for Isolation |
| Governed by | ADR-ORG-001 — Separate Organization Authority and Keycloak Projection |
| Conforms to | STD-GLB-002 — `FORCE ROW LEVEL SECURITY`, non-owner runtime role, no `SUPERUSER`/`BYPASSRLS`, isolation proven as the runtime role |
| Enterprise constraint | EAD-003 — private domain persistence; cross-domain database access is prohibited |
| Enterprise constraint | EAD-006 — tenant isolation, privileged access attribution, and default deny |
| Related design | TDD-foundation-platform-001 — outbox and event envelope |

### Open Questions

1. Whether `invitation` rows remain readable before Tenant binding exists for an
   invited Principal. The current policy scopes them to the sponsoring Tenant, which
   covers administration; the acceptance path is resolved through a separate
   unauthenticated lookup with enumeration resistance and is designed separately.
2. Whether `operation` rows covering an Organization-level action that spans several
   Tenants belong under provider scope alone. The current classification places every
   `operation` row under one Tenant, and a cross-Tenant lifecycle operation would need
   an explicit representation before that holds.

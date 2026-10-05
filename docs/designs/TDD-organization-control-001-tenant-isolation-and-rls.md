---
doc_meta:
  id: TDD-organization-control-001
  title: Tenant Isolation and Row-Level Security
  owner: Core Platform Team
  version: 1.14.0
  status: approved
  classification: restricted
  review_cycle_days: 90
  created_date: 2026-08-10
  last_reviewed: 2026-10-04
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
- How a caller's authority is read: the token's standard claims, this service's provider
  grants, and its consumer registry (`ADR-ORG-001 §5.11`).
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

-- A registered projection consumer acting on its own records.
CREATE ROLE organization_consumer_rt NOLOGIN;

-- No runtime role owns a table, holds SUPERUSER, holds BYPASSRLS,
-- or holds any DDL privilege.
```

Privileges are deny by default in every schema. `grants.sql` revokes everything from the
runtime roles, removes the default privileges that would hand a later table to them, and
then grants each table and privilege a statement in this repository needs. No runtime role
holds `DELETE` on any business table, because nothing deletes a business row. The provider role
reads and inserts `organization.provider_grant` and cannot change a grant (§Caller Authority). The tenant-scoped
role reads `membership.tenant_admin_grant` and writes it only inside a provider's act on one Tenant,
which a restrictive policy enforces (§The Tenant Administration Grant). The tenant-scoped
role cannot create or change a Tenant, reach `organization`, `operation`, `projection` or
`audit`, or read Membership history. The provider role reads Membership and cannot write
it. `tools/grantcheck` keeps the grant list honest in both directions (§Grant Derivation):
a statement needing an ungranted privilege fails CI, and so does a grant nothing needs.

Every role that publishes an event holds `INSERT` on `platform.outbox` and
`platform.outbox_delivery`, and `SELECT` on `consumer`, `event_types` and `retired_at` of
`platform.subscription`. `outbox.Append` writes the event and the delivery each subscriber is owed
in one statement, and reads which consumers subscribe (`ADR-GLB-018 §5.6`). Reading those three
columns shows which consumers exist and what each receives. That is configuration, not authority
data, and the role still cannot read the outbox it appends to.

The consumer role holds what a consumer's eight routes need and nothing else:

- `SELECT` on `membership.membership` and `tenant.tenant`, through a `SELECT` policy of its own
  on each, keyed on the cross-Tenant binding;
- `SELECT` on `projection.consumer`, and `UPDATE` on four of its columns: `snapshot_mark`,
  `last_reported_mark`, `last_reported_at`, `verify_calls_since_report`;
- `SELECT` on what a provider grant confers, by column: `grant_id`, `principal_id`, `scope`, `kind`,
  `revoked_at` and `grant_version` of `organization.provider_grant`, and `activation_id`,
  `grant_id`, `decision`, `ended_at` and `ends_at` of `organization.provider_activation`, for the
  provider authority snapshot;
- `SELECT` on `platform.outbox`, `platform.outbox_delivery` and `platform.dead_letter`, for
  the snapshot mark and the frontier: its own owed deliveries and its own dead letters;
- `SELECT` on `consumer`, `event_types` and `retired_at` of `platform.subscription`, for the
  event types its registry row reports;
- `SELECT` and `INSERT` on `platform.idempotency_key`, for the claim every recorded scope makes.

It cannot write a business row, change its own declared terms or un-retire itself, register
a consumer, read an invitation, an Organization, a Workspace or delivery evidence, or write
the audit trail. A consumer ran as the provider role before this role existed, so its
credential could do all of that.

The process opens a pool per runtime role, and the consumer's only when consumer authority
is configured. Provider traffic is routed to the provider pool by the authorization layer,
never by a request parameter. A defect in a tenant-scoped handler cannot reach the provider
pool, because the handler holds no reference to it. Each pool refuses a scope that is not
its own, so a consumer scope cannot open the provider pool and the reverse.

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

// WithProviderInTenant runs fn as a provider acting inside one Tenant. It requires a
// provider scope and a reason, records privileged access before fn executes, and runs fn on
// the tenant pool bound to tenantID, with the provider bound as the acting provider.
func WithProviderInTenant(ctx context.Context, provider *ProviderPool, tenants *TenantPool,
    tenantID id.UUID, reason string, fn Body) error

// WithTenantRead runs fn in a read-only transaction bound to the Tenant of the tenant scope in
// ctx. It makes no idempotency claim. Authentication reads a tenant caller's records with it.
func WithTenantRead(ctx context.Context, pool *TenantPool, fn Body) error
```

These functions are the only code permitted to bind `app.tenant_id`, `app.provider_scope` or
`app.acting_provider`. `WithTenantScope` and `WithTenantRead` take the Tenant from the
authenticated context rather than from an argument, so a handler cannot pass a Tenant it was
told about by the caller.

**`WithProviderInTenant` is the one that takes a Tenant as an argument, and only from a
provider.** A provider already holds authority over every Tenant. Naming one Tenant narrows
that authority to the Tenant's own policy; it never widens a caller's reach. The function
refuses any scope that is not a provider's. A Tenant identifier from a tenant caller therefore
still never reaches a binding.

It exists because some provider acts change one Tenant's tenant-scoped rows:

- making a Tenant's first administrator, which writes a Membership (§The Tenant Administration Grant);
- the offboarding freeze, which suspends Memberships (`TDD-organization-control-004`).

Running those writes on the provider pool would put them under the provider policy, which is
not constrained to the Tenant being changed. Running them on the tenant pool bound to that
Tenant puts them under the Tenant's policy, as the tenant role. The provider is still the
actor, and the access record is written first, on the provider connections, as for every
provider scope.

The transaction also binds `app.acting_provider` to the provider's `principal_id`. Only this
function sets it. A restrictive policy reads it, so the database refuses a tenant
administration grant written by any other transaction.

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
    actor    := authenticated principal and administrative context (§Caller Authority)
    requested := tenant identifier carried by the request, if any

    if actor is a registered projection consumer:
        return ConsumerScope                  -- cross-Tenant, opens only the consumer pool

    if actor holds a provider grant:
        require reason and correlation identifier
        emit privileged-administration event
        return ProviderScope

    resolved := the tenant_id the token selects
    require actor administers resolved (§Caller Authority):
        an active Membership, an active Tenant, a tenant administration grant
    otherwise refuse with 403 before opening a transaction

    if requested is present and requested != resolved:
        refuse with 403 before opening a transaction

    return TenantScope(resolved)
```

Refusing before the transaction opens matters: it keeps a cross-tenant attempt out of
the database entirely, so the RLS layer stays a compensating control rather than the
first line of defence.

**A consumer has a scope of its own, and no provider authority.** A registered consumer reads
across Tenants, so its scope binds the cross-Tenant setting. That scope opens only the consumer
pool, which connects as `organization_consumer_rt` (§Roles).

- A consumer may call only its own eight routes:
  - its consumer record, progress, and bootstrap;
  - the Organization snapshot and the provider authority snapshot, each only when it subscribes
    to that projection's types;
  - the frontier;
  - the two context checks.
- Each of those routes also checks that the consumer names itself. A provider calling the same
  routes with a reason is served on the provider pool.
- Every other provider route checks the caller's provider authority, and every tenant route
  refuses a consumer scope.

Two versions of this were wrong, and both were found on 2026-09-27, before any production
deployment:

- The consumer was given the provider scope and ran as `organization_provider_rt`, so its
  credential could read and write everything the control plane can.
- The provider routes checked the scope alone. A consumer token that added
  `X-Administrative-Reason` was admitted to every provider route: it could suspend a Tenant,
  retire an Organization, or close or waive a dead letter.

### Caller Authority

A caller is one of three, decided from the token's standard claims and this service's own
records, never from a role in the token (`ADR-ORG-001 §5.11`, `STD-IAM-002 §3.1.1`). Every
token needs a `principal_id` that is a UUID and a `subject_type` of `human` or `workload`; the
`principal_id` is the actor every event and every evidence row names, and `sub` is not read.

**`aud` names `organization-control-api`**, this service's keyless `resource` registration in the
Identity Control API (`privileged`, lifetime class `L0`), registered by a provider through
`POST /v1/registrations`. It is never a client that authenticates, this service's own workload
client `organization-control-workload` included: STD-IAM-002 §3.1 forbids one in `aud`, because
the kernel lets a client named there exchange the token sent to it. Each caller reaches this API
because its registration's audience names the resource.

| Caller | Claims | Record read for each request | Refused when |
| :-- | :-- | :-- | :-- |
| Tenant administrator | `subject_type` `human`, `tenant_id`, `acr` `aal2` or higher, `auth_time` | in that Tenant: an active Membership for the `principal_id`, the Tenant active, and a tenant administration grant in force (§The Tenant Administration Grant) | `tenant_id` is not a UUID; the token is a workload's, below `aal2`, or without `auth_time`; any of the three facts is absent |
| Provider | `subject_type` `human`, `acr`, `auth_time`; no `tenant_id` | provider authority in force for the `principal_id`: an emergency grant, or an approved activation of an eligible grant (§Provider Activation) | no grant, or `acr` or `auth_time` absent |
| Eligible provider | as a provider | an eligible grant with no activation in force | reaches only `/v1/provider-activations`; every other route answers `403` before a transaction opens |
| Projection consumer | `subject_type` `workload`, `workload_owner`; no `tenant_id` | an active consumer registered with the `principal_id` | none registered, or consumer authority not configured |

```text
authenticate(token):
    verify signature, iss, aud, typ, exp                 -- foundation-platform verify
    principal := principal_id, a UUID
    type      := subject_type, human or workload
    if tenant_id present:
        require type is human, acr aal2 or higher, and auth_time
        require, in tenant_id, read now in one read-only transaction:
            an active Membership for principal
            the Tenant active
            a tenant administration grant for principal, not revoked
        return Tenant(principal, tenant_id)
    if type is human:
        require acr and auth_time
        require a provider grant for principal          -- read now
        if an emergency grant, or an activation in force:
            return Provider(principal, emergency?)
        return Eligible(principal)
    require workload_owner
    consumer := the active consumer registered with principal   -- read now
    return Consumer(principal, consumer)
```

The claim rule runs inside the verifier, and the records are read after it, in one read-only
transaction. A provider's and a consumer's are read on the provider connections. A Tenant
administrator's are read on the tenant connections, bound to the Tenant its token selects, under
that Tenant's policy (`WithTenantRead`). The verifier sees only claims, so it cannot read a
record; a claim-shaped token reaches the record read and is refused there if it names nobody the
records know. A record read that fails answers `503` and admits nobody.

The read happens for every request rather than once per token. A token outlives the record it
was issued against by up to its lifetime; a read per request makes a revoked grant, a retired
consumer, a revoked Membership or a suspended Tenant stop at the next request (`ADR-ORG-001
§5.11`, `ADR-ORG-003 §5.3`). Each read is indexed lookups by `principal_id` and, for a Tenant
administrator, `tenant_id`.

**The provider grant.**

```sql
CREATE TABLE organization.provider_grant (
    grant_id           UUID        PRIMARY KEY,
    principal_id       UUID        NOT NULL,
    scope              TEXT        NOT NULL,
    granted_by         UUID,
    bootstrap_operator TEXT,
    reason             TEXT        NOT NULL,
    granted_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at         TIMESTAMPTZ,
    revoked_by         UUID,
    revoke_reason      TEXT,
    CONSTRAINT provider_grant_scope_check CHECK (scope IN ('provider:organization-control')),
    CONSTRAINT provider_grant_reason_check CHECK (btrim(reason) <> ''),
    CONSTRAINT provider_grant_origin_check
        CHECK ((granted_by IS NULL) = (bootstrap_operator IS NOT NULL)),
    CONSTRAINT provider_grant_revocation_check
        CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)
           AND (revoked_at IS NULL) = (revoke_reason IS NULL)
           AND (revoke_reason IS NULL OR btrim(revoke_reason) <> ''))
);
CREATE UNIQUE INDEX provider_grant_principal_scope ON organization.provider_grant (principal_id, scope)
    WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX provider_grant_single_bootstrap ON organization.provider_grant ((true))
    WHERE granted_by IS NULL;
```

`scope` is checked against the registered scopes, which today is the one this service checks
(`STD-IAM-002 §3.1.1`). A grant names who made it: a provider's `principal_id`, or, for the
first grant only, the operator who ran the bootstrap. A grant is active until it is revoked, and
the authority read counts only an active one. The provider role holds `SELECT` and `INSERT` on
the table, `UPDATE` on the three revocation columns alone, and no `DELETE`: who was granted, by
whom, when and why cannot be rewritten, and a revoked grant stays as the record. One Principal
holds at most one active grant per scope; after a revocation it may be granted again, as a new
row.

**Granting and revoking, through the API** (`ADR-ORG-001 §5.11`):

```text
GET   /v1/provider-grants                       every grant, active and revoked, newest first
POST  /v1/provider-grants                       {"principal_id": ...}
POST  /v1/provider-grants/{grant_id}/revoke
```

Each is a provider route: it needs a provider caller and `X-Administrative-Reason`, and runs in
the provider scope, so the access record names the actor and the reason before the work runs.
The reason is visible US-ASCII on every route: a value holding any other octet is refused with
`400` before a handler reads it (STD-GLB-001 §Request Header Values, RFC 9110 §5.5), because such
octets carry no encoding the access record could rely on.
The reason is also the grant's or the revocation's own. A grant names the calling provider as
`granted_by`; a revocation names it as `revoked_by`.

```text
grant(principal):
    insert the grant, granted_by = caller,
        doing nothing on conflict with the partial unique index
    refuse when nothing was inserted: principal already holds one   -- 409

revoke(grant):
    lock every active grant (FOR UPDATE)
    refuse when the grant is unknown                  -- 404
    refuse when it is already revoked                 -- 409
    refuse when it is the last active grant           -- 409
    set revoked_at, revoked_by = caller, revoke_reason
```

**The last active grant is not revoked.** With none left, nobody could grant again, and the
bootstrap refuses a table that holds any grant, revoked ones included, so the estate would have
no way back to a provider short of the database owner. A provider revokes another's grant, or its
own while another remains. Locking the active grants first is what makes the rule hold against two
revocations at once: the second waits, then counts one. A grant needs no lock: the partial unique
index is what refuses a second active grant, against two concurrent requests as well, and the
insert reports the conflict rather than failing on it.

A revocation takes effect at the next request, because the authority read happens for every
request. A Principal is not checked against the Identity Control API when granted: this service
reaches no other domain (`ADR-ORG-001 §5.4`), and a grant naming a `principal_id` no token carries
confers nothing.

**The bootstrap.** `organization-control bootstrap-provider -principal-id <uuid> -operator
<name> -reason <text>` makes the first grant, following `ADR-ORG-001 §5.11`:

1. it runs on the provider connections, in one transaction;
2. it refuses when any grant exists, revoked ones included, and the partial unique index refuses a
   second bootstrap row even against two concurrent runs;
3. it records the operator and the reason in the row, which no runtime role can change;
4. it creates no identity: the Principal is one the Identity Control API's ceremony minted.

A rerun naming the Principal the bootstrap already granted reports that grant and writes
nothing, so a run whose answer was lost can be repeated. A rerun naming anyone else is refused.

**The consumer.** `projection.consumer` carries the consumer's `principal_id`, unique across
every row, retired ones included, so a workload Principal is one consumer for good. Registration
requires it, and re-registering a consumer under a different `principal_id` is refused
(`TDD-organization-control-002` §Consumer Registry). Consumer authority exists only when
`ORGANIZATION_CONSUMER_DATABASE_URL` is set, because only then is there a pool to serve it.

**Why no setting names a claim or a role.** The claim names are fixed by `STD-IAM-002 §3.2`, so
`ORGANIZATION_TENANT_CLAIM`, `ORGANIZATION_PROVIDER_ROLE`, `ORGANIZATION_CONSUMER_ROLE` and
`ORGANIZATION_CONSUMER_CLAIM` are gone. Startup refuses a deployment that still sets one, because
such a deployment expects authority to come from where it no longer does, and a silently ignored
setting would leave it believing so.

### The Tenant Administration Grant

`ADR-ORG-003` makes a Tenant administrator a record. A token's `tenant_id` only selects a
Tenant. It confers nothing: "Treat client-supplied tenant identifiers as selectors only. Verify
that the authenticated principal is authorized to act in the selected tenant" (OWASP,
_Multi-Tenant Application Security Cheat Sheet_). Since `ADR-IAM-006` the kernel issues a
`tenant_id` to every member who signs in for a Tenant, so before this record existed every
member administered their Tenant.

**The record.**

```sql
CREATE TABLE membership.tenant_admin_grant (
    grant_id      UUID        PRIMARY KEY,
    tenant_id     UUID        NOT NULL REFERENCES tenant.tenant (tenant_id),
    principal_id  UUID        NOT NULL,
    granted_by    UUID        NOT NULL,
    reason        TEXT        NOT NULL,
    granted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at    TIMESTAMPTZ,
    revoked_by    UUID,
    revoke_reason TEXT,
    CONSTRAINT tenant_admin_grant_reason_check CHECK (btrim(reason) <> ''),
    CONSTRAINT tenant_admin_grant_revocation_check
        CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)
           AND (revoked_at IS NULL) = (revoke_reason IS NULL)
           AND (revoke_reason IS NULL OR btrim(revoke_reason) <> ''))
);
CREATE UNIQUE INDEX tenant_admin_grant_active ON membership.tenant_admin_grant (principal_id, tenant_id)
    WHERE revoked_at IS NULL;
```

The record lives in `membership` because it names one Tenant. Row-Level Security therefore
covers it like every other table there: a tenant-scoped transaction sees its own Tenant's grants
only. One Principal holds at most one active grant per Tenant. After a revocation it may be
granted again, as a new row. No event carries a grant, and no claim does (`ADR-ORG-003 §5.1`).

**Who writes it, and how the database knows.** Only a provider grants and revokes
(`ADR-ORG-003 §5.2`). The write changes one Tenant's rows, so it runs through
`WithProviderInTenant` on the tenant pool, as the tenant role, under the Tenant's policy
(§The Single Binding Path). The tenant role therefore holds the privileges, and two
restrictive policies keep them a provider's:

```sql
CREATE POLICY tenant_admin_grant_granted_by_provider ON membership.tenant_admin_grant
    AS RESTRICTIVE FOR INSERT TO organization_rt
    WITH CHECK (granted_by = NULLIF(current_setting('app.acting_provider', true), '')::uuid);

CREATE POLICY tenant_admin_grant_revoked_by_provider ON membership.tenant_admin_grant
    AS RESTRICTIVE FOR UPDATE TO organization_rt
    USING      (NULLIF(current_setting('app.acting_provider', true), '') IS NOT NULL)
    WITH CHECK (revoked_by = NULLIF(current_setting('app.acting_provider', true), '')::uuid);
```

PostgreSQL 17, _CREATE POLICY_, <https://www.postgresql.org/docs/17/sql-createpolicy.html>,
accessed 2026-10-04: "When a mix of permissive and restrictive policies are present, a record is
only accessible if at least one of the permissive policies passes, in addition to all the
restrictive policies." The Tenant's own policy still applies, and these narrow it. An `UPDATE`
policy also covers the revocation's row lock: it applies to "`UPDATE`, `SELECT FOR UPDATE`, and
`SELECT FOR SHARE` commands".

- A grant must name the acting provider as `granted_by`.
- A revocation must name it as `revoked_by`.
- An ordinary tenant-scoped transaction never binds `app.acting_provider`. It reads the setting
  as `NULL`, and the database refuses its write.

This is the one setting read with `missing_ok` true, on purpose. Unset is the expected state
for every tenant transaction, and the policy refuses that state, which a raise would also do
less clearly.

The tenant role holds `SELECT` and `INSERT` on the table, and `UPDATE` on the three revocation
columns alone. It has no `DELETE`. The provider role holds nothing on it.

**Granting, listing and revoking, through the API.**

```text
GET   /v1/tenants/{tenant_id}/administrators                       every grant in the Tenant, newest first
POST  /v1/tenants/{tenant_id}/administrators                       {"principal_id": ...}
POST  /v1/tenants/{tenant_id}/administrators/{grant_id}/revoke
```

Each is a provider route. It needs a provider caller and `X-Administrative-Reason`, and it runs
through `WithProviderInTenant` for the Tenant in the path. The reason is also the grant's or the
revocation's own.

```text
grant(tenant, principal):
    in the Tenant, as the acting provider:
        refuse unless the Tenant exists                          -- 404
        refuse unless it is active                               -- 409
        insert the grant, granted_by = caller,
            doing nothing on conflict with the partial unique index
        refuse when nothing was inserted: one is in force       -- 409
        the Principal's Tenant-wide Membership, not revoked, locked:
            active     -> kept
            suspended  -> refuse                                 -- 409
            none       -> granted: human, valid from now,
                          provenance "tenant administration grant <grant_id>"

revoke(tenant, grant):
    in the Tenant, as the acting provider:
        set revoked_at, revoked_by = caller, revoke_reason, where not revoked
        refuse when no grant in the Tenant has the identifier   -- 404
        refuse when it was revoked already                       -- 409
```

**The first administrator gets a Membership with the grant** (`ADR-ORG-003 §5.2`). The grant
and the Membership commit in one transaction, with the Membership's event, through the
Membership service's own `GrantWithin`. The Identity Control API projects that Membership into
the kernel, so the new administrator can sign in for the Tenant.

- A Principal who already holds an active Tenant-wide Membership keeps it.
- A suspended one is refused rather than replaced. A second, active Membership would undo
  the Tenant's suspension of that Principal without anyone restoring it.

**Revoking a grant does not touch the Membership.** The Principal stays a member, and stops
administering at the next request. Ending the Membership is the Membership service's act.

The grant needs no lock: the partial unique index refuses a second active grant, against two
concurrent requests as well. The grant is inserted before the Membership, so the second request
stops there and never reaches the Membership's own unique index.

**The check for each request** (`ADR-ORG-003 §5.3`). A token with a `tenant_id` is a Tenant
administrator only when all of these hold.

- It is a person's: `subject_type` `human`.
- It carries `acr` `aal2` or higher, and `auth_time`. Tenant administration is privileged
  access, and `ADR-IAM-004` requires two factors for it. The levels follow `STD-IAM-002 §3.2`'s
  order: `aal1`, `aal2`, `phr`. Any other value, the kernel's unmapped `0` and `1` included,
  is below `aal1`.
- In that Tenant, read in one read-only transaction bound to it:
  - an active Membership for the `principal_id`, inside its validity window;
  - the Tenant's status is `active`;
  - a tenant administration grant for the `principal_id` that is not revoked.

The claims are checked in the verifier, so a token that fails them never reaches a read. Every
failure is `403`, and one message covers the three facts: a caller learns that it does not
administer the Tenant, not which record is missing.

**Nothing is served to a plain member.** Every tenant route sits behind this check, so a
member who is not an administrator reaches none (`ADR-ORG-003 §5.3`).

### Provider Activation

`ADR-ORG-002` makes a provider grant **eligible**. Authority comes from an **activation** of the
grant, which lasts a bounded time and, in production, is approved by another provider. An
**emergency grant** is standing. This is Entra Privileged Identity Management's eligible
assignment with "Require approval to activate" [R1][R2], and Google Cloud Privileged Access
Manager's entitlement and grant [R4].

```sql
ALTER TABLE organization.provider_grant
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'eligible'
        CHECK (kind IN ('eligible', 'emergency'));
-- the bootstrap grant is an emergency grant (ADR-ORG-002 §5.2)
UPDATE organization.provider_grant SET kind = 'emergency' WHERE granted_by IS NULL;

CREATE TABLE organization.provider_activation (
    activation_id     UUID        PRIMARY KEY,
    grant_id          UUID        NOT NULL REFERENCES organization.provider_grant(grant_id),
    principal_id      UUID        NOT NULL,
    scope             TEXT        NOT NULL,
    reason            TEXT        NOT NULL CHECK (btrim(reason) <> ''),
    duration_seconds  INTEGER     NOT NULL CHECK (duration_seconds > 0),
    approval_required BOOLEAN     NOT NULL,
    requested_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by        UUID,
    decision          TEXT        CHECK (decision IN ('approved', 'denied', 'lapsed')),
    decision_reason   TEXT,
    decided_at        TIMESTAMPTZ,
    ends_at           TIMESTAMPTZ,
    ended_by          UUID,
    end_reason        TEXT,
    ended_at          TIMESTAMPTZ,
    CONSTRAINT provider_activation_decision_check
        CHECK ((decision IS NULL) = (decided_at IS NULL)
           AND (decision IS NULL OR decision = 'lapsed' OR decided_by IS NOT NULL)),
    CONSTRAINT provider_activation_window_check
        CHECK ((decision = 'approved') = (ends_at IS NOT NULL)),
    CONSTRAINT provider_activation_separation_check
        CHECK (NOT approval_required OR decision IS DISTINCT FROM 'approved' OR decided_by <> principal_id),
    CONSTRAINT provider_activation_end_check
        CHECK ((ended_at IS NULL) = (ended_by IS NULL) AND (ended_at IS NULL) = (end_reason IS NULL))
);
CREATE UNIQUE INDEX provider_activation_pending
    ON organization.provider_activation (grant_id) WHERE decision IS NULL;
```

`provider_activation_separation_check` is AC-5 in the database: an activation that required
approval is never recorded approved by its own holder. Whether approval was required is fixed
when the activation is requested, so the row states the rule it was held to. The provider role
holds `SELECT` and `INSERT`, and `UPDATE` only on the decision and end columns. A grant's `kind`
is fixed when the grant is made.

```text
in force for (principal, scope):
    an unrevoked emergency grant
    or an approved activation of an unrevoked grant, not ended, now < ends_at

request(grant, duration, reason), by the grant's holder, eligible or not:
    refuse an emergency grant: it needs no activation                          409
    refuse a duration above ORGANIZATION_PROVIDER_ACTIVATION_MAX                 400
    refuse while an activation of the grant is in force                          409
    record a pending request older than 24 hours lapsed, then
    refuse while another request for the grant is pending                        409
    approval required: record it pending                                         201
    approval optional (outside production, ORGANIZATION_PROVIDER_ACTIVATION_APPROVAL=optional):
        record it approved by its holder, ending after the duration              201

approve(activation, reason), by a holder of an unrevoked grant for the scope:
    refuse the activation's own holder                                           403
    refuse unless it is pending and requested within 24 hours                    409
    record it approved, ending duration after now                                200
deny(activation, reason): as approve, recorded denied                            200
end(activation, reason), by its holder or a provider in force:
    refuse unless it is in force                                                 409
    record who ended it and why; it confers nothing from the next request        200
```

**The approver need not be in force.** An Entra approver "doesn't have to have any roles"
[R2]. Requiring an approver to be active would mean an approval needs an approval first, and the
first activation of the day would have none. An approver must hold a grant for the same scope,
eligible or emergency, so approving stays a provider's.

**An unapproved request lapses after 24 hours**, as Entra's does [R3]. The next request for the
grant records it lapsed, with no decider.

**Emergency grants** are made with `"kind": "emergency"` on `POST /v1/provider-grants`. Every
request one authorizes logs at WARN: *a provider acted on an emergency grant*, with the
`principal_id`, the method and the route, so that each use is alerted on and reviewed [R5]. In
production, fewer than two unrevoked emergency grants are reported at startup.
The bootstrap grant is an emergency grant, because before a second provider exists no one could
approve its activation.

**An eligible caller reaches only the activation routes.** Authentication resolves a grant holder
with no authority in force to an eligible caller. Its scope is the provider scope, so the
privileged-access record names it and its reason, but the API admits it to
`/v1/provider-activations` alone. Every other route answers `403` before a transaction opens, and
`requireProvider` refuses it on any route regardless.

```text
GET   /v1/provider-activations                    pending, in force, and the last 100, newest first
POST  /v1/provider-activations                    {"grant_id": ..., "duration_seconds": ...}
POST  /v1/provider-activations/{id}/approve
POST  /v1/provider-activations/{id}/deny
POST  /v1/provider-activations/{id}/end
```

Each command takes `X-Administrative-Reason`.

**Built here, and left for its own change.** Activations are built for both registered scopes, and
the projection of `provider:identity-control` is §Provider Authority Projection. The 90-day
emergency validation report (`ADR-ORG-002 §5.2`) needs the last use of each emergency grant, which
the access records hold and nothing reads yet. It follows on its own.

| Ref | Source |
| :-- | :-- |
| R1 | Microsoft, *What is Privileged Identity Management?*, <https://learn.microsoft.com/en-us/entra/id-governance/privileged-identity-management/pim-configure>, accessed 2026-10-02 |
| R2 | Microsoft, *Configure Microsoft Entra role settings in PIM*, <https://learn.microsoft.com/en-us/entra/id-governance/privileged-identity-management/pim-how-to-change-default-settings>, accessed 2026-10-02 |
| R3 | Microsoft, *Approve requests for Azure resource roles in PIM*, <https://learn.microsoft.com/en-us/entra/id-governance/privileged-identity-management/pim-resource-roles-approval-workflow>, accessed 2026-10-02 |
| R4 | Google Cloud, *Privileged Access Manager overview*, <https://docs.cloud.google.com/iam/docs/pam-overview>, accessed 2026-10-02 |
| R5 | Microsoft, *Manage emergency access admin accounts*, <https://learn.microsoft.com/en-us/entra/identity/role-based-access-control/security-emergency-access>, accessed 2026-10-02 |

**The tradeoff.** A provider waits for an approver before acting, and at night that may mean an
emergency grant, which is reported. The emergency grants are standing authority: few, watched,
and still standing. A provider whose activation is in force acts alone for its duration, as
Entra's activated administrator does. The duration bounds that, not a second approval on each
action (`ADR-ORG-002` Alternative E).

### Provider Authority Projection

`ADR-ORG-002 §5.3` has the Identity Control API read `provider:identity-control` grants and
activations from a local projection, as a projection consumer. This service publishes them.

**Two scopes.** `provider_grant_scope_check` admits `provider:organization-control` and
`provider:identity-control`. `POST /v1/provider-grants` requires `scope`: a grant that does not say
what it grants is refused. Grants of either scope are made and revoked here, by a caller with
`provider:organization-control` in force, because this service is the record of provider
authority. The bootstrap grant is `provider:organization-control`. An approver holds a grant for
the scope it approves, as before. In production, fewer than two unrevoked emergency grants for a
scope are reported at startup, for each scope.

**Only a scope enforced elsewhere is published.** This service enforces
`provider:organization-control` from its own records on every request, so no consumer needs those
grants, and publishing them would tell another service which Principals can administer this one.
Least privilege applies to processes as well as to users: "allowing only authorized accesses for
users (or processes acting on behalf of users) that are necessary to accomplish assigned
organizational tasks" (NIST SP 800-53 AC-6 [R6]). `authority.PublishedScopes` lists the scopes
whose transitions are published: `provider:identity-control` today.

**Four events, each carrying the grant's whole state** (the Membership pattern,
`TDD-organization-control-002` §Published Events):

```text
com.scnehaux.organization.provider.lifecycle.granted    a grant made                  standard lane
com.scnehaux.organization.provider.lifecycle.activated  an activation approved,       standard lane
                                                        or self-activated outside production
com.scnehaux.organization.provider.security.ended       an activation ended early     priority lane
com.scnehaux.organization.provider.security.revoked     a grant revoked               priority lane
```

```json
{
  "grant_id": "…", "principal_id": "…", "scope": "provider:identity-control",
  "kind": "eligible", "grant_status": "active", "grant_version": 3,
  "activation": { "activation_id": "…", "ends_at": "2026-10-02T18:00:00Z" }
}
```

- **`grant_version`** starts at 1 and rises by one with each published transition of the grant or
  its activation. A consumer applies an event only when its version is higher than the one it
  holds, so delivery order never decides which state is newer. `grant_status` is `active` or
  `revoked`.
- **`activation`** is the activation in force, or `null`. A revoked grant carries `null`.
- **What is not published.** A pending, denied or lapsed request confers nothing, so it publishes
  nothing. An activation that reaches its `ends_at` publishes nothing either: the consumer
  evaluates the end locally from the recorded instant, with no delay (`ADR-ORG-002 §5.3`, maximum
  enforcement delay).
- **The lanes.** An ended activation or a revoked grant is a withdrawal, so it travels in the
  priority lane, as `ADR-ORG-002 §5.3` declares.
- **Debt.** The four types are authority-bearing (`projection.AuthorityEventTypes`), so a consumer
  owed one that dead-letters is refused for it (`TDD-organization-control-005`).
- **History.** Each event writes a row beside it, in the publishing transaction, to
  `organization.provider_grant_event (event_id, grant_id, grant_version, event_type, recorded_at)`,
  with `UNIQUE (grant_id, grant_version)`. As `membership.membership_event` does for Memberships, it
  lets an older provider event close as `SUPERSEDED` once the consumer applied a newer version of
  the same grant (`TDD-organization-control-005`).

**The snapshot.** `POST /v1/projections/provider-authority/snapshot` returns, under one mark, every
unrevoked grant of a published scope with the activation in force, paged by `grant_id`. It uses the
same mark, cursor and bootstrap contract as the Organization snapshot (`TDD-organization-control-002`
§Bootstrap Contract).

**A consumer reads only the snapshot of what it subscribes to.** The provider authority snapshot
answers a consumer whose subscription names a provider type. The Organization snapshot answers one
whose subscription names a Membership or Tenant type. Any other consumer gets `403`. foundation-reference
therefore cannot read provider grants, and the Identity Control API cannot read Memberships.

**The tradeoffs.**

- **Expiry is evaluated on the consumer's clock.** No event announces an activation's natural end,
  so the consumer compares `ends_at` with its own clock. A clock running behind extends authority by
  the skew. The alternative is an event at every expiry, which needs a scheduler in this service and
  still arrives late by the dispatcher's lag. NTP keeps the skew to well under the dispatcher's lag,
  so the local comparison is both earlier and simpler.
- **Every published transition is one more event.** That is four rows per activation cycle at most,
  against an estate where activations are counted per provider per day.
- **The Identity Control API sees grants, not the reasons for them.** Reasons, approvers and
  requests stay here, in the record `ADR-ORG-002 §5.5` keeps. The projection carries what a
  decision needs.
- **A Principal ceasing to be an active person** ends its activations under `ADR-ORG-002 §5.1`. This
  service does not yet learn of it. Until it does, the grant is revoked by hand.

| Ref | Source |
| :-- | :-- |
| R6 | NIST SP 800-53 Rev. 5, *AC-6 Least Privilege*, <https://csrc.nist.gov/pubs/sp/800/53/r5/upd1/final>: "Employ the principle of least privilege, allowing only authorized accesses for users (or processes acting on behalf of users) that are necessary to accomplish assigned organizational tasks." |

### Grant and Policy Assertion

Run against the integration database on every build:

```sql
-- Every table with a tenant_id column carries RLS, enabled and forced.
SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
FROM   pg_class c
JOIN   pg_namespace n ON n.oid = c.relnamespace
WHERE  n.nspname IN ('tenant','workspace','membership','invitation','operation')
  AND  c.relkind = 'r';

-- No runtime role owns a table or holds a dangerous attribute.
SELECT rolname, rolsuper, rolbypassrls
FROM   pg_roles
WHERE  rolname IN ('organization_rt','organization_provider_rt','organization_consumer_rt');
```

The assertion fails when any table in those schemas has `relrowsecurity = false` or
`relforcerowsecurity = false`, when a runtime role owns a table, when one holds
`SUPERUSER` or `BYPASSRLS`, or when one holds a DDL privilege. A policy beyond the tenant
and provider pair must be declared by name in `controldb.AdditionalPolicies`: the
resolver's reads of the two history tables, and the consumer's reads of
`membership.membership` and `tenant.tenant`.

Grants and policies drift through migrations. Asserting them on every build is what
keeps the boundary real after the engineer who wrote it has moved on.

### Grant Derivation

The privilege model is checked in three layers, and each is named for what it does rather
than called a proof:

| Layer | Mechanism | What it establishes | Its limit |
| :-- | :-- | :-- | :-- |
| Derivation | `tools/grantcheck` | Which role runs which statement, read from the code, and whether the grants match | The declared boundaries below are an input, not a derivation |
| Falsification | The integration suite, run as the real roles | A tested path that needs a missing grant fails | Only the paths the suite executes |
| Drift detection | Catalog assertions such as `platform_privileges_integration_test.go` | The catalog still matches what was declared | Asserts the declaration, not whether it is right |

`grantcheck` runs in two steps, each with one source of truth.

**Which role runs which statement: the code.** A statement runs under the role of the
connection its transaction was opened on. In this repository that is decided in two kinds
of place:

- A scope wrapper in `internal/db`. `WithTenantScope`, `WithTenantRead` and
  `WithProviderInTenant` are `organization_rt`.
  `WithProviderScope` and `WithProviderSnapshot` are `organization_provider_rt`.
  `WithResolutionScope` is `organization_resolution_rt`. `WithConsumerScope` and
  `WithConsumerSnapshot` are `organization_consumer_rt`.
- A declared boundary: a struct that holds a `Transactor` and opens its own transaction on
  it. Its role is whatever the composition root hands it, so it is declared in the tool with
  the `main.go` line it mirrors. A boundary built on two roles' connections lists both:

  | Boundary | Roles | Wired in `cmd/organization-control/main.go` |
  | :-- | :-- | :-- |
  | `access.Recorder.RecordProviderAccess` | `organization_provider_rt` | `access.New(providerConns)` |
  | `db.ClaimStore.Complete` | `organization_rt` | `db.NewClaimStore(tenantConns)` |
  | `projection.FrontierReader.FrontierFor` | `organization_provider_rt`, `organization_consumer_rt` | `projection.NewFrontierReader(providerConns)`, and `(consumerConns)` |

  Changing that wiring means changing the tool's table in the same change.

The consumer's routes reuse the provider routes' transaction bodies (`load`,
`recordProgressIn`, `bootstrapIn`, `snapshotIn`, `verifyIn`), each passed in a closure of its
own to the consumer wrapper. A body reachable from both wrappers is granted to both roles.

The tool builds SSA for the service and a VTA call graph. From each wrapper call site it
walks everything the body can reach, including through interface calls and captured
function values, and collects every SQL constant. Two rules keep the attribution honest:

- A body passed to a wrapper is entered only from its own call site. `withRecordedScope`
  is shared by the provider and resolution wrappers, so following its body parameter would
  attribute every provider statement to the resolution role and every resolution statement
  to the provider role.
- A declared boundary or a wrapper is entered only as its own root, whether it is reached by
  a direct call, an interface call or a function value. A tenant body that reads the
  frontier does not give `organization_rt` the frontier's statements. The wrappers call the
  recorder through an interface. Until the consumer role existed the tool stopped only at
  direct calls, so every recorded scope's role was attributed the recorder's `INSERT`. The
  provider and resolution roles hold that grant for other reasons, which hid the error. The
  consumer role does not, and the tool reported it missing.

When the tool cannot read something, the run fails. That covers:

- a SQL call whose statement is not a constant, a choice between constants, a parameter
  every caller fills with one, or a function returning one;
- a SQL constant no wrapper or boundary reaches;
- a raw transaction outside the wrappers that is not a declared boundary;
- a wrapper body the tool cannot resolve.

This is why `tenant.apply` uses written-out constants rather than assembling its `UPDATE`.
`TestEveryTransitionStatementMatchesItsRule` assembles each constant from `transitions`, so
the constants cannot drift from the state machine.

**What each statement needs: PostgreSQL.** The tool does not parse SQL or restate the
privilege rules. Those rules are the engine's: `ON CONFLICT` with a target needs `SELECT`,
`RETURNING` needs `SELECT` on the returned columns, and column grants follow their own
rules. PostgreSQL checks privileges when it builds a plan, so `EXPLAIN` is enough and
nothing executes. For each (role, statement) the tool runs `SET LOCAL ROLE`, then
`PREPARE`, then `EXPLAIN (VERBOSE, FORMAT JSON) EXECUTE` with a generic plan.

- **Missing.** A privilege refusal is a grant the code needs and the database does not
  give. This holds on every derived path, not only the paths the suite executes.
- **Unused.** Each grant a runtime role holds is revoked inside a transaction that is rolled
  back, and every statement of that role is planned again. If none is refused, nothing in
  this repository needs the grant. Every statement is re-planned, not only those whose plan
  shows the table. The engine checks every relation a statement names, while a plan shows
  only what survived planning.
- **Sequences** are the one place the tool reads SQL text. `nextval` checks its privilege
  when it runs, not when it is planned. A statement calling a sequence function is
  therefore checked with `has_sequence_privilege`, using PostgreSQL's rule for that
  function.

`tools/grantcheck/unused-baseline.txt` lists unused grants the design has accepted as debt.
It is empty. The first run found 52, from a schema loop in `grants.sql` that granted DML on
every table in every owned schema to both runtime roles. The loop was replaced by explicit
per-table grants, and the owned schemas became deny by default, as `platform` already was.
The run fails on an unused grant the file does not list, and on a line that is no longer an
unused grant. A grant appearing without a statement that needs it is revoked, not listed.

The tool runs only against a database whose name ends in `_test`, owned by a role able to
revoke: `make grantcheck` locally and the CI database. The dispatch role is out of scope: its
statements live in foundation-platform's dispatcher, which this service runs in `internal/delivery`
on the dispatch pool and foundation-reference runs in its own, and
`dispatch_role_integration_test.go` measures them against the role instead. The one statement this
repository runs on the dispatch pool, the registration check in `internal/delivery`, is listed in
grantcheck's `outOfScope` table for the same reason. A listed function that no longer exists
fails the run.

## Configuration

| Variable | Default | Purpose |
| :-- | :-- | :-- |
| `ORGANIZATION_TENANT_DATABASE_URL` | none, required | A login role inheriting `organization_rt` |
| `ORGANIZATION_PROVIDER_DATABASE_URL` | none, required | A login role inheriting `organization_provider_rt` |
| `ORGANIZATION_RESOLUTION_DATABASE_URL` | none, required | A login role inheriting `organization_resolution_rt` (TDD-005) |
| `ORGANIZATION_CONSUMER_DATABASE_URL` | none; set, it enables consumer authority | A login role inheriting `organization_consumer_rt` |
| `DB_MAX_CONNS` | `20` | Each pool's ceiling |
| `ORGANIZATION_ENVIRONMENT` | `production` | `production` or `non-production`. Production by default, so a deployment that forgets it gets the stricter rules |
| `ORGANIZATION_PROVIDER_ACTIVATION_MAX` | `8h` | Longest activation a request may ask for; Entra allows 1 to 24 hours (§Provider Activation) |
| `ORGANIZATION_PROVIDER_ACTIVATION_APPROVAL` | `required` | `required` or `optional`. `optional` lets a holder activate with a reason alone, and startup refuses it in production |

Startup refuses any of these DSNs equal to another, because two pools on one credential run
as one role and the separation exists only in the Go types. The consumer DSN has no fallback
to the provider one, since that fallback is the over-privilege the consumer role removes.

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

### Caller Authority

- A tenant token, a provider token, and a consumer token resolve to their callers, each by its
  `principal_id`, and `sub` is never the actor.
- A token without `principal_id`, with one that is not a UUID, or with a `subject_type` other than
  `human` or `workload`, is refused.
- A human token without `tenant_id` and without a grant is refused with `403`, and so is one
  without `acr` or `auth_time`.
- A workload token without `tenant_id` is refused when no active consumer carries its
  `principal_id`, when it lacks `workload_owner`, and when consumer authority is not configured.
- A token carrying `realm_access` roles gains nothing from them.
- A failed record read answers `503`.
- The bootstrap grants once, reports the same grant on a rerun for the same Principal, refuses a
  rerun for another, and refuses when any grant exists; two concurrent runs leave one row
  (integration, as the provider role).
- The provider role cannot delete a grant, and cannot update a column outside the three
  revocation columns.
- A provider grants another Principal, which is then a provider at its next request; a second
  active grant for the same Principal is refused with `409`.
- A revoked grant confers nothing at the next request, and stays listed with who revoked it and
  why. The Principal may be granted again.
- Revoking the last active grant is refused with `409`, and two concurrent revocations of the last
  two grants leave one.
- A grant or revocation without `X-Administrative-Reason`, or from a tenant or consumer caller, is
  refused before a transaction opens.
- Registering a consumer without `principal_id`, or under a different one than it holds, is
  refused; two consumers cannot share one.
- Startup refuses each of the four removed settings.

### Tenant Administration Grant

- A tenant token is admitted only with an active Membership, an active Tenant and a grant in
  force, read for that Tenant. Each fact missing alone is refused with `403`, with the same
  message.
- A tenant token from a workload, below `aal2`, or without `auth_time` is refused before any read.
- A grant made, a Membership revoked, a Tenant suspended or a grant revoked takes effect at the
  next request.
- A provider grants an administrator in an active Tenant. A Principal with no Membership gets a
  Tenant-wide one, with its event, in the same transaction, and the provenance names the grant.
  An active one is kept, and a suspended one is refused with `409`.
- A second active grant for the same Principal and Tenant is refused with `409`. A revoked
  grant stays listed with who revoked it and why, and the Principal may be granted again.
- A grant in a Tenant that is not active is refused with `409`, and in one that does not exist
  with `404`.
- A tenant-scoped transaction cannot insert or revoke a grant: the restrictive policies refuse
  it as the tenant role (integration). The tenant role cannot delete a grant, or update a column
  outside the three revocation columns.
- The grant routes refuse a tenant or consumer caller, and a request without
  `X-Administrative-Reason`, before a transaction opens.
- `WithProviderInTenant` refuses a tenant or consumer scope, a nil Tenant and a blank reason,
  and records the access before the transaction opens.

### Provider Activation

- An eligible grant holder is refused on every route but `/v1/provider-activations`, before a
  transaction opens; once an activation is approved it is a provider at its next request, and
  after `ends_at`, or once ended, it is not.
- An activation's holder cannot approve it, by the service and by the database; another grant
  holder, in force or not, can. A denial, an approval of a lapsed request, a second pending
  request and a request above the maximum are refused.
- A request older than 24 hours lapses at the next request for the grant.
- With approval optional, outside production, a holder activates with a reason alone; startup
  refuses `optional` in production.
- An emergency grant is in force without an activation, every request it authorizes logs the
  report, and fewer than two in production are reported.
- A revoked grant ends its activation's authority at the next request.

### Provider Authority Projection

- A grant of `provider:identity-control`, its activation, an early end and its revocation each
  publish one event in the transition's transaction, with the grant's whole state, a version one
  higher than the last, and a history row. The withdrawals travel in the priority lane.
- A `provider:organization-control` grant publishes nothing, and neither does a pending, denied or
  lapsed request.
- A grant without a scope, or with an unregistered one, is refused.
- The provider authority snapshot returns unrevoked grants of published scopes, each with its
  activation in force, under one mark. A consumer not subscribed to a provider type is refused it,
  and a consumer not subscribed to a Membership or Tenant type is refused the Organization snapshot.
- An older provider event closes as `SUPERSEDED` on a newer applied version of the same grant
  (`TDD-organization-control-005`).

### Structural

- Every table in the RLS schemas has `ENABLE` and `FORCE ROW LEVEL SECURITY`.
- Every table in the RLS schemas has a non-nullable `tenant_id`.
- Neither runtime role owns a table, holds `SUPERUSER`, holds `BYPASSRLS`, or holds a
  DDL privilege.
- `SET LOCAL app.` appears in no package other than `db`. `tools/grantcheck` is the one
  exception: it binds no request, and sets both values only inside the rolled-back
  transactions it plans statements in.
- A new table added to an RLS schema without a policy fails the migration test.
- `grantcheck` finds no missing grant, no unlisted unused grant, and no statement it cannot
  attribute. Its CI step revokes a grant the resolver needs and adds one nothing needs, and
  requires a finding for each. Its own tests run it on a fixture module with one case for
  every attribution rule and every refusal, and a mutation to either walk rule turns them
  red.

### Negative

- A request carrying a Tenant identifier that differs from the resolved administrative
  scope is refused with `403` before a transaction opens.
- Logged in as the consumer role (`consumer_role_integration_test.go`):
  - it reads Memberships and Tenants only under the binding;
  - it writes only its four position columns;
  - it cannot change its declared terms, un-retire itself, register a consumer, write a
    Membership or a Tenant, read an invitation, an Organization, a Workspace, delivery
    evidence or Membership history, close a dead letter, append to the outbox, or write the
    audit trail.
- A consumer scope opens only the consumer pool, and the consumer pool opens only a consumer
  scope (`TestTheConsumerScopeOpensOnlyTheConsumerPool`).
- `grantcheck`'s fixture has a consumer path and a provider path sharing a helper. The helper's
  statement is attributed to both roles, the provider path's own statements to the provider
  only, and the recorder's `INSERT` to neither wrapper's role. That last case was red before the
  walk stopped at boundaries reached through an interface.
- A registered consumer carrying `X-Administrative-Reason` is refused with `403` on every API
  route except its own seven, before a transaction opens. The routes are read from `routes.go`,
  so a new route is covered without being listed. With the authority check removed, the test
  reports provider routes answering `400` or `500` instead: the consumer got past authority.
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
| Governed by | ADR-ORG-001 — Separate Organization Authority and Keycloak Projection; §5.11 provider and consumer authority |
| Governed by | ADR-ORG-003 — Tenant Administration Is a Recorded Grant, Checked with Current Membership |
| Conforms to | STD-IAM-002 §3.1.1, §3.2, §3.5 — the grant's holder checks its own record; `principal_id` is the persisted identifier |
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

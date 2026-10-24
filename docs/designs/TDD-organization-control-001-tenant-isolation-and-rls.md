---
doc_meta:
  id: TDD-organization-control-001
  title: Tenant Isolation and Row-Level Security
  owner: Core Platform Team
  version: 1.33.0
  status: approved
  classification: restricted
  review_cycle_days: 90
  created_date: 2026-08-10
  last_reviewed: 2026-10-24
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
| `internal/controldb` | The SQL this design applies — roles, policies, grants — and the maintenance steps; the migrate deployable's alone |
| `internal/posture` | `AssertIsolation`, which reads the catalog back and refuses a database whose posture has drifted, and the readiness `Probe` built on it (1.19.0). A package of its own so the serving process can import it without importing the stage SQL |
| `internal/system` | One constant: the CloudEvents source naming this system in every envelope it publishes |

These four carry no domain authority and appear in no other design's component table, which is
why they are listed here rather than left implicit. A reader who found `TenantPool` in a service
signature and no design that mentions it would have to infer the isolation model from the code
that depends on it.

`internal/system` sits here for a different reason than the other three: it is the published
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
| `audit` | No | The privileged-access record and its reviews. A row records access *across* Tenants and names at most one, so a Tenant reads its own rows through a view instead (§Privileged Access Review, 1.21.0) |

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

-- A person reading their own contexts (1.17.0, ADR-ORG-005), and the one invitation their
-- token selects (1.26.0, ADR-ORG-007). Reached only by SET LOCAL ROLE from the tenant
-- connections, never inherited.
CREATE ROLE organization_self_rt NOLOGIN;
GRANT organization_self_rt TO organization_rt WITH INHERIT FALSE, SET TRUE;

-- identity-control's workload resolving an invitation token for an enrolment (1.30.0,
-- ADR-IAM-011 §5.7 (c)). Reached only by SET LOCAL ROLE from the tenant connections, never
-- inherited, as the self role.
CREATE ROLE organization_invitation_resolver_rt NOLOGIN;
GRANT organization_invitation_resolver_rt TO organization_rt WITH INHERIT FALSE, SET TRUE;

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

**The self role** (1.17.0) serves one read: a person's own contexts (`ADR-ORG-005`,
`TDD-organization-control-002` §The Context List). It holds `USAGE` on `membership` and `tenant`
and `SELECT` on the columns that read needs, each through a `SELECT` policy of its own keyed on
`app.principal_id`:

- `membership_id`, `principal_id`, `tenant_id`, `workspace_id` and `status` of
  `membership.membership`, where `principal_id` is the bound one;
- `tenant_id`, `display_name` and `status` of `tenant.tenant`, where the bound Principal holds an
  active Membership in it;
- `principal_id`, `tenant_id` and `revoked_at` of `membership.tenant_admin_grant`, where
  `principal_id` is the bound one.

**It also serves one read for acceptance** (1.26.0, `ADR-ORG-007 §5.2`): `USAGE` on `invitation`
and `SELECT` on `invitation_id`, `tenant_id`, `workspace_id`, `target_identifier`, `subject_type`,
`state`, `expires_at` and `token_hash` of `invitation.invitation`, through the `SELECT` policy
`invitation_self_redeem`, keyed on `app.invitation_token_hash`. `db.WithSelfInvitation` binds the
SHA-256 of the token the caller presented and nothing else, so the role sees at most the one row
that token selects (`token_hash` is unique across the table). The ADR asks for "a role with no
login, entered as `organization_self_rt` is"; this is that role, with one more policy, rather than a
sixth role. The two reads never meet: the contexts read binds no token hash, the acceptance read
binds no Principal. The context policies raise on an unbound transaction; the invitation policy
raises on a connection that never bound its setting and matches nothing on one that bound it in an
earlier transaction, because `LOCAL` leaves the setting empty and no `token_hash` is empty.

It writes nothing, reads no other Principal's row, and reaches no Tenant the Principal is not a
member of. It has no login and no DSN of its own. `organization_rt` is granted it
`WITH INHERIT FALSE, SET TRUE`, and the self read opens a read-only transaction on the tenant
connections and runs `SET LOCAL ROLE organization_self_rt` before it binds anything. PostgreSQL
defines both halves: with `INHERIT FALSE` "the new member does not inherit", and the `SET` option
"allows the member to change to the granted role using the `SET ROLE` command"
(<https://www.postgresql.org/docs/17/sql-grant.html>). After it, "permissions checking for SQL
commands is carried out as though the named role were the one that had logged in originally"
(<https://www.postgresql.org/docs/17/sql-set-role.html>), and `LOCAL` ends with the transaction. So
the tenant role gains none of the self role's grants or policies, and the self read holds none of
the tenant role's.

A login of its own was the alternative. It would put a fifth credential in every deployment and in
foundation-reference's system proof, which seeds the login roles from `scripts/login-roles.sql`, to
serve one read-only route. It would also buy nothing against the tenant credential: that credential
can already read any Tenant by binding it, so reaching a role that reads one Principal's rows across
Tenants gives it no new reach. What the separate role does buy is kept: the self read runs as a role
that can read three tables' named columns for one Principal, and a defect in it cannot write or read
anything else. The self transaction writes no privileged-access record, because it is not provider
access (`ADR-ORG-005 §5.1`).

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

**Two entry points serve acceptance** (1.26.0, `ADR-ORG-007 §5.2`), and both refuse every scope
but a self scope:

- `WithSelfInvitation(ctx, self *SelfPool, tokenHash string, fn Body)` runs fn read-only as
  `organization_self_rt`, with `app.invitation_token_hash` bound to the hash of the token the
  caller presented. It is the only code that binds that setting.
- `WithSelfInTenant(ctx, tenants *TenantPool, tenantID id.UUID, fn Body)` runs fn on the tenant
  pool, as the tenant role, bound to tenantID, with the self caller as actor, and claims the
  `Idempotency-Key` as `WithTenantScope` does. The Tenant is an argument, and it comes from the row
  the token selected, never from the caller's token or the request. The invitation service reloads
  that row by its token hash under the Tenant's policy as its first statement, so a Tenant that
  does not hold the token's invitation finds nothing and the acceptance is refused.

**One entry point serves the invitation resolve** (1.30.0, `ADR-IAM-011 §5.7` (c)), and it refuses
every scope but the one it serves:

- `WithInvitationResolver(ctx, pool *InvitationResolverPool, tokenHash string, fn Body)` records
  the access first, on the recorder's own connection, with the reporter authority, the reason
  `invitation:resolve` and no Tenant, then runs fn read-only on the tenant connections as
  `organization_invitation_resolver_rt`, with `app.invitation_resolve_hash` bound to the hash of the
  token identity-control presented. It is the only code that binds that setting. The scope it takes,
  `InvitationResolverScope`, names no Tenant and opens no other entry point. The role holds `SELECT`
  on `invitation_id` and `target_identifier` of `invitation.invitation` alone, through the `SELECT`
  policy `invitation_resolver_read`, which admits the row of the bound hash only while it is
  `pending` and `expires_at > now()`. Not a sixth use of the self role: that role is the signed-in
  invitee's, and the ADR asks for "a dedicated database role" (`TDD-organization-control-004`
  §API / Interface, 1.18.0).

**`WithProviderInTenant` is the one that takes a Tenant as an argument from a
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

    if actor is a self caller (1.17.0):
        return SelfScope(principal)           -- one Principal, opens only the self read

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

A caller is one of five, decided from the token's standard claims and this service's own
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
| Reporter (1.27.0) | `subject_type` `workload`, `workload_owner`; no `tenant_id`; on a report route only | an active reporter registered with the `principal_id`, for what that route reports (§Reporters) | none registered, or registered to report something else; on any other route the token is authorized as any workload's |
| Self (1.17.0) | `subject_type` `human`, as a Tenant administrator's or a provider's claims, with or without `tenant_id`, on `GET /v1/principals/{principal_id}/contexts` whose `principal_id` is the token's, or on `POST /v1/invitations/accept` (1.26.0) | none | reaches those two routes, for its own `principal_id`; a human token naming another Principal on the contexts route is authorized as above; a workload token on `accept` is `403` |

```text
authenticate(token):
    verify signature, iss, aud, typ, exp                 -- foundation-platform verify
    principal := principal_id, a UUID
    type      := subject_type, human or workload
    if type is human and the route is GET /v1/principals/{principal}/contexts:
        return Self(principal)                           -- no record read (1.17.0)
    if the route is POST /v1/invitations/accept:
        require type is human                            -- else 403 (1.26.0)
        return Self(principal)                           -- no record read
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
    if the route is a report route:                       -- 1.27.0, §Reporters
        reporter := the active reporter registered with principal   -- read now
        require it reports what the route takes        -- provisioning, or obligation:<domain>
        return Reporter(principal, reporter)
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
`granted_by`; a revocation names it as `revoked_by`. From 1.18.0 both `POST` routes require an
`Idempotency-Key`, as every command does (`TDD-organization-control-003` §The `Idempotency-Key` Is
Required on Commands): a grant retried after a lost response without one would be a second grant.

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

### Reporters

(1.27.0, `ADR-ORG-001 §5.11`, amended 2026-10-20.) A reporter is a workload Principal this service
registered to report one kind of outcome. The production provisioning system reports
`POST /v1/provisioning/realized`, `POST /v1/provisioning/failed` and
`POST /v1/offboardings/{offboarding_id}/deprovisioning`. An obligation-owning domain reports
`POST /v1/obligations/{obligation_id}/resolve`. Neither is a person, so neither holds a person's
provider token. Each is one workload client, registered through identity-control's workload
registration with an owner (`ADR-IAM-003 §5.8`), which gets its token with the client credentials
grant and a client assertion signed by its own key.

```sql
CREATE TABLE organization.reporter (
    reporter_id     UUID        PRIMARY KEY,
    principal_id    UUID        NOT NULL UNIQUE,      -- retired rows included: one reporter for good
    reports         TEXT        NOT NULL,             -- provisioning, obligation:<domain>, or invitation:resolve
    purpose         TEXT        NOT NULL,
    version         BIGINT      NOT NULL DEFAULT 1,
    registered_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    registered_by   UUID        NOT NULL,
    register_reason TEXT        NOT NULL,
    retired_at      TIMESTAMPTZ,
    retired_by      UUID,
    retire_reason   TEXT
);
```

`reports` is `provisioning`, `invitation:resolve` (1.30.0), or `obligation:` and a lowercase domain
of letters, digits and hyphens.
The provider role reads and inserts the table and updates the retirement columns and the version
alone; nobody deletes a row. Authentication reads it on the raw provider connections, as it reads
the consumer registry (`authority.Reader.ReporterFor`).

**Admitted from the record, per route.** A workload token on a report route is a reporter's or
nobody's. A `provisioning` reporter reaches the three provisioning reports. An
`obligation:<domain>` reporter reaches `/resolve`, and the domain comes from its record, so a
`domain` in the body must be that one (`403` otherwise) and the service still refuses an obligation
raised against another domain. On every other route the token is authorized as any workload's: a
consumer's when one is registered with it, and refused otherwise. No OAuth scope decides it, because
roles stay out of tokens (`STD-IAM-002 §3.2`) and retiring a reporter is a change to this record, not
to the kernel. Least privilege: "Access should also be granted with the least privileges needed to
complete the task" (NIST SP 800-207 §2.1, cited in `ADR-ORG-001` [R14]).

**identity-control's resolve (1.30.0, `ADR-IAM-011 §5.7` (a), `ADR-ORG-001 §5.11` amended
2026-10-21).** An `invitation:resolve` entry reaches `POST /v1/invitations/resolve` and no other
report: each other report route's check refuses it, and the resolve refuses every other entry. It is
not identity-control's consumer registration, which admits that workload to the consumer routes only:
each route has its own entry (AC-6). It resolves to `db.InvitationResolverScope`, not the provider
scope, because the resolve reads one invitation before any Tenant is known and writes nothing. Its
access row carries the authority `reporter` and the reason `invitation:resolve`, the entry's purpose,
because NIST SP 800-53 AU-3 asks a record to establish "What type of event occurred", and it names no
Tenant (`TDD-organization-control-004` §API / Interface).

**Its scope.** A reporter resolves to the provider scope under the authority `reporter`, because
the reports change records only the provider role writes. `requireProvider` refuses it on every
provider route, since it holds no provider authority, and the report handlers admit it alone. It
needs no `X-Administrative-Reason`: like a consumer, it is a workload doing what its record admits.
Its access row carries the header when one is sent, and otherwise names the reporter.

**A person's fallback stays a person's act.** A provider still reports provisioning on the system's
behalf, with the system's confirmation in its reason. No provider resolves an obligation: the route
refuses one, as `TDD-organization-control-004` §Offboarding on a Development Server already said no
control lets an operator resolve another domain's obligation.

**Recorded and reviewed through the owner.** Each report writes a privileged-access row with the
authority `reporter`. Like a consumer's, it is not due in the weekly provider review
(§Privileged Access Review) and not in a Tenant's view: the workload's owner reviews it
(`ADR-IAM-003 §5.8`).

**The registry, through the API.** Four provider routes, each with `X-Administrative-Reason`, the
two commands with an `Idempotency-Key`:

```text
GET   /v1/reporters                         ?after=&limit=&reports=&state=active|retired
POST  /v1/reporters                         {"principal_id", "reports", "purpose"}        201
GET   /v1/reporters/{reporter_id}
POST  /v1/reporters/{reporter_id}/retire    {"expected_version"}
```

A second registration of a workload, active or retired, is `409` `state-transition-refused`. A
stale `expected_version` is `409` `version-conflict`, and retiring a retired reporter `409`
`state-transition-refused`. A retirement takes effect at the workload's next request.

**On a development server** the stand-ins of `STD-GLB-009` rule 13 report as development reporter
workloads with their own keys. `scripts/dev-wire.ps1` step 6b creates one per stand-in in
identity-control and registers it here: `provisioning`, and `obligation:product`, `:hcm`,
`:billing` and `:audit`. Development runs production's path.

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
revocation's own. Both `POST` routes require an `Idempotency-Key` from 1.18.0.

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
GET   /v1/provider-activations/grants             the caller's own unrevoked grants, eligible and emergency
POST  /v1/provider-activations                    {"grant_id": ..., "duration_seconds": ...}
POST  /v1/provider-activations/{id}/approve
POST  /v1/provider-activations/{id}/deny
POST  /v1/provider-activations/{id}/end
```

Each route takes `X-Administrative-Reason`, and from 1.18.0 each `POST` requires an
`Idempotency-Key` (`TDD-organization-control-003` §The `Idempotency-Key` Is Required on Commands): a
request for an activation, an approval or an end retried without one would act twice.

**The holder reads its own grants.** A request names a `grant_id`, and `GET /v1/provider-grants`
is a provider's, so without this an eligible holder could not learn what it can activate. Entra
does the same: a user requests activation by "opening **My roles**" and selects a list "to see a
list of your eligible Microsoft Entra roles" [R6]. The route reads the unrevoked grants whose
`principal_id` is the caller's own, taken from the resolved scope and never from the request, and
answers `200 {"grants": [{"grant_id", "scope", "kind", "granted_at"}]}`, newest first. It names no
other Principal: `granted_by` and the revocation columns are left out. The read runs in the provider
scope, so the privileged-access record names the caller and its reason, as on every activation
route.

**Built here.** Activations are built for both registered scopes, and the projection of
`provider:identity-control` is §Provider Authority Projection. The 90-day validation of an
emergency grant is §Emergency Grant Validation.

| Ref | Source |
| :-- | :-- |
| R1 | Microsoft, *What is Privileged Identity Management?*, <https://learn.microsoft.com/en-us/entra/id-governance/privileged-identity-management/pim-configure>, accessed 2026-10-02 |
| R2 | Microsoft, *Configure Microsoft Entra role settings in PIM*, <https://learn.microsoft.com/en-us/entra/id-governance/privileged-identity-management/pim-how-to-change-default-settings>, accessed 2026-10-02 |
| R3 | Microsoft, *Approve requests for Azure resource roles in PIM*, <https://learn.microsoft.com/en-us/entra/id-governance/privileged-identity-management/pim-resource-roles-approval-workflow>, accessed 2026-10-02 |
| R4 | Google Cloud, *Privileged Access Manager overview*, <https://docs.cloud.google.com/iam/docs/pam-overview>, accessed 2026-10-02 |
| R5 | Microsoft, *Manage emergency access admin accounts*, <https://learn.microsoft.com/en-us/entra/identity/role-based-access-control/security-emergency-access>, accessed 2026-10-02 |
| R6 | Microsoft, *Activate Microsoft Entra roles in PIM*, <https://learn.microsoft.com/en-us/entra/id-governance/privileged-identity-management/pim-how-to-activate-role>, accessed 2026-10-07: "When you need to assume a Microsoft Entra role, you can request activation by opening **My roles** in Privileged Identity Management." and "Select **Microsoft Entra roles** to see a list of your eligible Microsoft Entra roles." |

**The tradeoff.** A provider waits for an approver before acting, and at night that may mean an
emergency grant, which is reported. The emergency grants are standing authority: few, watched,
and still standing. A provider whose activation is in force acts alone for its duration, as
Entra's activated administrator does. The duration bounds that, not a second approval on each
action (`ADR-ORG-002` Alternative E).

### Emergency Grant Validation

`ADR-ORG-002 §5.2` (2026-10-06): an emergency grant is validated by using it. Microsoft's drill is
to "Validate that the emergency access accounts can sign in and perform administrative tasks", "At
least every 90 days" [R5]. A request the grant authorizes proves both, so the use is recorded.

```sql
CREATE TABLE organization.emergency_grant_use (
    grant_id      UUID PRIMARY KEY REFERENCES organization.provider_grant (grant_id),
    first_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    uses          BIGINT      NOT NULL DEFAULT 1 CHECK (uses > 0),
    CHECK (first_used_at <= last_used_at)
);
```

- **Each use is recorded on the grant.** Authentication already logs every request an emergency
  grant authorizes at `WARN`. The same request also writes the use, on the provider connections,
  in its own transaction. The grant is looked up again in the write, so a grant revoked between the
  standing read and the write records nothing. A failure to record is logged at `ERROR` and does
  not refuse the request: the grant is in force, and a break-glass path that fails on its own
  bookkeeping fails when it is needed.
- **The record is evidence.** The provider role holds `SELECT`, `INSERT` and `UPDATE` of
  `last_used_at` and `uses`, and no runtime role deletes one. The maintenance stage purges a row
  last used more than 7 years ago, and nothing deletes a younger one (§Privileged Access Review,
  1.27.0). The consumer role cannot read it.
- **Overdue is ninety days.** The clock runs from the last use, or from the grant when it was never
  used.
- **The report is in two places.** `GET /v1/provider-grants:emergency-validation` lists every
  unrevoked emergency grant of `provider:organization-control` with `last_used_at`, `uses`,
  `due_at` and `overdue`, the oldest due first. It is a provider read with its reason, like the
  grant list. The daily `maintenance` stage logs each overdue grant at `WARN`, from the same
  statement, and does not fail on one: an overdue grant is still in force.
- **A drill is a request made on purpose.** Its `X-Administrative-Reason` says it is a drill, so
  the review Microsoft asks for after each use can tell "a planned drill" from "an actual
  emergency" [R5].
- **Only this scope.** The Identity Control API records and reports its own scope's grants, from
  its own projection and requests (`ADR-ORG-002 §5.3`). This service cannot see their use.

### Privileged Access Review

1.21.0, `ADR-ORG-002 §5.6`. Every provider-scoped transaction writes one row to
`audit.privileged_access` before it runs (§The Single Binding Path). Until 1.21.0 no runtime role
could read it, so the review in `docs/runbooks/provider-access-review.md` needed SQL on the migration
credential. NIST asks the record to be reviewed: "Review and analyze system audit records
[Assignment: frequency] for indications of [Assignment: inappropriate or unusual activity]" (AU-6
a.) [R7].

**What a row records.** Four columns join the actor, the correlation, the reason and the time:

```sql
ALTER TABLE audit.privileged_access
    ADD COLUMN authority     TEXT NOT NULL,
    ADD COLUMN activation_id UUID,
    ADD COLUMN tenant_id     UUID,
    ADD COLUMN operation     TEXT,
    ADD CONSTRAINT privileged_access_authority_check
        CHECK (authority IN ('emergency', 'activation', 'eligible', 'consumer', 'reporter')),
    ADD CONSTRAINT privileged_access_activation_check
        CHECK ((authority = 'activation') = (activation_id IS NOT NULL)),
    ADD CONSTRAINT privileged_access_operation_check
        CHECK (operation IS NULL OR btrim(operation) <> '');
CREATE INDEX privileged_access_tenant_idx
    ON audit.privileged_access (tenant_id, access_id) WHERE tenant_id IS NOT NULL;
```

- **`authority`** is what admitted the request. `emergency`: the caller's emergency grant.
  `activation`: an approved activation in force, named by `activation_id`. `eligible`: a grant holder
  with nothing in force, which reaches the activation routes alone. `consumer`: a registered
  projection consumer. `reporter`: a registered reporter workload on its report routes (1.27.0,
  §Reporters); like `consumer`, it is reviewed through the workload's owner and never due in the
  provider review, and a Tenant's view leaves it out. Authentication reads it with the standing (§Caller Authority), and the scope
  carries it: `db.ProviderScope` takes it as an argument and refuses a scope without one, so a
  provider transaction cannot record an access with no authority behind it.
- **`tenant_id`** is the one Tenant the access named, or null. It is the Tenant
  `db.WithProviderInTenant` binds, or else the `{tenant_id}` segment of the route's path. A list
  across Tenants, or a record addressed by its own identifier such as an offboarding, names none.
- **`operation`** is the route pattern, method included: `POST /v1/tenants/{tenant_id}/suspend`.
  The API mux records it for every route it serves. It is null only for a transaction no route
  opened, and the serving deployable has none.
- **What it does not record is the outcome.** The row is written before the work, so an access that
  fails still leaves evidence. The outcome is the request log's, joined by `correlation_id`.

AU-3 asks a record to establish "What type of event occurred", "Where the event occurred", "Source
of the event" and the "Identity of any individuals, subjects, or objects/entities associated with
the event" [R7]. Before 1.21.0 a row named only the actor and a sentence.

**The review record.** Insert-only, in the same schema:

```sql
CREATE TABLE audit.privileged_access_review (
    review_id          UUID        PRIMARY KEY,
    actor_id           UUID        NOT NULL,   -- the provider whose access is reviewed
    period_from        TIMESTAMPTZ NOT NULL,
    period_to          TIMESTAMPTZ NOT NULL,
    outcome            TEXT        NOT NULL,
    statement          TEXT        NOT NULL,
    accesses           BIGINT      NOT NULL,
    emergency_accesses BIGINT      NOT NULL,
    reviewed_by        UUID        NOT NULL,
    correlation_id     UUID        NOT NULL,
    reviewed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT privileged_access_review_outcome_check CHECK (outcome IN ('appropriate', 'escalated')),
    CONSTRAINT privileged_access_review_statement_check CHECK (btrim(statement) <> ''),
    CONSTRAINT privileged_access_review_count_check
        CHECK (accesses >= 0 AND emergency_accesses >= 0 AND emergency_accesses <= accesses),
    CONSTRAINT privileged_access_review_separation_check CHECK (reviewed_by <> actor_id),
    CONSTRAINT privileged_access_review_period_check
        CHECK (period_from < period_to AND period_to <= reviewed_at)
);
CREATE INDEX privileged_access_review_actor_idx
    ON audit.privileged_access_review (actor_id, period_from, period_to);
```

`privileged_access_review_separation_check` is AC-5 in the database, as
`provider_activation_separation_check` is for approval: no provider attests to its own access.
NIST's reason: "Individuals or roles with privileged access to a system and who are also the subject
of an audit by that system may affect the reliability of the audit information" (AU-9(4)
discussion) [R7]. The counts are taken in the transaction that inserts the review, so the review
says how many accesses the period held when it was recorded. A review covers an access when it is
of the access's actor and `period_from <= occurred_at < period_to`. Consumer rows are never counted
or due: a consumer is a workload, reviewed through its owner (`ADR-IAM-003 §5.8`).

**Who reads what.**

| Caller | Reads | Through |
| :-- | :-- | :-- |
| A provider in force | Every row, and every review | `organization_provider_rt`: `SELECT` on both tables, `INSERT` on the review |
| A Tenant administrator | The provider rows that name its Tenant, consumer rows excluded | `organization_rt`: `SELECT` on the view `audit.tenant_provider_access` only |
| An eligible holder, a consumer | Nothing | The routes refuse them before a transaction opens |
| Any runtime role | No `UPDATE`, `DELETE` or `TRUNCATE` on either table | AU-9 a.: "Protect audit information and audit logging tools from unauthorized access, modification, and deletion" [R7] |
| The maintenance stage | `DELETE` of a record older than 7 years, as the migration role; the database refuses any younger one, and every `TRUNCATE`, for every role | §Retention below (1.27.0) |

The Tenant's read is a view. `internal/controldb/rls.sql` creates it, because Atlas OSS models no
views. It is owned by the migration credential that applied the schema, which owns the table, and
declared `security_barrier`:

```sql
CREATE VIEW audit.tenant_provider_access WITH (security_barrier) AS
SELECT access_id, actor_id, authority, activation_id, tenant_id, operation, correlation_id,
       reason, occurred_at
  FROM audit.privileged_access
 WHERE tenant_id = current_setting('app.tenant_id', false)::uuid
   AND authority <> 'consumer';
```

- **The view is the boundary,** not a `WHERE` in the handler. PostgreSQL checks the relations a view
  reads "against the privileges of the rule owner, not the user invoking the rule" [R8], so
  `organization_rt` reads the rows the predicate admits and holds nothing on the table.
- **`security_barrier`,** because without it a view cannot "reliably conceal the data in unseen
  rows" [R8]: a function in the caller's query could see rows before the predicate drops them.
  PostgreSQL says the option "should be used if the view is intended to provide row-level security"
  [R8].
- **`missing_ok` is false,** as in every tenant policy. An unbound connection raises rather than
  reading no Tenant's rows.
- **Not row-level security on the table.** The recorder writes outside the transaction it describes
  (§The Single Binding Path), so a policy on the table would need an insert policy for each writing
  role, and the table would join the RLS set it is outside of by construction. The view leaves both
  untouched.

**The routes.** Each list is in the estate's form (`STD-GLB-001` 1.3.0): `after`, `limit` 1 to 100,
named filters, `access_id` or `review_id` order, and `{"<items>": [...], "next": …}`. A time window
is `from` inclusive and `to` exclusive, RFC 3339 instants with their offset (`STD-GLB-001` 1.6.0).
An unknown or repeated parameter, a malformed one, and a `to` not after `from` are refused `400`.

```text
GET   /v1/privileged-access             provider  ?after&limit&actor_id&tenant_id&correlation_id&authority&from&to
GET   /v1/privileged-access:unreviewed  provider  the Principals with unreviewed access, oldest first
GET   /v1/privileged-access/reviews     provider  ?after&limit&actor_id&reviewed_by
POST  /v1/privileged-access/reviews     provider  {"actor_id", "from", "to", "outcome"}
GET   /v1/provider-access               tenant    ?after&limit&authority&from&to
```

- **An access** is `{"access_id", "actor_id", "authority", "activation_id", "tenant_id",
  "operation", "correlation_id", "reason", "occurred_at"}`, nulls included. The provider list answers
  `{"accesses": [...], "next"}`, and the Tenant's list the same.
- **A provider read is itself an access.** It runs in the provider scope with the caller's
  `X-Administrative-Reason`, so it leaves a row, as every provider read does.
- **The Tenant's list** is a Tenant administrator's (§The Tenant Administration Grant). It names no
  Tenant: the Tenant is the scope's, and a `tenant_id` in the query is refused as unknown. It runs in
  a read-only transaction bound to that Tenant (`db.WithTenantRead`) and records nothing: a Tenant
  reading its own record is not provider access. Its `authority` filter takes `emergency`,
  `activation` and `eligible`.
- **`POST /v1/privileged-access/reviews`** is a command. It requires an `Idempotency-Key`
  (`TDD-organization-control-003` §The `Idempotency-Key` Is Required on Commands), and its
  `X-Administrative-Reason` is recorded as the reviewer's statement. It answers `201` with the review:
  `{"review_id", "actor_id", "from", "to", "outcome", "statement", "accesses", "emergency_accesses",
  "reviewed_by", "reviewed_at"}`. A review of the caller's own access is refused `403`, by the service
  and by the database. A period whose `from` is not before its `to`, a `to` in the future, a missing
  `actor_id` and an `outcome` other than `appropriate` or `escalated` are refused `400`. A period with
  no accesses may be reviewed: it states that nothing happened.
- **`GET /v1/privileged-access:unreviewed`** answers `{"review_due_days": 7, "actors": [{"actor_id",
  "unreviewed", "emergency", "oldest_at", "due_at", "overdue"}]}`: each Principal with a provider
  access no review covers, the count, the emergency ones among them, the oldest, and the date it
  falls due, oldest first. It is a report, not a list, and is not paginated: it has one row per
  Principal that has acted as a provider, which the grants bound.
- **The period's end is the database's.** A review whose `to` is ahead of the database's clock
  records nothing and is refused `400`, so a client's skew is a refusal rather than a check
  violation.

**A review is due weekly.** An access is due seven days after it occurred, and overdue after. CIS:
"Conduct reviews of audit logs to detect anomalies or abnormal events that could indicate a
potential threat. Conduct reviews on a weekly, or more frequent, basis" (Safeguard 8.11) [R9]. The
daily `maintenance` stage logs each Principal with an overdue access at `WARN`, from the statement
the route reads, and does not fail on one, as it reports an overdue emergency grant
(§Emergency Grant Validation).

#### Retention

(1.27.0, `STD-GLB-007` 1.1.0, `ADR-ORG-002 §5.6` amended 2026-10-20; the heading from 1.28.0, because
`STD-GLB-007` 1.2.0 cites this design's §Retention.) An audit
record is kept 7 years, purged after that by the maintenance stage, and never purged by a Tenant's
decommissioning. `STD-GLB-007` names the period: audit logs are "Retained in immutable storage for
`7 years`", and it counts this record, its reviews and the emergency uses among them. AU-11 leaves
the period to the organization [R7]; CIS asks for "a minimum of 90 days" (Safeguard 8.10) [R9]. Until
1.27.0 neither table was purged, and this paragraph said a retention standard would add the purge.
`STD-GLB-007` already named one.

- **What is purged, by which column.** The column is the one that says when the newest evidence in
  the row was made: `audit.privileged_access` by `occurred_at`, `audit.privileged_access_review` by
  `reviewed_at`, and `organization.emergency_grant_use` by `last_used_at`. The last is one row per
  emergency grant, updated on each use, so it is a counter, but it ages: `first_used_at` and the
  count are no younger than `last_used_at`, so a row last used 7 years ago holds nothing younger. A
  grant unused for 7 years then reads as never used, and `EmergencyValidation` takes its due date
  from `granted_at`, which is older still: it is overdue either way (§Emergency Grant Validation).
- **The boundary** is `now() - make_interval(years => 7)`, calendar years on the database clock,
  the clock that wrote each time. `MaintenanceConfig.AuditRetentionYears` is 7 and has no flag: the
  stage refuses fewer before any step runs.
- **Only that delete is admitted, by a trigger.** `rls.sql` puts a `BEFORE DELETE` row trigger on each
  table that raises on a row younger than 7 years, and a `BEFORE TRUNCATE` trigger that raises on
  any truncation. They fire for every role: the migration role, the owner, a superuser. A defect in
  the purge, or a hand-written `DELETE` on the migration credential, removes nothing younger.
- **Not a row-level security policy,** which is how `membership_batch_purge` admits the batch
  purge and nothing else. Three reasons. A policy does not bind a superuser: "Superusers and roles
  with the BYPASSRLS attribute always bypass the row security system when accessing a table" [R21],
  and CI and `deploy/dev` run the stages as one. A policy does not cover `TRUNCATE`: "Operations that
  apply to the whole table, such as TRUNCATE and REFERENCES, are not subject to row security" [R21];
  and neither does a row trigger, since "TRUNCATE will not fire any ON DELETE triggers that might
  exist for the tables. But it will fire ON TRUNCATE triggers" [R21], hence the second trigger. And
  under `FORCE ROW LEVEL SECURITY` every reader and writer of these tables would need a policy of its
  own: the recorder, which writes outside any binding; the resolution role's insert; the provider's
  upsert of a grant's use; the owner reading through `audit.tenant_provider_access`. Each is a new
  predicate to get wrong, and the trigger changes none of them.
- **What the trigger does not stop.** Turning it off needs the table's owner ("You must own the
  table to use ALTER TABLE") or a superuser, through `session_replication_role` ("Only superusers and
  users with the appropriate SET privilege can change this setting") [R21]. Those are the people who
  could drop a policy too. The integration suite turns triggers off the same way, for its own seeded
  rows, on its disposable database.
- **How the 7 years count (1.29.0, `STD-GLB-007` 1.3.0 §Data Retention Lifecycle).** Seven years is a
  minimum, not a deadline. An evidence row counts from when it was written, as the column that says
  when its newest evidence was made, which is the column chosen above for each table. A row that
  records something that stays in force counts from the later of when it was written and when it
  stopped being in force, and is never purged while in force. A row a current decision depends on is
  state and is kept while it does. The three tables here are evidence rows: an access, a review and
  the last use of an emergency grant, which `EmergencyValidation` already reads as "never used" once
  purged (above); the 90-day validation reads only recent uses, so it is evidence, not state. None is a record of a grant or a standing, so the later-of count has no row to apply
  to; the grants are `organization.provider_grant`, which is not purged. The sources are the
  standard's: CA/Browser Forum BR §5.4.3 and §5.5.2, NIST AU-11, OMB M-21-31.
- **Decommissioning purges none.** Retiring a Tenant through offboarding deletes no audit record,
  however old. A provider's access to a retired Tenant is what a later investigation needs, and
  `STD-GLB-007` exempts audit records from the decommissioning purge. There is no decommissioning
  purge yet; the test below fails the day one is added that deletes this record.
- **An unreviewed access leaves the report when it is purged.** The weekly report will have named it
  as overdue for 7 years by then.

#### The Tradeoffs of the Review

- **Providers review providers.** The reviewers also administer access, which AC-5's discussion asks
  to keep apart from administering audit [R7]. No one administers this record at runtime, the review
  is held to a second person, and the Tenant's read is a check from outside the providers
  (`ADR-ORG-002 §5.6`, Alternative G).
- **A Tenant sees what names it.** A provider list across Tenants, or a read of an offboarding by its
  identifier, names no Tenant and is shown to none.
- **The unreviewed report scans the record.** It asks of every provider row whether a review covers
  it, one probe of `privileged_access_review_actor_idx` each, so its cost grows with the record,
  which the 7-year retention bounds (1.27.0).
- **The purge scans the record too.** No index leads with `occurred_at`, so the daily purge reads
  `audit.privileged_access` whole. That is one scan a day, in the maintenance stage; an index on
  `occurred_at` is the remedy if it ever matters.
- **Reviewing is itself provider access.** Reading the record and recording a review each leave a
  row under the reviewer, which another provider reviews in turn. Every week a provider who
  reviewed is reviewed, which is the point: nobody's reads of the record go unexamined.

| Ref | Source |
| :-- | :-- |
| R7 | NIST SP 800-53 Rev. 5, OSCAL catalog, <https://raw.githubusercontent.com/usnistgov/oscal-content/main/nist.gov/SP800-53/rev5/json/NIST_SP-800-53_rev5_catalog.json>, accessed 2026-10-08. AU-3: "Ensure that audit records contain information that establishes the following:" "What type of event occurred;" "When the event occurred;" "Where the event occurred;" "Source of the event;" "Outcome of the event; and" "Identity of any individuals, subjects, or objects/entities associated with the event." AU-6 a.: "Review and analyze system audit records [Assignment: frequency] for indications of [Assignment: inappropriate or unusual activity] and the potential impact of the inappropriate or unusual activity;" AU-9 a.: "Protect audit information and audit logging tools from unauthorized access, modification, and deletion; and" AU-9(4) discussion: "Individuals or roles with privileged access to a system and who are also the subject of an audit by that system may affect the reliability of the audit information by inhibiting audit activities or modifying audit records." AU-11: "Retain audit records for [Assignment: time period] to provide support for after-the-fact investigations of incidents and to meet regulatory and organizational information retention requirements." AC-5 discussion: "Separation of duties includes dividing mission or business functions and support functions among different individuals or roles, conducting system support functions with different individuals, and ensuring that security personnel who administer access control functions do not also administer audit functions." |
| R8 | PostgreSQL 17 Documentation, §39.5 *Rules and Privileges*, <https://www.postgresql.org/docs/17/rules-privileges.html>, and *CREATE VIEW*, <https://www.postgresql.org/docs/17/sql-createview.html>, accessed 2026-10-08: "all relations that are used due to rules get checked against the privileges of the rule owner, not the user invoking the rule"; views "cannot be used to reliably conceal the data in unseen rows unless the security_barrier flag has been set"; `security_barrier` "should be used if the view is intended to provide row-level security." |
| R9 | Center for Internet Security, *CIS Controls Assessment Specification v8.1*, Controls 8, <https://cas.docs.cisecurity.org/en/latest/source/Controls8/>, accessed 2026-10-08. 8.10: "Retain audit logs across enterprise assets for a minimum of 90 days." 8.11: "Conduct reviews of audit logs to detect anomalies or abnormal events that could indicate a potential threat. Conduct reviews on a weekly, or more frequent, basis." |
| R21 | PostgreSQL 17 Documentation, accessed 2026-10-20. *Row Security Policies*, <https://www.postgresql.org/docs/17/ddl-rowsecurity.html>: "Superusers and roles with the BYPASSRLS attribute always bypass the row security system when accessing a table." and "Operations that apply to the whole table, such as TRUNCATE and REFERENCES, are not subject to row security." *TRUNCATE*, <https://www.postgresql.org/docs/17/sql-truncate.html>: "TRUNCATE will not fire any ON DELETE triggers that might exist for the tables. But it will fire ON TRUNCATE triggers." *ALTER TABLE*, <https://www.postgresql.org/docs/17/sql-altertable.html>: "You must own the table to use ALTER TABLE." *Client Connection Defaults*, `session_replication_role`, <https://www.postgresql.org/docs/17/runtime-config-client.html>: "Only superusers and users with the appropriate SET privilege can change this setting." |

### Pausing Tenant Administration

1.22.0. SAD-004 §9.1.1 has a restore to an older point reconciled and contained "before normal
operation", with the whole Tenancy control plane as the blast radius until it is. The reconciliation
is `TDD-organization-control-002` §After a Restore to an Older Point. This is the containment: a
provider pauses every Tenant administrator's command, so authority does not change under the operator
while the security versions are reconciled, and lifts the pause when they are.

```text
GET  /v1/tenant-administration-pause              provider, X-Administrative-Reason
POST /v1/tenant-administration-pause  {"paused": true | false}
                                                  provider, X-Administrative-Reason, Idempotency-Key
  200 {"paused", "pause_id", "reason", "actor_id", "correlation_id", "recorded_at"}
```

**What it pauses.** Every request a Tenant administrator makes with a method other than `GET`, `HEAD`
or `OPTIONS` (safe methods, RFC 9110 §9.2.1; `TRACE`, the fourth, is served by no route), refused `503` before it reaches a handler, with a
detail saying the same command can be sent again once the pause is lifted. RFC 9110 §15.6.4 gives
`503` to a server "currently unable to handle the request due to a temporary overload or scheduled
maintenance, which will likely be alleviated after some delay". The problem type is foundation-platform's
`dependency-unavailable`, the one `503` it defines; a type of its own would be a foundation-platform
change. Reads continue, so a Tenant administrator can still see its Tenant. Provider acts continue,
because the operator's repairs are provider acts, and so do consumers' protocol calls.

**Where it is enforced.** At authentication, beside the caller records, for the reason those are read
there: a Tenant administrator's standing is read for each request, so a pause takes effect at the
next command and so does its lifting. It is read on the provider connections, like the provider grants
and consumer registrations, because the table is about every Tenant at once and lives in the
`organization` schema, where the tenant role holds nothing (§Roles). A pause that cannot be read
refuses the command, `503`: a command let through because the record was unreachable is the change
the pause exists to stop.

**The record.** `organization.tenant_administration_pause` is append-only. Each row is one decision,
paused or lifted, with the provider who made it, the correlation identifier and the reason, which is
the request's `X-Administrative-Reason`; the latest row is the state. The provider role holds
`SELECT` and `INSERT` and no `UPDATE`, so a decision is never rewritten. No row means not paused.
Each read and each decision is also a privileged access, recorded with the reason.

**A lost Membership withdrawal is re-applied by a provider, under the pause** (1.28.0, decided
2026-10-20, `ADR-ORG-003 §5.6`). The pause refuses a Tenant administrator's command, so a Membership
withdrawal the restore lost cannot be re-applied by the Tenant's own administrators. Until 1.28.0
this section left that to the owner, with three options: (a) a provider route that suspends or
revokes one Membership inside a Tenant; (b) a pause that exempts named Tenants; (c) the Tenant
suspension alone. The owner chose (a). A provider in force sends
`POST /v1/tenants/{tenant_id}/memberships/{membership_id}/suspend` or `/revoke`, with a reason, an
`Idempotency-Key` and the expected version, through `db.WithProviderInTenant`, recorded naming the
Tenant, which its administrators read in `GET /v1/provider-access` (§Privileged Access Review). It
never restores or grants one (`TDD-organization-control-002` §A Provider Withdraws One Membership,
ROADMAP backlog item 55). Provider acts are not paused, so it runs during the pause.

**From before the service starts.** A pause made through the API after a restore leaves the time
between the service starting and the call in which a Tenant administrator can still change authority.
`deploy/dev/restore.sh` with `PAUSE_REASON` records the pause in the restored database before anything
serves. That row names no actor: no person made it through the API. A check holds that only a pause
may name none, so lifting one is always a provider's named decision.

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
and provider pair must be declared by name in `posture.AdditionalPolicies`: the
resolver's reads of the two history tables, and the consumer's reads of
`membership.membership` and `tenant.tenant`. From 1.22.0 also the migration role's three `SELECT`
policies on `operation.offboarding`, `operation.offboarding_obligation` and
`tenant.provisioning_request`, which serve `operation.lifecycle_signals`, a view of counts and ages
naming no row (`TDD-organization-control-004` §Operational Notes).

Grants and policies drift through migrations. Asserting them on every build is what
keeps the boundary real after the engineer who wrote it has moved on.

### Verifying the Posture at Runtime

(1.19.0.) The build asserts a throwaway database, and `organization-migrate -stage=post`
asserts the database a deploy just changed. Neither sees the time between deploys, when a
superuser action during an incident could drop `FORCE` from one table and leave the catalog
reporting row-level security as enabled. The serving process therefore checks the posture
itself, with the same `posture.AssertIsolation`:

- **At startup**, on the tenant connections, as `organization_rt`, before the port is bound.
  A problem is logged one line per problem and the process exits. EAD-006 §8 requires a
  security-control failure to fail closed, and an unprotected table is that failure.
- **Behind `GET /readyz`**, on every probe: the database is reachable and the posture is
  intact. A failure answers `503` and takes the replica out of the load balancer without
  restarting it. Kubernetes states the mechanism: "If the readiness probe returns a failed
  state, the EndpointSlice controller removes the Pod's IP address from the EndpointSlices of
  all Services that match the Pod" [K8S-PROBES]. A restart cannot repair a dropped policy, so
  `GET /healthz` still touches no dependency; the same page recommends that "the readiness
  probe additionally checks that each required back-end service is available" while
  liveness checks the application itself [K8S-PROBES].

The probe reads the catalog on every call rather than caching a pass: the read is two catalog
queries, and a cached pass would keep a replica serving for the cache's lifetime after the
posture broke. The runtime role can run the check because `pg_class`, `pg_policy`,
`pg_attribute`, `pg_namespace`, `pg_roles` and `pg_tables` are readable by `PUBLIC`;
`TestTheRuntimeRoleSeesTheWholePosture` asserts it sees every protected table the owner sees,
so the check is not vacuous. `tools/grantcheck` declares `posture.AssertIsolation` a boundary
on `organization_rt` and plans its statements as that role.

The check reads and never repairs. Repair belongs to the migration job, which holds DDL; a
process that could fix its own isolation could also change it.

[K8S-PROBES]: Kubernetes documentation, "Liveness, Readiness, and Startup Probes",
https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/

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

- A scope wrapper in `internal/db`. `WithTenantScope`, `WithTenantRead`,
  `WithProviderInTenant` and `WithSelfInTenant` are `organization_rt`. `WithSelfRead` and
  `WithSelfInvitation` are `organization_self_rt`. `WithInvitationResolver` is
  `organization_invitation_resolver_rt` (1.30.0).
  `WithProviderScope` and `WithProviderSnapshot` are `organization_provider_rt`.
  `WithResolutionScope` is `organization_resolution_rt`. `WithConsumerScope` and
  `WithConsumerSnapshot` are `organization_consumer_rt`. `WithConsumerReconciliation` and
  `WithConsumerReconciliationSnapshot` are `organization_provider_rt` (1.27.0): a consumer reconciling
  its own copy (`TDD-organization-control-002` §Reconciliation Interval) runs this service's
  comparison and repair on the provider connections, recorded under the consumer's authority. The
  consumer role gains nothing: granting it `INSERT` on the outbox would let a leaked consumer
  credential publish any authority event.
- A declared boundary: a struct that holds a `Transactor` and opens its own transaction on
  it. Its role is whatever the composition root hands it, so it is declared in the tool with
  the `main.go` line it mirrors. A boundary built on two roles' connections lists both:

  | Boundary | Roles | Wired in `cmd/organization-control/main.go` |
  | :-- | :-- | :-- |
  | `access.Recorder.RecordProviderAccess` | `organization_provider_rt` | `access.New(providerConns)` |
  | `db.ClaimStore.Complete` | `organization_rt` | `db.NewClaimStore(tenantConns)` |
  | `projection.FrontierReader.FrontierFor` | `organization_provider_rt`, `organization_consumer_rt` | `projection.NewFrontierReader(providerConns)`, and `(consumerConns)` |
  | `projection.SignalsReader.Read`, `.ReadLifecycle` (1.22.0) | `organization_provider_rt` | `projection.NewSignalsReader(providerConns)` |
  | `authority.Reader.ProviderStanding`, `.ConsumerFor`, `.TenantAdministrationPaused` (1.22.0), and the reader's other methods | `organization_provider_rt` | `authority.NewReader(providerConns)` |
  | `tenant.ScheduledSweep.SweepUnresolved`, `invitation.ScheduledExpiry.ExpireLapsed` (1.23.0): the scheduled sweeps, through `operation.provisioning_sweep` and `operation.invitation_expiry` (`TDD-organization-control-003` §Scheduled Sweeps) | `organization_provider_rt` | `provisioning.Scheduled(providerConns)`, `invitations.Scheduled(providerConns)` |

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

### Self Caller

- A human token whose `principal_id` is the path's reaches `GET /v1/principals/{principal_id}/contexts`
  with or without `tenant_id`, with no provider grant, and with an eligible grant, and writes no
  privileged-access record.
- The same token on any other route is authorized as before: no grant and no `tenant_id` is `403`.
- A workload token, or a human token naming another Principal, is not a self caller.
- Logged in as the tenant role, with `app.principal_id` bound and the self role not set, nothing
  more is readable than the Tenant binding allows: the self policies do not reach the tenant role.
- As the self role, bound to one Principal, no other Principal's Membership, Tenant or
  administration grant is readable, nothing is writable, and an unbound read raises.
- A human token, with or without `tenant_id`, with or without a provider grant, is a self caller on
  `POST /v1/invitations/accept`; a workload token there is `403` (1.26.0).
- As the self role with a token hash bound, only the invitation holding that hash is readable, no
  invitation is writable, and an unbound read raises or reads nothing; the tenant role reaches nothing through
  `invitation_self_redeem` (1.26.0).
- `WithSelfInvitation` and `WithSelfInTenant` refuse a tenant, provider or consumer scope, a blank
  hash and a nil Tenant (1.26.0).

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
- `GET /v1/provider-activations/grants` answers an eligible holder `200` with its own unrevoked
  grants and neither another holder's nor a revoked one; a tenant caller is refused `403`, and a
  request without `X-Administrative-Reason` `400`.

### Emergency Grant Validation

- Each request an emergency grant authorizes records its use. An activated provider's request does
  not, and a failure to record is reported and refuses nothing.
- The report lists each emergency grant of this scope with its uses, the oldest due first, and
  leaves eligible grants out. A grant never used and made more than 90 days ago is overdue, and so
  is a used one 90 days after its last use.
- The provider role cannot delete a recorded use; the consumer role cannot read one.
- The report is a provider's.

### Privileged Access Review

- Every recorded row carries its authority: `emergency` for an emergency grant's request,
  `activation` with the activation's identifier, `eligible` on the activation routes, `consumer` for
  a consumer. A provider scope without an authority is refused before anything is written.
- A provider act in one Tenant records that Tenant; a route with `{tenant_id}` in its path records
  the path's; a list across Tenants records none. Every served route records its pattern.
- The provider list filters by actor, Tenant, correlation, authority and window, pages by
  `access_id`, and refuses an unknown filter, a malformed instant, an instant without an offset, and
  a `to` not after `from`.
- A review of the caller's own access is refused `403`; the database refuses one inserted directly.
  A review counts the accesses and emergency accesses of its period, consumer rows excluded, and
  covers them: they leave the unreviewed report, and an access outside the period stays.
- An access is overdue seven days after it occurred.
- A Tenant administrator reads its own Tenant's provider rows through the view, and no other
  Tenant's, no consumer row, and no row that names no Tenant. `organization_rt` cannot read the
  table, and an unbound tenant connection reading the view raises.
- No runtime role can update or delete a row of either table.
- Retention (1.27.0, `STD-GLB-007` 1.1.0 compliance check 4):
  `TestTheAuditPurgeDeletesOnlyRecordsOlderThanSevenYears` seeds an 8-year-old and a 6-year-old
  record in each of the three tables, purges as `organization_migrator` and then runs the stage, and
  finds only the 8-year-old ones gone. `TestTheDatabaseRefusesToDeleteAYoungAuditRecord` deletes each
  6-year-old record directly, as the migration role and as a superuser, and truncates each table;
  every one is refused by the trigger. `TestMaintenanceRefusesAnAuditRetentionBelowSevenYears`.
  `TestRetirementLeavesTheTenantsPrivilegedAccessRecord` (`internal/offboarding`) retires a Tenant
  through the offboarding service and finds its privileged-access rows, from today, 6 and 8 years
  ago, all still there.

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

### The Schemas Against the Handlers (1.32.0)

Built 2026-10-22 (`ROADMAP.md` item 62). `ROADMAP.md` §API specification states the approach and its
sources.

- `internal/httpapi/contract_test.go` checks every exchange the route tests make against
  `api/openapi.yaml`: the response against the schema its status names, and an accepted request
  against its operation's parameters and body.
- The route tests stop a request before the database, so they check the problems. The success
  exchanges are `contract_live_test.go` and its `contract_live_*_test.go` files: the same handlers on
  the services `cmd/organization-control` wires, each pool on its production login role, against the
  integration database. The privileged-access recorder is stubbed: `internal/access` is not an edge
  of `internal/httpapi` (`arch.json`), and its records are its own suite's. They drive each resource through its lifecycle by the API itself, so every
  request body they send is checked too. Seeding is on the owner connection: a Tenant, the fixture's
  emergency grant, an abandoned delivery and its dead letter, an applied receipt.
- After a whole run of the package, every operation must have had a success and a problem checked.
  A run with no integration database is not held to it; CI sets `REQUIRE_INTEGRATION`, so there it
  always applies.
- Found and fixed: an empty snapshot page wrote `"rows": null` or `"grants": null` where the
  specification documents an array (`projection.endPage`, `endProviderPage`).
- Found, and fixed in 1.33.0 (`ROADMAP.md` item 67): a Membership with no Workspace was written with the
  nil UUID as `workspace_id`, and one with no end with `0001-01-01T00:00:00Z` as `valid_until`. Both
  satisfied the schema, which required the two fields and did not say what an absent value is. A refused
  context decision wrote the nil UUID as `membership_id`. `STD-GLB-001` 2.0.0 (`ADR-GLB-021`) decided
  it: an absent value is `null`, declared `type: [<t>, "null"]`, and a sentinel is never allowed
  (`TDD-organization-control-002` 1.20.0 §API / Interface).
- The null case is checked for each (1.33.0), as the standard's §Enforcement Mechanism asks: "A
  service's tests assert that a response with an absent optional value carries `null` and not the zero
  value of its type, and its schema check includes a response whose optional member is absent." The live
  exchanges grant a Membership with a Workspace and an end and one with neither, read each, and require
  `null` for both members of the second and a value for both of the first. A refused context check is
  exchanged and must carry no `membership_id`. Each of these bodies also passes the schema check.

### Restore

- `deploy-dev`'s restore drill (§Restore Evidence) backs the wired stack up, deletes its volume,
  restores it, and compares every table, sequence, role and the schema with owners, grants and RLS
  policies against the source. The restarted service must answer three provider reads identically.
- A second `restore.sh` over the restored database is refused.
- `backup.sh` keeps exactly the newest 14 complete sets of 16 seeded, leaves a lone file and a
  `.partial` alone, and a run against a stopped database fails and deletes nothing (§Restore Evidence
  steps 3a and 4a, 1.28.0).
- The drill's checks are load-bearing. One row of `platform.delivery_receipt` removed inside a
  transaction that rolls back must change the fingerprint, and the same dump restored into a
  cluster without its roles must stop on a role that does not exist.

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
- With `FORCE` removed from one table, the readiness probe and the startup check fail and name
  the table, as `organization_app`; restored, the probe passes again
  (`TestReadinessAndStartupRefuseAWeakenedDatabase`, 1.19.0).

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
| Readiness failing on the isolation posture, or a start refused on it (1.19.0) | — | any occurrence |
| A provider's access unreviewed for more than seven days (1.21.0) | any occurrence | — |

A `WITH CHECK` rejection means application code attempted to write a row into a Tenant
it was not bound to. That is a defect or an attack, and it is treated as a security
finding rather than a validation error.

**The first two are exported from 1.22.0.** Both reach the HTTP surface as an internal error, which
answers `500` and withholds the cause. Every such error is now logged at ERROR with the method, the
route pattern and the cause. One an isolation control raised is classified by its SQLSTATE and logged
as "an isolation control refused a statement" with its `control`, and counted in
`organization_isolation_refusals_total{control}`:

| `control` | PostgreSQL raises | Alert |
| :-- | :-- | :-- |
| `with_check` | `42501`, "new row violates row-level security policy", which only a `WITH CHECK` expression raises | `IsolationWithCheckRejection`, critical at any in 15 minutes |
| `unset_binding` | `42704`, "unrecognized configuration parameter", on a connection that never set the scope; `22P02`, an empty value cast to `uuid` or `boolean`, on one where an earlier `SET LOCAL` left it empty | `IsolationUnsetBindingWarning` at any in an hour, `IsolationUnsetBindingCritical` at 5 |

A counter in the process, not a gauge read from the database: the refusal leaves no row, since the
statement it refused rolled back. The surface reads the driver's error through its `SQLState()`
method alone, because `arch.json` denies this repository the driver. The other signals are as before:
the provider transaction without a reason cannot happen (`db.ErrReasonRequired` refuses it before the
transaction opens), the assertion and the posture are CI and readiness failures, and the unreviewed
access is §Privileged Access Review's report.

Runbooks required before production: unset-binding investigation, `WITH CHECK`
rejection triage, provider-access review, and suspected cross-tenant exposure.
Written: `docs/runbooks/provider-access-review.md`, which reads the record through the routes of
§Privileged Access Review from 1.21.0. Written (1.22.0): `docs/runbooks/unset-binding.md`,
`docs/runbooks/with-check-rejection.md` and `docs/runbooks/cross-tenant-exposure.md`.

### Restore Evidence

1.20.0. The production gate asks for restore evidence for the Organization Database, including the
outbox and the projection cursor state (`ROADMAP.md` §Gates, SAD-004 §6.6, STD-GLB-002 §Restore
Evidence). It belongs here because a restore lives or dies on the roles of §Roles: every table is
owned by `organization_migrator` and granted to the `_rt` roles, the five login roles inherit them,
and `organization_self_rt` and `organization_invitation_resolver_rt` are reached by `SET LOCAL ROLE`. A database dump carries none of those
roles, so a restore that skips them fails on its first owner. One that recreates them by hand risks
the posture §Verifying the Posture at Runtime refuses to start on.

`deploy-dev` produces the evidence on every change and daily, as its last step,
`scripts/dev-restore-drill.sh`, after the wiring proof has filled the three stacks:

1. `scripts/dev-restore-read.ps1 -Seed` begins an offboarding on a new Tenant, because the wiring
   makes none. It reads `GET /v1/provider-grants`, `GET /v1/organizations?limit=100` and
   `GET /v1/offboardings?limit=100` as the bootstrap operator. The service is then stopped, and its
   dispatcher with it.
2. `scripts/restore-fingerprint.sql` records the newest Atlas revision, every table's row count and
   the md5 of its rows' sorted md5s, every sequence, and every role with its attributes and
   memberships. `pg_dump --schema-only --create`, with a fixed `--restrict-key`, records the schema
   with owners, grants, default privileges and RLS policies.
3. `deploy/dev/backup.sh`, the operator's cron line, writes `pg_dumpall --globals-only` and
   `pg_dump --format=custom`.

   3a. **Retention by count** (1.28.0, `STD-GLB-009` rule 9, 1.10.0). In a directory of its own,
   seeded with 16 older complete sets, one lone file and one `.partial`, a backup must keep exactly
   the newest 14 complete sets, the one it wrote among them, and leave the lone file and the
   `.partial` alone. The rule prunes by count, never by age, so a backup that stops deletes no
   good set.
4. `docker compose down --volumes` deletes the volume, and the drill checks that it is gone.

   4a. **A failed backup deletes nothing** (1.28.0, the same rule). A backup run while the database is
   down must fail and leave the seeded directory as it was.
5. `deploy/dev/restore.sh` restores into the new, empty volume: the roles, then
   `pg_restore --create --exit-on-error`. The drill fingerprints again before the migrate job runs,
   and a second `restore.sh` must refuse the now-occupied cluster.
6. `docker compose up -d --build` runs the migrate job, whose `control database ready` asserts the
   privilege shape on the restored database. Then `/readyz`, and the three reads again.
7. `restore-evidence.json` and the fingerprints are kept as the job's `restore-evidence` artifact
   for 90 days. The backup files are never uploaded: they hold role password hashes.
8. **The restore bump** (1.28.0, `ADR-ORG-003 §5.5`, `STD-GLB-002` 3.3.0). Step 5 restores with
   `RESTORE_BUMP=0`, so the comparison sees the tables as backed up. The drill then runs the bump on
   the restored database and requires every Tenant version and security version at 10^9 or above,
   each withdrawn Tenant published at its new version and no active one, every active consumer asked
   to reconcile, and identity-control applying a bumped Tenant version
   (`TDD-organization-control-002` §After a Restore to an Older Point).

It fails unless schema, migration version, every table, every sequence and the roles are equal, and
the three reads are identical. These tables must hold rows in the source: `platform.outbox`,
`platform.outbox_delivery`, `platform.delivery_receipt`, `platform.subscription`,
`projection.consumer` (with its snapshot, reported and reconciled marks),
`membership.membership`, `membership.membership_event`, `tenant.tenant`,
`organization.provider_grant` and `operation.offboarding`. It fails above the 15-minute RTO of
`PAD-PLT-002 §6.2`, timed from `restore.sh` on the empty volume to the verified read.

**Why the roles come from the backup.** Until 1.20.0 the README ran the migrate job first, for the
roles, and `pg_restore --clean` over the schema it had built. That suits only a dump of the same
release: a dump of an older one would restore older migration history over tables the newer migrate
job had made, and the next migration would fail. Restored whole with `--create` after the roles,
the database is as it was, and the migrate job upgrades it as it upgrades any database. It still
runs after the restore, so `roles.sql`, `rls.sql`, `grants.sql` and the login roles' passwords from
`.env` are asserted again.

**What it does not prove.**

- **RPO.** A daily dump loses up to 24 hours, against the 1 minute of `PAD-PLT-002 §6.2`, which needs
  continuous WAL archiving with point-in-time recovery on the production platform. A recorded gap,
  not a claim.
- **A restore to an older point.** The drill restores to the instant of its own backup. A real
  restore loses the events after it, while consumers keep them: a consumer holds a higher version
  than authority, and authority's next version of that Membership can equal one the consumer
  already holds, which the consumer discards. SAD-004 §6.6 requires a reconciliation and
  containment plan for this. From 1.22.0 the containment is §Pausing Tenant Administration and the
  reconciliation of Memberships is `TDD-organization-control-002` §After a Restore to an Older Point;
  `docs/runbooks/organization-database-restore.md` runs both. From 1.28.0 this records what changed
  on 2026-10-20: Tenant versions, Tenant security versions and provider grant versions are bumped by
  10^9 before the service serves (`ADR-ORG-003 §5.5`), and the drill proves the bump (step 8). The
  Membership advance and the pause are proven by integration tests, not by the drill, which restores
  to the instant of its own backup.
- **Production size.** The duration is measured on CI data.
- **Erasure.** This service has no right-to-erasure path, so it keeps no tombstones for a restore
  to re-apply (`STD-GLB-007` §GDPR Right-to-Erasure). When one is built, the drill proves it.

### The Migrate Image and Its Exceptions

1.24.0. The migrate image (Dockerfile target `migrate`) runs the Control Database pipeline,
`deploy/dev/migrate.sh`, and the one-off tasks. Its findings are governed by `STD-GLB-009` 1.8.0
§Container Images, rules 4, 5, 7, 8 and 10, which land with scnehaux-architecture #86. On 2026-10-10
the daily `image-scan` failed on it, on Go advisories in a binary this repository copies and does not
build. This section records what each finding is, the evidence for it, and what keeps that evidence
true. `.grype.yaml` holds the statements, and each one's reason points here.

1.25.0. On 2026-10-17 the daily scan failed on the image again, on a new advisory in the same binary,
GO-2026-6617. By then Ariga had published Atlas 1.3.4, built with the fixed toolchain and modules.
1.25.0 moves the pin to it and removes every statement about the binary (§1.25.0: The Pin Moves).
The 1.24.0 analysis below stays as the record of why the statements held, and as the method for the
next advisory in the binary.

**What the image runs.** The entrypoint is `organization-dev-migrate`. It runs `psql`, this repository's
`organization-migrate`, and Atlas once, as `atlas migrate apply --env local`. Both of that environment's
URLs are `postgres://` with `sslmode=disable`, and `atlas.hcl` has only `file://` sources: it has no
`data` block, no `atlas://` directory, and no Atlas Cloud token. The `bootstrap-provider`
and `maintenance` tasks run `organization-control bootstrap-provider` and
`organization-migrate -stage=maintenance` on the same image, and neither of them runs Atlas.

**Atlas calls out unless it is told not to.** Until 1.25.0, `/usr/local/bin/atlas` came from
`arigaio/atlas:1.3.3`, pinned by digest. On 2026-10-10 that is the newest numbered tag, and the newer `latest` tags are
`v1.3.4-…-canary` builds. Its build information names go1.26.6, `golang.org/x/net` v0.58.0 and
`google.golang.org/grpc` v1.83.1. Atlas serves nothing. It does make two kinds of outbound call:

- **A release check.** It checks for a newer release around every command. Atlas v1.3.0's source
  defines `envNoUpdate = "ATLAS_NO_UPDATE_NOTIFIER"`, with the comment "envNoUpdate when enabled it
  cancels checking for update", and `vercheckURL = "https://vercheck.ariga.io"`. The check returns
  early `if v := os.Getenv(envNoUpdate); v != ""` [R10].
- **Anonymous telemetry.** Ariga: "If you wish to opt-out of telemetry data collection, you can do
  so by setting the ATLAS_NO_ANON_TELEMETRY environment variable to true. This will disable all
  anonymous telemetry collection" [R11].

1.24.0 sets `ATLAS_NO_UPDATE_NOTIFIER=true` and `ATLAS_NO_ANON_TELEMETRY=true` in the image, with
`ENV`. They are image settings, not compose settings, because the statements below must hold
wherever the image runs: the pipeline, the one-off tasks, CI, and an operator's `docker compose run`.

**Evidence.** Rule 7 asks for two kinds.

1. **Symbols.** `govulncheck` v1.8.0 ran with `-mode binary` on the file (sha256
   `a41ea66b5aaad1e363fd829613a6725c3934378792d0be5591269c041c112b96`), against the Go vulnerability
   database of 2026-10-08. It reports every advisory below at the symbol level, so
   `vulnerable_code_not_present` is open to none of them. The symbols cannot separate client code
   from server code either. The database lists `net/http.Client.Do` among the symbols of
   GO-2026-6613, a server bug, because exported functions on both sides reach the shared code. And
   govulncheck "may also report false positives for code that is in the binary but unreachable"
   [R12]. Each advisory's own text therefore decides which side its bug is on.
2. **What the image runs.** On 2026-10-10 the binary ran under `strace -f -e
   trace=socket,connect,bind,listen,accept,accept4` as uid 70, on a Docker network with no egress.
   identity-control took the trace first with its own `atlas.hcl`. It was repeated here in this
   repository's migrate image, with this `atlas.hcl`, and the results were the same:

   | Command | `ATLAS_NO_UPDATE_NOTIFIER` | `ATLAS_NO_ANON_TELEMETRY` | Connections | `listen`, `accept`, inet `bind` |
   | :-- | :-- | :-- | :-- | :-- |
   | `migrate apply --env local`, database refused | unset | unset | the database; 8 to the resolver, querying `vercheck.ariga.io` | none |
   | the same | unset | `true` | the database; 8 to the resolver, querying `vercheck.ariga.io` | none |
   | the same | `true` | unset | the database only | none |
   | the same | `true` | `true` | the database only | none |
   | `migrate hash`, which needs no database | unset | unset | 8 to the resolver | none |
   | the same | `true` | `true` | none | none |

   No database could run where these traces were taken, so the apply in them fails at its first
   query. `scripts/atlas-execute-path.sh` takes the same trace on the whole pipeline, and
   `image-scan` runs it on every change and daily. It starts the pinned Control Database on a Docker
   network with no egress. It then runs the image's own entrypoint with Atlas, and only Atlas,
   wrapped in `strace`, and names the database by address, so any DNS query is a connection to the
   resolver. It fails on a `listen`, an `accept`, an inet `bind`, or an inet connection to anything
   but the database's address and port. A control run with `ATLAS_NO_UPDATE_NOTIFIER` emptied must
   show the release check, or the trace is blind, and the run fails too.

**Per advisory.** Every finding the gate fails on in the 1.3.3 binary is in this table, with
GO-2026-6609, which govulncheck reports and Grype does not. The side of each comes from the advisory
[R13]. GO-2026-6617 is the 1.25.0 finding; the pin move made its statement unnecessary, and the row
gives the statement it would have had.

| Advisory | CVE | Packages in atlas | Side, in the advisory's words | Statement |
| :-- | :-- | :-- | :-- | :-- |
| GO-2026-6603 | CVE-2026-78659 | stdlib go1.26.6, x/net v0.58.0 | HTTP/2 server: "For HTTP/2 servers, a malicious client can exploit this" | not in execute path: Atlas never listens |
| GO-2026-6605 | CVE-2026-56866 | stdlib | HTTP/1 client: "When http.Transport sends an HTTP/1 CONNECT request with a non-empty Request.Body" | not in execute path: no HTTP request with the release check off |
| GO-2026-6607 | CVE-2026-97031 | stdlib | TLS server: a client "could trigger memory exhaustion in the server process" | not in execute path: Atlas never listens, and its one connection is plaintext |
| GO-2026-6608 | CVE-2026-94440 | stdlib | MIME parsing of a peer's message: "Parsing a multipart form can bypass memory limits" | not in execute path: Atlas reads no HTTP message, as server or client |
| GO-2026-6609 | CVE-2026-78667 | stdlib | HTTP server: "FileServer(FS), ServeContent, and ServeFile(FS) can consume an excessive amount of CPU" | Grype does not report it, so no rule; it would be not in execute path |
| GO-2026-6610 | CVE-2026-78660 | stdlib, x/net v0.58.0 | HTTP/2 client: the transport accepts malformed framing-related headers, a risk "when acting as a reverse proxy" | not in execute path: no HTTP request with the release check off |
| GO-2026-6611 | CVE-2026-78669 | stdlib, x/net v0.58.0 | both: "A malicious HTTP/2 peer can cause excessive CPU consumption in the client or server" | not in execute path: neither side runs |
| GO-2026-6612 | CVE-2026-78663 | stdlib, x/net v0.58.0 | HTTP/2 server: "The HTTP/2 server can refund connection-level flow control twice" | not in execute path: Atlas never listens |
| GO-2026-6613 | CVE-2026-94439 | stdlib | HTTP/1 server: "When an HTTP server handler sends a 2xx response to an HTTP/1 CONNECT request" | not in execute path: Atlas never listens |
| GHSA-2v4p-qf9q-27wj (GO-2026-6443) | CVE-2026-84445 | grpc v1.83.1 | gRPC server: "servers configured with xDS routing can panic" | not in execute path: Atlas never listens |
| GO-2026-6617 (1.25.0) | CVE-2026-97032 | stdlib, x/net v0.58.0 | HTTP/2 server: "A malicious client can repeatedly send a request while changing the header table size to crash the server" | would be not in execute path: Atlas never listens; moot, the pin moved |

Every gated finding in the binary is therefore `not_affected/vulnerable_code_not_in_execute_path`,
and none is `affected`. A `not_affected` statement has no remediation time, only rule 5's 90-day
review. Each is due on 2026-12-09 all the same. That is the date rule 8 would set if a reviewer
rejected the statement: not publicly exposed, not in the KEV, and, in CISA's Vulnrichment of
2026-10-09, Automatable yes, with Technical Impact partial (total for CVE-2026-78663). That gives
60 days from detection on 2026-10-10 [R14] [R15]. The statements rest on a setting, and a release ends
them, so Ariga is to be asked for one built with go1.26.9, x/net v0.60.0 and grpc v1.83.2. The
request is drafted in the pull request that brought 1.24.0, for the owner to file. On 2026-10-10
even Atlas's canary builds carried go1.26.6 and x/net v0.58.0.

**1.25.0: The Pin Moves.** The scan of 2026-10-17 failed on GO-2026-6617 in the 1.3.3 binary,
reported in `stdlib` go1.26.6 (fixed 1.26.9) and `golang.org/x/net` v0.58.0 (fixed 0.60.0), High.
The advisory is "HTTP/2 server crash due to HPACK encoder race in net/http": "the server modifies the
HPACK encoder from two goroutines without synchronization", and "A malicious client can repeatedly
send a request while changing the header table size to crash the server" [R19]. It is server code,
like GO-2026-6603 and GO-2026-6612. `govulncheck` v1.8.0 `-mode binary`, with the database of
2026-10-08, finds it at the symbol level in the 1.3.3 file, in both modules, among them
`golang.org/x/net/http2.Server.ServeConn` and `serverConn.processSetting`, so
`vulnerable_code_not_present` was not open. With the trace above, it would have been
`not_affected/vulnerable_code_not_in_execute_path`: Atlas never listens.

No statement was needed. Ariga pushed `arigaio/atlas:1.3.4` on 2026-10-14, the newest numbered tag
on 2026-10-18 [R20]. `go version -m` on its `/atlas` (sha256
`aec7e73a561889836b5fb7d8ba8cc55969c593e53188048d83cdf087f23bc476`, `atlas version v1.3.4`) names
go1.26.9, `golang.org/x/net` v0.60.0 and `google.golang.org/grpc` v1.83.2: the fixed version of
every advisory in the table, and the release the request drafted with 1.24.0 named. Rule 4 says a
finding in an upstream image "is fixed by moving the pin to a release that fixes it", so 1.25.0:

- pins `arigaio/atlas:1.3.4` by digest, the same distroless flavour as before;
- removes all thirteen statements about `/usr/local/bin/atlas` from `.grype.yaml`;
- keeps the two `ENV` lines and `scripts/atlas-execute-path.sh`. Atlas 1.3.4 still holds
  `ATLAS_NO_UPDATE_NOTIFIER`, `ATLAS_NO_ANON_TELEMETRY` and `vercheck.ariga.io`, and the trace's
  control run fails if the release check no longer shows. With them, a new advisory in the binary is
  decided against evidence taken that day.

`govulncheck -mode binary` on the 1.3.4 file finds one advisory, GO-2026-5932, the unmaintained
`golang.org/x/crypto/openpgp` package, which has no fixed version, so the gate does not fail on it,
and Grype does not report it. The gate passes on the migrate image with no rule about the binary;
the zlib rule is the only one left.

identity-control made the same move the same day, in its TDD-001 1.20.0.

**What was rejected.**

- **A `not_affected` statement for GO-2026-6617 on 1.3.3.** The evidence above would support it, but
  a release with the fix exists. A statement keeps vulnerable code in the image for up to 90 days
  where a pin move removes it now.
- **`affected` until an Ariga release.** The trace shows no path to any of this code. An `affected`
  statement would claim a reachable path that the evidence says is absent.
- **Building Atlas from source with go1.26.9.** Rule 9 asks for a source build in a publicly exposed
  image, and this image is not exposed. The open-source build is not the binary Ariga ships either:
  "You're running the community build of Atlas, which differs from the official version" [R10].
- **An egress-less compose network instead of the variables.** That would be a property of one
  compose file, not of the image, and the statements must hold wherever the image runs. It would add
  depth, and it is left for the production platform's network policy.

**zlib.** CVE-2026-85091 is in zlib 1.3.2-r0. `postgres:17.11-alpine` carries that version, and it
is both the migrate image's base and the Control Database. Alpine 3.24 ships the fix as 1.3.2-r1,
and on 2026-10-10 the tag still resolves to the pinned digest. In rule 8 terms the finding is not
publicly exposed and not in the KEV, with Automatable no and Technical Impact total [R14] [R15], so it
is fixed on system upgrade.

- **The migrate image** upgrades zlib where it is built, under rule 10:
  `RUN apk add --no-cache 'zlib>=1.3.2-r1'`, before `USER postgres`, with a comment that names the
  CVE. On the pinned base the line upgrades 1.3.2-r0 to 1.3.2-r1, and a constraint no repository
  can meet fails the build, which was checked with `zlib>=9`. The line goes when the base pin moves
  to an image that has the fix.
- **The Control Database** runs the official image without building it. It keeps an `affected`
  rule for zlib 1.3.2-r0 at `/lib/apk/db/installed`, due on 2026-11-16. The rule waits for the
  official image to be rebuilt with the fix. PostgreSQL's next minor release, 17.12, is scheduled
  for "November 12th, 2026" [R16], and the official images rebuild on their maintainers' schedule.
  Now that the migrate image holds 1.3.2-r1, and the service image has no package database, the rule
  matches the Control Database image alone. That satisfies rule 5's "one image at a time".

**The Go toolchain.** The build stage is `golang:1.26.9-alpine`, pinned by digest, and `go version`
in the pinned image reads go1.26.9. Every binary this repository builds therefore carries the
standard library fixes above. The tag has since been rebuilt under a new digest, and the pin
stays: it already carries go1.26.9.

**What the scan enforces.** This is `STD-GLB-009` §5 Enforcement item 4. `scripts/image-scan.sh`
runs `scripts/image-scan-rules.py` on `.grype.yaml` before it scans. Both files are byte-identical
in identity-kernel, identity-control and organization-control. The checker fails on any rule:

- that lacks `vulnerability`, `package.name`, `package.version` or `package.type`;
- for a package found inside a file, which is any type but an operating system package database
  (`apk`, `deb`, `rpm`), that lacks `package.location`;
- that gives a `package.location` which is not a full path, or which uses a wildcard;
- whose `reason` lacks rule 5's parts in order: `review-by`, `affected` or `not_affected/` with a
  justification rule 7 accepts, `detected`, the images, and the statement;
- whose review date has passed, is more than 90 days ahead, or is before `detected`;
- whose `detected` is in the future;
- for `affected`, whose `review-by` is more than 90 days after `detected`, which is rule 8's longest
  time.

Every rule here gives a location. Until 1.25.0 the Go modules named `/usr/local/bin/atlas`. The
zlib rule, whose type does not require one, names `/lib/apk/db/installed` as well. Whether a statement is true is
left to review. Outside CI, the scan's database and image archives go under `./.image-scan`, which
git ignores, and never under `/tmp`.

**The migrate image is built, never pulled.** `deploy/dev/compose.yaml` gives the migrate service
both a `build` and an image name, so the one-off tasks can reuse what it built. Compose's
specification says what that does: "When Compose is confronted with both a build subsection for a
service and an image attribute, it follows the rules defined by the pull_policy attribute. If
pull_policy is missing from the service definition, Compose attempts to pull the image first and
then builds from source if the image isn't found in the registry or platform cache" [R17]. An
image published under the same name would therefore run in place of this one, along with every
statement above. The migrate service sets `pull_policy: build`, which reads "Compose builds the
image. Compose rebuilds the image if it's already present". The one-off tasks name the same image
without building it and set `pull_policy: never`, which reads "Compose doesn't pull the image from a
registry and relies on the platform cached image. If there is no cached image, a failure is
reported" [R18]. The default for a service that does not build is `missing`, under which a task run
before the first build would have pulled the name. `deploy-dev` asserts both on every run. Every image
the stack names without a digest must carry `build` where its service builds it and `never`
elsewhere, and the migrate image in use must carry the commit under test (1.31.0).

The check on the image itself is positive since 1.31.0. It reads the commit under test back from the
image the running migrate container uses. Compose sets the label: "`labels` add metadata to the
resulting image" [R24], here `org.opencontainers.image.revision`, fed from `GITHUB_SHA` in CI and
from `git rev-parse HEAD` on a server (`BUILD_REVISION`). The OCI Image Specification defines it:
"**org.opencontainers.image.revision** Source control revision identifier for the packaged
software", in the namespace that "is reserved for use in the OCI Image Specification" [R25].
`deploy-dev` takes the image ID of the migrate container, not a tag, and requires that image's label
to equal `GITHUB_SHA`. An image pulled under the same name would not carry the revision of a commit
that did not exist when it was published.

Until 1.31.0 the check required the image to carry no registry digest. It failed on 2026-10-22 in
identity-control's `deploy-dev`, run 39735938275, on the runner image `ubuntu-26.04` (its last pass,
run 39651327871, ran on `ubuntu-24.04`): the log shows the image built, not pulled, with its
"exporting attestation manifest" and "exporting manifest list" lines, and the same check then
reported a registry digest. Docker's containerd image store is "the default storage backend for
Docker Engine 29.0 and later on fresh installations", and it is the store that handles "images that
include attestations (provenance, SBOM)", which "use image indices that the classic store doesn't
support" [R26]. The explanation is that a locally built image in that store carries a digest too, so
an absent digest no longer shows that an image was built here. The log does not print the digests,
so the check now prints them. The runner image is not pinned to dodge it.

**Residual risk.**

- Since 1.25.0 no statement rests on how the image runs Atlas. The next statement about the binary
  would, and each of these changes would void it: a change to `migrate.sh`'s Atlas command, to the
  entrypoint, to `atlas.hcl`'s URLs or sources, or to the two `ENV` lines.
  `scripts/atlas-execute-path.sh` catches the network side of such a change, and the review of any
  such change rereads `.grype.yaml`.
- The trace covers the paths the pipeline takes. A path Atlas would take only on input this image
  never gives it, such as a cloud directory or a login, is not traced. The statements name the
  configuration they hold for.
- The zlib line lets the build take any zlib at or above 1.3.2-r1 from Alpine's index on the day it
  runs. The base is still named by digest.

| Ref | Source |
| :-- | :-- |
| R10 | Ariga, Atlas v1.3.0 source, `cmd/atlas/main.go`, <https://github.com/ariga/atlas/blob/v1.3.0/cmd/atlas/main.go>, accessed 2026-10-10: "// envNoUpdate when enabled it cancels checking for update"; `envNoUpdate = "ATLAS_NO_UPDATE_NOTIFIER"`; `vercheckURL = "https://vercheck.ariga.io"`; `if v := os.Getenv(envNoUpdate); v != "" { return noText }`; "You're running the community build of Atlas, which differs from the official version." |
| R11 | Ariga, *Atlas: Data Privacy and the CLI*, <https://atlasgo.io/cli/data-privacy>, accessed 2026-10-10: "When you run the Atlas CLI, we may collect anonymous telemetry data"; "If you wish to opt-out of telemetry data collection, you can do so by setting the ATLAS_NO_ANON_TELEMETRY environment variable to true. This will disable all anonymous telemetry collection." |
| R12 | The Go Project, *govulncheck* command documentation, <https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck>, accessed 2026-10-10: "Govulncheck uses the binary's symbol information to find mentions of vulnerable functions"; "It may also report false positives for code that is in the binary but unreachable." |
| R13 | The Go Project, Go vulnerability database entries GO-2026-6603, GO-2026-6605, GO-2026-6607 to GO-2026-6613 (published 2026-10-08) and GO-2026-6443 (GHSA-2v4p-qf9q-27wj), <https://vuln.go.dev/ID/GO-2026-6603.json> and siblings, accessed 2026-10-10. The quotations in the table above are from their details. Fixed in go1.26.9, golang.org/x/net v0.60.0 and google.golang.org/grpc v1.83.2. |
| R14 | CISA, *Vulnrichment*, <https://github.com/cisagov/vulnrichment>, accessed 2026-10-10: CVE-2026-78659, CVE-2026-56866, CVE-2026-97031, CVE-2026-94440, CVE-2026-78660, CVE-2026-78669, CVE-2026-94439 and CVE-2026-84445 with Exploitation none, Automatable yes, Technical Impact partial; CVE-2026-78663 with Exploitation none, Automatable yes, Technical Impact total; CVE-2026-85091 with Exploitation poc, Automatable no, Technical Impact total. |
| R15 | CISA, *Known Exploited Vulnerabilities Catalog*, catalog version 2026.10.08, <https://www.cisa.gov/known-exploited-vulnerabilities-catalog>, accessed 2026-10-10. None of the CVEs in R14 is listed. |
| R16 | The PostgreSQL Global Development Group, *Roadmap*, <https://www.postgresql.org/developer/roadmap/>, accessed 2026-10-10: "The PostgreSQL project aims to make at least one minor release every quarter, on a predefined schedule"; "The current schedule for upcoming releases is: November 12th, 2026". |
| R17 | Docker, *Compose Build Specification*, §Using build and image, <https://docs.docker.com/reference/compose-file/build/>, accessed 2026-10-10: "When Compose is confronted with both a build subsection for a service and an image attribute, it follows the rules defined by the pull_policy attribute. If pull_policy is missing from the service definition, Compose attempts to pull the image first and then builds from source if the image isn't found in the registry or platform cache." |
| R18 | Docker, *Compose file reference: Services*, `pull_policy`, <https://docs.docker.com/reference/compose-file/services/#pull_policy>, accessed 2026-10-10: "never: Compose doesn't pull the image from a registry and relies on the platform cached image. If there is no cached image, a failure is reported"; "missing: Compose pulls the image only if it's not available in the platform cache. This is the default option if you are not also using the Compose Build Specification"; "build: Compose builds the image. Compose rebuilds the image if it's already present." |
| R19 | The Go Project, Go vulnerability database entry GO-2026-6617 (CVE-2026-97032, published 2026-10-08), <https://vuln.go.dev/ID/GO-2026-6617.json>, accessed 2026-10-18: "HTTP/2 server crash due to HPACK encoder race in net/http"; "the server modifies the HPACK encoder from two goroutines without synchronization: one uses the encoder to encode a HEADERS frame as part of a response sent to a client and the other modifies the encoder's table size when handling a SETTINGS frame containing SETTINGS_HEADER_TABLE_SIZE that a client sends. A malicious client can repeatedly send a request while changing the header table size to crash the server." Fixed in go1.26.9 and golang.org/x/net v0.60.0. |
| R20 | Ariga, `arigaio/atlas` on Docker Hub, tag `1.3.4` (pushed 2026-10-14, `sha256:3cd6883d5ec2703741f750fd5f9efed3a61f8e164b9dcea93ab7a7be429a1b5b`), <https://hub.docker.com/r/arigaio/atlas/tags>, accessed 2026-10-18. Its `/atlas` reports `atlas version v1.3.4`, and `go version -m` on it reads `go1.26.9`, `golang.org/x/net v0.60.0` and `google.golang.org/grpc v1.83.2`. |

### The Build Context

1.28.0. `docker compose build` sends the build context to the builder, and the Dockerfile's build
stage runs `COPY . .`, so every file in the context reaches a layer of the build image. `deploy-dev`
checks sibling repositories out into this workspace (`.kernel/`, `.identity/`), and the kernel's
`deploy/dev/keys/signing` holds its realm keystore. `.dockerignore` (from 2026-10-20, `ad76d3e`)
leaves out the sibling checkouts, every `keys/` directory, `*.pem`, `.env`, a server's
`compose.override.yaml`, a local `.token`, and version control and local output. The Dockerfile reads
`go.mod`, `go.sum`, the Go source, `atlas.hcl`, `schema.hcl`, `migrations/`,
`scripts/login-roles.sql` and `deploy/dev/migrate.sh`, and none of them is excluded.

- **Why.** Docker: "You can use a `.dockerignore` file to exclude files or directories from the
  build context" [R22]. A secret copied into a layer stays there, as one passed to the build does:
  "Build arguments and environment variables are inappropriate for passing secrets to your build,
  because they persist in the final image" [R23].
- **The same file in identity-control.** identity-control made the same change (`aa31462`). No
  standard states the rule yet; it is reported for `STD-GLB-009` §Container Images.
- **The rule and its check (1.29.0).** `STD-GLB-009` 3.0.0 §Container Images rule 12 now states it
  (`ADR-GLB-020`): the file MUST exclude secrets and key material, local state, and `.git` and
  `.github`. This file already does. `scripts/check-dockerignore.sh`, byte-identical in
  identity-kernel and identity-control, builds a scratch context with the file and a probe of each
  kind and fails when one reaches the context, by Docker's own matching (§5 item 9). The
  `dockerignore` job of `ci.yml` runs it.

| Ref | Source |
| :-- | :-- |
| R22 | Docker, *Build context*, <https://docs.docker.com/build/concepts/context/>, accessed 2026-10-20: "You can use a `.dockerignore` file to exclude files or directories from the build context." |
| R23 | Docker, *Build secrets*, <https://docs.docker.com/build/building/secrets/>, accessed 2026-10-20: "Build arguments and environment variables are inappropriate for passing secrets to your build, because they persist in the final image." |
| R24 | Docker, _Compose Build Specification_, `labels`, <https://docs.docker.com/reference/compose-file/build/#labels>, accessed 2026-10-22. "`labels` add metadata to the resulting image." |
| R25 | Open Container Initiative, _Image Format Specification: Annotations_, <https://github.com/opencontainers/image-spec/blob/main/annotations.md>, accessed 2026-10-22. "**org.opencontainers.image.revision** Source control revision identifier for the packaged software." "Keys using the `org.opencontainers.image` namespace are reserved for use in the OCI Image Specification and MUST NOT be used by other specifications and extensions, including other OCI specifications." The page defines annotations; that the same key is set as an image label is Compose's `labels` [R24]. |
| R26 | Docker, _containerd image store_, <https://docs.docker.com/engine/storage/containerd/>, accessed 2026-10-22. "The containerd image store is the default storage backend for Docker Engine 29.0 and later on fresh installations." "Working with images that include attestations (provenance, SBOM). These use image indices that the classic store doesn't support." |

## Traceability

| Relationship | Target |
| :-- | :-- |
| Parent system | SAD-004 — Scnehaux Organization Control |
| Realizes capability | PAD-PLT-002 — Organization & Tenancy Platform |
| Governed by | ADR-GLB-002 — Enterprise PostgreSQL Row-Level Security for Isolation |
| Governed by | ADR-ORG-001 — Separate Organization Authority and Keycloak Projection; §5.11 provider, consumer and reporter authority (reporter amended 2026-10-20, 1.27.0) |
| Governed by | ADR-ORG-003 — Tenant Administration Is a Recorded Grant, Checked with Current Membership |
| Governed by | ADR-ORG-005 §5.1 — a self caller reads its own contexts, on one route, with no provider record (1.17.0) |
| Governed by | ADR-ORG-007 §5.1, §5.2 — the self caller accepts an invitation; the token selects one row through the self role (1.26.0) |
| Governed by | ADR-IAM-011 §5.7 (a), (c), ADR-ORG-001 §5.11 (amended 2026-10-21) — the `invitation:resolve` reporter entry, `InvitationResolverScope`, `WithInvitationResolver` and `organization_invitation_resolver_rt` (1.30.0) |
| Governed by | ADR-ORG-002 §5.6 — the privileged-access record is read and reviewed (1.21.0); kept 7 years, purged by maintenance, never by decommissioning (amended 2026-10-20, 1.27.0) |
| Conforms to | STD-GLB-007 1.1.0 §Data Retention Lifecycle and compliance check 4 — audit records kept 7 years, purged through a delete-only control, exempt from decommissioning (1.27.0) |
| Conforms to | STD-GLB-007 1.3.0 §Data Retention Lifecycle — how the 7 years count, evidence against state (1.29.0, §Retention) |
| Conforms to | STD-GLB-009 3.0.0 §Container Images rule 12 and §5 item 9, authorized by ADR-GLB-020 — `.dockerignore` and its CI check (1.29.0, §The Build Context) |
| Conforms to | STD-GLB-007 1.2.0 §Data Retention Lifecycle and compliance check 4 — the `BEFORE DELETE` and `BEFORE TRUNCATE` triggers it describes as this service's, under the §Retention heading it cites (1.28.0) |
| Conforms to | STD-GLB-007 1.2.1 enforcement item 4, "Where it stands" — names this service as implementing the purge, the triggers and the tests (organization-control #75, `TestTheAuditPurgeDeletesOnlyRecordsOlderThanSevenYears`, `TestTheDatabaseRefusesToDeleteAYoungAuditRecord`) (1.28.0) |
| Governed by | ADR-ORG-003 §5.5, §5.6 (amended 2026-10-20) — the drill proves the restore bump; a provider re-applies a lost Membership withdrawal under the pause (1.28.0) |
| Conforms to | STD-GLB-009 2.0.0 rule 9 — `backup.sh` keeps the newest 14 complete sets, and the drill proves it (1.28.0, §Restore Evidence steps 3a and 4a) |
| Conforms to | STD-GLB-001 1.6.0 §Pagination — the list form and its time window |
| Conforms to | STD-GLB-001 2.0.0 §Enforcement Mechanism, `ADR-GLB-021` — the schema check includes responses whose optional members are absent, and the tests require `null`, not the zero value (1.33.0) |
| Conforms to | STD-IAM-002 §3.1.1, §3.2, §3.5 — the grant's holder checks its own record; `principal_id` is the persisted identifier |
| Conforms to | STD-GLB-002 — `FORCE ROW LEVEL SECURITY`, non-owner runtime role, no `SUPERUSER`/`BYPASSRLS`, isolation proven as the runtime role |
| Conforms to | STD-GLB-009 1.8.0 §Container Images rules 4, 5, 7, 8 and 10 — the migrate image's exceptions as VEX statements, and the zlib upgrade; 1.8.0 lands with scnehaux-architecture #86 (1.24.0, §The Migrate Image and Its Exceptions) |
| Conforms to | STD-GLB-009 1.8.0 §Container Images rules 1 and 4 — the Atlas pin moves to 1.3.4, which fixes GO-2026-6617 and every earlier advisory in the binary, and its statements go (1.25.0, §The Migrate Image and Its Exceptions) |
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

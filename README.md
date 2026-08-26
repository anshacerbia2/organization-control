# Organization Control

Go control-plane service and the enterprise authority for Organization, Tenant,
Workspace, Membership, and trusted operating context. It realizes **SAD-004 Scnehaux
Organization Control** under **PAD-PLT-002 Organization & Tenancy Platform**.

## What this service owns

- Organization identity, classification, status, and tenancy-relevant relationships.
- Tenant identity, lifecycle, and `tenant_security_version`.
- Workspace identity and lifecycle within one Tenant.
- Membership between a Principal or workload and a Tenant or optional Workspace.
- Operating-context eligibility and context-switch validation.
- Invitation intent and onboarding correlation.
- Tenant suspension, restoration, offboarding, and retirement coordination.
- Projection publication: snapshots, high-water marks, consumer registry, and
  reconciliation.

## What it does not own

Authentication, credentials, sessions, tokens, federation, and protocol trust belong
to the identity kernel. Product permissions, entitlements, and business roles belong to
their owning domains. Membership establishes contextual relationship and
tenancy-administrative authority — nothing more, per ADR-ORG-001 §5.6.

**This service never calls Keycloak.** ADR-ORG-001 §5.4 prohibits Organization from
writing to Keycloak or its database, and the prohibition is structural here: this
process holds no Keycloak credential and has no network route to it. Membership reaches
Keycloak only as

```text
organization-control → canonical event → identity-control → Keycloak Admin API
```

## Two runtime roles, deliberately

The Organization Database is pooled, so isolation is enforced by PostgreSQL as well as
by the application:

| Role | Scope | Pool |
| :-- | :-- | :-- |
| `organization_rt` | Exactly one Tenant, bound per transaction | Tenant-scoped |
| `organization_provider_rt` | Deliberately cross-Tenant | Provider, held small |

A single role serving both cannot be constrained: any policy permissive enough for
provider work is permissive enough for a defect in a tenant-scoped path. Separating
them at the role level makes a cross-tenant read attributable at the connection.

Neither role owns a table, holds `SUPERUSER`, holds `BYPASSRLS`, or holds any DDL
privilege. Row-Level Security is enabled and forced on every tenant-scoped table, and
`TDD-organization-control-001` specifies the rest.

## Governance lineage

```text
PAD-PLT-002                  Organization & Tenancy Platform
    ↓
SAD-004                      Scnehaux Organization Control
    ↓
TDD-organization-control-*   Technical designs   (docs/designs)
    ↓
Source code
```

## Repository map

| Repository | Role |
| :-- | :-- |
| `identity-kernel` | Keycloak extensions, realm configuration, login theme, image build |
| `identity-control` | Identity Control Service, holds the Keycloak Admin credential |
| **`organization-control`** | **This service** |
| `foundation-platform` | Shared Go substrate |
| `identity-experience` | Identity administration UI and its BFF |
| `organization-experience` | Organization administration UI and its BFF |

Cross-system state moves only through versioned domain events on the broker. Where an
authoritative answer is required, a consumer calls the published HTTP contract that
every other consumer uses; sharing a foundation grants no privileged interface.

## Layout

| Path | Contents |
| :-- | :-- |
| `cmd/organization-control/` | Deployable entrypoint and composition root |
| `internal/organization/` | Organization registry |
| `internal/tenant/` | Tenant lifecycle and security version |
| `internal/workspace/` | Workspace lifecycle |
| `internal/membership/` | Membership, validity, administrative roles |
| `internal/invitation/` | Invitation intent and onboarding correlation |
| `internal/context/` | Operating-context eligibility and the authoritative fresh check |
| `internal/offboarding/` | Obligation tracking and retirement |
| `internal/projection/` | Snapshots, consumer registry, reconciliation |
| `docs/designs/` | Technical Design Documents |

Outbox, dispatcher, event envelope, deduplication, idempotency, problem details, and
telemetry are imported from `foundation-platform` rather than reimplemented here.

## Designs

| TDD | Subject | Status |
| :-- | :-- | :-- |
| `TDD-organization-control-001` | Tenant isolation and Row-Level Security | approved |
| `TDD-organization-control-002` | Membership authority, revocation, and projection publication | approved |

## The revocation contract

Acknowledgement of a revocation means the change is **durable and queued**, never that
it is enforced. Every response carries the accepted timestamp so enforcement delay is
measurable from a recorded origin.

Enforcement is the sum of a propagation term this service starts and a token-lifetime
term the token profile owns. No single owner states the interval; the operational
dashboard presents the sum.

## Building the database

Four sources build one database, and each is owned by whoever owns the SQL:

| Source | Owner | What it applies |
| :-- | :-- | :-- |
| `internal/controldb/roles.sql` | this repository | The three cluster roles |
| `schema.hcl` via Atlas | this repository | The seven owned schemas and their tables |
| `foundation-platform/migrations/platform` | the shared module | The `platform` schema |
| `internal/controldb/rls.sql`, `grants.sql` | this repository | Policies, then privileges |

```powershell
$env:ORGANIZATION_MIGRATION_DATABASE_URL = 'postgres://…/organization_control_dev?sslmode=disable'
$env:DATABASE_URL   = $env:ORGANIZATION_MIGRATION_DATABASE_URL
$env:ATLAS_DEV_URL  = 'postgres://…/org_atlas_dev?sslmode=disable'

go run ./cmd/organization-migrate -stage=pre    # cluster roles
atlas migrate apply --env local                 # the owned schemas and their tables
go run ./cmd/organization-migrate -stage=post   # platform schema, RLS, privileges
```

**The order differs from identity-control's, and the reason is Atlas rather than preference.**

Atlas refuses to apply against a database it considers unclean, and in database scope any
pre-existing schema counts — including `platform`, and including one named in `exclude`, which
the clean check does not consult. Roles are cluster objects, so creating them leaves the
database clean; that is what makes this order possible at all.

Database scope is itself forced. identity-control bounds Atlas to one schema with a
`search_path` on both URLs, and this service declares seven: Atlas rejects a multi-schema HCL
source against a schema-scoped dev URL. `atlas.hcl` bounds the scope with `schemas` and
`exclude` instead, and `public` is declared and managed empty — without it, the first generated
plan ended in `DROP SCHEMA "public" CASCADE`.

## Row-Level Security is not in `schema.hcl`, and that is a vendor limitation rather than a design choice

Atlas OSS models neither `ENABLE`/`FORCE ROW LEVEL SECURITY` nor `CREATE POLICY`. Verified
against v1.3.2: `schema inspect` emits no trace of either and prints *"Skipping … advanced
objects. Upgrade to Pro."* So the policies live in `internal/controldb/rls.sql` and are applied
after Atlas.

**This is a workaround. Three consequences follow, and two of them are closed.**

*A runtime role cannot remove a policy.* Neither holds any DDL privilege and neither owns a
table, so the application cannot weaken its own isolation.

*Drift heals on deploy.* `rls.sql` recreates every policy on every run and discovers its table
set from the catalog, so a dropped policy returns and a newly added table is protected without
anyone remembering to extend a list. That is better than a declarative list, which someone has
to maintain.

*Nothing reconciles a policy the way Atlas reconciles a column.* This one stays open, and it is
narrower than it first looks: reconciliation happens at deploy time, and `rls.sql` already
converges at deploy time. What a declarative tool would add is a **diff** — the ability to review
a policy change as a schema change and to detect drift without applying anything.

Closing that half requires Atlas Pro, which also supplies `atlas migrate lint` — the destructive
gate `ADR-GLB-004` mandates and that CI currently stands in for with a text-level grep. One
purchase clears both, and both are recorded as debt in [ROADMAP.md](ROADMAP.md) rather than
presented as equivalent to the mandated mechanism.

### The safety gap is closed without any vendor

The dangerous half was never reconciliation. It was that **between deploys, in production,
nothing checked** — CI asserts the posture of a throwaway database, which says nothing about the
one serving traffic.

`controldb.AssertIsolation` reads `pg_class`, `pg_policy`, and `pg_roles` and reports every way
the posture is not intact. `-stage=post` calls it as a post-condition, so a deploy that applied
SQL without achieving the posture fails instead of reporting green. Once the HTTP surface exists
it is called at startup and behind the readiness probe: a replica whose database lost a policy
leaves the load balancer, because serving tenant-scoped traffic with isolation disabled is worse
than not serving, and `EAD-006 §8` requires a security-control failure to fail closed.

Six weakenings are tested and each is detected: `FORCE` removed, RLS disabled, a policy dropped,
a table added to a tenant-scoped schema without `tenant_id`, and a runtime role granted
`BYPASSRLS` or `LOGIN`. Every one of them leaves the schema matching its declared state, which is
exactly why a schema tool would not have caught them either.

## Proving isolation

`TDD-organization-control-001` is explicit about what counts:

> Isolation tests MUST prove cross-tenant denial using the actual application runtime role; a
> test executed on an administrative or owning connection is not isolation evidence.

Every isolation assertion connects as a login role that inherits `organization_rt` or
`organization_provider_rt`. On an owning connection they would all pass for the wrong reason,
because PostgreSQL exempts an owner from its own table's policies unless `FORCE` is set.

```powershell
$env:TEST_DATABASE_URL      = 'postgres://postgres:…@127.0.0.1:5432/organization_control_dev?sslmode=disable'
$env:TEST_RUNTIME_PASSWORD  = '…'   # organization_app, inherits organization_rt
$env:TEST_PROVIDER_PASSWORD = '…'   # organization_provider_app, inherits organization_provider_rt

go test ./... -count=1 -p 1
```

**`-p 1` is required, not preferred.** Every integration suite runs against one database, and
`internal/controldb` deliberately weakens the isolation posture to prove its assertions catch
weakening: it drops `membership_tenant_scope`, removes `FORCE`, and grants `organization_rt`
`BYPASSRLS`, restoring each afterwards. Those changes are table-wide and cluster-wide, so any
other package's RLS assertion that runs in that window observes a weakened database and fails
for a reason unrelated to the code it tests. Without the flag `go test ./...` passes roughly
three runs in four, which is the worst possible failure rate — often enough to be dismissed as
flakiness and rare enough to survive review. An advisory lock does not close it, because
`ALTER ROLE` races with every role-dependent assertion in the estate regardless of what locks
the two suites hold.

CI creates both login roles, seeds two Tenants, and sets `REQUIRE_INTEGRATION=1` so a service
container that never came up fails the build rather than leaving every cross-tenant assertion
unrun and the build green.
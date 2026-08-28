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
| `cmd/organization-migrate/` | Schema stages: roles, then platform, RLS, and privileges |
| `internal/config/` | Environment configuration, read once at startup |
| `internal/httpapi/` | Routing, request decoding, and the domain-error-to-problem mapping |
| `internal/access/` | The privileged-access recorder: evidence for cross-Tenant work |
| `internal/db/` | The single scope-binding path and the two pool types |
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
| `schema.hcl` via Atlas | this repository | The eight owned schemas and their tables |
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

**`roles.sql` creates the three group roles and no login role.** `organization_migrator`,
`organization_rt`, and `organization_provider_rt` are all `NOLOGIN`: they carry privileges and
nobody authenticates as them. The login roles that inherit them are a deployment concern, because
their passwords are, so the pipeline creates them and this repository does not. Locally and in CI
that means:

```sql
CREATE ROLE organization_app LOGIN PASSWORD '…';
CREATE ROLE organization_provider_app LOGIN PASSWORD '…';
GRANT organization_rt          TO organization_app;
GRANT organization_provider_rt TO organization_provider_app;
```

Without them the integration suites skip when `TEST_DATABASE_URL` is unset and fail to authenticate
when it is set — see `.github/workflows/ci.yml` for the exact block CI runs.

**The order differs from identity-control's, and the reason is Atlas rather than preference.**

Atlas refuses to apply against a database it considers unclean, and in database scope any
pre-existing schema counts — including `platform`, and including one named in `exclude`, which
the clean check does not consult. Roles are cluster objects, so creating them leaves the
database clean; that is what makes this order possible at all.

Database scope is itself forced. identity-control bounds Atlas to one schema with a
`search_path` on both URLs, and this service declares eight: Atlas rejects a multi-schema HCL
source against a schema-scoped dev URL. `atlas.hcl` bounds the scope with `schemas` and
`exclude` instead, and `public` is declared and managed empty — without it, the first generated
plan ended in `DROP SCHEMA "public" CASCADE`.

## Running the service

Every value comes from the environment, per STD-GLB-009. Nothing is defaulted that would let a
misconfigured process start and fail later.

| Variable | Required | Purpose |
| :-- | :-- | :-- |
| `ORGANIZATION_TENANT_DATABASE_URL` | yes | Connects as `organization_app` → `organization_rt` |
| `ORGANIZATION_PROVIDER_DATABASE_URL` | yes | Connects as `organization_provider_app` → `organization_provider_rt` |
| `ORGANIZATION_TOKEN_ISSUER` | yes | Compared for exact equality |
| `ORGANIZATION_TOKEN_AUDIENCE` | yes | This resource's registered identifier |
| `ORGANIZATION_JWKS_URL` | yes | Key source. Never read from a token |
| `ORGANIZATION_TENANT_CLAIM` | yes | The claim carrying the Tenant a caller administers |
| `ORGANIZATION_PROVIDER_ROLE` | yes | The realm role conferring cross-Tenant authority |
| `ORGANIZATION_LISTEN_ADDRESS` | no | Defaults to `:8080` |
| `ORGANIZATION_TOKEN_MAX_SKEW` | no | 30s; capped at 60s by STD-IAM-002 §3.5 |

**The two DSNs must differ, and startup refuses them if they are identical.** They are two
credentials for two PostgreSQL login roles with different policies, and the whole isolation posture
rests on ordinary tenant traffic being unable to authenticate as the cross-Tenant role. One DSN
reused for both would compile, pass every test that does not inspect `current_user`, and silently run
the estate's tenant traffic under the role that can read every Tenant.

```powershell
$env:ORGANIZATION_TENANT_DATABASE_URL   = 'postgres://organization_app:…@localhost:5432/organization_control_dev?sslmode=disable'
$env:ORGANIZATION_PROVIDER_DATABASE_URL = 'postgres://organization_provider_app:…@localhost:5432/organization_control_dev?sslmode=disable'
$env:ORGANIZATION_TOKEN_ISSUER   = 'https://…/realms/scnehaux'
$env:ORGANIZATION_TOKEN_AUDIENCE = 'organization-control'
$env:ORGANIZATION_JWKS_URL       = 'https://…/realms/scnehaux/protocol/openid-connect/certs'
$env:ORGANIZATION_TENANT_CLAIM   = 'tenant_id'
$env:ORGANIZATION_PROVIDER_ROLE  = 'organization-provider'

go run ./cmd/organization-control
```

### Three muxes, not one with an exemption list

| Mux | Authentication | Scope resolution | Holds |
| :-- | :-- | :-- | :-- |
| `Probes` | none, ever | none | `GET /healthz`, `GET /readyz` |
| `Anonymous` | none | none | `POST /v1/invitations/lookup` |
| `API` | required | required | everything else |

An exemption list is edited by whoever adds a route, and the failure mode of forgetting is an
unauthenticated mutation. Here a route is unauthenticated only if its author writes it into
`Anonymous`. identity-control learned the other half of this in an outage: one mux meant the
authentication middleware also wrapped `/readyz`, every probe answered 401, and no replica entered
service.

The anonymous route reads nothing. SAD-004 §5.5 requires an invitation lookup to answer identically
for an absent, expired, revoked, accepted, and valid token, and the only construction where that
holds for the status code, the body, *and* the response time is one that looks nothing up.

### Mutations are not idempotent yet

There is deliberately no `Idempotency-Key` header. foundation-platform's `idempotency.Claim` takes a
`db.Tx` because the claim must commit with the effect it guards; the services here open their own
transactions and accept no claim, so a middleware could only claim outside them. Accepting the header
without honouring it would tell a client its retries are safe at exactly the moment they are not.

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
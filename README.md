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

## Row-Level Security is not in `schema.hcl`

Atlas OSS models neither `ENABLE`/`FORCE ROW LEVEL SECURITY` nor `CREATE POLICY`, so the
policies live in `internal/controldb/rls.sql` and are applied after Atlas.

That has a consequence worth stating: **nothing reconciles a policy the way Atlas reconciles a
column.** A policy dropped by hand stays dropped while the schema still matches its declared
state. The assertions in `internal/controldb/rls_integration_test.go` read `pg_class` and
`pg_policy` because they are the only thing that would notice — and they were verified to
notice, by removing `FORCE` from one table and dropping one policy and watching them fail.

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

go test ./internal/controldb/... -v
```

CI creates both login roles, seeds two Tenants, and sets `REQUIRE_INTEGRATION=1` so a service
container that never came up fails the build rather than leaving every cross-tenant assertion
unrun and the build green.
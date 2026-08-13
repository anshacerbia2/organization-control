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

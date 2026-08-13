# Organization Control — Roadmap

Execution tracker for this repository only. Architecture lives in
`scnehaux-architecture`; nothing here overrides a SAD, an ADR, or a standard.

Week numbers are relative to the first build week, not calendar dates.

## Position in the build order

**This is the repository with the most buildable surface and the fewest blockers.**
It touches Keycloak nowhere, so no proof-of-concept answer gates any of it. Its only
cross-repository dependency is `foundation-platform`, which lands first.

## Design status

| TDD | Subject | Status |
| :-- | :-- | :-- |
| `TDD-organization-control-001` | Tenant isolation and Row-Level Security | approved |
| `TDD-organization-control-002` | Membership authority, revocation, projection publication | approved |
| `TDD-organization-control-003` | Organization, Tenant, and Workspace lifecycle | approved |
| `TDD-organization-control-004` | Invitation, onboarding correlation, and offboarding obligations | approved |

Design 003 closed the gap that blocked the first migration. Design 002 writes a
composite foreign key against `workspace.workspace (tenant_id, workspace_id)` and
increments `tenant_security_version` on Tenant-wide changes; 003 supplies both tables,
both state machines, and the `UNIQUE` constraint that composite key depends on.

## Week 1 · Database and isolation

Atlas migrations under ADR-GLB-004 and STD-GLB-002, run by `organization_migrator`.

- Schemas `organization`, `tenant`, `workspace`, `membership`, `invitation`,
  `operation`, `projection`, `platform`
- `membership.membership` with the composite foreign key and the partial unique index
- `tenant_security_version` and `membership_version`
- Row-Level Security: enabled and forced, tenant and provider policies, `WITH CHECK`
  mirroring `USING`
- `organization_rt` and `organization_provider_rt`, two pools, grant assertion in CI
- `db.WithTenantScope` and `db.WithProviderScope` as the only paths that set scope

**Exit:** cross-tenant denial proven by tests executed as `organization_rt`; an unset
binding raises an error rather than returning an empty set; `SET LOCAL app.` appears in
no package other than `db`.

## Week 2 · Authority and revocation

- Membership state machine, refusing every transition outside the diagram
- Revocation transaction: status, version increments, and the priority outbox append in
  one commit
- Suspend, restore, and their events
- Tenant suspension incrementing `tenant_security_version`
- Accepted timestamp on every acknowledgement

**Exit:** injecting a failure after the status change and before the outbox append rolls
back both; `membership_version` never decreases.

## Week 3 · Projection publication

- `projection.consumer` registry with declared freshness and stale behavior
- Snapshot generation, high-water mark, paging, admission control
- `GET /v1/projections/organization/snapshot`, `:reconcile`, consumer status
- Bootstrap contract: a cursor without a snapshot mark is refused
- Reconciliation comparing authority against reported projection state

**Exit:** a consumer that has not registered receives no projection; a snapshot plus the
events after its mark reconstruct the authoritative set with no gap and no duplicate.

## Week 4 · Lifecycle and offboarding

- Organization, Tenant, and Workspace command surfaces
- Invitation intent, expiry, and identity-onboarding correlation
- Offboarding: access freeze, obligation tracking, staged retirement
- Provider administration paths with reason, approval, and evidence
- `GET /v1/context/{tenant_id}/{principal_id}:verify` with its rate signal

**Exit:** offboarding is resumable and infers completion from no single response;
`:verify` call rate is measured per consumer.

## Waiting on nothing

No item above waits on the Keycloak proof-of-concept. The three questions that touch
this domain — projected context representation, session removal granularity, context
switch mechanism — are answered in `identity-kernel` and consumed by `identity-control`.
They change how Membership reaches Keycloak, not what Membership is.

## Not this service

Recorded so scope creep is visible rather than convenient:

- Authentication, credentials, sessions, tokens, federation — the identity kernel.
- Applying context into Keycloak and removing Keycloak sessions — `identity-control`.
- Product permissions, entitlements, business roles — their owning domains.
- Physical Tenant provisioning — external, coordinated through desired state.

This service holds no Keycloak credential. That is what makes the ADR-ORG-001 §5.4
prohibition structural rather than procedural, and it is asserted by test.

## Gates

**Design gate.** All four designs at `1.0.0`.

**Production gate.** The design gate, plus: restore evidence for the Organization
Database including outbox and projection cursor state, cross-tenant denial proven as
the runtime role, measured accept-to-publication delay inside budget for priority
events, and runbooks written for revocation not enforced within budget, projection
drift repair, provider-access review, and stuck offboarding.

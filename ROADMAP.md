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

- ✅ Schemas `organization`, `tenant`, `workspace`, `membership`, `invitation`,
  `operation`, `projection`, plus `platform` from the shared module
- ✅ `membership.membership` with the composite foreign key and the partial unique index
- ✅ `tenant_security_version` and `membership_version`
- ✅ Row-Level Security: enabled and forced, tenant and provider policies, `WITH CHECK`
  mirroring `USING`
- ✅ `organization_rt` and `organization_provider_rt`, neither owning a table nor holding
  `SUPERUSER`, `BYPASSRLS`, `CREATEROLE`, or `LOGIN`; asserted against the catalog in CI
- ✅ `db.WithTenantScope` and `db.WithProviderScope` as the only paths that set scope

**Exit:** cross-tenant denial proven by tests executed as `organization_rt`; an unset
binding raises an error rather than returning an empty set; `SET LOCAL app.` appears in
no package other than `db`.

**Met.** Twelve assertions run as the runtime login roles rather than on an
owning connection, which is what `TDD-organization-control-001` requires and what makes them
evidence: a read bound to Tenant A sees none of Tenant B and exactly one Tenant row, an insert
carrying another Tenant's identifier is refused by `WITH CHECK`, an update cannot move a row
across Tenants, an unbound query raises rather than returning empty, and a second transaction
on the same pooled connection raises — proving `SET LOCAL` reverted.

Verified non-vacuous. Removing `FORCE` from one table failed the structural assertion; dropping
one policy failed both it and the read assertion. Re-running `-stage=post` restored them, which
also demonstrates the stage is self-healing.

The third clause is open because `db.WithTenantScope` does not exist yet. The binding is issued
inline by the isolation suite, which is honest for a test of the policy and is not the
production path; Week 2 adds the two functions and the architecture test that `SET LOCAL app.`
appears nowhere else.

### What building it found

| Finding | Consequence |
| :-- | :-- |
| `operation.offboarding_obligation` carries no `tenant_id` in `TDD-organization-control-004`, while `TDD-001` requires one on every table in an RLS schema and has its test reject a table without one | The two designs contradict each other, and the rule wins: RLS evaluates a predicate per row and cannot follow a join, so a policy on the parent protects nothing here. `tenant_id` is denormalized onto the child, and a composite foreign key to `(tenant_id, offboarding_id)` on the parent keeps the copy honest — a row cannot claim a Tenant its offboarding does not belong to. Recorded as a departure below |
| Atlas rejects a multi-schema HCL source against a schema-scoped dev URL | identity-control bounds Atlas with one `search_path`; this service cannot. Both URLs are database-scoped and `atlas.hcl` bounds the scope with `schemas` and `exclude` instead |
| Neither `schemas` nor `exclude` prevented `DROP SCHEMA "public" CASCADE` | The same plan identity-control produced, for the same reason: in database scope an existing schema absent from the source reads as drift. `public` is now declared and managed empty, which is both true and useful — a table appearing there is drift, and Atlas reports it |
| Atlas refuses to apply against a database it considers unclean, and `exclude` does not affect that check | The pipeline order differs from identity-control's. Roles are cluster objects so creating them leaves the database clean; Atlas runs second; the `platform` schema is applied third rather than first, because it would otherwise make the database unclean before Atlas ever ran |
| The `tenant_security_version` column and the `workspace_tenant_scope_unique` constraint are each declared in two TDDs | Applied as SQL in the order the designs state, the second declaration fails. The declarative schema resolves it by construction — each is declared once in `schema.hcl` — and the duplication is recorded here rather than silently deduplicated |

### Departures from the designs, recorded

**`WithTenantScope` and `WithProviderScope` carry `db.Tx`, not `pgx.Tx`.** The design writes both
signatures as `fn func(pgx.Tx) error`. `arch.json` denies this repository any import of pgx, and
foundation-platform's db package exists so a driver type never reaches a domain signature —
replacing the driver is then one module's change rather than every consumer's. The departure is
the type name; the shape is unchanged.

**`operation.offboarding_obligation` carries `tenant_id`.** `TDD-organization-control-004`
declares it without one. Adding it is the smaller change: the alternative was to exclude the
table from the RLS set, which `TDD-organization-control-001` explicitly refuses, or to leave it
in an RLS schema unprotected, which is the failure that design exists to prevent. The
composite foreign key is what makes the denormalized column safe rather than a second source
of truth.

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

## Debt, named rather than implied

Two items, and one purchase clears both.

### `atlas migrate lint` is Atlas Pro only since v0.38

`ADR-GLB-004` requires the pipeline to block a destructive plan rather than report it, and names
that command as the mechanism. On the free CLI it aborts. CI stands in with `atlas migrate
validate` — which checks directory integrity and nothing about destructiveness — plus a
text-level grep for `DROP TABLE`, `RENAME`, `TRUNCATE` and their neighbours.

The grep fails, which is the property that matters. It is also cruder than the analyzer it
replaces: it reads text rather than a parsed plan, so it cannot tell a destructive statement from
the same words inside a comment, and a reviewer can silence it with an annotation. Recorded here
rather than presented as equivalent.

### Row-Level Security is outside the declarative schema

Atlas OSS models neither `ENABLE`/`FORCE ROW LEVEL SECURITY` nor `CREATE POLICY` — verified
against v1.3.2, whose `schema inspect` emits no trace of either. The policies therefore live in
`internal/controldb/rls.sql`.

**What this does not cost.** A runtime role cannot remove a policy: neither holds DDL and neither
owns a table. Drift heals on deploy, because `rls.sql` recreates every policy on every run and
discovers its table set from the catalog — so a new table is protected without anyone extending a
list, which a declarative set would require.

**What it costs.** No diff. A policy change cannot be reviewed as a schema change, and drift
cannot be detected without applying anything.

**What it does not cost, contrary to the first version of this note.** Production safety. The
gap that mattered was never reconciliation — it was that nothing checked production between
deploys, and a declarative tool would not have closed that either, because reconciliation happens
at deploy time. `controldb.AssertIsolation` closes it: `-stage=post` calls it as a
post-condition, and the future composition root calls it at startup and behind readiness. Six
weakenings are tested and each is detected, and every one of them leaves the schema matching its
declared state — which is why a schema tool would not have caught them.

### Resolving both

An Atlas Pro login with a CI token supplies the destructive analyzer and RLS in HCL. The
alternative for the first item alone is an amendment to `ADR-GLB-004` naming a different
mechanism; there is no alternative for the second short of writing a policy differ, which is not
work this repository should own.

Until then, neither is presented as satisfied. `ADR-GLB-004`'s destructive gate has a waiver in
effect by substitution, and that is the honest description.
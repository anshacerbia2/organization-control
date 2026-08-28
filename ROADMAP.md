# Organization Control — Roadmap

Execution tracker for this repository only. Architecture lives in
`scnehaux-architecture`; nothing here overrides a SAD, an ADR, or a standard.

Week numbers are relative to the first build week, not calendar dates.

## Position in the build order

**This is the repository with the most buildable surface and the fewest blockers.**
It touches Keycloak nowhere, so no proof-of-concept answer gates any of it. Its only
cross-repository dependency is `foundation-platform`, which lands first.

## Design status

| TDD | Version | Subject | Status |
| :-- | :-- | :-- | :-- |
| `TDD-organization-control-001` | 1.0.0 | Tenant isolation and Row-Level Security | approved |
| `TDD-organization-control-002` | 1.0.0 | Membership authority, revocation, projection publication | approved |
| `TDD-organization-control-003` | 1.2.0 | Organization, Tenant, and Workspace lifecycle | approved |
| `TDD-organization-control-004` | 1.2.0 | Invitation, onboarding correlation, and offboarding obligations | approved |

**No design now contradicts another, and none contradicts the implementation.** Every
departure Weeks 1 and 2 recorded has been folded back into the design that was wrong, with
the reasoning kept rather than summarised away — the sections below still name what changed
so the history is readable, and the designs are the current statement. Five disagreements
were closed:

| Was | Resolved by |
| :-- | :-- |
| `operation.offboarding_obligation` declared without `tenant_id` in 004, while 001 requires one on every table in an RLS schema | 004 §"Why the obligation carries `tenant_id`": the column, the composite foreign key that keeps the copy honest, and the `UNIQUE (tenant_id, offboarding_id)` on the parent it needs |
| The retirement event named `tenant.offboarding.retired` in 004 and `tenant.lifecycle.retired` in 003 | 003's name, in both documents. An event type says what happened to which aggregate; the cause is carried by the correlation identifier |
| `tenant_security_version` and `workspace_tenant_scope_unique` each declared by both 002 and 003 | 003 is the sole declaring authority, stated in its §Purpose. 002 records the dependency in a table instead |
| 002 §Revocation incremented `tenant_security_version` when "the revocation is tenant-wide", contradicting its own §Data Model and 003's increment table | Removed. A Membership revocation reads the version and carries it. 002 explains why incrementing it would make one person's revocation invalidate every cached context in the Tenant |
| 001 wrote both binding signatures as `fn func(pgx.Tx) error` against `*Pool` | 001 §"The Single Binding Path": `db.Tx`, the two distinct pool types, and why the binding is `set_config(..., true)` rather than literal `SET LOCAL` |

Three implementation decisions the designs had not stated are now stated in them: restore
clears `suspended_at`, every Tenant transition is provider-scoped and why, and the
provisioning transitions publish nothing by design.

A sixth disagreement was closed in the governance repository, because it was not local to
this one. 001 and 002 sat below `1.0.0` while carrying `status: approved`: Semantic
Versioning, which GDC-000 §2.6 mandates, reserves major version zero for initial
development where anything may change, and `approved` means approved for implementation.
An implementer reading 0.4.0 was told the contract may move underneath their code while
the same document's status told them it would not — and code was already written against
both. Both are now `1.0.0`, along with the four other designs across `identity-control`
and `identity-kernel` that carried the same contradiction.

GDC-000 §2.6 states the rule as item 6, the Baseline Stability Mandate, and the
`approved_version_not_stable` fitness function enforces it, so a baseline status at major
version zero now fails CI rather than waiting to be noticed. `chartered`, `draft`, and
`proposed` are deliberately left free to sit at `0.y.z` — twelve chartered SADs are
correctly there, and a rule that moved them would destroy the signal it exists to protect.

**The design gate is therefore met:** all four designs at `1.0.0`, all four
non-contradicting, all four passing the governance linter.

Design 003 closed the gap that blocked the first migration. Design 002 writes a
composite foreign key against `workspace.workspace (tenant_id, workspace_id)` and carries
`tenant_security_version` in every Membership event; 003 supplies both tables, both state
machines, the `UNIQUE` constraint that composite key depends on, and the authoritative list
of which transitions move the security version.

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

### Departures from the designs — Week 1, now folded back in

Both were resolved in the designs on 2026-08-23. Kept here because the reasoning is the
part worth keeping, and because a reader comparing an older design version to this code
needs to know which way the disagreement went.

**`WithTenantScope` and `WithProviderScope` carry `db.Tx`, not `pgx.Tx`.**
`TDD-organization-control-001` v0.1.0 wrote both signatures as `fn func(pgx.Tx) error`.
`arch.json` denies this repository any import of pgx, and foundation-platform's db package
exists so a driver type never reaches a domain signature — replacing the driver is then one
module's change rather than every consumer's. The disagreement was the type name and never
the shape. **Design corrected in v0.2.0**, which also records the two distinct pool types and
why the binding is issued as `set_config(..., true)` rather than as literal `SET LOCAL`.

**`operation.offboarding_obligation` carries `tenant_id`.**
`TDD-organization-control-004` v1.1.0 declared it without one. Adding it was the smaller
change: the alternatives were to exclude the table from the RLS set, which
`TDD-organization-control-001` explicitly refuses, or to leave it in an RLS schema
unprotected, which is the failure that design exists to prevent. The composite foreign key
is what makes the denormalized column safe rather than a second source of truth.
**Design corrected in v1.2.0**, including the `UNIQUE (tenant_id, offboarding_id)` on the
parent that the composite key requires.

## Week 2 · Authority and revocation

- ✅ Membership state machine, refusing every transition outside the diagram
- ✅ Revocation transaction: status, version increments, and the priority outbox append in
  one commit
- ✅ Suspend, restore, and their events
- ✅ Tenant suspension incrementing `tenant_security_version` — and restore incrementing it too
- ✅ Accepted timestamp on every acknowledgement
- ✅ `db.WithTenantScope` / `db.WithProviderScope` as the only paths that bind scope, with the
  architecture test that `SET LOCAL app.` appears nowhere else — the clause Week 1 left open

**Exit:** injecting a failure after the status change and before the outbox append rolls
back both; `membership_version` never decreases.

**Met, and asserted by failing inside the window it protects.** Both services carry a
`beforeAppend` seam that is nil outside tests. With a failure injected there, the Membership
revocation leaves the status, `membership_version`, and the outbox exactly as they were, and
the Tenant suspension leaves the status, `version`, and `tenant_security_version` unchanged
with no row in `platform.outbox`. Each test then removes the injection and repeats the
transition, so the rollback is shown to have left the row usable rather than merely unchanged
— a row left locked or half-written would pass the first half of that assertion.

`membership_version` is walked across grant → suspend → restore → revoke and asserted to
increase at every step, with one event per transition on one aggregate. The increment is
`membership_version = membership_version + 1` in the same statement as the status change
rather than a value computed in Go, so two concurrent transitions cannot both write the
version they read.

Both suites connect as `organization_app` and `organization_provider_app` — login roles
inheriting the runtime roles — never as the owner. On an owning connection the cross-Tenant
assertions would pass while proving nothing.

### What building Week 2 found

| Finding | Consequence |
| :-- | :-- |
| `TDD-organization-control-003` names the retirement event `...tenant.lifecycle.retired`; `TDD-organization-control-004` names the same fact `...tenant.offboarding.retired` | The 003 name is used. An event type says what happened to which aggregate; naming it after the process that caused it gives one fact two types depending on how it arose, and the cause is already carried by the correlation identifier. Recorded as a departure below |
| Tenant retirement increments `tenant_security_version` and its event is *not* on the priority lane | Consistent, and it reads like an oversight, so there is a test named after it. The only way into `retired` is from `offboarding`, which already published a priority event and already froze context — by the time a Tenant retires there is no access left to withdraw. What is enforced instead is that the lane agrees with the event's own classification: a name containing `security` and a standard-lane append cannot coexist |
| `organization_rt` holds no `SELECT` on `organization.organization`, so the activation precondition on the sponsoring Organization is not evaluable on a tenant-scoped connection | `TenantService` binds to the provider pool. This is not a convenience: it is the only binding under which the checks `TDD-organization-control-003` §"Tenant Activation" requires can run, and it brings the mandatory reason and recorded evidence with it |
| `event.ParseSource` requires an absolute-path URI reference; `"scnehaux/organization-control"` is rejected | The source is `/systems/organization-control`, declared once in `internal/system` rather than per publishing package. A duplicated system identity fails no compiler — it surfaces as two sources in a consumer's stream for one system |
| Two Tenant transitions publish the same event type | Deliberate, per 003 §"Published Events": `tenant.security.suspended` is the security *consequence*, emitted for `active → suspended` and for either entry into `offboarding`. The payload carries the status, so a consumer that must tell them apart still can. The Membership suite's "one event type per action" assertion is therefore not repeated for Tenant |

### Deliberately not exposed yet

The Tenant state machine is whole — every transition in the 003 diagram is in the table, and
a test walks the full cross product of actions and states. `TenantService` exposes three of
them: `Activate`, `Suspend`, `Restore`.

`begin-offboarding` and `retire` are transitions here and commands elsewhere.
`TDD-organization-control-004` assigns both to `OffboardingService`: each is a stage of a
process that also creates an `operation.offboarding` row, raises obligations across domains,
and — for the freeze — suspends every Membership in the Tenant. A version of either that
moved only the Tenant row would look complete and leave access running, so `arch.json` gives
`internal/tenant` no edge to `internal/membership` and the commands wait for the package that
owns the rest of the work.

`provision` and `fail` are the provisioning-correlation transitions and publish nothing. That
silence is a declared set rather than an absent map key, and a test asserts an action cannot
be in neither: no context exists to invalidate before a Tenant has ever been active, and no
consumer projects a Tenant that has never existed to it.

### Departures from the designs — Week 2, now folded back in

All four were resolved in the designs on 2026-08-23, in the same pass as the Week 1 pair.

**The retirement event is `com.scnehaux.organization.tenant.lifecycle.retired`.** The two
designs disagreed; the reasoning is in the finding above and beside the table in
`internal/tenant/state.go`. **004 corrected in v1.2.0**, which also records why retirement
increments the security version and still travels the standard lane.

**A Membership revocation reads `tenant_security_version` and does not increment it.** 002
v0.3.0 incremented it "if the revocation is tenant-wide", contradicting its own §Data Model
and 003's increment table. The phrase was ambiguous in a way that mattered: a *Tenant-wide
Membership* is one scoped to the Tenant rather than to a Workspace, which is a property of
one relationship and not a Tenant-wide event. Incrementing on it would make one person's
revocation invalidate every cached context for every Principal in the Tenant, and in a busy
Tenant the counter would move constantly — which destroys what a cheap staleness test is
for. **Design corrected in v0.4.0.**

**`suspended_at` is cleared on restore.** Neither design said. The column records when the
*current* suspension began, and left populated it would make a restored Tenant
indistinguishable from a suspended one to every report and alert that filters on it — which
is the reading someone will take, because a non-null timestamp named `suspended_at` says the
Tenant is suspended. The history of past suspensions belongs to the event stream, which is
the record that is supposed to be append-only. **Stated in 003 v1.2.0** §Data Model.

**Every Tenant mutation requires the `version` the caller was shown, and every one is
provider-scoped.** 003 §"API / Interface" stated the version rule for the HTTP surface only;
it is enforced in the service, so the check cannot be bypassed by a second caller of the same
method. The provider binding is forced rather than chosen: `organization_rt` holds no `SELECT`
on `organization.organization`, so the activation precondition on the sponsoring Organization
is not evaluable on a tenant-scoped connection at all. **Both stated in 003 v1.2.0**, along
with the check order and the evidence obligations the provider path carries.

## Week 3 · Projection publication

- ✅ `projection.consumer` registry with declared freshness and stale behavior
- ✅ Snapshot generation, high-water mark, paging, admission control
- ⏳ `GET /v1/projections/organization/snapshot`, `:reconcile`, consumer status — the
  service layer is complete and asserted; the HTTP surface waits for the composition root
  that Week 4 builds alongside the command surfaces
- ✅ Bootstrap contract: a cursor without a snapshot mark is refused
- ✅ Reconciliation comparing authority against reported projection state

**Exit:** a consumer that has not registered receives no projection; a snapshot plus the
events after its mark reconstruct the authoritative set with no gap and no duplicate.

**Met, with one clause of the contract corrected rather than satisfied as written.** Twelve
assertions run as `organization_provider_app`. An unregistered consumer is refused with
`ErrNotRegistered` and a registered one is served, so the refusal is the registration check
rather than a broken snapshot. A progress report before any snapshot is refused, and accepted
immediately after `Bootstrap`. Keyset paging returns every seeded row exactly once, every page
of one snapshot carries the same mark, and continuing without carrying that mark is refused
rather than served with a fresh one.

Each page is read in a read-only `REPEATABLE READ` transaction with the mark taken in the same
transaction as the rows. Under `READ COMMITTED` each statement takes its own snapshot, so a
mutation committing between the two reads would produce rows that omit a Membership and a mark
claiming its event was already represented.

### What building it found

| Finding | Consequence |
| :-- | :-- |
| The bootstrap contract's "events at or below the mark are acknowledged as already represented by the snapshot" is unsound | `platform.outbox.sequence` is allocated by `nextval` at INSERT, not at COMMIT, so a transaction can hold sequence 103 while a later one takes 104 and commits first. A snapshot in that window reports mark 104 and cannot see the row at 103 — nor its Membership, same transaction. Discarding at the mark drops the only event that would ever deliver it, and nothing reports the loss. `TestAMutationInFlightDuringASnapshotIsNotSilentlyLost` holds a transaction open across a snapshot and reproduces it: mark 104, held sequence 103, row absent. TDD-002 v1.1.0 amends step 4 — the consumer applies every buffered event and decides by version comparison, which is the rule this design already stated everywhere else |
| `projection.consumer` had no column to hold a snapshot mark, while the bootstrap contract required refusing a report without one | The rule had nowhere to read from. `snapshot_mark BIGINT` added to `schema.hcl` and to TDD-002; the migration is `20260826071822_add_projection_snapshot_mark.sql` |
| `com.scnehaux.organization.projection.reconciled` has five segments; `event.ParseType` requires six or seven and reserves the fifth for the event's class | The design's name could not express a classification at all. Now `...projection.repair.reconciled`. `repair` and not `security`, because the fifth segment routes the dispatch lane and a large sweep on the reserved lane would delay the live revocations it exists for |
| `SET TRANSACTION ISOLATION LEVEL` is refused once any statement has run, and the scope binding is a statement | The snapshot variant had to live in `internal/db` so isolation is set before the binding. `WithProviderSnapshot` also declares `READ ONLY`: a read-only transaction cannot take the mark and then write below it |
| `db.Pool` exposes only `InTx`, and `arch.json` denies this repository the driver | Holding a transaction open across a snapshot needs a goroutine and two channels in the test. That is the correct trade — the one thing this test needs is the one thing production code must never do |
| A snapshot mark of `0` is legitimate and indistinguishable from an omitted field on an `int64` | `SnapshotRequest.Mark` is a `*int64`. With a plain value, the second page of the first snapshot a deployment ever takes would be refused |

### Departures from the designs, recorded in Week 3

**The high-water mark is a progress coordinate, not a discard boundary.** Reasoning above and
in TDD-002 §"Why the mark is not a discard boundary". Making the mark exact would take
transaction-id arithmetic against the snapshot's `xmin` or a lock serialising every outbox
append against every snapshot; the second buys a property nothing needs, at the price of a
global serialisation point on the mutation path.

**Reconciliation publishes one event per sweep, on a fresh aggregate.** A finding-per-event
stream would let a consumer apply half a sweep and report itself reconciled. The aggregate is
per sweep rather than derived from the consumer name, so sweeps for one consumer are not
partition-ordered — ordering is carried by `mark` in the payload, the same rule that governs
every other event here. Ordering nothing relies on is a constraint to maintain, not a
guarantee to gain.

**Findings are sorted, security first.** Go randomises map iteration deliberately, so an
unsorted result would make two sweeps over identical state return different output — which
breaks the idempotence this design requires and makes a diff of two runs meaningless.

## Week 4 · Lifecycle and offboarding

- ✅ Organization, Tenant, and Workspace command surfaces — the services and their state
  machines; the HTTP routes wait with the composition root below
- ✅ Invitation intent, expiry, and identity-onboarding correlation
- ✅ Offboarding: access freeze, obligation tracking, staged retirement
- ✅ Provider administration paths with reason, approval, and evidence — every cross-Tenant
  path runs through `db.WithProviderScope`, which refuses a blank reason and refuses to
  proceed when the access record cannot be written
- ✅ `:verify` with its rate signal — the service and the measurement; the route waits
- ⬜ Composition root and the HTTP surface — no `cmd/organization-control` and no
  `internal/httpapi` yet. Every endpoint in all four designs is unrouted

**Exit:** offboarding is resumable and infers completion from no single response;
`:verify` call rate is measured per consumer.

**Both met at the service layer.** Resumability is asserted by failing between a stage's work
and the column that records it: the stage does not move, the timestamp stays null, no event is
published, and the retry succeeds — then a second advance from a stage that has moved on is
refused. Completion comes from the obligation registry and from a recorded deprovisioning
outcome, never from one response: `unresolved` and `failed` both hold retirement, and they are
distinguished because an operator waits on one and investigates the other. The `:verify` rate is
per consumer and per interval, with the numerator counted here and the denominator arriving in
the consumer's own progress report.

**What is not done, stated plainly:** there is no running service. Nothing in this repository
listens on a port. Ten packages of authority exist and are asserted against a real database; the
composition root that wires them to HTTP is the one remaining Week 4 item, and with it every
endpoint in all four designs.

The invitation flow closed the second-to-last item. Its exit property is SAD-004 §5.5 — Membership
activates on the join of two independent facts and neither alone activates anything — and the suite
takes that sentence apart: a valid token presented before verification is refused, a verification
for an identifier other than the invited one is refused, a verification correlated to no invitation
is refused, and only both together produce a Membership. Failure injected between the invitation's
state change and the grant rolls back both, and the retry succeeds.

### Where the designs and the implementation disagree — audited 2026-08-28

Checked mechanically rather than asserted: every `com.scnehaux.*` event type in the designs
against every one in the code, every `internal/*` package the designs name against the packages
on disk, and every column the designs declare against `schema.hcl`.

| Finding | Resolution |
| :-- | :-- |
| Three events published by code and declared by no design: `organization.registry.restored`, `workspace.lifecycle.restored`, `tenant.offboarding.released` | Added to the Published Events lists in 003 and 004 with the reasoning. A consumer reading the design would not have known to expect them, which is the whole purpose of that list |
| Four events declared and not published: three `membership.invitation.*`, and `tenant.lifecycle.requested` | Correct — the invitation flow and the Tenant create path are the unbuilt Week 4 items above |
| `internal/db`, `internal/controldb`, and `internal/system` exist and appeared in no component table | Added to TDD-001 §"Packages". A reader who met `TenantPool` in a service signature had no design that mentioned it |
| `internal/invitation` named by 004 and absent from disk | Correct, and tracked above |
| Every schema column the designs declare | Present in `schema.hcl`; no drift |

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

**Design gate. Met.** All four designs at `1.0.0`, no design contradicting another or the
implementation, and `approved_version_not_stable` now refuses a regression in CI.

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

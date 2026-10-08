# Organization Control — Roadmap

Execution tracker for this repository only. Architecture lives in
`scnehaux-architecture`; nothing here overrides a SAD, an ADR, or a standard.

Week numbers are relative to the first build week, not calendar dates.

## Position in the build order

**This is the repository with the most buildable surface and the fewest blockers.**
It touches Keycloak nowhere, so no proof-of-concept answer gates any of it. Its only
cross-repository dependency is `foundation-platform`, which lands first.

## Design status

Each design's version is in its own `doc_meta` and is not repeated here, because a copy of it
went stale on every change.

| TDD | Subject | Status |
| :-- | :-- | :-- |
| `TDD-organization-control-001` | Tenant isolation, Row-Level Security, and grant derivation | approved |
| `TDD-organization-control-002` | Membership authority, revocation, projection publication | approved |
| `TDD-organization-control-003` | Organization, Tenant, and Workspace lifecycle | approved |
| `TDD-organization-control-004` | Invitation, onboarding correlation, and offboarding obligations | approved |
| `TDD-organization-control-005` | Dead-letter resolution, scope and limits | approved |

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

The third clause was open at the end of Week 1 because `db.WithTenantScope` did not exist yet. The
binding was issued inline by the isolation suite, which is honest for a test of the policy and is
not the production path. Week 2 closed it: it added the two functions and the architecture test that
`SET LOCAL app.` appears nowhere else (see Week 2 below). Kept as history.

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
- ✅ The projection routes — `POST /v1/projections/snapshot`, `POST /v1/projections/reconcile`
  and `GET /v1/projections/consumers/{consumer_id}` for consumer status, served since the
  Week 4 composition root (TDD-organization-control-002 §API / Interface). The snapshot route is a `POST`
  at that path, not the `GET .../organization/snapshot` first planned here. identity-control
  calls it, and `deploy-dev`'s wiring job proves the call across the stacks
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
- ✅ `:verify` with its rate signal — the service, the measurement, and the routes
- ✅ Composition root and the HTTP surface — `cmd/organization-control` listens, and
  `internal/httpapi` routes 53 endpoints: 50 authenticated, one anonymous, two probes
- ✅ Tenant intake and the `ProvisioningCoordinator` — `POST /v1/tenants` creates a Tenant
  in `requested`, records its desired provisioning state, and publishes it in one
  transaction; the coordinator owns the three transitions between `requested` and
  `provisioning` and correlates the realized status back by correlation identifier

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

**There is now a running service.** `cmd/organization-control` opens two pools as two login roles,
builds the ten services and the privileged-access recorder, and serves three muxes: probes with no
authentication, one anonymous route, and the authenticated API behind scope resolution. Verified by
running it rather than by reading it — `/healthz` and `/readyz` answer 200 with no credential, an
unauthenticated `POST /v1/memberships` answers 401, and `POST /v1/invitations/lookup` answers 200
with a body identical for two different tokens and 400 for a malformed one.

**The Tenant lifecycle is now whole.** `internal/tenant` declared `requested`, `provisioning`,
`failed` and the `provision` and `fail` transitions, and `Service` exposed only `Activate`,
`Suspend`, and `Restore` — so the front half of the state machine was reachable in `Resolve` and
unreachable in practice, which is why `tenant.lifecycle.requested` was declared and never
published. `Service.Request` and `Coordinator` close it, and the path is asserted end to end:
intake → dispatch → realized → activation, with two events published across the whole of it.

**What is not done, stated plainly:**

- ~~**`Idempotency-Key` is honoured but not yet required.**~~ Required on every command from
  2026-10-07 (backlog item 34, TDD-organization-control-003 1.10.0 §The `Idempotency-Key` Is
  Required on Commands).
- ~~**The response is recorded after the domain transaction, not inside it.**~~ Recorded inside it
  from 2026-10-07 for every command whose effect commits in one transaction (TDD-organization-control-003
  1.11.0 §The Response Is Recorded with the Effect). No `Within` variants were needed. The handler puts
  a renderer in the context (`httpapi.answer`), the service hands its result to `db.Respond` as the
  last step of its transaction, and the claim, the effect and the response commit together. A process
  dying between the commit and the reply now leaves a key a retry replays. Three commands span several
  transactions and keep the old window, each named with its reason in `internal/httpapi/commands_test.go`:
  batch execution, the offboarding freeze batch and the offboarding cancellation. A fourth, consumer
  retirement, answers 204 with no body to record.
- ~~Request validation happens inside the domain transaction.~~ Audited 2026-10-07: every command
  already refused a malformed request before opening a transaction, except a Membership grant, whose
  checks ran inside `GrantWithin`. `Grant` now runs them first (`TestAMalformedCommandOpensNoTransaction`).
  The checks left inside a transaction depend on stored state or the database clock: a resumed batch's
  `fail_on_errors`, and a waiver's expiry.

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
| Four events declared and not published: three `membership.invitation.*`, and `tenant.lifecycle.requested` | All four now published. The three invitation events came with the invitation flow; `tenant.lifecycle.requested` came with Tenant intake, where it doubles as the desired-state publication — no event drift remains in either direction |
| `internal/db`, `internal/controldb`, and `internal/system` exist and appeared in no component table | Added to TDD-001 §"Packages". A reader who met `TenantPool` in a service signature had no design that mentioned it |
| `internal/invitation` named by 004 and absent from disk | Correct, and tracked above |
| Every schema column the designs declare | Present in `schema.hcl`; no drift |

### What building the HTTP surface found

Four defects, each of which would have shipped as something that looked like it worked.

| Finding | Resolution |
| :-- | :-- |
| **`db.PrivilegedRecorder` had no implementation.** The interface makes a recorder a mandatory argument to `NewProviderPool`, which reads as an enforced control — but every implementation in the repository was a test fake that discarded its argument. PAD-PLT-002 §3.3 invariant 22 was satisfied by the type system and by nothing that survived a restart | `internal/access` writes `audit.privileged_access` in its own transaction, so evidence survives a domain rollback. Asserted against the engine as the provider login role |
| **Every validation failure was an unclassified 500.** The services returned bare `errors.New` for a missing field, so a caller who omitted `provenance` was told the service was broken rather than that the request was | An `ErrInvalid` sentinel per package, 54 sites wrapped. Constructor guards and stored-value decoders deliberately keep their 500s: those are a process built wrong and a row that should not exist |
| **The provider role could rewrite and delete its own audit trail.** `grants.sql` grants DML on every table in every owned schema, which on the evidence table hands the role being audited the ability to amend the evidence | `REVOKE SELECT, UPDATE, DELETE` on `audit` from `organization_provider_rt`, leaving INSERT. Found by querying `has_table_privilege` after the first clean deploy, not by reading the file |
| **The composition root would not have started.** `verify.New` refuses to build without a `ClaimRequirement`, and the first version of `main.go` passed none | `httpapi.Requirement` applies the same function the middleware uses to build the caller, so the verifier and the mapper cannot drift apart |

A fifth was caught by an existing control rather than by me: the evidence table was first written into
`operation`, and `-stage=post` refused the deploy because `rls.sql` checks that every table in an RLS
schema carries a non-nullable `tenant_id`. The rule's own hint prescribed the fix — a table that is
not tenant-scoped belongs in a schema outside the RLS set — so the table moved to a new `audit`
schema rather than the invariant growing a carve-out. That check had never fired before.

### What building the provisioning path found

One live defect and four places where the design stops short of what an implementation has to decide.
All five are recorded rather than resolved quietly, because four of them are design amendments and
that is not this repository's call to make alone.

| Finding | Resolution |
| :-- | :-- |
| **`unresolved` was never produced by any code.** `internal/offboarding` refuses retirement on an unresolved deprovisioning — SAD-004 §7.5, because a timeout is not proof the target did nothing — and nothing in the repository could ever set the state. The gate was correct, tested against a hand-written row, and unreachable in production | `Coordinator.SweepUnresolved` ages unanswered requests in **both** directions, since the timeout is a property of the correlation table rather than of one flow. The offboarding gate is now reachable by the mechanism that is supposed to reach it |
| **The design's API list names no provisioning-correlation route**, while requiring realized-status correlation and giving this service no inbound transport but HTTP | Four routes added, mirroring `POST /v1/offboardings/{id}/deprovisioning`, which already reports the other direction's outcome. All four are on the authenticated provider surface: a callback exempted for an external system's convenience would let anyone holding a correlation identifier declare a Tenant's boundary built, and activation reads exactly that statement |
| **The machine has no `requested -> failed` edge**, but a provisioning system can refuse before the dispatch was recorded | The refusal walks the declared path, `provision` then `fail`, which is safe precisely because both are silent and neither increments the security version. Adding the missing edge would have been the smaller diff and the larger change: the machine is asserted as one table, and an edge added for one caller is inherited by every other |
| **The shared `Payload` cannot carry a desired profile**, and `tenant.lifecycle.requested` is the desired-state publication — the event by which the external system learns what to build | `RequestedPayload` embeds `Payload` so the five common fields are unchanged for any consumer projecting Tenants, and adds the profile, the region, and the two correlation identifiers. Widening the shared payload instead would have made every lifecycle event carry an empty display name |
| **"Match by correlation identifier" does not say what happens when it matches two Tenants**, which it does whenever one call creates two | Refused as ambiguous rather than resolved by taking the most recent. Resolving the newest would silently mark the wrong Tenant's boundary as built |

**The design amendments this implies**, for 003: the four provisioning routes in §"API / Interface",
the `requested -> failed` question in §"Tenant State Machine", the desired-state payload in
§"Published Events", and the ambiguous-correlation rule in §"Provisioning Correlation".

### What wiring idempotency found

| Finding | Resolution |
| :-- | :-- |
| **The claim had nowhere correct to live in the obvious design.** A middleware can only claim in a transaction of its own, which commits separately — so a key held by a mutation that then rolled back refuses every retry of a request that never happened, and says "already in progress" while doing it | The claim travels in the context and is made by `internal/db` inside the scoped transaction every service already opens. The services never see it, which is what stops one being written without honouring it. Proven by moving the claim out and watching `TestAFailedMutationReleasesItsKey` fail with exactly that message |
| **`Complete` needs the HTTP response, which the service layer does not have.** The two halves of one guarantee sit in two layers | The claim is transactional and the completion is not. Documented as a window rather than hidden: a crash in between refuses later retries instead of replaying them, and the mutation still happened once. The alternative was a `Within` variant on thirty methods |
| **A replay is not byte-identical.** `platform.idempotency_key.response_body` is `jsonb`, so PostgreSQL sorts object keys and drops whitespace | Asserted semantically rather than by bytes, and stated in the README. Changing the column to `text` to make a byte comparison pass would trade the store's rejection of a malformed document for a guarantee no client should rely on |
| **The two conflicts were first mapped to `VersionConflict`**, whose title is "The record changed since it was read" — false for a reused key, and it points the caller at re-reading the resource, which is the wrong fix | foundation-platform already declares `IdempotencyKeyConflict` and `RequestInProgress`. Found by reading the response body in the by-hand walkthrough rather than by reading the mapping table |
| **One request can open two scoped transactions.** `internal/invitation` binds a tenant pool and a provider pool, and the second claim would read its own uncommitted first claim and refuse the request as in progress | The claim carries a consumption flag: the first transaction to reach it claims, the rest skip. Asserted by `TestOneRequestOpeningTwoScopesClaimsOnce` |

## Dead-letter resolution · closed 2026-09-24

✅ **Built and proven across real processes.** `TDD-organization-control-005` is the current statement of
the design. It covers:

- **Resolution:** `REPLAYED`, only on the active consumer's `consumer_applied` receipt. `SUPERSEDED`
  was added afterwards as backlog item 1, on the same receipt for a newer version of the Membership.
- **Provider API:** replay and resolve endpoints.
- **The `organization_resolution_rt` role:** it may update only the four resolution columns, through its
  own credential.
- **Two audit records:** the attempt is written before the transaction, the outcome inside it.

The closing evidence is foundation-reference CI run 36026176642, at organization-control `7aa69d7`,
foundation-reference `3fff642`, and foundation-platform `v0.2.7` (producer) / `v0.2.6` (consumer). The
review record is `RESPONSE-7` through `RESPONSE-26` in the architecture-description workspace.

Nothing here reopens without a concrete violation of that contract. An improvement goes in the backlog
below.

## Backlog after dead-letter resolution

Ordered. None of the open items blocks this service's production gate, but each has a named
failure it prevents. The Source column gives the review record that decided the item.

✅ is built. ☑ is closed by a recorded decision not to build, with the condition that reopens it.
Item 10 lives in the identity repositories, item 11 in foundation-platform, and items 15, 16 and 18
mostly in foundation-reference. They are listed here so the whole P1 backlog reads from one table.

Items 11 to 18 were recorded as P1 in the review record but were missing from this table until
2026-09-27, when every `RESPONSE-*` was swept for P1:

- items 14 to 18 are the P1 list of the Proof A rounds, RESPONSE-7 to RESPONSE-10;
- items 11 to 13 are findings recorded during P0, RESPONSE-15 to RESPONSE-19;
- `first_failed_at` taking `now()`, P1 in RESPONSE-16 and RESPONSE-17, is tracked in
  foundation-platform's ROADMAP.

| # | Item | Why | Source |
| :-- | :-- | :-- | :-- |
| 1 | ✅ **`SUPERSEDED` resolution** | Built. A dead letter closes as `SUPERSEDED` on the active consumer's `consumer_applied` receipt for a newer version of the same Membership, with versions read from `membership.membership_event` rather than stream positions, which a replay reassigns (TDD-005 §The resolution predicate). Without it, an overtaken event could never resolve, and estate-wide debt blocked every projection-backed check permanently | RESPONSE-23, RESPONSE-24 |
| 2 | ✅ **`grantcheck`** | Built. `tools/grantcheck` derives which role runs which statement from the code, and PostgreSQL judges each one as that role: a missing grant fails on every derived path, and a grant nothing needs is reported (TDD-001 §Grant Derivation). Runs in CI, with a mutation for each direction | RESPONSE-22 |
| 2a | ✅ Narrow the `grants.sql` schema loop | Done. The loop that gave both runtime roles DML on every owned table is gone; every schema is deny by default and each grant names a table a derived statement needs. The 52 unused grants are revoked, including `DELETE` on every business table and the tenant role's ability to create Tenants. `unused-baseline.txt` is empty (TDD-001 §Roles) | grantcheck |
| 3 | ☑ `RESNAPSHOTTED` resolution: **closed by decision, not built** | Generation replacement must be built in full or not at all, and its case now has an honest path: retire the consumer, rebuild it under a new identity from a fresh snapshot, and waive the old identity's incidents (TDD-005 §Rebuilding a consumer). Not built "just to complete an older checklist". **Reopen when** a consumer cannot afford the rebuild outage | RESPONSE-13, RESPONSE-18, RESPONSE-23 |
| 4 | ✅ `WAIVED` | Done. A waiver records who, why and until when (foundation-platform v0.2.9), never touches the closure, and never clears debt. It silences the stale alert until it expires and lets the payload be disposed. It is refused for the active consumer's incident and for unattributed ones, so it can only cover a retired consumer's incident with no corrective path (TDD-005 §WAIVED) | RESPONSE-11, RESPONSE-12, RESPONSE-18 |
| 5 | ✅ Per-consumer debt attribution | Done. The dispatcher records the refusing consumer (foundation-platform v0.2.8) and the frontier counts a consumer's own dead letters plus unattributed ones; a provider reads the estate. Consumer A's debt no longer refuses consumer B, and a retired consumer's debt stops blocking its replacement (TDD-005 §Scope). Lifting the single-consumer restriction still needs item 6 | RESPONSE-11, RESPONSE-23 |
| 6 | ✅ Multi-consumer delivery | Done. Reopened 2026-10-02: the Identity Control Service is a second projection consumer (ADR-ORG-002 §5.3). foundation-platform v0.3.0 and v0.3.1 deliver one outbox to several named consumers, each with its own delivery, receipt and dead letter (ADR-GLB-018). Registration declares `event_types` and is the subscription; a change or a revival clears the snapshot mark. Retiring abandons what the consumer was owed. `consumer_single_active` is dropped. Frontier, signals and debt are per consumer, and replay, resolve and waive address a dead letter by `(event_id, consumer)`. The post stage refuses any unattributed authority-bearing dead letter (TDD-002 1.6.0 §Consumer Registry, TDD-005 2.0.0) | RESPONSE-14, RESPONSE-15, RESPONSE-23, ADR-GLB-018 |
| 7 | ✅ `platform.delivery_receipt` retention | Done. foundation-platform v0.2.8 prunes a receipt past 90 days only while no incident is open and never one a closure cites; `organization-migrate -stage=maintenance` runs it daily with the outbox partitions and dead-letter disposal, none of which any deployable ran before (README §Building the database). It exits 3 on an incident open past 24 hours | RESPONSE-17, RESPONSE-20, RESPONSE-22 |
| 8 | ✅ Coverage floor for this repository | Done. CI fails below 65% over shipped packages, measured with the integration suites as the real roles (68.3% at introduction), mirroring foundation-platform's mechanism. It bounds what a green falsification run covers (README §Coverage floor) | RESPONSE-22 |
| 9 | ✅ Scheduled cross-repository compatibility runs | Done. Proof A now runs on both sides and against both mains: this repository's `system-proof` job proves every pull request against foundation-reference at a pin, and daily against its main; foundation-reference's `system-proof-main` proves this repository's main daily. Unpinned runs are logged as such and are never closure records; the pins stay (TDD-005 §Testing Strategy) | RESPONSE-24, RESPONSE-25 |
| 10 | ✅ Proof B: Keycloak drift | Done, in `identity-control` (its ROADMAP §Proof B, all seven steps). Drift between registered desired state and live Keycloak is detected, classified, reconciled and shown to converge, on every `deploy-dev` run against a real kernel. A lifespan changed in the console is repaired and attributed through admin events. A redirect URI changed in the console disables the client until an operator lifts it. A change under a drift exception stays until it expires. An unreachable Keycloak is `unresolved`. A Principal whose user is deleted is reported and relinked by an operator under the same `principal_id`. Decided 2026-09-28: desired state in the Control Database, redirect URIs blocked, a one-time script for the registration credential, and portability through an explicit `:relink`. Left outside it: confidential and workload registration, disabling unmanaged clients, and the rest of the Principal sweep. The cross-service test that Memberships survive a relink runs in this repository's `deploy-dev` from 2026-10-07 (`scripts/dev-wiring-proof.ps1` step 5). A Membership granted here to a Principal whose Keycloak user is then deleted and relinked keeps its `principal_id`, status and version here and in identity-control's Tenant context report. The new user joins the Tenant's Organization at the Tenant's next convergence, because a relink marks no Tenant | RESPONSE-4, RESPONSE-23 |
| 11 | ✅ The dispatcher leases rows instead of holding a transaction across delivery | Done in foundation-platform `v0.2.10` (migration `0008`, TDD-foundation-platform-001 §Dispatch). A short transaction leases the batch, publication happens outside any transaction, and each outcome commits on its own, fenced on the lease. The cost is that a crashed dispatcher's rows wait up to 30 s. This repository applies `0008`; the dispatcher is built in foundation-reference, which moved from `v0.2.6` to `v0.2.10` with it. That move also made the running dispatcher name the refusing consumer on a dead letter for the first time: item 5 had only been exercised by module tests. The system proof now asserts it | RESPONSE-15, RESPONSE-16, RESPONSE-17 |
| 12 | ✅ Legacy dead-letter recovery: refused at deploy | Decided 2026-09-28: refuse, with no sanctioned closure. A dead letter from before foundation-platform v0.2.3 may carry no `aggregate_id` or `priority`, so it cannot replay itself. One with no history row cannot be superseded. One naming no consumer, or naming the active one, cannot be waived. An authority-bearing row with all three gaps would block every consumer forever. `-stage=post` now lists such rows and fails. A sanctioned closure was rejected because it would declare authority delivered without evidence (TDD-005 §The superseded case) | RESPONSE-18, RESPONSE-19 |
| 13 | ✅ The dispatcher refuses an unregistered consumer name | Done. `DISPATCH_CONSUMER_NAME`, `REFERENCE_CONSUMER_NAME` and the registered `consumer_id` are set in three places, and a one-character mismatch produced receipts no resolution reads. Decided 2026-09-28: `organization_dispatch_rt` reads `consumer_id` and `retired_at` of `projection.consumer`, and foundation-reference's dispatcher refuses to start when its name is not an active registered consumer (`dispatch.CheckRegistered`). The bootstrap's name was already refused by this service when unregistered. The serving consumer's own name keys only its inbox and is not checked (TDD-005 §Configuration) | RESPONSE-18, TDD-005 §Configuration |
| 14 | ✅ Metrics and alerts for enforcement | Done. Decided 2026-09-28: metrics are exported over OTLP to the Collector (STD-GLB-003), and alert rules live in each repository as Prometheus rule files tested with `promtool`. No deployable exported anything before this, since foundation-platform started no exporter; v0.2.11 adds `observability.Export`. This service exports dispatcher lag per lane, security-debt depth and age, stale dead letters, and each consumer's report age against its budget and its fresh-check ratio (README §Metrics and alerts). foundation-reference exports its refusals by class and a fixed reason code, its intake outcomes, and its projection age against its budget. Each side alerts in its own rule files (here `observability/alerts`, moved from `deploy/alerts` 2026-10-07) at the thresholds SAD-004 §9.3.2 and the TDDs set | RESPONSE-7 to RESPONSE-10 |
| 15 | ✅ Failure injection as repeatable tests | Done. The dead consumer, the timeout before commit and backlog recovery, each observed once by hand during Proof A, are now phases 7 and 8 of foundation-reference's system proof, run on every build. The outage phase found a design defect on its first run. A standard-lane row was dead-lettered at its third attempt, so a two-second consumer restart dead-lettered every queued grant, and that security debt refused every projection-backed check until an operator intervened. Decided 2026-09-28: an outage never dead-letters, in either lane (foundation-platform v0.2.12, TDD-foundation-platform-001 §Dispatch) | RESPONSE-7 to RESPONSE-10 |
| 16 | ✅ Tenant state enforcement at the consumer | Done. foundation-reference applies Tenant events into `projection.tenant`, ordered by `tenant_security_version`, and refuses every member of a Tenant that is not active; the system proof suspends and restores a Tenant on every run. It also acknowledges, without a receipt, the named event types it does not apply, which it had been dead-lettering as poison. Here, Tenant events count as security debt, since a lost suspension withdraws every member, and `tenant.tenant_event` records each one's security version so a suspension overtaken by a restoration closes as `SUPERSEDED` (TDD-005 §The resolution predicate, TDD-003 §Tenant Event History) | RESPONSE-7 to RESPONSE-10 |
| 17 | ✅ A narrow database role for consumer callers | Done. A registered consumer ran as `organization_provider_rt`, so its credential could read and write every table the control plane can. Its seven routes now run on their own pool as `organization_consumer_rt`, under a consumer scope the provider and tenant pools refuse. The role holds `SELECT` on Memberships and Tenants under its own policies, `UPDATE` on four columns of its registry row, and read access to the outbox and dead letters for the frontier; grantcheck derives each grant. It needs its own credential, `ORGANIZATION_CONSUMER_DATABASE_URL`. Building it found a grantcheck defect: a boundary reached through an interface was walked under the caller's role, which the provider and resolution roles' grants had hidden (TDD-001 §Roles, §Grant Derivation) | RESPONSE-7 to RESPONSE-10 |
| 17a | ✅ A consumer token holds no provider authority | Fixed. `requireProvider` checked the scope alone, so a consumer token that added `X-Administrative-Reason` was admitted to every provider route: it could suspend a Tenant, retire an Organization, and close or waive a dead letter. The consumer's client credential was, in effect, the provider's. It now checks the caller's authority. `TestAConsumerIsRefusedOnEveryRouteThatIsNotItsOwn` reads every route from `routes.go` and requires `403` on all but the consumer's seven (TDD-001 §Scope Resolution) | found while starting item 17, 2026-09-27 |
| 18 | ✅ Latency distribution | Done. Phase 9 of foundation-reference's system proof revokes 40 principals and reports p50, p95, p99 and the maximum from each revocation's outbox commit to the consumer's inbox record, on one database clock. It then does the same for 20 more whose first delivery is refused. Either maximum over the 6 s that §Enforcement Budget gives commit-to-claim plus dispatch-to-applied fails the run. The first CI run measured smooth p50 32 ms, p99 99 ms, max 99 ms, and failing p50 260 ms, p99 320 ms, max 320 ms, on localhost. Accept to commit is not measured there. It is measured from 2026-10-07 in this repository's `deploy-dev` (`scripts/dev-wiring-proof.ps1` step 6): 20 revocations a Tenant administrator makes, timed from `accepted_at` to the response (an upper bound on accept to commit, budget 100 ms at p95), to `published_at`, and to identity-control's `consumer_applied` receipt (budget 10 s at the maximum), on the host clock the containers share. The first run (PR #61, `deploy-dev` run 37738457894, 2026-10-08) measured accept to response p50 2 ms, p95 2 ms, max 2 ms; accept to publication p50 807 ms, p95 817 ms, max 832 ms; accept to identity-control applied p50 807 ms, p95 818 ms, max 833 ms. All are inside budget. Publication is stamped after the applied acknowledgement, so the last two differ by the receipt write alone | RESPONSE-7 to RESPONSE-10 |
| 19 | ✅ Reconciliation repairs reach the consumer | Done. Reconciliation published a repair event whose findings carried only versions, so foundation-reference refused it as poison and every repair dead-lettered. Decided 2026-09-28: the finding carries the state it repairs. Each finding carries the authoritative Membership, the withdrawn one for an `extra` authority revoked, or null for one authority never granted (TDD-002 §Reconciliation). foundation-reference applies the sweep in one transaction by the higher-version-wins rule, and refuses a `missing` or `mismatch` finding with no state rather than read it as a removal. Phase 10 of its system proof deletes a served row and invents an active one, then asserts the sweep classifies both and the repair restores the first and removes the second | found while building item 16 |
| 20 | ✅ The access token type is checked | Done. STD-IAM-002 §3.5 step 5 has a verifier refuse a token whose header `typ` is not `at+jwt`, which is what keeps an ID token from passing as an access token. foundation-platform `v0.2.13` carries the check; `ORGANIZATION_TOKEN_TYPE` is `report` by default and `enforce` once the issuer's clients carry `at+jwt`. The dev issuer types its tokens `at+jwt` | scnehaux-architecture #24 |
| 21 | ✅ Provider and consumer authority from the standard's claims | Done. Decided 2026-10-01 (ADR-ORG-001 §5.11, STD-IAM-002 §3.1.1): a caller is read from the standard's claims and this service's own records, never from a role. A provider is a person's token without a Tenant, carrying `acr` and `auth_time`, whose `principal_id` holds a grant in `organization.provider_grant`; a consumer is a workload's token carrying `workload_owner`, whose `principal_id` an active `projection.consumer` row carries. Each is read for every request, so a revoked grant or a retired consumer stops at the next one. The grant is not projected into the kernel, because this service holds it. `organization-control bootstrap-provider` makes the first grant once, recording the operator and the reason. A consumer registers with its `principal_id`, which never changes. Every actor is the `principal_id`, never `sub`. `ORGANIZATION_TENANT_CLAIM`, `ORGANIZATION_PROVIDER_ROLE`, `ORGANIZATION_CONSUMER_ROLE` and `ORGANIZATION_CONSUMER_CLAIM` are gone, and startup refuses them (TDD-001 1.6.0 §Caller Authority, TDD-002 1.5.0) | found while building item 20, 2026-10-01 |
| 22 | ✅ Grant and revoke provider authority through the API | Done. `GET /v1/provider-grants`, `POST /v1/provider-grants` and `POST /v1/provider-grants/{grant_id}/revoke`, each a provider route with a reason, in the provider scope. A grant names the calling provider as `granted_by`; a revocation sets `revoked_at`, `revoked_by` and `revoke_reason`, the three columns the provider role may update, and the grant stays as the record. The authority read counts only active grants, so a revocation stops the next request. One active grant per Principal, by a partial unique index; a revoked Principal may be granted again. The last active grant cannot be revoked, under a lock, so two concurrent revocations leave one (TDD-001 1.7.0 §Caller Authority) | ADR-ORG-001 §5.11, 2026-10-01 |
| 23 | ✅ Eligible provider authority, activated with another provider's approval | Done for `provider:organization-control` (TDD-001 1.8.0 §Provider Activation). A grant is `eligible` or `emergency`, and the bootstrap grant is emergency. An eligible grant confers authority only while an approved activation lasts: at most `ORGANIZATION_PROVIDER_ACTIVATION_MAX` (8h), with a reason. In production another grant holder approves it, and a check constraint holds that in the database. An unapproved request lapses after 24 hours, and the holder or any provider ends an activation early. An eligible caller reaches only `/v1/provider-activations`. Every request an emergency grant authorizes logs WARN, and fewer than two emergency grants in production are reported at startup. Grants and activations for `provider:identity-control` are projected to the Identity Control API (ADR-ORG-002 §5.3, TDD-001 §Provider Authority Projection). An emergency grant is validated by its use: each request it authorizes is recorded, and one unused for 90 days is overdue on `GET /v1/provider-grants:emergency-validation` and logged at WARN by the daily maintenance stage (TDD-001 1.15.0 §Emergency Grant Validation). An eligible holder reads its own unrevoked grants, the `grant_id` it activates, at `GET /v1/provider-activations/grants`, as Entra lists a user's eligible roles under "My roles" (TDD-001 1.16.0, 2026-10-07). The activation screens are built in Organization Experience (SAD-012): provider mode requests and enters an activation, and the approval surface approves or denies others' requests (organization-experience ROADMAP Week 2 and 3) | ADR-ORG-002, 2026-10-02 |
| 24 | ✅ Publish provider authority for the Identity Control API | Done for the producer (TDD-001 1.10.0 §Provider Authority Projection). A grant names `provider:organization-control` or `provider:identity-control`, and the second publishes `provider.lifecycle.granted`, `provider.lifecycle.activated`, `provider.security.ended` and `provider.security.revoked`, each carrying the grant's whole state and a `grant_version`, with a history row for `SUPERSEDED`. Withdrawals take the priority lane, and all four are authority debt. This service's own scope is never published (NIST SP 800-53 AC-6). `POST /v1/projections/provider-authority/snapshot` serves a consumer subscribed to the provider types, and each snapshot answers only its subscribers. The last organization-control grant is still kept. The consumer side is identity-control's | ADR-ORG-002 §5.3 |
| 25 | ✅ Deliver to consumers from this process, as this service's workload | Done (TDD-005 2.2.0 §Technical Context, ADR-GLB-018 §5.4). Each consumer in `ORGANIZATION_DELIVERY_TARGETS` gets a dispatcher here, on the dispatch role's own pool. It uses foundation-platform v0.4.0's `outbox/httpdelivery` and a `clientauth` workload token, so no consumer holds this database's credential and no shared secret authenticates a delivery (STD-IAM-001 §3). A target waits for its consumer's registration. foundation-reference's dispatcher stays where it is until it moves here | ADR-GLB-018 §5.4 |
| 26 | ✅ Run on the development server | `deploy/dev` and a root `Dockerfile`, the way every service on the server is deployed (STD-GLB-009 §Development Server Deployment): its own Postgres in a named volume, a one-shot migration job, the service with its dispatcher, and `bootstrap-provider` and `maintenance` as profiled tasks on the migrate image. It joins the kernel's network for Keycloak and identity-control's for delivery and the consumer routes. The runtime login roles moved to `scripts/login-roles.sql`, shared with CI's fixture. The wiring runs as `scripts/dev-wire.ps1`, and `deploy-dev` stands the kernel's, identity-control's and this stack up on every change and nightly, wires them with it, and asserts README step 7 with `scripts/dev-wiring-proof.ps1`: the grant reaches identity-control's projection, the ceremony's grant is retired, both services record the emergency grants' use, and an Organization outage leaves the emergency grant honored. Writing it as a script found the procedure's audience change aimed at `identity-control-caller`, which has no registration; the operator's caller of both APIs is now a registered `dev-provider-caller`. From 2026-10-07 it also proves that a Membership granted here reaches identity-control (step 4). A provider makes a Tenant and its first administrator (item 27). The administrator signs in for the Tenant at `aal2` through a `tenant-scoped` privileged client and grants a Membership with that token. The Membership appears in identity-control's Tenant context report and in the kernel's Organization for the Tenant (ADR-IAM-006). Steps 5 and 6 are items 10 and 18 | STD-GLB-009, TDD-identity-control-006 §Operational Notes |
| 27 | ✅ A Tenant administrator is a recorded grant | Done (TDD-001 1.14.0 §The Tenant Administration Grant). Since ADR-IAM-006 the kernel issues a `tenant_id` to every member who signs in for a Tenant, and this service admitted any token carrying one as that Tenant's administrator. A tenant token is now admitted only for a person at `aal2` or higher with `auth_time`, whose `principal_id` holds, in that Tenant, an active Membership, an active Tenant and a grant in `membership.tenant_admin_grant`, read for every request under the Tenant's own policy. A provider grants, lists and revokes at `/v1/tenants/{tenant_id}/administrators`; a first administrator gets a Tenant-wide Membership in the same transaction. The writes run through `db.WithProviderInTenant`, on the tenant pool bound to the Tenant, and two restrictive policies refuse a grant or revocation from any other transaction. Building it found the offboarding freeze route unusable: it reached the tenant pool with a provider scope, which is refused. It now runs through the same function | ADR-ORG-003 |
| 28 | ✅ The read side Tenant and provider administration needs | Done (TDD-002 1.9.0, TDD-003 1.6.0, TDD-004 1.6.0, 2026-10-07). Organization Experience's Week 3 screens (TDD-organization-experience-002: the Organization registry, Tenant lifecycle, Workspaces, Memberships and invitations) list what they administer, and this service served each record only by its identifier, with no Membership read at all. Six reads now: `GET /v1/workspaces`, `GET /v1/memberships`, `GET /v1/memberships/{membership_id}` and `GET /v1/invitations` for a Tenant administrator, the Tenant from the token and Row-Level Security confining the read; `GET /v1/organizations` and `GET /v1/tenants` for a provider, each page recording the access with the caller's reason. Every list takes the STD-GLB-001 1.3.0 §Pagination form identity-control serves: `after` and `limit` (50, at most 100, anything else `400`), keyset order on the UUIDv7 key, filters from a closed set, and `{"<items>": [...], "next": id \| null}`. The same change closes a gap with TDD-002 §API, which requires a version and a reason on every mutation: **a Membership suspend, restore or revoke now requires `{"expected_version": n}`** and answers `409` on a stale one, and a revocation requires `X-Administrative-Reason` from a tenant caller too. The actor, correlation and reason are recorded on the transition's `membership.membership_event` row, which had no column for them. foundation-reference's system proof sends `expected_version` on each revocation, and `systemproof/foundation-reference.rev` has moved past that revision; it is `4a93d9d` today | TDD-organization-experience-002, TDD-002 §API |
| 29 | ✅ The offboarding read side | Done (TDD-003 1.7.0, TDD-004 1.7.0, 2026-10-07). Organization Experience's Week 4 (TDD-organization-experience-003) presents offboarding as staged and resumable, with an obligation board and an API-computed count on the begin screen, and this service could not serve any of it: no list of offboardings and no path from a Tenant to its own, an obligations read of `"domain/type (state)"` strings with no identifiers, no entry instant for `release`, the deprovisioning state readable only as a retirement refusal, and no count of a Tenant's active Memberships anywhere. Now: `GET /v1/offboardings?after=&limit=&stage=&tenant_id=` in the STD-GLB-001 1.3.0 §Pagination form, each page recording the access; the offboarding view adds `obligations_at` (served from `frozen_at`, one transaction and one instant), `released_at` (a new column, stamped on release), `deprovisioning` (the latest command and its outcome) and `active_memberships`; `GET /v1/offboardings/{id}/obligations` keeps `outstanding` and adds `obligations`, every row with `resolved_by` and `resolved_at` (new columns, recorded by `POST /v1/obligations/{id}/resolve`), overdue open rows first; and `GET /v1/tenants/{id}` adds `offboarding_id` and `active_memberships`. Additive: every field and route a client already reads is unchanged | TDD-organization-experience-003, TDD-organization-experience-001 §Irreversible Operations |
| 30 | ✅ Membership batches, and revocation shown by its evidence | Done (TDD-002 1.10.0, ADR-ORG-004, 2026-10-07). `SAD-004 §8.3` requires bulk operations to validate each item independently and report a per-item outcome, and `TDD-organization-experience-001` asks for a preview, a binding execution, outcomes of succeeded, failed or not attempted, and a revocation presented as accepted, propagating, enforced or over budget. None of it was served. Now: `POST /v1/membership-batches` previews up to 500 suspend, restore or revoke items through the single command's own checks and stores the batch for 15 minutes; `POST /v1/membership-batches/{id}/execute` runs each item in its own transaction held to the version the preview read, with SCIM's `fail_on_errors` and an `Idempotency-Key`; `GET /v1/membership-batches/{id}` reads it. Every transition response names its `event_id`, and `GET /v1/memberships/{id}/enforcement` derives the state from the deliveries, receipts and dead letters of the latest transition, against the 10 s propagation budget. The tenant role gains column-level `SELECT` on `membership.membership_event` and on three platform tables for it. Since closed (item 35, TDD-002 1.13.0): expired previews are purged by the maintenance stage, a batch left `executing` by a request that ended is resumed, and more than 500 items answers `413` naming the limit, through foundation-platform v0.4.1's `PayloadTooLarge` | ADR-ORG-004, SAD-004 §8.3, TDD-organization-experience-001 |
| 31 | ✅ Projection health and the provisioning request, readable | Done (TDD-002 1.11.0, TDD-003 1.8.0, 2026-10-07). `TDD-organization-experience-002` 1.2.0 left two surfaces waiting on this service: no route listed the projection consumers, and `unresolved`, a state of the provisioning request rather than of the Tenant, was not on the Tenant read, so the console could neither disable retry on it nor show a failure's reason. Now: `GET /v1/projections/consumers?after=&limit=&state=active\|retired`, provider-only, in the STD-GLB-001 1.3.0 §Pagination form keyed on `consumer_id`, each page recording the access, each item the single read's shape plus `state`, `retired_at` and a `stale` computed in the read; and `GET /v1/tenants/{id}` adds `provisioning`, the latest provisioning request `{request_id, correlation_id, state, detail, requested_at, resolved_at}` or null, read in the record's transaction. No migration and no new grant: both read tables the provider role already reads. The reconciliation age, not served then, is item 36 | TDD-organization-experience-002 §Projection Health, §Tenant States Are Rendered Individually |
| 32 | ✅ A person lists their own contexts | Done (TDD-001 1.17.0, TDD-002 1.12.0, ADR-ORG-005, 2026-10-07). TDD-002 named `GET /v1/principals/{principal_id}/contexts` and it was never served, and a person signed in without a Tenant and holding no provider grant reached nothing, so Organization Experience asked for a Tenant identifier typed by hand. Now a fourth caller class, the self caller: a human token whose `principal_id` is the path's, admitted to that one route with or without `tenant_id` or a grant. It reads one item per active Membership in an active Tenant, `{membership_id, tenant_id, tenant_display_name, tenant_status, workspace_id, administers}`, in the STD-GLB-001 1.3.0 §Pagination form, as `organization_self_rt`: a role with `SELECT` on named columns of three tables under policies keyed on `app.principal_id`, reached by `SET LOCAL ROLE` from the tenant connections (granted `WITH INHERIT FALSE, SET TRUE`), so no new credential exists. A self read records no privileged access; a provider reads anyone's with a reason, recorded | ADR-ORG-005 |
| 33 | ✅ A mistaken offboarding is cancelled before release | Done (TDD-003 1.9.0, TDD-004 1.8.0, ADR-ORG-006, 2026-10-07). The design called the freeze reversible and nothing reversed it. Now `POST /v1/offboardings/{id}/cancel {"expected_version"}` with a reason, in `freeze` or `obligations` only, ends the offboarding in a terminal `cancelled` stage with who, why and when; returns the Tenant to the status recorded when the offboarding began (`offboarding -> active` publishing `tenant.security.restored`, `offboarding -> suspended` publishing `tenant.security.suspended`, both incrementing the security version, both types every consumer already applies); restores exactly the Memberships the freeze recorded in `membership.offboarding_freeze`, each through the ordinary restore transition after the Tenant's commits; and closes open obligations as `cancelled`. An offboarding begun before the migration has no record and its cancellation is refused. Organization Experience's control for it is built (TDD-organization-experience-003, `apps/admin/src/features/offboarding.tsx`) | ADR-ORG-006 |
| 34 | ✅ The `Idempotency-Key` is required on commands | Done (TDD-003 1.10.0, TDD-001 1.18.0, TDD-002 1.13.0, TDD-004 1.9.0, TDD-005 2.3.0, 2026-10-07). TDD-003 and TDD-002 said every mutation requires the key; it was honoured and optional, so a command retried after a lost response ran twice. Now every command (42 routes: Memberships and batches, Workspaces, invitations, Organizations, Tenants and provisioning dispatch, offboardings and obligations, provider grants and activations, Tenant administrators, consumer registration and retirement) is refused `400` without one, or with a blank one, after the caller's authority and before anything is read. Sixteen `POST` routes honour a key without requiring it, each with its reason in `internal/httpapi/commands.go`: reads in a body, the consumer's protocol reports, the provisioning system's correlated reports, sweeps, reconcile, and the dead-letter acts TDD-005 keeps optional. A test fails on a `POST` classified neither way. The consumer that sent no key on a command changed in lockstep: foundation-reference's system proof sends an `Idempotency-Key` on every `POST`, at the pin `4a93d9d` | draft-ietf-httpapi-idempotency-key-header-07 §2.7 |
| 35 | ✅ Membership batches: purge, resume, and `413` | Done (TDD-002 1.13.0, 2026-10-07). The maintenance stage purges previews expired more than a day ago, as `organization_migrator` through two policies that admit expired previews and no write but the delete. An execution is leased: `lease_id` and `heartbeat_at`, renewed in every item's transaction, which is fenced on the lease. An execute on an `executing` batch whose heartbeat is older than 30 s (longer than any request may live; startup refuses an `HTTP_REQUEST_TIMEOUT` at or above it) takes the lease over and finishes the items with no outcome, each held to its previewed version; one with an outcome is never applied again. The same `Idempotency-Key` adopts its uncompleted claim. More than 500 items is `413` naming the limit (foundation-platform v0.4.1, ADR-ORG-004 §5.1). v0.4.1 also ships platform migration `0010_outbox_delivery_event.sql`, an index on `platform.outbox_delivery(event_id)`, which `organization-migrate -stage=post` applies with the rest of the embedded set | ADR-ORG-004, RFC 7644 §3.7.4 |
| 36 | ✅ Reconciliation age per consumer | Done (TDD-002 1.13.0, 2026-10-07). Every reconciliation run, clean ones included, records `last_reconciled_at`, `_mark` and `_findings` on the consumer, in the transaction that publishes the repair, and the consumer read and list serve them with `reconciliation_age_seconds`. Additive columns, no new grant | TDD-organization-experience-002 §Projection Health |
| 37 | ✅ The isolation posture checked at startup and behind readiness | Done (TDD-001 1.19.0 §Verifying the Posture at Runtime, 2026-10-07). `AssertIsolation` moved to `internal/posture`, a package that reads the catalog and holds no stage SQL, so the serving binary does not import `internal/controldb`. `cmd/organization-control` runs it as the tenant login role before it binds a port and refuses to start on a problem, and `GET /readyz` runs it on every probe, so a replica whose database loses a policy between deploys leaves the load balancer. `TestReadinessAndStartupRefuseAWeakenedDatabase` drops `FORCE` from one table as the service's own role watches; `TestTheRuntimeRoleSeesTheWholePosture` shows that role sees every protected table. grantcheck plans the catalog reads as `organization_rt` | ROADMAP §Debt, `internal/controldb/assert.go` |
| 38 | ✅ The response is recorded with the effect | Done (TDD-003 1.11.0 §The Response Is Recorded with the Effect, 2026-10-07). The window the HTTP surface left between a command's commit and its `Idempotency-Key` completion is closed for every command whose effect commits in one transaction: the service hands its result to `db.Respond`, which renders it with the handler's renderer and completes the claim in that transaction. The provider role gains `UPDATE` on the three completion columns of `platform.idempotency_key` and nothing else. `TestAResponseRecordedWithTheEffectSurvivesACrashBeforeTheReply` commits, skips everything after the commit, and shows the retry replayed; before, the same sequence was refused as in progress. `TestEveryCommandRecordsItsResponseWithItsEffect` holds each new command route to it or to a named reason. Request validation was audited in the same change | draft-ietf-httpapi-idempotency-key-header-07 §2.6, brandur.org/idempotency-keys |
| 39 | ✅ The production gate's five runbooks | Written (`docs/runbooks/`, 2026-10-07): revocation not enforced within budget, projection drift repair, provider-access review, stuck offboarding, and dead-letter resolution. Each is grounded in the routes, alerts and tables it names, and lists the gaps it found, which stay open: an `extra` reconciliation finding is not alerted, accept to enforcement has no alert of its own, TDD-004's offboarding signals are not exported, a provider cannot read the enforcement route, no route lists dead letters or reads `audit.privileged_access`, and a failed deprovisioning cannot be sent again. The other runbooks the TDDs require are not written yet | STD-GLB-004 §3.15, Google SRE Workbook "On-Call", NIST SP 800-61r3 §2.3 |

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

**Design gate. Met.** All five designs at `1.0.0` or later, no design contradicting another or the
implementation, and `approved_version_not_stable` now refuses a regression in CI.

**Production gate.** The design gate, plus: restore evidence for the Organization
Database including outbox and projection cursor state, cross-tenant denial proven as
the runtime role, measured accept-to-publication delay inside budget for priority
events (measured on every `deploy-dev` run from 2026-10-07; the first run read a maximum of 833 ms
from acceptance to identity-control's applied receipt, against 10 s; backlog item 18), `SUPERSEDED`
resolution built (backlog item 1, done), and runbooks written for revocation not enforced within
budget, projection drift repair, provider-access review, stuck offboarding, and dead-letter
resolution (written 2026-10-07, `docs/runbooks/`, backlog item 39). No restore evidence for the
Organization Database is recorded here yet.

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
at deploy time. `posture.AssertIsolation` (moved from `internal/controldb` on 2026-10-07) closes it:
`-stage=post` calls it as a post-condition, and `cmd/organization-control` calls it at startup and
behind `GET /readyz` (TDD-organization-control-001 1.19.0 §Verifying the Posture at Runtime). Six
weakenings are tested and each is detected, and every one of them leaves the schema matching its
declared state — which is why a schema tool would not have caught them.

### Resolving both

An Atlas Pro login with a CI token supplies the destructive analyzer and RLS in HCL. The
alternative for the first item alone is an amendment to `ADR-GLB-004` naming a different
mechanism; there is no alternative for the second short of writing a policy differ, which is not
work this repository should own.

Until then, neither is presented as satisfied. `ADR-GLB-004`'s destructive gate has a waiver in
effect by substitution, and that is the honest description.

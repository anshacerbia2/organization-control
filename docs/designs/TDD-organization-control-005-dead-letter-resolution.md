---
doc_meta:
  id: TDD-organization-control-005
  title: Dead-Letter Resolution, Scope and Limits
  owner: Core Platform Team
  version: 2.4.0
  status: approved
  classification: restricted
  review_cycle_days: 90
  created_date: 2026-09-17
  last_reviewed: 2026-10-07
  parent_sad: SAD-004
---

# Dead-Letter Resolution, Scope and Limits

## Purpose

Record what may close an authority-bearing dead letter, how the closure is built, and what this
scope still cannot recover from.

An authority-bearing dead letter is a Membership or Tenant event the dispatcher abandoned
after the consumer refused it permanently. Tenant events count because the authority refuses
every member of a Tenant that is not active, so a dead-lettered Tenant suspension withdraws
every member at once. Provider grant events count too: a dead-lettered revocation of a
`provider:identity-control` grant is provider authority the Identity Control API still honors.
`projection.AuthorityEventTypes` lists the twelve types. While one is unresolved, the publication
frontier reports security debt and every projection-backed enforcement check refuses. So
resolving one is the act that returns service, and resolving one wrongly is the act that
returns service to a consumer holding a revocation it never received.

The scope described here is built. `REPLAYED` was proven across real processes at the revisions
recorded under **Testing Strategy**. `SUPERSEDED` is proven by this repository's integration
suite against the real roles.

## Scope

Two resolution reasons are in scope:

```
REPLAYED      the dead letter's consumer applied this event
SUPERSEDED    the dead letter's consumer applied a newer event for the same Membership or Tenant
```

A waiver is also in scope, and it is not a third resolution reason:

```
WAIVED        known, accepted, until a date: silences the alert, never clears the debt
```

A waiver records an operational exception, in foundation-platform v0.2.9's four waiver columns.
It never touches the resolution columns, so it cannot make an incident look delivered, and the
frontier keeps counting the incident as debt. See **WAIVED**.

One reason is out, by decision rather than by omission:

| Reason | Why it is out |
| :-- | :-- |
| `RESNAPSHOTTED` | Requires generation replacement in full: a generation built empty, dual-apply during the rebuild, atomic promotion, generation-scoped lookup, and a race proof. Implementing part is worse than none, because a promotion that reads do not respect is a pointer swap, not a replacement. Its case is covered instead by rebuilding the consumer under a new identity (§Rebuilding a consumer), the "honest outage" RESPONSE-13 named as the alternative. It is not built "just to complete an older checklist" (RESPONSE-18). |

**Reopen when** a consumer cannot afford the outage a rebuild costs. That is the one thing
generation replacement buys over a rebuild: serving from the old generation while the new one
builds.

`SUPERSEDED` rests on the same evidence as `REPLAYED`: a `consumer_applied` receipt from the
consumer whose delivery the dead letter records. What differs is which event the receipt is for. The domain proof that lets a
newer event close the failed one's effect is in **Algorithms / Logic**.

**A dead letter is one consumer's** (`ADR-GLB-018 §5.3`). Since foundation-platform v0.3.0
each event owes each subscribed consumer a delivery of its own, and a dead letter is keyed
`(event_id, consumer)`. Several consumers are active at once, and every act below names the
incident by both halves of that key: one consumer's refusal is replayed to that consumer, closed
on that consumer's evidence, or waived for that consumer, and no other consumer's incident for the
same event is touched. Up to v1.6.0 this design scoped enforcement to one projection consumer, and
`projection.consumer` refused a second active registration. That limit existed because the outbox
could record one outcome per event, and it is gone with the per-consumer delivery that ended it.

Security debt is reported per consumer. The dispatcher records which consumer refused each
event in `platform.dead_letter.consumer` (foundation-platform v0.2.8). When a consumer reads the
frontier, it counts only its own dead letters plus those that name no consumer. A provider reads
the whole estate. Rows from before v0.2.8 carry `NULL` and are counted for everyone, because
reading `NULL` as nobody's would clear every incident that predates the column. So one
consumer's poison event no longer refuses another consumer's traffic. A consumer retired with
debt outstanding also stops blocking the one registered after it, which bootstraps from a
snapshot taken after the event.

## Technical Context

Membership security events reach the consumer through `platform.outbox` and a dispatcher that
delivers over HTTP: the Direct Durable Delivery profile of ADR-GLB-016. One dispatcher runs per
consumer and claims that consumer's deliveries alone (`ADR-GLB-018 §5.4`).

**This service runs its consumers' dispatchers** (`ADR-GLB-018 §5.4`, Organization Control SAD
§4.5). Each consumer named in `ORGANIZATION_DELIVERY_TARGETS` gets one dispatcher in this process.
It runs on the dispatch role's own credential, posts to that consumer's acceptance API with
foundation-platform's `outbox/httpdelivery`, and authenticates as this service's workload with an
access token from `clientauth`. No consumer holds a credential to this database, and no shared
secret authenticates the delivery (`STD-IAM-001 §3`).

- **A target waits for its registration.** A dispatcher starts only once its consumer is an active
  registered consumer, which it checks on the dispatch role. Until then, and after any failure of
  the dispatcher itself, it logs the reason and checks again after
  `ORGANIZATION_DELIVERY_RETRY_INTERVAL`. A consumer is often registered after this service is
  deployed, and a misconfigured target must not stop the control plane from serving.
- **The outbound calls are these two and no others.** The consumers' acceptance APIs, and the
  kernel's token endpoint for the workload token. The `net/http` denial in `arch.json` and
  `internal/httpapi`'s outbound walk still hold for every package of this repository: the clients
  are foundation-platform's, built in `internal/delivery`.
- **foundation-reference's dispatcher** still runs in `foundation-reference`, for the reference
  consumer and its system proof, until it moves here (`ADR-GLB-018` Alternative G).

Security events travel in the priority lane. The dispatcher dead-letters one only when the
consumer refuses it permanently (`400`, `409`, `422`), which makes it poison. An unreachable
consumer never dead-letters one: after three attempts the row is released back for delivery.
A dead-lettered delivery is copied to `platform.dead_letter` under its consumer's name and marked
published, so redelivery to that consumer stops. Other consumers' deliveries of the same event are
unaffected.

The consumer applies an event and its inbox guard in one transaction, and only then
acknowledges. Acknowledgement therefore means applied. That is a property of this transport,
not of the type, and it ends the moment a broker sits between the two.

`platform` is versioned by foundation-platform (v0.3.1) on its own release cadence. This
repository applies that schema and owns the grants on it, which is why the privilege posture in
**Data Model** is stated here rather than upstream.

## Component Design

| Component | Responsibility |
| :-- | :-- |
| `outbox.Dispatcher` (foundation-platform) | Claims, delivers, decides retry, release, or dead-letter, and writes the delivery receipt. Verifies its database contract before starting any worker and refuses to start when it is unmet. |
| `internal/delivery` | Runs one dispatcher per configured consumer in this process, once that consumer is registered, with `outbox/httpdelivery` as its publisher and a `clientauth` workload token as its credential. |
| `dispatch.HTTPPublisher` (foundation-reference) | Delivers one envelope. Classifies `400`/`409`/`422` as poison and everything else as unavailable. Takes the evidence class from the consumer's reply rather than from its own judgement. |
| Delivery intake (foundation-reference) | Applies the event, then emits the application receipt on exactly the paths where the assertion holds. |
| `projection.FrontierReader` | Reports publication facts and unresolved security debt. Computes no verdict. |
| `projection.Registry` | Registers projection consumers with their subscriptions, and retires them. Retiring abandons the deliveries a consumer was still owed (`ADR-GLB-018 §5.5`). |
| `projection.Replayer` | Re-appends a dead letter to the outbox under its original `event_id` and priority, from the row itself, owed to the dead letter's consumer alone (`outbox.To`). Leaves the dead letter untouched. |
| `projection.Resolver` | Closes a dead letter as `REPLAYED` on its consumer's `consumer_applied` receipt for the event, or as `SUPERSEDED` on that consumer's `consumer_applied` receipt for a newer version of the same Membership or Tenant. Nothing else. |
| `membership.Service` | Writes `membership.membership_event` beside every Membership event it publishes, in the publishing transaction. |
| `tenant.Service` | Writes `tenant.tenant_event` beside every Tenant event it publishes, in the publishing transaction. |
| `db.ResolutionPool` | The only pool that can write the resolution columns. It connects as `organization_resolution_rt` through its own credential. |

## Data Model

`platform.dead_letter` holds the incident. It retains `aggregate_id` and `priority` alongside
the envelope, so a row can be re-appended from itself. Without them, a replay depended on the
originating outbox partition, which retention eventually removes: the incident record would
outlive the ability to act on it.

A closure is four columns, all set or all null, which foundation-platform's
`dead_letter_resolution_complete` constraint enforces:

```
resolved_at             when
resolution_type         REPLAYED | SUPERSEDED
resolved_by             the operator's principal
resolution_reference    platform.delivery_receipt:<event_id>:<consumer>
```

For `REPLAYED` the reference names the failed event's own receipt. For `SUPERSEDED` it names the
receipt of the newer event that made the failed one moot.

`membership.membership_event` records which Membership, at which version, each published event
concerns:

```
event_id             uuid, primary key       the published event
membership_id        uuid, FK membership     the aggregate
tenant_id            uuid                    RLS discriminator
membership_version   bigint                  the version the event carries
event_type           text
recorded_at          timestamptz
UNIQUE (membership_id, membership_version)
```

It is written in the transaction that publishes the event, so the row exists if and only if the
event does. The version is the event's own and is fixed at publication. The stream position is
not: a replay reassigns it, which is why the `SUPERSEDED` predicate reads this table and never
positions. There is no backfill. An event published before the table existed has no row and
cannot be closed as `SUPERSEDED`. Nothing was in production when the table arrived.

`tenant.tenant_event` does the same for Tenant events, keyed on the Tenant's security version:

```
event_id                  uuid, primary key       the published event
tenant_id                 uuid, FK tenant         the aggregate and the RLS discriminator
tenant_security_version   bigint                  the version the event carries
event_type                text
recorded_at               timestamptz
UNIQUE (tenant_id, tenant_security_version)
```

Every published Tenant event increments `tenant_security_version`, so the version identifies the
event within its Tenant. The same rules hold: written in the publishing transaction, fixed at
publication, and no backfill.

`organization.provider_grant_event` does the same for provider grant events, keyed on the grant's
`grant_version` (`TDD-organization-control-001` §Provider Authority Projection):

```
event_id        uuid, primary key                  the published event
grant_id        uuid, FK provider_grant            the aggregate
grant_version   bigint                             the version the event carries
event_type      text
recorded_at     timestamptz
UNIQUE (grant_id, grant_version)
```

Every published transition of a grant or its activation increments `grant_version`, and each event
carries the grant's whole state, so a newer applied version supersedes an older one by the same
proof. A revocation is terminal: nothing supersedes it but itself.

`platform.delivery_receipt` is keyed `(event_id, consumer)` and carries an `evidence` column
constrained to `consumer_applied` or `transport_accepted`. It is the root of trust for
resolution: a row asserting that this event reached this consumer. The first receipt wins
(`ON CONFLICT DO NOTHING`), so a later replay cannot weaken it.

Privileges, derived from execution paths rather than convention:

```
platform.delivery_receipt   organization_dispatch_rt     INSERT, SELECT
                            organization_resolution_rt   SELECT
                            organization_rt              none
                            organization_provider_rt     none
                            no runtime role              UPDATE, DELETE
                            organization_migrator        DELETE, only through retention (below)

platform.outbox             organization_dispatch_rt     SELECT   (the envelope; publication state is the delivery's)

platform.dead_letter        organization_dispatch_rt     INSERT, SELECT
                            organization_provider_rt     SELECT   (frontier debt facts, replay source)
                            organization_resolution_rt   SELECT, and UPDATE on the four resolution columns
                                                         and the four waiver columns only
                            organization_rt              none

organization.provider_grant_event
                            organization_provider_rt     INSERT   (written when publishing)
                            organization_resolution_rt   SELECT   (the SUPERSEDED predicate)
                            no runtime role              UPDATE, DELETE, TRUNCATE

tenant.tenant_event         organization_provider_rt     INSERT   (written when publishing)
                            organization_resolution_rt   SELECT   (the SUPERSEDED predicate)
                            organization_rt              none
                            no runtime role              UPDATE, DELETE, TRUNCATE

membership.membership_event organization_rt              INSERT   (written when publishing)
                            organization_resolution_rt   SELECT   (the SUPERSEDED predicate)
                            organization_provider_rt     none
                            no runtime role              UPDATE, DELETE, TRUNCATE

platform.outbox_delivery    organization_dispatch_rt     SELECT, UPDATE   (its consumer's claim and outcome)
                            organization_rt              INSERT   (outbox.Append)
                            organization_provider_rt     INSERT   (outbox.Append), SELECT (frontier, signals),
                                                         UPDATE on the columns outbox.Abandon writes (retirement)
                            organization_consumer_rt     SELECT   (its own frontier)

platform.subscription       organization_provider_rt     SELECT, INSERT, UPDATE (retired_at)
                                                         (registration and retirement)
                            organization_rt              SELECT (consumer, event_types, retired_at)
                                                         (outbox.Append's fan-out)
                            organization_consumer_rt     SELECT (consumer, event_types, retired_at)

projection.consumer         organization_resolution_rt   SELECT   (whether the dead letter's consumer is active, for a waiver)
                            organization_dispatch_rt     SELECT (consumer_id, retired_at) only
                                                         (the dispatcher's startup check on its own name)
audit.privileged_access     organization_resolution_rt   INSERT   (the outcome record)
platform.idempotency_key    organization_resolution_rt   INSERT, SELECT   (the claim of a keyed /resolve)
```

**Retention.** `organization-migrate -stage=maintenance` runs daily as the migration role. Among
its other steps it deletes receipts older than 90 days through foundation-platform's
`PruneDeliveryReceipts`. That function deletes nothing while any dead letter is unresolved,
because the receipt that closes an incident may be for a different event, as with `SUPERSEDED`.
It also never deletes a receipt that a closure's `resolution_reference` cites, because the
closure record is permanent. The resolver builds that reference with `outbox.ReceiptReference`,
which is exactly the form pruning recognises. The same stage removes payloads of dead letters
resolved more than 90 days ago, and exits 3 while an incident has been open longer than 24 hours.

`SELECT` for the dispatcher is not for reading incidents. `ON CONFLICT` with a conflict target
makes PostgreSQL require it on the table being inserted into. This was measured per table rather
than inferred, because inferring it across tables is how a preflight shipped asserting `INSERT`
alone.

The resolution role can close an incident and can do nothing else to it. It cannot change
`failure_class` or the envelope, cannot insert or delete a dead letter, cannot write a receipt,
and cannot write Membership or Tenant history. It cannot manufacture the evidence it closes on, and it
cannot rewrite the incident it closes.

`membership.membership_event` is under the same Row-Level Security as every table in the
`membership` schema: the tenant-scope and provider-scope policies, plus one declared extra,
`membership_event_resolution_read`. That is a `SELECT` policy for the resolution role, keyed on
the provider binding. `tenant.tenant_event` carries the same arrangement with
`tenant_event_resolution_read`. `controldb.AssertIsolation` checks policies by name and accepts an extra
one only when it is declared in `AdditionalPolicies`.

The request path holds nothing on either table. A caller able to insert a `consumer_applied`
receipt could forge the proof that closes a security debt, which makes forging the evidence and
forging the resolution the same act. New tables in the `platform` schema arrive closed: the
schema's default privileges grant nothing, and each table is named explicitly with the path
that earns it.

## API / Interface

Two provider-only operations. Each requires an `X-Administrative-Reason`.

```
POST /v1/dead-letters/{event_id}/consumers/{consumer}/replay    202  {event_id, consumer, event_type, position, resolved: false}
POST /v1/dead-letters/{event_id}/consumers/{consumer}/resolve   200  {event_id, consumer, resolution_type, resolution_reference}
POST /v1/dead-letters/{event_id}/consumers/{consumer}/waive     200  {event_id, consumer, waived_until, waiver_reason, resolved: false}
```

The path is the dead letter's key. An event dead-lettered at two consumers is two incidents, and
an operator acting on one acts on it alone. A request naming a consumer with no dead letter for the
event is `404`, and its message lists the consumers that do have one, so a mistyped name reads as
what it is.

A dead letter that names no consumer has no address here. It predates foundation-platform v0.2.8,
when the dispatcher began recording the consumer, and no code path writes one now. An
authority-bearing one would refuse every consumer and could be closed by none, so the post stage
refuses to deploy while one is unresolved (§The superseded case).

`resolve` takes an optional body naming the reason:

```
(no body)                             REPLAYED
{"resolution_type": "REPLAYED"}       REPLAYED
{"resolution_type": "SUPERSEDED"}     SUPERSEDED
anything else                         400
```

The operator chooses the incident and the reason. The incident fixes whose evidence counts: it is
the dead letter's own consumer, so a caller cannot close one consumer's incident with another
consumer's receipt. For `SUPERSEDED` the server finds the newer event rather than letting the
caller name it.
`resolved_by` is the authenticated operator. An unknown reason, a lower-case spelling, or an
unknown field is `400`, never a fallback to `REPLAYED`.

| Refusal | Status |
| :-- | :-- |
| Not a provider caller | `403` |
| No administrative reason, a malformed or nil `event_id`, or an unsupported `resolution_type` | `400` |
| No dead letter for the event | `404` |
| Already resolved | `409` |
| `REPLAYED`: no `consumer_applied` receipt from the dead letter's consumer | `412` |
| `SUPERSEDED`: no recorded version for the event, or no newer version of the Membership applied by the dead letter's consumer | `412` |
| Waive: no reason, no expiry, or an expiry not after now or beyond 90 days | `400` |
| Waive: the dead letter's consumer is active | `412` |
| Replay only: the consumer no longer subscribes to the event's type, because it was retired or resubscribed | `412` |
| Waive: already under an unexpired waiver, or already resolved | `409` |
| Replay only: the row cannot re-append itself (no envelope, or a null `aggregate_id` or `priority`) | `412` |

Both honour `Idempotency-Key`. The claim is made inside the operation's own transaction, and a
retry after a lost response replays the stored answer. Without the key, a retried `resolve` would
read the incident as already resolved and answer `409` to a request that succeeded.

**Honoured, not required** (2.3.0). When `TDD-organization-control-003` 1.10.0 made the key required
on every command, replay, resolve and waive stayed optional. None can act twice: a replay re-sends a
delivery the consumer deduplicates by `event_id`, and a second resolve or a second waiver is refused
`409` by the incident's own state, so a retry without a key is safe and only its answer is less
helpful. Requiring it would change the client contract of the incident tooling, the system proof
included, for no property gained.

The `412` for missing evidence names the dead letter's consumer and lists the receipts it did find as
`consumer (evidence)`. A receipt under another consumer name therefore reads as a
`DISPATCH_CONSUMER_NAME` mismatch rather than as an absence, and a `transport_accepted` receipt
reads as a delivery that was never confirmed applied.

`X-Application-Receipt: applied` is returned by the delivery intake, and it is the only thing
that produces `consumer_applied` evidence. It is emitted when the projection row and the inbox
guard committed, and on no other path. A duplicate carries it, because the inbox guard finding
the event already applied makes the assertion true. A superseded outcome does not, because the
event was discarded rather than applied.

The producer cannot claim the class on the consumer's behalf. `outbox.Receipt` carries an
unexported field, so `consumer_applied` is reachable only through the constructor that takes
the consumer's marker. A broker cannot produce that marker, so introducing one degrades receipts
to `transport_accepted` and resolution stops finding proof. It fails loudly, which is the
intended outcome rather than a regression to work around.

## Algorithms / Logic

### The resolution predicate

A dead letter `X` may be resolved as `REPLAYED` only if all of the following hold:

```
X is unresolved
a row exists in platform.delivery_receipt for X.event_id
that row's consumer is X.consumer
that row's evidence is 'consumer_applied'
```

A dead letter `X` may be resolved as `SUPERSEDED` only if all of the following hold:

```
X is unresolved
membership.membership_event holds X.event_id, giving membership M and version V
a row exists in platform.delivery_receipt for some event E
that row's consumer is X.consumer
that row's evidence is 'consumer_applied'
membership.membership_event holds E.event_id for the same M, with version W > V
```

The reference names the lowest such `E`.

A provider grant event is superseded the same way, with `organization.provider_grant_event` and
`grant_version` in place of the Tenant's. A Tenant event is superseded the same way. `tenant.tenant_event` holds `X.event_id` with Tenant
`T` and security version `V`, and `E` is an applied event for the same `T` with
`tenant_security_version W > V`. Tenant events carry the Tenant's complete status and its
security version, and the consumer orders them by that version, so the same proof holds.
Without it, a Tenant suspension dead-lettered and then overtaken by a restoration could never
close. A replay is discarded, so `REPLAYED` never gets its receipt, and the incident would block
every check.

This is domain proof and not a version comparison alone. Every Membership event carries the
complete security state and its version, not a delta, and the consumer applies an event only
when its version is higher than the one it holds. Once the consumer has applied `W`, the event at
`V < W` can never take effect. Replaying it is discarded by the monotonicity guard, and the
consumer already holds a state at least as new as the one it missed. The receipt proves the
consumer applied `W`, and the history proves `W` is newer than `V` for the same Membership.

Versions come from `membership.membership_event` and `tenant.tenant_event`, never from stream positions, receipts, or the
dead letter's envelope. A replay reassigns `stream_position`, so an older grant replayed after a
dead-lettered revocation carries the higher position. A predicate reading positions would close
the revocation's incident while the consumer holds the older grant, clearing the debt for a
withdrawal that reached nobody.

For either reason there is no fallback, no scalar progress comparison, no operator assertion,
and no `transport_accepted` receipt. A newer applied event does not make `REPLAYED` true: the two
reasons state different facts, and each is recorded as what it is.

### The resolver

```
record ATTEMPT   "resolve dead-lettered event <id>"          own transaction, before anything else
BEGIN            as organization_resolution_rt
  read X = (event_id, consumer): absent -> 404, resolved -> 409
  REPLAYED:   EXISTS consumer_applied receipt for (X.event_id, X.consumer): no -> 412, naming what was found
  SUPERSEDED: read (M, V) for X: none -> 412
              find the lowest applied E for M with W > V: none -> 412
  UPDATE platform.dead_letter SET the four columns
   WHERE event_id = X.event_id AND consumer = X.consumer AND resolved_at IS NULL
  rows affected != 1 -> 409
  record OUTCOME "closed dead-lettered event <id> as <reason> on <reference>"
                 SUPERSEDED appends "; Membership <M> version <V> superseded by version <W> (<type>)"
COMMIT
```

There is no `FOR UPDATE`. Two concurrent closes are settled by `resolved_at IS NULL` in the
`UPDATE` and the affected-row check, so exactly one wins and the other reads as already
resolved.

**Two audit records, deliberately split.**

- **The attempt** is written before the transaction opens, in its own transaction, so it
  survives a refusal or a rollback. An attempt to close a security debt is evidence whether or
  not it succeeded.
- **The outcome** is written inside the transaction that closes the incident, so it exists if
  and only if the closure does.

A refused closure therefore leaves an attempt and no account of a closure. Both records go to
`audit.privileged_access`. The attempt is written through the provider pool's recorder, and the
outcome by the resolution role.

### WAIVED

Some incidents have no corrective path. When the consumer that refused the event has been
retired, no replay can reach it, and no receipt will justify `REPLAYED` or `SUPERSEDED`. Left
alone, the row alerts as stale forever and keeps its restricted payload forever. A waiver records
that an operator knows about it, why, and until when:

```
waive X until T because R
  X is unresolved and not under an unexpired waiver
  X's consumer is not active
  now < T <= now + 90 days, on the database clock
  R is not blank
  -> waived_at = now, waived_until = T, waived_by = the operator, waiver_reason = R
```

**A waiver is not a closure.** `resolved_at` stays `NULL`. The estate frontier keeps counting
the incident as debt, and the closure record stays empty. The outcome audit record says
"waived", never "closed". The response carries `resolved: false`, as a replay's does.

**It never makes a consumer fresh.** The consumer's own frontier never counted another
consumer's dead letter in the first place (§Scope). The two refusals keep it that way:

- **An active consumer's incident.** This is a live outage with corrective paths: replay it,
  or supersede it. Silencing its alert would hide exactly what the alert exists to show.

**What it changes is operational.** foundation-platform's helpers read the waiver columns:

- the stale alert is silenced until `waived_until`;
- the payload is disposed 90 days after `waived_at`;
- the incident no longer holds receipt pruning.

**It expires.** A waiver past `waived_until` alerts again, so an exception somebody forgot
becomes a question again. A waiver is not renewed in place: waiving again after expiry is a new
decision, recorded as one.

It runs as `organization_resolution_rt`, which holds `UPDATE` on the four waiver columns and on
no other part of the row. It files the same two records as a resolution: the attempt before the
transaction, and the outcome inside it.

### Rebuilding a consumer

This is the case `RESNAPSHOTTED` was for: a consumer whose projection has to be rebuilt from a
new snapshot, rather than repaired event by event. The rebuild is done under a new consumer
identity, which is generation replacement at the level the estate already enforces. A new
identity has no debt, no applied position, and nothing inherited.

```
1. retire the consumer:           POST /v1/projections/consumers/{old}/retire
                                  (its subscription is retired and what it was owed abandoned)
2. register the new identity:     POST /v1/projections/consumers        {consumer_id: new, event_types, ...}
3. point all three names at it:   DISPATCH_CONSUMER_NAME, REFERENCE_CONSUMER_NAME, and the
                                  registration, which must agree (§Configuration)
4. bootstrap from a snapshot:     POST /v1/projections/consumers/{new}/bootstrap, then the
                                  snapshot pages; the consumer rebuilds its projection empty,
                                  from a snapshot that already reflects every committed event
5. waive the old identity's incidents (§WAIVED): they no longer block anyone, and waiving them
   stops their alert and lets their payload be disposed
```

It is an outage, and it is chosen rather than inherited. Between steps 1 and 4 the consumer
serves nothing projection-backed. That is the honest form of a rebuild RESPONSE-13 named, and
the cost generation replacement would have avoided.

Why it is sound, step by step:

- **Old debt is shed without being declared delivered.** Debt is attributed to the consumer that
  refused the event (§Scope), so the old identity's dead letters are never counted for the new
  one.
- **The new projection cannot miss the failed event's effect.** A snapshot reads the
  authoritative tables, and the failed event committed before it was ever dead-lettered.
- **Unattributed dead letters are the exception.** Those from before foundation-platform v0.2.8
  name no consumer, so they count for the new identity too. None can exist on a deployed
  database, because the post stage refuses one (§The superseded case).

### Why scalar progress is not evidence

A consumer's highest applied mark does not establish that any particular event was applied.
`platform.outbox` allocates `sequence` at INSERT rather than at COMMIT, so positions are not a
contiguous acknowledgement. Event 100 can be abandoned while 101 is applied, leaving a reported
mark of 101 that says nothing about 100. The same reasoning rules out `snapshot_mark`: a row can
be invisible to a snapshot while carrying a position below its mark.

### Replay ordering

When several events for the same Membership are in `platform.dead_letter`, replaying only the
highest version is enough:

```
dead letters v5, v6, v7 for one membership_id

replay v7   ->  applied; v7 resolves as REPLAYED
            ->  v5 and v6 resolve as SUPERSEDED on v7's receipt
```

Replaying v5 and v6 as well is harmless but wasted: each is either applied before v7 arrives or
discarded after it. The order is no longer a correctness condition.

The replay endpoint refuses a row that cannot replay itself. A null `aggregate_id` or
`priority` means the row predates their retention and its outbox partition is gone. Inventing
either value is refused: a guessed `aggregate_id` names a real aggregate somewhere, and the
replay would deliver a security event attributed to the wrong subject.

### The superseded case, and what remains unrecoverable

```
v5 suspension dead-lettered
v7 for the same Membership delivered and applied

replay v5                   ->  the monotonicity guard discards it; no application receipt
resolve v5 as SUPERSEDED    ->  closes on v7's receipt
```

The consumer is in the correct state: v7 is newer and applied, and `SUPERSEDED` is the sanctioned
way to say so. Emitting an application receipt on the discard path instead would have made
`REPLAYED` reachable, with an audit record reading `REPLAYED / consumer_applied` for an event the
consumer discarded. That is a false statement in the table whose only purpose is explaining why a
security debt stopped blocking. Delivery intake still emits no receipt on that path.

Two states remain that neither reason closes except by `REPLAYED`:

- **A terminal event with nothing after it.** A revocation is the last event its Membership
  carries, so no newer version can supersede it. That is correct: the only acceptable proof is
  the revocation itself being applied.
- **A retirement with nothing after it.** `tenant.lifecycle.retired` is terminal for its Tenant
  in the same way.
- **An event with no history row.** It was published before its history table existed, or it is
  neither a Membership nor a Tenant event.

**A retired consumer's incident** has no corrective path at all, because no replay reaches its
consumer. It blocks no other consumer (§Scope), and it is waived rather than closed (§WAIVED).

**A dead letter with no closure path at all is refused at deploy.** Rows written by older
platform versions can lack everything a closure needs:

- no `aggregate_id` or `priority`, from before foundation-platform v0.2.3, so it cannot replay
  itself;
- no history row, so it cannot be superseded;
- an active consumer's name, so it cannot be waived.

An authority-bearing row with all three would refuse its consumer's projection-backed checks
forever. So would one that names no consumer at all, whatever else it carries: it counts for every
consumer, and the API addresses a dead letter by its consumer, so nothing can replay, close or
waive it.
`organization-migrate -stage=post` lists such rows after the isolation check
(`controldb.UnclosableDeadLetters`, with `projection.AuthorityEventTypes`), logs each
`event_id`, and fails. Nothing is in production, so the expected count is zero; the stage asserts
that rather than assuming it. No sanctioned closure was defined for these rows: one would be a way
to declare authority delivered without evidence, the one thing this design refuses.

## Configuration

| Setting | Effect |
| :-- | :-- |
| `ORGANIZATION_DELIVERY_TARGETS` | The consumers this service delivers to, as `consumer=https://acceptance-url` pairs separated by commas. Empty runs no dispatcher. Each name must be a registered consumer, and its dispatcher waits until it is. |
| `ORGANIZATION_DISPATCH_DATABASE_URL` | Required with targets. A login role inheriting `organization_dispatch_rt`, as its own pool. Refused when it equals another pool's DSN. |
| `ORGANIZATION_WORKLOAD_CLIENT_ID`, `ORGANIZATION_WORKLOAD_KEY_FILE`, `ORGANIZATION_WORKLOAD_TOKEN_URL` | Required with targets. This service's workload client in the kernel, its private key file (RSA, at least 3072 bits), and the token endpoint at the address this process reaches. The assertion's audience is `ORGANIZATION_TOKEN_ISSUER`. |
| `ORGANIZATION_DELIVERY_TIMEOUT`, `ORGANIZATION_DELIVERY_RETRY_INTERVAL` | One publication's bound (default `5s`), and how long a target waits before checking its registration again or restarting its dispatcher (default `1m`). |
| `ORGANIZATION_RESOLUTION_DATABASE_URL` | Required, with no fallback. The credential of a login role that inherits `organization_resolution_rt`, opened as its own pool of two connections. Refused at startup when it equals the provider or tenant DSN: a resolution credential shared with another pool would give that pool the power to close incidents. |
| `DISPATCH_CONSUMER_NAME` (foundation-reference) | The consumer's name, not its endpoint. Each consumer runs its own dispatcher under its own name, and that dispatcher claims only the deliveries owed to that name. Delivery receipts are keyed by it, and the resolution predicate asks whether a specific consumer holds a specific event. An endpoint cannot answer that, because an endpoint moves and the identity does not. The dispatch role may read `consumer_id` and `retired_at` of `projection.consumer`, so the dispatcher can refuse to start when this name is not an active registered consumer. Without that check, a mismatch produced receipts no resolution reads, found only when an incident could not be closed. |
| `REFERENCE_CONSUMER_NAME` (foundation-reference) | The consumer's own identity for its inbox guard. It must equal the above and the name registered with this service. |

Nothing validates that the three names agree. They are configured separately, and the
dispatcher holds no provider credential with which to check. A mismatch produces receipts no
resolution reads, which is why the resolver's refusal names what it found rather than reporting
an absence of evidence.

## Testing Strategy

Each property is held by a named test, and each mutation listed has been observed turning its
test red, not assumed to.

| Property | Where it is proven |
| :-- | :-- |
| An incident closes on applied evidence, sets all four columns, and clears the frontier debt | `internal/projection/resolve_integration_test.go` `TestAnIncidentClosesOnAppliedEvidence` |
| No evidence, another consumer's receipt, or a `transport_accepted` receipt resolves nothing | same file: `TestAnIncidentWithNoEvidenceStaysOpen`, `TestAReceiptUnderAnotherConsumerNameResolvesNothing`, `TestTransportAcceptanceIsNotResolutionEvidence` |
| A keyed resolution claims its key under the resolution role, and a retry replays the stored response | same file: `TestAKeyedResolutionClaimsItsKeyAndARetryIsAnsweredFromIt` |
| A closed or unknown incident is refused, and a consumer with no dead letter for the event is refused with the consumers that have one | same file: `TestResolvingAClosedIncidentIsRefused`, `TestResolvingAnUnknownEventIsRefused`, `TestAnIncidentIsAddressedByItsConsumer` |
| One consumer's incident closes on its own evidence and leaves another consumer's incident for the same event open | same file: `TestOneConsumersClosureLeavesAnothersIncidentOpen` |
| The resolution role cannot rewrite the incident or manufacture its own evidence | same file: `TestTheResolutionRoleCannotRewriteTheIncident`, `TestTheResolutionRoleCannotManufactureItsOwnEvidence` |
| The replay role cannot resolve what it replayed | `internal/projection/replay_integration_test.go` `TestTheReplayRoleCannotResolveWhatItReplayed` |
| The outcome is recorded inside the closing transaction; a refusal leaves no account of a closure | `resolve_integration_test.go` `TestAClosureRecordsItselfInsideTheTransactionThatMadeIt`, `TestARefusedClosureLeavesNoAccountOfAClosure` |
| An incident closes as `SUPERSEDED` on a newer applied event, names both versions in its outcome record, and clears the frontier debt | `internal/projection/superseded_integration_test.go` `TestAnIncidentClosesAsSupersededOnANewerAppliedEvent` |
| A replayed older event's receipt does not supersede a newer revocation | same file: `TestAReplayedOlderEventDoesNotSupersedeANewerRevocation`; a mutation removing the version comparison was observed turning it red |
| A newer event that is weakly receipted, receipted by another consumer, or about another Membership supersedes nothing; an event with no recorded version cannot be superseded | same file: `TestANewerEventThatIsNotEvidenceSupersedesNothing`, `TestAnIncidentWithNoRecordedVersionCannotBeSuperseded` |
| A newer applied event does not close an incident as `REPLAYED` | same file: `TestANewerAppliedEventDoesNotCloseAnIncidentAsReplayed` |
| The resolution role cannot insert, renumber, or erase Membership history | same file: `TestTheResolutionRoleCannotRewriteMembershipHistory` |
| A Tenant event closes as `SUPERSEDED` on a newer applied one for the same Tenant, and an older one never supersedes it. A mutation replacing the version comparison was observed turning it red | `internal/projection/superseded_tenant_integration_test.go` `TestATenantEventClosesAsSupersededOnANewerAppliedOne`, `TestAnOlderTenantEventDoesNotSupersedeANewerOne` |
| The resolution role reads Tenant history and cannot insert, renumber, or erase it | same file: `TestTheResolutionRoleCannotRewriteTenantHistory` |
| The post stage names exactly the dead letters no resolution can close: every unattributed one, and those that are unreplayable, unversioned and an active consumer's. It does not name a waivable, replayable, versioned or non-authority row. A mutation dropping the replayability condition was observed turning it red | `internal/controldb/unclosable_integration_test.go` `TestThePostStageNamesOnlyDeadLettersNoResolutionCanClose` |
| Every published Tenant event has a history row that agrees with it, and a rolled-back transition leaves none | `internal/tenant/service_integration_test.go` `TestEveryPublishedTenantEventRecordsItsSecurityVersion` |
| The frontier counts every type the Membership and Tenant state machines publish, and nothing else | `internal/httpapi/frontier_debt_test.go` `TestTheFrontierDebtCoversEveryAuthorityEvent` |
| A retired consumer's incident is waived and stays open: `resolved_at` stays null, the estate frontier still reports the debt, an active consumer is not charged with it, and no closure record is filed | `internal/projection/waive_integration_test.go` `TestARetiredConsumersIncidentIsWaivedAndStaysOpen` |
| A waiver is refused for an active consumer's incident, and the refusal names the corrective path. It is also refused without a reason, or with an expiry outside (now, now + 90 days]. A mutation making an active consumer's incident waivable was observed turning it red | same file: `TestAWaiverIsRefusedWhereItWouldHideALiveOutage` |
| A standing waiver is not stacked, and a resolved incident is not waived | same file: `TestAWaiverIsNotStackedOrAppliedToAClosedIncident` |
| A malformed waiver, or one from a tenant caller, is refused before the database | `internal/httpapi/dead_letter_resolve_test.go` `TestAMalformedWaiverIsRefusedBeforeTheDatabase`, `TestAWaiverIsProviderScoped` |
| Every published Membership event has a history row that agrees with it, and a rolled-back transition leaves none | `internal/membership/service_integration_test.go` `TestEveryPublishedEventRecordsItsVersion` |
| A missing or undeclared RLS policy is reported by name | `internal/controldb/assert_integration_test.go` `TestAssertIsolationDetectsEachWeakening` |
| An unsupported `resolution_type`, a lower-case spelling, or an unknown field is `400` before the database is reached | `internal/httpapi/dead_letter_resolve_test.go` `TestAResolutionNamingAnUnsupportedReasonIsRefused` |
| The attempt is recorded before the transaction and survives its rollback | `internal/db/binding_test.go` `TestProviderAccessIsRecordedBeforeTheTransaction`; `internal/access/access_integration_test.go` `TestEvidenceSurvivesADomainRollback` |
| A resolution credential shared with another pool is refused | `internal/config/config_test.go` `TestAResolutionCredentialSharedWithAnotherPoolIsRefused`, with the suite clearing ambient `ORGANIZATION_*` variables |
| Several consumers are active at once, each owed only its subscribed types; a subscription change or a revival clears the snapshot mark; retiring abandons only that consumer's deliveries; a type the registry does not offer is refused | `internal/projection/consumer_integration_test.go` |
| A replay is owed to the refusing consumer alone, and one to a consumer that no longer subscribes is refused | `internal/projection/replay_integration_test.go` `TestAReplayIsOwedToTheRefusingConsumerAlone`, `TestAReplayToAConsumerThatNoLongerSubscribesIsRefused` |
| Each consumer's frontier counts its own owed deliveries; the estate counts all | `internal/projection/frontier_integration_test.go` `TestEachConsumersFrontierCountsItsOwnDeliveries` |
| Every role holds exactly its declared platform privileges, and new platform tables arrive closed | `internal/controldb/platform_privileges_integration_test.go`; CI mutation |
| A superseded delivery carries no application receipt | `foundation-reference/internal/httpapi/receipt_test.go`; CI mutation |
| Applied evidence cannot be claimed without the consumer's marker | `foundation-platform/outbox/receipt_integration_test.go`; CI mutation |
| A dispatcher whose database contract is unmet refuses to start | `foundation-platform/outbox/preflight_integration_test.go` |
| The in-process dispatcher waits for its consumer's registration, never starts on a failed check, restarts after it stops, and one target's failure stops no other | `internal/delivery/delivery_test.go` |
| A delivery target needs the dispatch credential and the workload client, a malformed target is refused, and a dispatch DSN shared with another pool is refused | `internal/config/config_test.go` |
| The dispatch role can run the registration check the dispatcher makes | `internal/controldb/dispatch_role_integration_test.go` `TestTheDispatchRoleCanCheckItsOwnRegistration` |
| A delivery authenticates with the workload token, a 401 drops it, and a delivery is never sent without one | `foundation-platform/outbox/httpdelivery/publisher_test.go` |
| A dead letter retains enough to replay itself | `foundation-platform/outbox/replay_integration_test.go`; CI mutation |
| `dead_lettered_at` names the transition, not the transaction start | `foundation-platform/outbox/temporal_integration_test.go`; CI mutation |

**The external proof, in two layers.**

- **Component.** `foundation-reference`'s
  `TestResolvingTheDebtRestoresTheBystanderAndLeavesTheRevocationEnforced` takes a revoked
  principal and an active one. Both are refused while the debt stands. After resolution the
  active one is restored and the revoked one is still refused.
- **System.** `foundation-reference`'s `TestProofAAcrossTheProcessBoundary` runs this service,
  the consumer, and the dispatcher as real processes. It dead-letters the revocation itself
  through a proxy answering `422`, replays and resolves it through this API, and asserts that
  the revoked principal's refusal reason changes from debt to withdrawal. That change is the
  evidence the revocation landed. A test asserting only that both stay refused would pass even
  if it never did. It then suspends the Tenant through this API and asserts that the consumer
  refuses its active member because the Tenant is not active, and restores it and asserts that
  the member is allowed again.

The system proof closed this scope on 2026-09-24, in foundation-reference CI run 36026176642,
at these revisions:

- organization-control `7aa69d776f03dac69a749eb1f6585402c53d124d`
- foundation-reference `3fff64200a6e25a07fcfcbfbf62b2ea10add7822`
- foundation-platform `v0.2.7` on the producer, `v0.2.6` on the consumer

**Keeping the proof current.** The closure run is pinned: foundation-reference proves this
repository at one revision, which keeps the run reproducible, but a change here never ran it. Two
more runs close that gap (RESPONSE-24 §3). Neither replaces the pin, and both are logged as
`UNPINNED`, so neither can be mistaken for a closure record:

- **`system-proof` in this repository's CI.** On every pull request it runs the same proof
  against the PR head, with foundation-reference at the revision in
  `systemproof/foundation-reference.rev`. On the daily schedule it uses foundation-reference's
  `main`.
- **`system-proof-main` in foundation-reference's CI.** Daily, it runs the proof against this
  repository's `main`.

A red scheduled run means the two repositories have drifted apart. Fix the side that broke the
contract, then bump the pins deliberately.

The system proof covers `REPLAYED`. `SUPERSEDED` has no cross-process proof yet. Its predicate
runs entirely in this service's database, and the integration suite above runs it as the real
resolution role against real receipts and history.

The HTTP handlers are thin over the resolver and replayer, which are tested directly. The
handler-level tests cover only refusals that must land before the database is reached.

## Traceability

| Relationship | Reference |
| :-- | :-- |
| Parent system architecture | SAD-004 §7.6, §9.1.1 |
| Delivery profile and evidence rules | ADR-GLB-016 §5.4 |
| Membership authority, revocation and the projection contract | TDD-organization-control-002 |
| Tenant isolation and the role model the grants extend | TDD-organization-control-001 |
| No outbound HTTP from this service, which places the dispatcher in foundation-reference | ADR-ORG-001 §5.4 |
| Migration integrity and schema drift gates | ADR-GLB-004 |
| Review record of these decisions | `RESPONSE-7` through `RESPONSE-26` in the architecture-description workspace |

## Operational Notes

The step-by-step procedure is `docs/runbooks/dead-letter-resolution.md` (2.4.0).

While an authority-bearing dead letter is unresolved, every projection-backed check refuses.
That is the designed behaviour and not an incident in itself: the alternative is serving
authority the consumer cannot vouch for. The incident is the undelivered event.

Recovery:

1. Fix the cause the consumer refused for.
2. Replay the highest dead-lettered version of each affected Membership, to that consumer.
3. Confirm that the replay produced a `consumer_applied` receipt from that consumer.
4. Resolve it as `REPLAYED`, and each lower version of the same Membership as `SUPERSEDED`.

Each step names the consumer. An event dead-lettered at two consumers is recovered twice, once
per consumer, because each consumer's evidence is its own.

Skipping step 3 leads to a resolve with no evidence behind it, which the predicate refuses. If
a newer event for the Membership was already delivered and applied, step 2 is unnecessary for
the older dead letters: resolve them as `SUPERSEDED` directly.

A dead letter refused by a consumer that has since been retired no longer blocks anyone. Waive it
with a reason and an expiry within 90 days, so the stale alert stops and the payload can be
disposed. If it is still there when the waiver expires, it alerts again.

## Security Notes

The evidence chain is the control. Three properties carry it, and each is enforced rather than
documented:

- Applied evidence cannot be produced without the consumer's own marker.
- The tables holding that evidence are not writable by the request path.
- The only role able to close or waive an incident can write nothing but the four resolution
  columns and the four waiver columns, and a waiver cannot clear debt.
- The Membership history that orders versions is written only in the publishing transaction,
  and no runtime role can edit it.

`delivery_receipt` accepts no `UPDATE` or `DELETE` from any role. A delivery worker able to edit
the evidence of its own deliveries is one whose bug rewrites the record that would have shown
it.

## Performance Notes

The frontier is polled by consumers, so its reads are deliberately not routed through the
provider scope wrapper. Doing so would file a privileged-access record per poll and fill the
evidence table an investigation reads with rows about nothing. The consumer caches the answer
for a quarter of its maximum projection age.

Resolution is an operator action measured in minutes, not a request-path operation. No latency
budget applies to it, and none is claimed.

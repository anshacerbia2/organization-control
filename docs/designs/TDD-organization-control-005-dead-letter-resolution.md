---
doc_meta:
  id: TDD-organization-control-005
  title: Dead-Letter Resolution, Scope and Limits
  owner: Core Platform Team
  version: 1.2.1
  status: approved
  classification: restricted
  review_cycle_days: 90
  created_date: 2026-09-17
  last_reviewed: 2026-09-27
  parent_sad: SAD-004
---

# Dead-Letter Resolution, Scope and Limits

## Purpose

Record what may close an authority-bearing dead letter, how the closure is built, and what this
scope still cannot recover from.

An authority-bearing dead letter is a Membership security event the dispatcher abandoned
after the consumer refused it permanently. While one is unresolved, the publication
frontier reports security debt and every projection-backed enforcement check refuses. So
resolving one is the act that returns service, and resolving one wrongly is the act that
returns service to a consumer holding a revocation it never received.

The scope described here is built. `REPLAYED` was proven across real processes at the revisions
recorded under **Testing Strategy**. `SUPERSEDED` is proven by this repository's integration
suite against the real roles.

## Scope

Two resolution reasons are in scope:

```
REPLAYED      the active consumer applied this event
SUPERSEDED    the active consumer applied a newer event for the same Membership
```

Two are out, and each is out for a stated reason rather than by omission:

| Reason | Why it is out |
| :-- | :-- |
| `RESNAPSHOTTED` | Requires generation replacement in full: a generation built empty, dual-apply during the rebuild, atomic promotion, generation-scoped lookup, and a race proof. Implementing part is worse than none, because a promotion that reads do not respect is a pointer swap, not a replacement. |
| `WAIVED` | An operational exception. Closing an incident is not repairing authority state, and one column serving both conflates them. |

`SUPERSEDED` rests on the same evidence as `REPLAYED`: a `consumer_applied` receipt from the
active consumer. What differs is which event the receipt is for. The domain proof that lets a
newer event close the failed one's effect is in **Algorithms / Logic**.

The enforcement scope is also one producer and one projection consumer. `projection.consumer`
refuses a second active registration, and the database enforces it through a partial unique
index rather than through the registry alone. Security debt is reported estate-wide rather
than per consumer, so a second consumer's poison event would refuse the first consumer's
traffic.

## Technical Context

Membership security events reach the consumer through `platform.outbox` and a dispatcher that
delivers over HTTP: the Direct Durable Delivery profile of ADR-GLB-016. The dispatcher runs in
`foundation-reference`, because ADR-ORG-001 §5.4 forbids outbound HTTP from this service.

Security events travel in the priority lane. The dispatcher dead-letters one only when the
consumer refuses it permanently (`400`, `409`, `422`), which makes it poison. An unreachable
consumer never dead-letters one: after three attempts the row is released back for delivery.
A dead-lettered row is copied to `platform.dead_letter` and marked published, so redelivery
stops.

The consumer applies an event and its inbox guard in one transaction, and only then
acknowledges. Acknowledgement therefore means applied. That is a property of this transport,
not of the type, and it ends the moment a broker sits between the two.

`platform` is versioned by foundation-platform (v0.2.7) on its own release cadence. This
repository applies that schema and owns the grants on it, which is why the privilege posture in
**Data Model** is stated here rather than upstream.

## Component Design

| Component | Responsibility |
| :-- | :-- |
| `outbox.Dispatcher` (foundation-platform) | Claims, delivers, decides retry, release, or dead-letter, and writes the delivery receipt. Verifies its database contract before starting any worker and refuses to start when it is unmet. |
| `dispatch.HTTPPublisher` (foundation-reference) | Delivers one envelope. Classifies `400`/`409`/`422` as poison and everything else as unavailable. Takes the evidence class from the consumer's reply rather than from its own judgement. |
| Delivery intake (foundation-reference) | Applies the event, then emits the application receipt on exactly the paths where the assertion holds. |
| `projection.FrontierReader` | Reports publication facts and unresolved security debt. Computes no verdict. |
| `projection.Registry` | Registers and retires projection consumers, and refuses a second active one. |
| `projection.Replayer` | Re-appends a dead letter to the outbox under its original `event_id` and priority, from the row itself. Leaves the dead letter untouched. |
| `projection.Resolver` | Closes a dead letter as `REPLAYED` on the active consumer's `consumer_applied` receipt for the event, or as `SUPERSEDED` on that consumer's `consumer_applied` receipt for a newer version of the same Membership. Nothing else. |
| `membership.Service` | Writes `membership.membership_event` beside every Membership event it publishes, in the publishing transaction. |
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
                            no role                      UPDATE, DELETE

platform.dead_letter        organization_dispatch_rt     INSERT, SELECT
                            organization_provider_rt     SELECT   (frontier debt facts, replay source)
                            organization_resolution_rt   SELECT, and UPDATE on the four resolution columns only
                            organization_rt              none

membership.membership_event organization_rt              INSERT   (written when publishing)
                            organization_resolution_rt   SELECT   (the SUPERSEDED predicate)
                            organization_provider_rt     none
                            no runtime role              UPDATE, DELETE, TRUNCATE

projection.consumer         organization_resolution_rt   SELECT   (the active consumer, derived server-side)
audit.privileged_access     organization_resolution_rt   INSERT   (the outcome record)
platform.idempotency_key    organization_resolution_rt   INSERT, SELECT   (the claim of a keyed /resolve)
```

`SELECT` for the dispatcher is not for reading incidents. `ON CONFLICT` with a conflict target
makes PostgreSQL require it on the table being inserted into. This was measured per table rather
than inferred, because inferring it across tables is how a preflight shipped asserting `INSERT`
alone.

The resolution role can close an incident and can do nothing else to it. It cannot change
`failure_class` or the envelope, cannot insert or delete a dead letter, cannot write a receipt,
and cannot write Membership history. It cannot manufacture the evidence it closes on, and it
cannot rewrite the incident it closes.

`membership.membership_event` is under the same Row-Level Security as every table in the
`membership` schema: the tenant-scope and provider-scope policies, plus one declared extra,
`membership_event_resolution_read`. That is a `SELECT` policy for the resolution role, keyed on
the provider binding. `controldb.AssertIsolation` checks policies by name and accepts an extra
one only when it is declared in `AdditionalPolicies`.

The request path holds nothing on either table. A caller able to insert a `consumer_applied`
receipt could forge the proof that closes a security debt, which makes forging the evidence and
forging the resolution the same act. New tables in the `platform` schema arrive closed: the
schema's default privileges grant nothing, and each table is named explicitly with the path
that earns it.

## API / Interface

Two provider-only operations. Each requires an `X-Administrative-Reason`.

```
POST /v1/dead-letters/{event_id}/replay    202  {event_id, event_type, position, resolved: false}
POST /v1/dead-letters/{event_id}/resolve   200  {event_id, consumer, resolution_type, resolution_reference}
```

`resolve` takes an optional body naming the reason:

```
(no body)                             REPLAYED
{"resolution_type": "REPLAYED"}       REPLAYED
{"resolution_type": "SUPERSEDED"}     SUPERSEDED
anything else                         400
```

The operator chooses the reason, and the server chooses the subject the evidence must be about.
The consumer is read from `projection.consumer` and is never accepted from the request, and for
`SUPERSEDED` the server finds the newer event rather than letting the caller name it.
`resolved_by` is the authenticated operator. An unknown reason, a lower-case spelling, or an
unknown field is `400`, never a fallback to `REPLAYED`.

| Refusal | Status |
| :-- | :-- |
| Not a provider caller | `403` |
| No administrative reason, a malformed or nil `event_id`, or an unsupported `resolution_type` | `400` |
| No dead letter for the event | `404` |
| Already resolved | `409` |
| No active projection consumer registered | `412` |
| `REPLAYED`: no `consumer_applied` receipt for the active consumer | `412` |
| `SUPERSEDED`: no recorded version for the event, or no newer version of the Membership applied by the active consumer | `412` |
| Replay only: the row cannot re-append itself (no envelope, or a null `aggregate_id` or `priority`) | `412` |

Both honour `Idempotency-Key`. The claim is made inside the operation's own transaction, and a
retry after a lost response replays the stored answer. Without the key, a retried `resolve` would
read the incident as already resolved and answer `409` to a request that succeeded.

The `412` for missing evidence names the active consumer and lists the receipts it did find as
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
that row's consumer is the active projection consumer, derived server-side
that row's evidence is 'consumer_applied'
```

A dead letter `X` may be resolved as `SUPERSEDED` only if all of the following hold:

```
X is unresolved
membership.membership_event holds X.event_id, giving membership M and version V
a row exists in platform.delivery_receipt for some event E
that row's consumer is the active projection consumer, derived server-side
that row's evidence is 'consumer_applied'
membership.membership_event holds E.event_id for the same M, with version W > V
```

The reference names the lowest such `E`.

This is domain proof and not a version comparison alone. Every Membership event carries the
complete security state and its version, not a delta, and the consumer applies an event only
when its version is higher than the one it holds. Once the consumer has applied `W`, the event at
`V < W` can never take effect. Replaying it is discarded by the monotonicity guard, and the
consumer already holds a state at least as new as the one it missed. The receipt proves the
consumer applied `W`, and the history proves `W` is newer than `V` for the same Membership.

Versions come from `membership.membership_event`, never from stream positions, receipts, or the
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
  read X: absent -> 404, resolved -> 409
  read the active consumer: none -> 412
  REPLAYED:   EXISTS consumer_applied receipt for (X, consumer): no -> 412, naming what was found
  SUPERSEDED: read (M, V) for X: none -> 412
              find the lowest applied E for M with W > V: none -> 412
  UPDATE platform.dead_letter SET the four columns WHERE event_id = X AND resolved_at IS NULL
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
- **An event with no history row.** It was published before `membership.membership_event`
  existed, or it is not a Membership event.

## Configuration

| Setting | Effect |
| :-- | :-- |
| `ORGANIZATION_RESOLUTION_DATABASE_URL` | Required, with no fallback. The credential of a login role that inherits `organization_resolution_rt`, opened as its own pool of two connections. Refused at startup when it equals the provider or tenant DSN: a resolution credential shared with another pool would give that pool the power to close incidents. |
| `DISPATCH_CONSUMER_NAME` (foundation-reference) | The consumer's name, not its endpoint. Delivery receipts are keyed by it, and the resolution predicate asks whether a specific consumer holds a specific event. An endpoint cannot answer that, because an endpoint moves and the identity does not. |
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
| A closed or unknown incident is refused; nothing registered to enforce is refused | same file: `TestResolvingAClosedIncidentIsRefused`, `TestResolvingAnUnknownEventIsRefused`, `TestTheResolverRefusesWhenNothingIsRegisteredToEnforce` |
| The resolution role cannot rewrite the incident or manufacture its own evidence | same file: `TestTheResolutionRoleCannotRewriteTheIncident`, `TestTheResolutionRoleCannotManufactureItsOwnEvidence` |
| The replay role cannot resolve what it replayed | `internal/projection/replay_integration_test.go` `TestTheReplayRoleCannotResolveWhatItReplayed` |
| The outcome is recorded inside the closing transaction; a refusal leaves no account of a closure | `resolve_integration_test.go` `TestAClosureRecordsItselfInsideTheTransactionThatMadeIt`, `TestARefusedClosureLeavesNoAccountOfAClosure` |
| An incident closes as `SUPERSEDED` on a newer applied event, names both versions in its outcome record, and clears the frontier debt | `internal/projection/superseded_integration_test.go` `TestAnIncidentClosesAsSupersededOnANewerAppliedEvent` |
| A replayed older event's receipt does not supersede a newer revocation | same file: `TestAReplayedOlderEventDoesNotSupersedeANewerRevocation`; a mutation removing the version comparison was observed turning it red |
| A newer event that is weakly receipted, receipted by another consumer, or about another Membership supersedes nothing; an event with no recorded version cannot be superseded | same file: `TestANewerEventThatIsNotEvidenceSupersedesNothing`, `TestAnIncidentWithNoRecordedVersionCannotBeSuperseded` |
| A newer applied event does not close an incident as `REPLAYED` | same file: `TestANewerAppliedEventDoesNotCloseAnIncidentAsReplayed` |
| The resolution role cannot insert, renumber, or erase Membership history | same file: `TestTheResolutionRoleCannotRewriteMembershipHistory` |
| Every published Membership event has a history row that agrees with it, and a rolled-back transition leaves none | `internal/membership/service_integration_test.go` `TestEveryPublishedEventRecordsItsVersion` |
| A missing or undeclared RLS policy is reported by name | `internal/controldb/assert_integration_test.go` `TestAssertIsolationDetectsEachWeakening` |
| An unsupported `resolution_type`, a lower-case spelling, or an unknown field is `400` before the database is reached | `internal/httpapi/dead_letter_resolve_test.go` `TestAResolutionNamingAnUnsupportedReasonIsRefused` |
| The attempt is recorded before the transaction and survives its rollback | `internal/db/binding_test.go` `TestProviderAccessIsRecordedBeforeTheTransaction`; `internal/access/access_integration_test.go` `TestEvidenceSurvivesADomainRollback` |
| A resolution credential shared with another pool is refused | `internal/config/config_test.go` `TestAResolutionCredentialSharedWithAnotherPoolIsRefused`, with the suite clearing ambient `ORGANIZATION_*` variables |
| A second active projection consumer is refused, by registry and by constraint | `internal/projection/consumer_single_active_integration_test.go`; two CI mutations |
| Every role holds exactly its declared platform privileges, and new platform tables arrive closed | `internal/controldb/platform_privileges_integration_test.go`; CI mutation |
| A superseded delivery carries no application receipt | `foundation-reference/internal/httpapi/receipt_test.go`; CI mutation |
| Applied evidence cannot be claimed without the consumer's marker | `foundation-platform/outbox/receipt_integration_test.go`; CI mutation |
| A dispatcher whose database contract is unmet refuses to start | `foundation-platform/outbox/preflight_integration_test.go` |
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
  if it never did.

The system proof closed this scope on 2026-09-24, in foundation-reference CI run 36026176642,
at these revisions:

- organization-control `7aa69d776f03dac69a749eb1f6585402c53d124d`
- foundation-reference `3fff64200a6e25a07fcfcbfbf62b2ea10add7822`
- foundation-platform `v0.2.7` on the producer, `v0.2.6` on the consumer

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

While an authority-bearing dead letter is unresolved, every projection-backed check refuses.
That is the designed behaviour and not an incident in itself: the alternative is serving
authority the consumer cannot vouch for. The incident is the undelivered event.

Recovery:

1. Fix the cause the consumer refused for.
2. Replay the highest dead-lettered version of each affected Membership.
3. Confirm that the replay produced a `consumer_applied` receipt.
4. Resolve it as `REPLAYED`, and each lower version of the same Membership as `SUPERSEDED`.

Skipping step 3 leads to a resolve with no evidence behind it, which the predicate refuses. If
a newer event for the Membership was already delivered and applied, step 2 is unnecessary for
the older dead letters: resolve them as `SUPERSEDED` directly.

## Security Notes

The evidence chain is the control. Three properties carry it, and each is enforced rather than
documented:

- Applied evidence cannot be produced without the consumer's own marker.
- The tables holding that evidence are not writable by the request path.
- The only role able to close an incident can write nothing but the four resolution columns.
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

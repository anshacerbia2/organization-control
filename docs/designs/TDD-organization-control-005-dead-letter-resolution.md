---
doc_meta:
  id: TDD-organization-control-005
  title: Dead-Letter Resolution, Scope and Limits
  owner: Core Platform Team
  version: 1.1.0
  status: approved
  classification: restricted
  review_cycle_days: 90
  created_date: 2026-09-17
  last_reviewed: 2026-09-25
  parent_sad: SAD-004
---

# Dead-Letter Resolution, Scope and Limits

## Purpose

Record what may close an authority-bearing dead letter, how the closure is built, and the one
state this scope cannot recover from.

An authority-bearing dead letter is a Membership security event the dispatcher abandoned
after the consumer refused it permanently. While one is unresolved, the publication
frontier reports security debt and every projection-backed enforcement check refuses. So
resolving one is the act that returns service, and resolving one wrongly is the act that
returns service to a consumer holding a revocation it never received.

The scope described here is built, and it was proven across real processes at the revisions
recorded under **Testing Strategy**.

## Scope

One resolution reason is in scope:

```
REPLAYED
```

Three are out, and each is out for a stated reason rather than by omission:

| Reason | Why it is out |
| :-- | :-- |
| `RESNAPSHOTTED` | Requires generation replacement in full: a generation built empty, dual-apply during the rebuild, atomic promotion, generation-scoped lookup, and a race proof. Implementing part is worse than none, because a promotion that reads do not respect is a pointer swap, not a replacement. |
| `SUPERSEDED` | Requires domain proof that a newer event closes the effect of the failed one. Version comparison is the input to that proof, not the proof. It is the first of the three to build, because of the permanent-block condition below. |
| `WAIVED` | An operational exception. Closing an incident is not repairing authority state, and one column serving both conflates them. |

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
| `projection.Resolver` | Closes a dead letter as `REPLAYED` on the active consumer's `consumer_applied` receipt, and on nothing else. |
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
resolution_type         REPLAYED
resolved_by             the operator's principal
resolution_reference    platform.delivery_receipt:<event_id>:<consumer>
```

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

projection.consumer         organization_resolution_rt   SELECT   (the active consumer, derived server-side)
audit.privileged_access     organization_resolution_rt   INSERT   (the outcome record)
```

`SELECT` for the dispatcher is not for reading incidents. `ON CONFLICT` with a conflict target
makes PostgreSQL require it on the table being inserted into. This was measured per table rather
than inferred, because inferring it across tables is how a preflight shipped asserting `INSERT`
alone.

The resolution role can close an incident and can do nothing else to it. It cannot change
`failure_class` or the envelope, cannot insert or delete a dead letter, and cannot write a
receipt. It cannot manufacture the evidence it closes on, and it cannot rewrite the incident it
closes.

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

`resolve` takes no body. The operator chooses the action, and the server chooses the subject
the evidence must be about: the consumer is read from `projection.consumer` and is never
accepted from the request. `resolved_by` is the authenticated operator.

| Refusal | Status |
| :-- | :-- |
| Not a provider caller | `403` |
| No administrative reason, or a malformed or nil `event_id` | `400` |
| No dead letter for the event | `404` |
| Already resolved | `409` |
| No active projection consumer registered | `412` |
| No `consumer_applied` receipt for the active consumer | `412` |
| Replay only: the row cannot re-append itself (no envelope, or a null `aggregate_id` or `priority`) | `412` |

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

There is no fallback, no scalar progress comparison, no operator assertion, and no
`transport_accepted` receipt.

### The resolver

```
record ATTEMPT   "resolve dead-lettered event <id>"          own transaction, before anything else
BEGIN            as organization_resolution_rt
  read X: absent -> 404, resolved -> 409
  read the active consumer: none -> 412
  EXISTS consumer_applied receipt for (X, consumer): no -> 412, naming what was found
  UPDATE platform.dead_letter SET the four columns WHERE event_id = X AND resolved_at IS NULL
  rows affected != 1 -> 409
  record OUTCOME "closed dead-lettered event <id> as REPLAYED on <reference>"
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

Replay the versions of one aggregate lowest first. When several events for the same Membership
are in `platform.dead_letter`, the order decides whether they can be recovered at all:

```
dead letters v5, v6, v7 for one membership_id

v5, v6, v7  ->  each applied; all three resolvable
v7 first    ->  v6 and v5 then discarded as superseded, permanently unresolvable
```

Nothing enforces the order. It is an operator procedure, and getting it wrong is not
recoverable within this scope.

The replay endpoint refuses a row that cannot replay itself. A null `aggregate_id` or
`priority` means the row predates their retention and its outbox partition is gone. Inventing
either value is refused: a guessed `aggregate_id` names a real aggregate somewhere, and the
replay would deliver a security event attributed to the wrong subject.

### Known permanent-block condition

A dead letter whose event has been superseded cannot be resolved in this scope.

```
v5 revocation dead-lettered
v7 for the same Membership delivered and applied

replay v5
  -> the monotonicity guard discards it
  -> no application receipt, so no consumer_applied evidence
  -> REPLAYED impossible, and the other three reasons are out of scope
  -> the debt blocks, and because debt is estate-wide, it blocks everything
```

The consumer is in the correct state: v7 is newer and applied. What is missing is a sanctioned
way to say so, which is the `SUPERSEDED` contract.

Granting the receipt on that path would make the resolution reachable, and would even reach a
safe state. But the audit record would read `REPLAYED / consumer_applied` for an event the
consumer discarded: a false statement in the table whose only purpose is explaining why a
security debt stopped blocking.

This is a latent permanent outage, and building `SUPERSEDED` is required before this service's
production gate. See ROADMAP.md.

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
| A closed or unknown incident is refused; nothing registered to enforce is refused | same file: `TestResolvingAClosedIncidentIsRefused`, `TestResolvingAnUnknownEventIsRefused`, `TestTheResolverRefusesWhenNothingIsRegisteredToEnforce` |
| The resolution role cannot rewrite the incident or manufacture its own evidence | same file: `TestTheResolutionRoleCannotRewriteTheIncident`, `TestTheResolutionRoleCannotManufactureItsOwnEvidence` |
| The replay role cannot resolve what it replayed | `internal/projection/replay_integration_test.go` `TestTheReplayRoleCannotResolveWhatItReplayed` |
| The outcome is recorded inside the closing transaction; a refusal leaves no account of a closure | `resolve_integration_test.go` `TestAClosureRecordsItselfInsideTheTransactionThatMadeIt`, `TestARefusedClosureLeavesNoAccountOfAClosure` |
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

The API itself has no HTTP-level test here. Its handlers are thin over the resolver and
replayer, which are tested directly, and the system proof exercises both endpoints over HTTP.

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
2. Replay the affected versions, lowest first.
3. Confirm that each replay produced a `consumer_applied` receipt.
4. Resolve.

Skipping step 3 leads to a resolve with no evidence behind it, which the predicate refuses.

## Security Notes

The evidence chain is the control. Three properties carry it, and each is enforced rather than
documented:

- Applied evidence cannot be produced without the consumer's own marker.
- The tables holding that evidence are not writable by the request path.
- The only role able to close an incident can write nothing but the four resolution columns.

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

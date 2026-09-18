---
doc_meta:
  id: TDD-organization-control-005
  title: Dead-Letter Resolution, Scope and Limits
  owner: Core Platform Team
  version: 1.0.0
  status: approved
  classification: restricted
  review_cycle_days: 90
  created_date: 2026-09-17
  last_reviewed: 2026-09-17
  parent_sad: SAD-004
---

# Dead-Letter Resolution, Scope and Limits

## Purpose

Record what may close an authority-bearing dead letter, what may not, and the one state
this scope cannot recover from.

An authority-bearing dead letter is a Membership security event the dispatcher abandoned
after the consumer refused it permanently. While one is unresolved, the publication
frontier reports security debt and every projection-backed enforcement check refuses — so
resolving one is the act that returns service, and resolving one wrongly is the act that
returns service to a consumer holding a revocation it never received.

This document records decisions already taken in review. It does not reopen them, and it
designs nothing that is not built.

## Scope

One resolution reason is in scope:

```
REPLAYED
```

Three are out, and each is out for a stated reason rather than by omission:

| Reason | Why it is out |
| :-- | :-- |
| `RESNAPSHOTTED` | Requires generation replacement in full — a generation built empty, dual-apply during the rebuild, atomic promotion, generation-scoped lookup, and a race proof. Implementing part is worse than none: a promotion that reads do not respect is a pointer swap, not a replacement. |
| `SUPERSEDED` | Requires domain proof that a newer event closes the effect of the failed one. Version comparison is the input to that proof, not the proof. |
| `WAIVED` | An operational exception. Closing an incident is not repairing authority state, and one column serving both conflates them. |

The enforcement scope is also one producer and one projection consumer. `projection.consumer`
refuses a second active registration, and the database enforces it through a partial unique
index rather than through the registry alone — because security debt is reported estate-wide
rather than per consumer, so a second consumer's poison event would refuse the first
consumer's traffic.

## Technical Context

Membership security events reach consumers through `platform.outbox` and a dispatcher that
delivers over HTTP. A consumer that refuses an event permanently (400, 409, 422) causes the
dispatcher to abandon it: the row is copied to `platform.dead_letter` and marked published
so redelivery stops.

The consumer applies an event and its inbox guard in one transaction, and only then
acknowledges. Acknowledgement therefore means applied — a property of this transport, not of
the type, and one that ends the moment a broker sits between the two.

`platform` is versioned by foundation-platform on its own release cadence. This repository
applies that schema and owns the grants on it, which is why the privilege posture in
**Data Model** is stated here rather than upstream.

## Component Design

Built and in service:

| Component | Responsibility |
| :-- | :-- |
| `outbox.Dispatcher` (foundation-platform) | Claims, delivers, records the outcome, writes the receipt. Verifies its database contract before starting any worker and refuses to start when it is unmet. |
| `dispatch.HTTPPublisher` (foundation-reference) | Delivers one envelope; derives the evidence class from the consumer's reply rather than from its own judgement. |
| Delivery intake (foundation-reference) | Applies the event, then emits the application receipt on exactly the paths where the assertion holds. |
| `projection.FrontierReader` | Reports publication facts and unresolved security debt. Computes no verdict. |
| `projection.Registry` | Registers, retires, and refuses a second active projection consumer. |

Not built, and named so the absence is deliberate:

```
resolution endpoint
organization_resolution_rt and its scope wrapper
production replay path
```

## Data Model

`platform.dead_letter` holds the incident. It retains `aggregate_id` and `priority`
alongside the envelope so a row can be re-appended from itself; without them a replay
depended on the originating outbox partition, which retention eventually removes — the
incident record outliving the ability to act on it.

`platform.delivery_receipt` is keyed `(event_id, consumer)` and carries an `evidence` column
constrained to two values. It is the root of trust for resolution: a row asserting that this
event reached this consumer.

Privileges on both, derived from execution paths rather than convention:

```
platform.delivery_receipt   organization_dispatch_rt   INSERT, SELECT
                            organization_rt            none
                            organization_provider_rt   none
                            no role                    UPDATE, DELETE

platform.dead_letter        organization_dispatch_rt   INSERT, SELECT
                            organization_provider_rt   SELECT   (frontier debt facts)
                            organization_rt            none
```

`SELECT` for the dispatcher is not for reading incidents: `ON CONFLICT` with a conflict
target makes PostgreSQL require it on the table being inserted into. Measured per table
rather than inferred, because inferring it across tables is how a preflight shipped
asserting `INSERT` alone.

The request path holds nothing on either table. A caller able to insert a `consumer_applied`
receipt could forge the proof that closes a security debt, which makes forging the evidence
and forging the resolution the same act. New tables in the `platform` schema arrive closed:
the schema's default privileges grant nothing, and each table is named explicitly with the
path that earns it.

## API / Interface

`X-Application-Receipt: applied` — returned by the delivery intake, and the only thing that
produces `consumer_applied` evidence.

It is emitted when the projection row and the inbox guard committed, and on no other path.
A duplicate carries it, because the inbox guard finding the event already applied makes the
assertion true. A superseded outcome does not, because the event was discarded rather than
applied.

The producer cannot claim the class on the consumer's behalf: `outbox.Receipt` carries an
unexported field, so `consumer_applied` is reachable only through the constructor that takes
the consumer's marker. A broker cannot produce that marker, so introducing one degrades
receipts to `transport_accepted` and resolution stops finding proof — loudly, which is the
intended outcome rather than a regression to work around.

The resolution endpoint is not built. When it is, the consumer is derived server-side from
`projection.consumer` and never accepted from the request: the operator chooses the action,
the server chooses the subject the evidence must be about.

## Algorithms / Logic

### The resolution predicate

A dead letter `X` may be resolved as `REPLAYED` only if all of the following hold:

```
X is unresolved
a row exists in platform.delivery_receipt for X.event_id
that row's consumer is the active projection consumer, derived server-side
that row's evidence is 'consumer_applied'
```

No fallback, no scalar progress comparison, no operator assertion, no `transport_accepted`
receipt.

### Why scalar progress is not evidence

A consumer's highest applied mark does not establish that any particular event was applied.
`platform.outbox` allocates `sequence` at INSERT rather than at COMMIT, so positions are not
a contiguous acknowledgement: event 100 can be abandoned while 101 is applied, leaving a
reported mark of 101 that says nothing about 100. The same reasoning rules out
`snapshot_mark` — a row can be invisible to a snapshot while carrying a position below its
mark.

### Replay ordering

Replay the versions of one aggregate lowest first. When several events for the same
Membership are in `platform.dead_letter`, the order decides whether they can be recovered at
all:

```
dead letters v5, v6, v7 for one membership_id

v5, v6, v7  ->  each applied; all three resolvable
v7 first    ->  v6 and v5 then discarded as superseded, permanently unresolvable
```

Nothing enforces the order. It is an operator procedure, and getting it wrong is not
recoverable within this scope.

Before replaying, confirm the row can replay itself:

```sql
SELECT event_id, aggregate_id, priority, envelope IS NOT NULL AS replayable
  FROM platform.dead_letter
 WHERE resolved_at IS NULL
 ORDER BY dead_lettered_at;
```

A NULL `aggregate_id` or `priority` means the row predates their retention and its outbox
partition is gone. Such a row cannot be re-appended, and inventing either value is refused: a
guessed `aggregate_id` names a real aggregate somewhere, and the replay would deliver a
security event attributed to the wrong subject.

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

The consumer is in the correct state — v7 is newer and applied. What is missing is a
sanctioned way to say so, which is the `SUPERSEDED` contract.

Granting the receipt on that path would make the resolution reachable and would even reach a
safe state, but the audit record would read `REPLAYED / consumer_applied` for an event the
consumer discarded — a false statement in the table whose only purpose is explaining why a
security debt stopped blocking.

This is recorded rather than discovered during an incident, and it is the case that justifies
taking `SUPERSEDED` out of deferral when it is met.

## Configuration

| Setting | Effect |
| :-- | :-- |
| `DISPATCH_CONSUMER_NAME` | The consumer's name, not its endpoint. Delivery receipts are keyed by it, and the resolution predicate asks whether a specific consumer holds a specific event — which an endpoint cannot answer, because an endpoint moves and the identity does not. |
| `REFERENCE_CONSUMER_NAME` | The consumer's own identity for its inbox guard. Must equal the above and the name registered with this service. |

Nothing validates that the three agree: they are configured separately, and the dispatcher
holds no provider credential with which to check. A mismatch produces receipts no resolution
reads. The resolver's refusal must therefore name the mismatch rather than report an absence
of evidence.

## Testing Strategy

Each property is held by a named test, and the mutations listed have been observed red rather
than assumed.

| Property | Where it is proven |
| :-- | :-- |
| A second active projection consumer is refused, by registry and by constraint | `internal/projection/consumer_single_active_integration_test.go`; two CI mutations |
| The dispatcher holds exactly its declared privileges, and executes its statements with them | `internal/controldb/dispatch_role_integration_test.go` |
| New platform tables arrive closed | `internal/controldb/platform_privileges_integration_test.go`; CI mutation |
| A superseded delivery carries no application receipt | `foundation-reference/internal/httpapi/receipt_test.go`; CI mutation |
| Applied evidence cannot be claimed without the consumer's marker | `foundation-platform/outbox/receipt_integration_test.go`; CI mutation |
| A dispatcher whose database contract is unmet refuses to start | `foundation-platform/outbox/preflight_integration_test.go` |
| A dead letter retains enough to replay itself | `foundation-platform/outbox/replay_integration_test.go`; CI mutation |
| `dead_lettered_at` names the transition, not the transaction start | `foundation-platform/outbox/temporal_integration_test.go`; CI mutation |

Outstanding, and required before this scope is complete: the external A/B proof — a revoked
principal and an active one, both refused while the debt stands, the active one restored and
the revoked one still refused after resolution, with the refusal reason changing from debt to
withdrawal. A test asserting only that both remain refused would pass even if the revocation
never landed.

## Traceability

| Relationship | Reference |
| :-- | :-- |
| Parent system architecture | SAD-004 |
| Membership authority, revocation and the projection contract | TDD-organization-control-002 |
| Tenant isolation and the role model the grants extend | TDD-organization-control-001 |
| No outbound HTTP from this service, which places the dispatcher in foundation-reference | ADR-ORG-001 §5.4 |
| Migration integrity and schema drift gates | ADR-GLB-004 |

## Operational Notes

While an authority-bearing dead letter is unresolved, every projection-backed check refuses.
That is the designed behaviour and not an incident in itself: the alternative is serving
authority the consumer cannot vouch for. The incident is the undelivered event.

Recovery is: fix the cause the consumer refused for, replay the affected versions lowest
first, confirm each produced a `consumer_applied` receipt, then resolve. Skipping the
confirmation leaves a resolution with no evidence behind it, which the predicate refuses.

## Security Notes

The evidence chain is the control. Two properties carry it, and both are enforced rather than
documented: applied evidence cannot be produced without the consumer's own marker, and the
tables holding that evidence are not writable by the request path.

`delivery_receipt` accepts no `UPDATE` or `DELETE` from any role. A delivery worker able to
edit the evidence of its own deliveries is one whose bug rewrites the record that would have
shown it.

## Performance Notes

The frontier is polled by consumers, so its reads are deliberately not routed through the
provider scope wrapper: doing so would file a privileged-access record per poll and fill the
evidence table an investigation reads with rows about nothing.

Resolution is an operator action measured in minutes, not a request-path operation. No
latency budget applies to it, and none is claimed.

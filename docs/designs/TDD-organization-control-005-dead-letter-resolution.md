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
frontier reports security debt and every projection-backed enforcement check refuses —
so resolving one is the act that returns service, and resolving one wrongly is the act
that returns service to a consumer holding a revocation it never received.

This document records decisions already taken. It does not reopen them.

## Scope

One resolution reason is in scope:

```
REPLAYED
```

Three are explicitly out, and each carries a prerequisite that is not built:

| Reason | Why it is out |
| :-- | :-- |
| `RESNAPSHOTTED` | Requires generation replacement — a new generation built empty, dual-apply during the rebuild, atomic promotion, generation-scoped lookup, and a race proof. Implementing part of it is worse than none: a promotion that reads do not respect is a pointer swap, not a replacement. |
| `SUPERSEDED` | Requires domain-specific proof that a newer event closes the effect of the failed one. The version comparison alone is not that proof, it is the input to it. |
| `WAIVED` | An operational exception, not a correctness proof. Closing an incident is not the same as repairing the authority state, and one column serving both conflates them. |

The scope is also one producer and one projection consumer. `projection.consumer` refuses
a second active registration, and the database enforces it through a partial unique index
rather than only the registry — because security debt is reported estate-wide rather than
per consumer, so a second consumer's poison event would refuse the first consumer's
traffic.

## The predicate

A dead letter `X` may be resolved as `REPLAYED` only if all of the following hold:

```
X is unresolved
a row exists in platform.delivery_receipt for X.event_id
that row's consumer is the active projection consumer, derived server-side
that row's evidence is 'consumer_applied'
```

No fallback. No scalar progress comparison. No operator assertion. No
`transport_accepted` receipt.

The consumer is derived by the server from `projection.consumer`, never accepted from the
request: the operator chooses the action, the server chooses the subject the evidence must
be about.

### Why scalar progress is not evidence

A consumer's highest applied mark does not establish that any particular event was applied.
`platform.outbox` allocates `sequence` at INSERT rather than at COMMIT, so positions are
not a contiguous acknowledgement: event 100 can be abandoned while event 101 is applied,
leaving a reported mark of 101 that says nothing about 100. The same reasoning rules out
`snapshot_mark` — a row can be invisible to a snapshot while carrying a position below its
mark.

### What `consumer_applied` means

The consumer applies the event and its inbox guard in one transaction, and only then
returns `X-Application-Receipt: applied`. The dispatcher records the class from that header
and cannot name it otherwise: `outbox.Receipt` carries an unexported field, so the strong
class is reachable only through the constructor that requires the marker.

That is a property of the transport in place today, not of the type. A broker cannot
produce the consumer's marker, so introducing one degrades the receipts to
`transport_accepted` and resolution stops finding proof — loudly, which is the intended
outcome rather than a regression to work around.

## Known permanent-block condition

**A dead letter whose event has been superseded cannot be resolved in this scope.**

```
v5 revocation dead-lettered
v7 for the same Membership delivered and applied

operator replays v5
  -> the monotonicity guard discards it: excluded.membership_version > membership.membership_version
  -> Outcome.Superseded, no application receipt
  -> no consumer_applied evidence
  -> REPLAYED impossible, and the other three reasons are out of scope
  -> the debt blocks, and because debt is estate-wide, it blocks everything
```

The consumer is in the correct state — v7 is newer and is applied. What is missing is a
sanctioned way to say so, and that is the `SUPERSEDED` contract.

This is written down rather than discovered during an incident. It is the case that
justifies taking `SUPERSEDED` out of deferral, and the trigger for doing so is meeting it
in production, not predicting it.

### Why the receipt is withheld rather than granted

Granting it would make the resolution reachable, and the resolution would even reach a safe
state. But the audit record would read `resolution_type = REPLAYED, evidence =
consumer_applied` for an event the consumer discarded — a false statement in the table whose
only purpose is explaining why a security debt stopped blocking. Reaching a correct state
through a false record is not a cheaper correctness; it is the same defect with the evidence
removed.

## Replay procedure

**Replay the versions of one aggregate lowest first.**

When several events for the same Membership are in `platform.dead_letter`, the order
decides whether they can be recovered at all:

```
dead letters: v5, v6, v7 for one membership_id

replay v5 -> applied      (nothing newer is projected)
replay v6 -> applied      (v6 > v5)
replay v7 -> applied      (v7 > v6)
    all three resolvable

replay v7 first -> applied
replay v6       -> SUPERSEDED, permanently unresolvable
replay v5       -> SUPERSEDED, permanently unresolvable
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

`aggregate_id` or `priority` being NULL means the row predates their retention and its
originating outbox partition is gone. Such a row cannot be re-appended, and inventing either
value is refused: a guessed `aggregate_id` names a real aggregate somewhere, and the replay
would deliver a security event attributed to the wrong subject.

## Authority over the evidence

The evidence chain is writable by exactly the roles that produce it:

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

The request path holds nothing on either table. A caller able to INSERT a
`consumer_applied` receipt could forge the proof that closes a security debt, which makes
forging the evidence and forging the resolution the same act.

New tables in the `platform` schema arrive closed. The schema is versioned by
foundation-platform on its own cadence, so its default privileges grant nothing and each
table is named explicitly with the execution path that earns it.

## Out of scope, recorded

```
resolution endpoint and organization_resolution_rt   not built
production replay path                               not built
per-consumer debt attribution                        deferred; one consumer is enforced instead
delivery_receipt retention                           unbounded; needs the resolution contract first
```

## Evidence

| Property | Where it is proven |
| :-- | :-- |
| A second active projection consumer is refused, by registry and by constraint | `internal/projection/consumer_single_active_integration_test.go`, plus two CI mutations |
| The dispatcher holds exactly its declared privileges, and can execute its statements with them | `internal/controldb/dispatch_role_integration_test.go` |
| New platform tables arrive closed | `internal/controldb/platform_privileges_integration_test.go`, plus a CI mutation |
| A superseded delivery carries no application receipt | `foundation-reference/internal/httpapi/receipt_test.go`, plus a CI mutation |
| Applied evidence cannot be claimed without the consumer's marker | `foundation-platform/outbox/receipt_integration_test.go`, plus a CI mutation |
| A dispatcher whose database contract is unmet refuses to start | `foundation-platform/outbox/preflight_integration_test.go` |

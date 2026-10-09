# Runbook: dead-letter resolution

Version 1.1.0. Owner: Core Platform Team. Last reviewed 2026-10-09.

A consumer refused an event and the dispatcher dead-lettered it. A dead letter is keyed
`(event_id, consumer)`: an event refused by two consumers is two incidents, recovered once per
consumer (TDD-organization-control-005).

## Trigger

- `SecurityDebt` (critical): `organization_security_debt_dead_letters{consumer}` above 0. An
  authority-bearing event (Membership, Tenant or provider grant) is undelivered at that consumer,
  and every projection-backed check of that consumer refuses until it closes.
- `UnresolvedDeadLetterStale` (warning): a dead letter open over 24 hours with no live waiver.
- `organization-migrate -stage=maintenance` exits 3: unresolved dead letters older than 24 hours.
- `organization-migrate -stage=post` fails listing unclosable dead letters
  (`controldb.UnclosableDeadLetters`). See "Unclosable" below.

## Impact

- While the debt is open the consumer refuses. That is designed. The incident is the undelivered
  event, not the refusal (TDD-005 §Operational Notes).
- A standard-lane dead letter blocks nothing, and still holds receipt pruning and its payload.

## Diagnosis

1. List the consumer's incidents, as a provider:

   ```sh
   curl -sS "$OC/v1/projections/consumers/$CONSUMER/dead-letters?state=open&limit=100" \
     -H "Authorization: Bearer $TOKEN" -H "X-Administrative-Reason: INC-123 dead-letter diagnosis"
   ```

   `state` is `open` (unresolved, waived or not) or `resolved`; absent lists both. The page is
   `{"dead_letters": [...], "next": "<event_id>" | null}`, keyset on `event_id`, `limit` 1 to 100.
   Each item carries:
   - `event_id`, `consumer`, `event_type`, and `authority_bearing`: true for a Membership, Tenant or
     provider grant event, which is security debt;
   - `aggregate_id` and `version`: the Membership, Tenant or grant the event is about, and the
     version it carried (`membership_version`, `tenant_security_version` or `grant_version`, read
     from the stored payload). Null on a row that predates them, or whose payload was disposed of
     after a waiver;
   - `lane` (`priority` or `standard`, the lane a replay takes), `failure_class`, `failure_detail`,
     `attempts`, `first_failed_at`, `dead_lettered_at`;
   - `resolved_at`, `resolution_type`, `resolved_by`, `resolution_reference`;
   - `waived_at`, `waived_until`, `waived_by`, `waiver_reason`.

   Group by `aggregate_id` and order by `version`: the decision table turns on the highest version
   of each. A retired consumer is listed too; a consumer never registered answers `404`. Each page is
   recorded in `audit.privileged_access` with the reason.

2. Read `failure_detail`. It is the consumer's refusal. Fix that cause with the consumer's owner
   before replaying anything: a replay of an event the consumer still refuses dead-letters again.

3. Is the consumer active? `GET /v1/projections/consumers/{consumer}` as a provider.

4. Has a newer version of the same Membership been applied by that consumer? Read the Membership's
   latest transition as a provider, naming its Tenant (the payload's `tenant_id`, or the Tenant the
   incident is about):

   ```sh
   curl -sS "$OC/v1/tenants/$TENANT_ID/memberships/$MEMBERSHIP_ID/enforcement" \
     -H "Authorization: Bearer $TOKEN" -H "X-Administrative-Reason: INC-123 superseded check"
   ```

   If its `event_id` is not the dead letter's and `consumers` shows `consumer_applied` for this
   consumer, a newer version is applied: SUPERSEDED (B). For a Tenant event, or to see every version's
   receipt, read the receipts (SQL, read-only; no route reads them):

   ```sql
   SELECT e.event_id, e.membership_version, r.evidence, r.recorded_at
     FROM membership.membership_event e
     JOIN platform.delivery_receipt r ON r.event_id = e.event_id AND r.consumer = $CONSUMER
    WHERE e.membership_id = $MEMBERSHIP_ID
    ORDER BY e.membership_version DESC;
   ```

   For a Tenant, join `tenant.tenant_event` on `event_id` instead and read
   `tenant_security_version`.

## Decision table

| Situation | Act |
| :-- | :-- |
| Consumer active, the dead letter is the highest version of its Membership | Replay, confirm `consumer_applied`, resolve `REPLAYED` (Remediation A) |
| Consumer active, a newer version of the same Membership (or a higher `tenant_security_version` of the same Tenant) has `consumer_applied` from that consumer | Resolve `SUPERSEDED` directly, no replay (Remediation B) |
| Consumer retired | Waive (Remediation C). No replay can reach it |
| Replay answers `412` because the consumer no longer subscribes to the type | It was retired or resubscribed. Retired: waive (C). Active: resolve `SUPERSEDED` (B) if a newer version was applied; otherwise escalate, because no path closes it |
| Replay answers `412` because the row cannot re-append itself | Unclosable. See below |
| The consumer's projection must be rebuilt, not repaired | [Projection drift repair](projection-drift-repair.md), "Rebuild" |

`RESNAPSHOTTED` does not exist. It was closed by decision (ROADMAP backlog item 3); a rebuild under
a new consumer identity replaces it.

## Remediation

Every call below is a provider call with `X-Administrative-Reason`. An `Idempotency-Key` is honoured
and not required. Send one anyway: without it, a retried `resolve` answers `409` to a request that
succeeded.

**A. Replay, then resolve as `REPLAYED`.** Replay the highest dead-lettered version of each
Membership:

```sh
curl -sS -X POST "$OC/v1/dead-letters/$EVENT_ID/consumers/$CONSUMER/replay" \
  -H "Authorization: Bearer $TOKEN" -H "X-Administrative-Reason: INC-123 replay after fix" \
  -H "Idempotency-Key: $(uuidgen)"
```

`202` with `resolved: false`. Replay resolves nothing. Wait for the receipt. For a Membership
whose replayed event is its latest transition, the provider enforcement read of diagnosis 4 shows it
as `consumer_applied`; otherwise read it (SQL, read-only):

```sql
SELECT evidence, recorded_at FROM platform.delivery_receipt
 WHERE event_id = $EVENT_ID AND consumer = $CONSUMER;
```

Only `consumer_applied` is evidence. `transport_accepted` is not. Then resolve (no body means
`REPLAYED`):

```sh
curl -sS -X POST "$OC/v1/dead-letters/$EVENT_ID/consumers/$CONSUMER/resolve" \
  -H "Authorization: Bearer $TOKEN" -H "X-Administrative-Reason: INC-123 replayed and applied" \
  -H "Idempotency-Key: $(uuidgen)"
```

`412` means the receipt is missing. The message lists the receipts it did find as
`consumer (evidence)`. A receipt under another name means the consumer's configured name differs
from its registration; fix the name, do not retry the resolve.

**B. Resolve as `SUPERSEDED`.** For each lower version of the same Membership:

```sh
curl -sS -X POST "$OC/v1/dead-letters/$EVENT_ID/consumers/$CONSUMER/resolve" \
  -H "Authorization: Bearer $TOKEN" -H "X-Administrative-Reason: INC-123 superseded by v7" \
  -H "Content-Type: application/json" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"resolution_type":"SUPERSEDED"}'
```

The server finds the newer applied event. You do not name it. A terminal event (a revocation, or
`tenant.lifecycle.retired`) has nothing after it and can only close as `REPLAYED`.

**C. Waive a retired consumer's incident.**

```sh
curl -sS -X POST "$OC/v1/dead-letters/$EVENT_ID/consumers/$CONSUMER/waive" \
  -H "Authorization: Bearer $TOKEN" -H "X-Administrative-Reason: INC-123 consumer retired" \
  -H "Content-Type: application/json" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"reason":"consumer retired 2026-10-01, rebuilt as x-v2","expires_at":"2026-12-31T00:00:00Z"}'
```

- `expires_at` must be after now and at most 90 days ahead.
- `resolved: false`: a waiver is not a closure. It silences the stale alert until it expires, lets
  the payload be disposed 90 days after `waived_at`, and stops holding receipt pruning.
- It is refused `412` for an active consumer. Use A or B instead.
- When it expires and the row is still open, the alert returns. Waive again as a new decision.

**Unclosable.** A row with no `aggregate_id` or `priority`, no history row, and an active
consumer's name, or one naming no consumer, has no closure path. `-stage=post` refuses to deploy
while one is authority-bearing. No sanctioned closure exists, by decision (ROADMAP backlog
item 12). Do not update the row by hand. Escalate to the design owner.

## Verification

- `organization_security_debt_dead_letters{consumer}` returns to 0 and `SecurityDebt` clears.
- `GET /v1/projections/frontier` reads `security_debt: false`.
- `GET /v1/projections/consumers/{consumer_id}/dead-letters?state=open` no longer lists it, and
  with `state=resolved` it carries `resolved_at`, `resolution_type`, `resolved_by` and
  `resolution_reference`. `GET /v1/privileged-access?correlation_id=...` holds both the attempt and the
  outcome.
- The next `-stage=maintenance` run exits 0.

## Escalation

- The consumer's refusal: the consumer's owning team.
- `412` with no explanation in the message, or an unclosable row: Core Platform Team and the
  design owner of TDD-organization-control-005.

## Gaps

- No route reads `platform.delivery_receipt` beyond a Membership's latest transition, so a Tenant
  event's receipts, and an older version's, need SQL on the migration credential.
- A dead letter that names no consumer (`consumer` NULL, from before foundation-platform v0.2.8) is
  in no consumer's list. It counts in every consumer's debt; find it with SQL:
  `SELECT event_id, event_type, dead_lettered_at FROM platform.dead_letter WHERE consumer IS NULL AND resolved_at IS NULL;`.
- `version` comes from the stored payload. Once a waiver lets the maintenance stage dispose of the
  payload, it reads null.

## References

| # | Source |
| :-- | :-- |
| R1 | TDD-organization-control-005 §API / Interface (the list, 2.5.0), §The resolution predicate, §WAIVED, §Rebuilding a consumer, §The superseded case, §Operational Notes |
| R2 | Google, *Site Reliability Workbook*, "On-Call", <https://sre.google/workbook/on-call/>: "On-call engineers should update the playbook with fresh information when the corresponding page fires." |

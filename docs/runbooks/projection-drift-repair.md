# Runbook: projection drift repair

Version 1.0.0. Owner: Core Platform Team. Last reviewed 2026-10-07.

A consumer's projection of Memberships disagrees with authority. Reconciliation finds the
difference and publishes a repair. Authority is never changed from a projection: repair runs one
way, toward authority (TDD-organization-control-002 §Reconciliation).

## Trigger

- A reconciliation run returns findings. `GET /v1/projections/consumers/{consumer_id}` shows
  `last_reconciled_findings` above 0.
- A person is refused a context they hold, or admitted to one they do not, at a consumer, while
  `POST /v1/context/verify` (`{"consumer_id", "tenant_id", "principal_id"}`) answers the opposite in `granted`.
- `reconciliation_age_seconds` on a consumer is older than the team's sweep interval.

## Impact

| Finding | Meaning | Severity |
| :-- | :-- | :-- |
| `missing` | Authority grants a context the consumer does not project. Access is wrongly denied | Ticket |
| `mismatch` | Both know the Membership at different versions | Ticket; security if the consumer's version is higher |
| `extra` | The consumer projects a context authority does not grant. Somebody holds access nothing granted | Security incident, always |

Findings are sorted security first. An `extra` is escalated as a potential privilege escalation,
because reaching it needs a defect in the projection path or a write outside it.

## Diagnosis

1. Get the consumer's report: its active Memberships as `{membership_id, membership_version}` at a
   stated position (`mark`). The consumer produces it. For identity-control it is
   `GET /v1/projections/tenant-context/report` on the Identity Control API (provider-only), which
   answers `{"consumer_id", "mark", "rows": [{"membership_id", "membership_version"}]}`.
2. Check the consumer's registration and subscription:

   ```sh
   curl -sS "$OC/v1/projections/consumers/$CONSUMER" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 drift check"
   ```

   Note `event_types`. A consumer that does not subscribe to
   `com.scnehaux.organization.projection.repair.reconciled` never receives a repair; only its next
   bootstrap corrects it.

## Remediation

**A. Reconcile.** Send the report as it came:

```sh
curl -sS -X POST "$OC/v1/projections/reconcile" -H "Authorization: Bearer $TOKEN" \
  -H "X-Administrative-Reason: INC-123 reconcile identity-control" -H "Content-Type: application/json" \
  -d @report.json
```

- `mark` is required and must be above 0. A report without its position would turn ordinary
  progress into findings.
- The response is `{consumer_id, mark, run_at, findings}`. Each finding carries `classification`,
  `membership_id`, `tenant_id`, `principal_id`, `authoritative_version`, `projected_version`, and
  `state`: the authoritative Membership, or null for an `extra` authority never granted.
- The same call publishes `projection.repair.reconciled` to subscribers, and records
  `last_reconciled_at`, `_mark` and `_findings` on the consumer. A clean run is recorded too.
- Repeating it against the same report finds the same findings. The repair is applied by version,
  so a second one changes nothing.

**B. Act on each finding.**

| Finding | Act |
| :-- | :-- |
| `missing`, `mismatch` with authority higher, `extra` with authority holding a withdrawn Membership | The repair carries the state. The consumer applies it by higher-version-wins. Wait for its next report and reconcile again |
| `extra` with `state` null | The repair tells the consumer to remove the row. Open a security incident anyway: find out how the row got there before closing it |
| `mismatch` or `extra` with `projected_version` above `authoritative_version` | The consumer is ahead of authority. It is corrupt, and no repair applies. Rebuild it (C) and open a security incident |
| Findings return after a repair the consumer applied | The projection path has a defect. Rebuild (C) and raise a defect with the consumer's owner |

**C. Rebuild under a new identity** (TDD-005 §Rebuilding a consumer). It is an outage: between steps
1 and 4 the consumer serves nothing projection-backed.

1. `POST /v1/projections/consumers/{old}/retire` (`Idempotency-Key` required). Its subscription
   is retired and what it was owed is abandoned.
2. `POST /v1/projections/consumers` with a new `consumer_id`, its workload `principal_id` (a new
   workload: a `principal_id` names one consumer for good), `projection_version`,
   `max_accepted_age_seconds`, `stale_behavior` and `event_types` (`Idempotency-Key` required).
3. Point every name at it: `ORGANIZATION_DELIVERY_TARGETS` here, and the consumer's own
   configuration.
4. The consumer bootstraps: `POST /v1/projections/consumers/{new}/bootstrap` with `mark`, then
   `POST /v1/projections/snapshot` pages, from an empty projection.
5. Waive the old identity's open dead letters ([Dead-letter resolution](dead-letter-resolution.md), C).

For a consumer whose projection is merely behind, not corrupt, a re-bootstrap under the same
identity is enough. A re-bootstrap may move the mark forward and never backward.

## Verification

- Reconcile again with a fresh report. `findings` is empty, and the consumer read shows
  `last_reconciled_findings` 0 and a new `last_reconciled_at`.
- For an `extra`: `POST /v1/context/verify` for that principal and Tenant answers `"granted": false` at the
  consumer and here.

## Escalation

- Any `extra`, or any consumer ahead of authority: security incident, security lead notified.
  Preserve the consumer's report and the reconcile response.
- A defect in the projection path: the consumer's owning team.

## Gaps

- `Result.SecurityFindings` exists and nothing calls it. An `extra` finding is not logged or
  alerted by this service, although TDD-002 §Operational Notes lists it as critical at any
  occurrence. Whoever runs reconciliation must read the response.
- Nothing here runs reconciliation on a schedule. A consumer, or an operator, calls it.
- Reconciliation compares Memberships only. Tenant and provider grant projections are corrected
  by their events and by bootstrap.

## References

| # | Source |
| :-- | :-- |
| R1 | TDD-organization-control-002 §Reconciliation, §Consumer Registry, §Bootstrap Contract, §Operational Notes |
| R2 | TDD-organization-control-005 §Rebuilding a consumer |
| R3 | NIST SP 800-61r3, §2.3, <https://doi.org/10.6028/NIST.SP.800-61r3>: "Playbooks provide actionable steps or tasks for people to perform during various scenarios or situations." |

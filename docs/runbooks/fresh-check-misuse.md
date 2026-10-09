# Runbook: consumer misuse of the fresh-check path

Version 1.0.0. Owner: Core Platform Team. Last reviewed 2026-10-09.

`POST /v1/context/verify` is the authoritative fresh check. It is reserved for high-risk and
irreversible operations and for a consumer whose `stale_behavior` is `revalidate` and whose projection
is past its age (TDD-organization-control-002 §Staleness Policy). "A consumer whose `:verify` rate
approaches its request rate has misclassified its operations, and that is treated as a defect rather
than as load" (TDD-002 §Enforcement Evidence). This runbook finds the consumer and gets it back to its
projection.

## Trigger

- `ConsumerFreshCheckRateWarning` (warning): `organization_projection_consumer_verify_ratio > 0.05`,
  labelled with the `consumer`. 0.05 is `ORGANIZATION_VERIFY_RATE_ALERT`'s default.
- `ConsumerFreshCheckRateCritical` (critical): above 0.5, ten times the threshold
  (TDD-002 §Operational Notes).

## Impact

- Each fresh check is a synchronous read of authority on the consumer's request path (p95 target
  200 ms) and an `UPDATE` of the consumer's own registry row, so a consumer's checks serialise on
  that row (TDD-002 §How the verify rate is measured). The contention is confined to the consumer.
- The consumer's availability now depends on this service's, which is the universal synchronous
  dependency EAD-002 rules out. An outage here becomes an outage there.
- Usually not a security exposure. A fresh check answers from authority, so it errs toward
  correctness. A rising ratio together with refused checks can be a consumer probing contexts it
  does not hold: every check is metered, refused ones included.

## Diagnosis

1. Which consumers, at which ratio. As a provider:

   ```sh
   curl -sS "$OC/v1/context/rate/over-threshold?threshold=0.05" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 fresh-check rate"
   ```

   `{"rates": [{"consumer_id", "verify_calls", "requests", "ratio"}]}`. `ratio` is the last reported
   interval's checks per request; `requests` is what the consumer reported for that interval;
   `verify_calls` is the checks counted since that report. `threshold` defaults to 0.5.
2. How the ratio is measured. This service counts each check; the denominator is the request count
   reported with `POST /v1/context/rate` `{"consumer_id", "requests"}`, and each report closes the
   interval and resets the count. An interval reporting 0 requests leaves the ratio null.
3. The consumer's freshness. `GET /v1/projections/consumers/{consumer_id}`: `max_accepted_age_seconds`,
   `stale_behavior`, `last_reported_at`. A consumer with `revalidate` whose projection is stale calls
   the fresh check for every decision by design: that is a stale projection, not misuse
   ([revocation not enforced within budget](revocation-not-enforced.md) for why it is behind).
4. Is it probing? Ask the consumer's owner for its refused checks (`"granted": false`) in the window.
   Many refusals over many principals or Tenants is not misclassification.

## Decision table

| Finding | Cause | Act |
| :-- | :-- | :-- |
| `stale_behavior` is `revalidate` and the consumer is stale | The projection is behind, and the policy falls back to the fresh check | Fix the lag (A) |
| Fresh projection, high ratio since a release of the consumer | An ordinary path calls the fresh check | The consumer fixes its classification (B) |
| `requests` reported far below the consumer's real traffic | The consumer under-reports its denominator | The consumer fixes its rate report (B) |
| Many refused checks across principals or Tenants | Probing | Contain the consumer (C), security lead |

## Remediation

- **A. Projection behind.** Work [revocation not enforced within budget](revocation-not-enforced.md)
  diagnosis 1 and 3 for that consumer. The ratio falls when its reports are fresh again.
- **B. Misclassified operations.** The consumer's owner moves ordinary reads back to its projection
  and keeps the fresh check for the operations TDD-002 §Staleness Policy names. Nothing here
  throttles a consumer: the signal is the control, and the fix is in the consumer.
- **C. Probing.** Retire the consumer's registration
  (`POST /v1/projections/consumers/{consumer_id}/retire`, provider, `Idempotency-Key`). Its token is
  refused from the next request. It is an outage for what the consumer serves, so take it with the
  consumer's owner and the security lead.

## Verification

- The next interval's `ratio` in `GET /v1/context/rate/over-threshold?threshold=0.05` is below the
  threshold, or the consumer is absent from the list, and the alerts clear.

## Escalation

- Misclassification or under-reporting: the consumer's owning team, as a defect.
- Probing: security lead.

## Gaps

- The ratio is only as good as the request count the consumer reports. A consumer that stops
  reporting keeps its last ratio until it reports again.
- The counter does not separate granted from refused checks, so probing is established from the
  consumer's side.
- The one-interval warning on reconciliation age and an alert on a consumer that stops reporting its
  rate are not built: no reporting interval is declared per consumer.

## References

| # | Source |
| :-- | :-- |
| R1 | TDD-organization-control-002 §Enforcement Evidence, §How the verify rate is measured, §Staleness Policy, §Configuration (`ORGANIZATION_VERIFY_RATE_ALERT`), §Operational Notes |
| R2 | Google, *Site Reliability Workbook*, "On-Call", <https://sre.google/workbook/on-call/>: "Playbooks contain high-level instructions on how to respond to automated alerts. They explain the severity and impact of the alert, and include debugging suggestions and possible actions to take to mitigate impact and fully resolve the alert." |

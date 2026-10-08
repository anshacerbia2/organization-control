# Runbook: revocation not enforced within budget

Version 1.0.0. Owner: Core Platform Team. Last reviewed 2026-10-07.

A Membership revocation, suspension, or Tenant suspension was accepted here and has not reached
every subscribed consumer within the propagation budget. Access that authority has withdrawn may
still be served.

## Trigger

Any of:

- `PriorityOutboxLagWarning` (over 30 s) or `PriorityOutboxLagCritical` (over 2 min), labelled
  with the `consumer` that is behind.
- `SecurityDebt` (critical): an authority-bearing event was dead-lettered at a consumer. Go to
  [Dead-letter resolution](dead-letter-resolution.md) and come back for verification.
- `ConsumerProjectionStale` (critical): a consumer has not reported progress within its declared
  `max_accepted_age_seconds`.
- `EnforcementTelemetryAbsent` (critical): no enforcement metric for 10 minutes. Every alert above
  is blind. Treat it as this runbook until the exporter is back.
- A Tenant administrator reports that `GET /v1/memberships/{membership_id}/enforcement` reads
  `over_budget`.

## Impact

- The budget is TDD-organization-control-002 §Enforcement Budget: accept to outbox commit 100 ms,
  commit to dispatch claim 1 s, dispatch to Keycloak projection 2 s, dispatch to a consumer read
  model 5 s. Propagation subtotal under 10 s (`membership.PropagationBudget`).
- Past it, a consumer may still admit a person whose Membership is revoked. After propagation,
  access tokens already issued live out their remaining lifetime (STD-IAM-002 token profile).
- A consumer with `stale_behavior: fail_closed` refuses once its projection is older than its
  budget. That is the designed failure: an outage, not an exposure.
- Incident response works from the enforced time, not the accepted time (TDD-002 §Security Notes).

## Diagnosis

1. Which consumer, and how far behind. As a provider:

   ```sh
   curl -sS "$OC/v1/projections/frontier" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 revocation over budget"
   curl -sS "$OC/v1/projections/consumers?state=active" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 revocation over budget"
   ```

   - The frontier is the estate's: `oldest_unpublished_age_seconds`, `unpublished`,
     `security_dead_lettered`, `security_debt`.
   - Each consumer item carries `stale`, `last_reported_at`, `last_reported_mark` and
     `max_accepted_age_seconds`.

2. One Membership's evidence. Only a Tenant administrator of that Tenant can read it:

   ```sh
   curl -sS "$OC/v1/memberships/$MEMBERSHIP_ID/enforcement" -H "Authorization: Bearer $TENANT_TOKEN"
   ```

   It returns `event_id`, `accepted_at`, `published_at`, `budget_seconds` (10), `state`, and per
   consumer `evidence` (`pending`, `transport_accepted`, `consumer_applied`, `dead_lettered`) with
   `recorded_at`.

3. Is this process delivering? Read the service log for each consumer in
   `ORGANIZATION_DELIVERY_TARGETS`:

   - `delivering to the consumer`: the dispatcher runs.
   - `the consumer is not an active registered consumer; its dispatcher waits for the registration`.
   - `the consumer's registration could not be checked; the dispatcher waits`.
   - `the consumer's dispatcher stopped; it starts again after the retry interval`.

   A consumer that is not in `ORGANIZATION_DELIVERY_TARGETS` (today foundation-reference) is
   delivered by its own dispatcher. Check that process instead.

4. What the deliveries say (SQL, read-only, see README §Conventions):

   ```sql
   SELECT consumer, priority, count(*), min(created_at), max(attempts),
          max(last_error), max(failure_class), min(next_attempt_at)
     FROM platform.outbox_delivery
    WHERE NOT published
    GROUP BY consumer, priority;
   ```

## Decision table

| Finding | Cause | Go to |
| :-- | :-- | :-- |
| `security_debt` true, or `evidence: dead_lettered` | The consumer refused the event | [Dead-letter resolution](dead-letter-resolution.md) |
| Unpublished rows with rising `attempts` and a `last_error` naming a timeout, refused connection or 5xx | The consumer endpoint is down or slow. An outage never dead-letters; the dispatcher keeps retrying | Remediation A |
| `last_error` or the service log names the workload token or the token endpoint | This service cannot get its workload token | Remediation B |
| Log says the dispatcher waits for the registration | The consumer is retired or its name does not match | Remediation C |
| No `delivering to the consumer` line and no unpublished rows are being claimed | The dispatcher is not running | Remediation D |
| Rows are published (`published_at` set) and `evidence` is `transport_accepted` or `pending`, consumer report age rising | Delivered, not applied: the consumer is behind | Remediation E |
| `EnforcementTelemetryAbsent` | No metrics reach the Collector | Remediation F |

## Remediation

- **A. Consumer endpoint down.** Page the consumer's owner (identity-control or the product
  consumer). Nothing to replay: rows stay owed and are delivered when the endpoint answers. Do not
  retire the consumer to clear the lag: retiring abandons what it was owed
  (TDD-002 §Consumer Registry).
- **B. Workload token.** Check `ORGANIZATION_WORKLOAD_CLIENT_ID`, `ORGANIZATION_WORKLOAD_KEY_FILE`
  and `ORGANIZATION_WORKLOAD_TOKEN_URL` against the kernel (TDD-005 §Configuration). Restart the
  service after a fix.
- **C. Registration.** `GET /v1/projections/consumers/{consumer_id}`. If it is retired by mistake,
  register it again (`POST /v1/projections/consumers`, `Idempotency-Key` required); the consumer
  must bootstrap again before its progress is accepted. If the name is wrong, correct
  `ORGANIZATION_DELIVERY_TARGETS`.
- **D. Dispatcher not running.** Restart the service. A crashed dispatcher's leased rows wait up to
  30 s, then are claimed again (ROADMAP backlog item 11).
- **E. Consumer behind.** The consumer's owner works its intake. While it is behind, its own stale
  policy decides what it serves. If a single high-risk decision cannot wait, the consumer calls
  `POST /v1/context/verify`, the authoritative fresh check.
- **F. Telemetry.** Check `OTEL_EXPORTER_OTLP_ENDPOINT` (startup logs a warning when unset) and the
  Collector. Until it is back, run diagnosis step 1 every 5 minutes by hand.

If enforcement cannot be restored and the exposure is serious, contain it at the source. Suspend
the Tenant (`POST /v1/tenants/{tenant_id}/suspend` with `expected_version`, provider,
`Idempotency-Key`). That is a priority `tenant.security.suspended` event and withdraws every
member, so it is a business decision. Take it with the Tenant's owner and record it.

## Verification

- The alert clears, and the frontier reads `unpublished` 0 on the priority lane or falling.
- The enforcement read reads `enforced`, with `consumer_applied` for every consumer.
- Record in the incident: `accepted_at`, the last consumer's `recorded_at`, and the difference.
  That difference is the enforcement delay to report, not the time the alert cleared.

## Escalation

- Consumer endpoint or intake: the consumer's owning team.
- Dispatcher, token or this process: Core Platform Team on call.
- Over 2 minutes on the priority lane with a revocation outstanding: security incident. Notify the
  security lead. NIST SP 800-61r3 treats response as part of risk management and asks for
  documented procedures for urgent processes [R2].

## Gaps

- No alert reads accept-to-enforcement directly. TDD-002 §Operational Notes lists it as a signal;
  the rules alert on its parts (lag, debt, report age).
- A provider cannot read the enforcement route; it is Tenant-scoped. Diagnosis of one Membership
  needs the Tenant administrator or SQL.

## References

| # | Source |
| :-- | :-- |
| R1 | Google, *Site Reliability Workbook*, "On-Call", <https://sre.google/workbook/on-call/>: "Playbooks contain high-level instructions on how to respond to automated alerts. They explain the severity and impact of the alert, and include debugging suggestions and possible actions to take to mitigate impact and fully resolve the alert." |
| R2 | NIST SP 800-61r3, §2.3, <https://doi.org/10.6028/NIST.SP.800-61r3>: "organizations should also develop and maintain procedures for particularly important processes that may be urgently needed during emergency situations" |
| R3 | TDD-organization-control-002 §Enforcement Budget, §Enforcement Evidence, §Consumer Registry, §Operational Notes |
| R4 | TDD-organization-control-005 §Configuration; `observability/alerts/organization-control.rules.yml` |

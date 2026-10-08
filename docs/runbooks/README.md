# Organization Control runbooks

Version 1.0.0. Owner: Core Platform Team. Last reviewed 2026-10-07.

These are the five runbooks the production gate requires (ROADMAP §Gates). Each one answers an
alert or a signal this service already emits, with the reads and the commands this service already
serves.

| Runbook | Answers |
| :-- | :-- |
| [Revocation not enforced within budget](revocation-not-enforced.md) | `PriorityOutboxLag*`, `SecurityDebt`, `ConsumerProjectionStale`, an enforcement read of `over_budget` |
| [Projection drift repair](projection-drift-repair.md) | a reconciliation finding, a consumer serving the wrong context, a consumer ahead of authority |
| [Provider-access review](provider-access-review.md) | the scheduled review of provider authority, and each use of an emergency grant |
| [Stuck offboarding](stuck-offboarding.md) | an offboarding that does not reach `retired` or `cancelled` |
| [Dead-letter resolution](dead-letter-resolution.md) | `SecurityDebt`, `UnresolvedDeadLetterStale`, `organization-migrate -stage=maintenance` exiting 3 |

## Why these exist

- NIST SP 800-61r3 asks organizations to "develop and maintain procedures for particularly
  important processes that may be urgently needed during emergency situations" [R1].
- STD-GLB-004 §3.15: "Alerts **MUST** have an owner and runbook action" [R2]. Each alert rule in
  `observability/alerts/organization-control.rules.yml` maps to one runbook above.
- Google SRE: "whenever an alert is created, a corresponding playbook entry is usually created" [R3].

## Conventions

- Every route, header, body field, metric and table named here exists in this repository. A
  runbook that names something the code does not serve is a defect. Fix the runbook in the same
  change that changes the code.
- Every provider route needs a provider token and `X-Administrative-Reason`. The reason is written
  to `audit.privileged_access`, so write it as a sentence a reviewer can read: the incident or ticket
  and what you are doing.
- Every command (a `POST` in `internal/httpapi/commands.go` that is not in `keyOptional`) needs an
  `Idempotency-Key`. Use one new value per command. Send the same value again only to retry the same
  command. The dead-letter replay, resolve and waive routes, the provisioning reports, the sweeps
  and reconcile honour a key and do not require one.
- `$OC` is the service base URL. `$TOKEN` is a provider access token. Examples use `curl`.
- Direct SQL reads use the migration credential (`ORGANIZATION_MIGRATION_DATABASE_URL`). Use
  `SELECT` only, inside `BEGIN READ ONLY;`, and paste the statement and its output into the
  incident record. No runtime role can read `audit.privileged_access` or list dead letters, which is
  why these reads are not API calls (see each runbook's "Gaps").
- Severity follows the alert rule. A `critical` alert is a page. A `warning` alert is a ticket for
  the next working day.
- After each use, update the runbook with what was missing. "Details in playbooks go out of date at
  the same rate as production environment changes" [R3].

## References

| # | Source |
| :-- | :-- |
| R1 | NIST SP 800-61r3, *Incident Response Recommendations and Considerations for Cybersecurity Risk Management*, §2.3, <https://doi.org/10.6028/NIST.SP.800-61r3>: "organizations should also develop and maintain procedures for particularly important processes that may be urgently needed during emergency situations" and "Playbooks provide actionable steps or tasks for people to perform during various scenarios or situations." |
| R2 | STD-GLB-004 *Event-Driven Architecture* 3.0.1, §3.15, scnehaux-architecture `02-standards/_global/STD-GLB-004-event-driven.md`: "Alerts **MUST** have an owner and runbook action." |
| R3 | Google, *Site Reliability Workbook*, "On-Call", <https://sre.google/workbook/on-call/>: "In SRE, whenever an alert is created, a corresponding playbook entry is usually created. These guides reduce stress, the mean time to repair (MTTR), and the risk of human error." and "Details in playbooks go out of date at the same rate as production environment changes." |
| R4 | Google, *Site Reliability Engineering*, "Introduction", <https://sre.google/sre-book/introduction/>: "thinking through and recording the best practices ahead of time in a 'playbook' produces roughly a 3x improvement in MTTR as compared to the strategy of 'winging it.'" |

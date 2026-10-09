# Runbook: suspected cross-tenant exposure

Version 1.0.0. Owner: Core Platform Team; incident policy: Security. Last reviewed 2026-10-09.

A Tenant's data or authority may have been read or changed by someone acting for another Tenant, or
by a provider without a recorded reason. SAD-004 §9.2 treats "a cross-tenant authorization defect" as
a Sev-1 enterprise incident, and §9.1.1 prescribes "emergency provider-admin disable and affected
Tenant containment" for a cross-tenant policy defect. Security owns the incident policy for
cross-tenant exposure (SAD-004 §9.3.4). This runbook is the Core Platform Team's part: contain, keep
the evidence, establish the extent.

## Trigger

Any of:

- `IsolationWithCheckRejection` that [WITH CHECK rejection triage](with-check-rejection.md) could not
  explain as a single defect.
- `/readyz` failing on the isolation posture, or a replica refusing to start on it: the log lines
  `readiness failed` or `isolation posture` (TDD-organization-control-001 §Verifying the Posture at
  Runtime). The posture check found a table without `FORCE`, a missing or undeclared policy, or a
  runtime role holding `BYPASSRLS`, `SUPERUSER` or a table's ownership.
- A reconciliation `extra` finding (`ReconciliationExtraFinding`) whose `tenant_id` the consumer
  served to a principal outside that Tenant.
- A Tenant administrator reports, from `GET /v1/provider-access`, provider access it did not expect,
  or reports seeing a record of another Tenant.
- A provider-access review escalates (`POST /v1/privileged-access/reviews` with outcome
  `escalated`; [provider-access review](provider-access-review.md)).

## Impact

- Potentially every Tenant. Until the extent is known, treat it as estate-wide.
- A posture failure means Row-Level Security may not hold for the role and table it names: reads may
  return another Tenant's rows without any error.
- A provider read without a recorded reason would be outside PAD-PLT-002 §3.3 invariant 22. Every
  provider transaction records first or does not run (`db.WithProviderScope`), so a missing record
  points at a path outside the wrappers.

## Diagnosis

1. Open a Sev-1 and notify the security lead before anything else.
2. Preserve the evidence. Export the privileged-access record for the window, page by page, with
   the incident in every reason (each page is itself recorded):

   ```sh
   curl -sS "$OC/v1/privileged-access?from=2026-10-01T00:00:00Z&to=2026-10-09T00:00:00Z&limit=100" \
     -H "Authorization: Bearer $TOKEN" -H "X-Administrative-Reason: INC-123 Sev-1 evidence export"
   ```

   Filters: `actor_id`, `tenant_id`, `correlation_id`, `authority` (`emergency`, `activation`,
   `eligible`, `consumer`), `from` (inclusive) and `to` (exclusive), RFC 3339 with an offset. Each
   row carries `access_id`, `actor_id`, `authority`, `activation_id`, `tenant_id`, `operation` (the
   route pattern), `correlation_id`, `reason`, `occurred_at`. Nothing purges the record, and no
   runtime role can update or delete it.
3. Establish the posture. `curl -sS $OC/readyz`; the service log names each problem the check found.
4. Establish the extent for each affected Tenant:
   - provider access that named it: `GET /v1/privileged-access?tenant_id=$TENANT_ID`;
   - what its administrators see: `GET /v1/provider-access` with a Tenant administrator's token for
     that Tenant;
   - the requests in the incident window, from the request log by `correlation_id`.
5. For a projection finding, the consumer's report and the reconcile response are evidence: keep
   both ([projection drift repair](projection-drift-repair.md)).

## Decision table

| Finding | Act |
| :-- | :-- |
| Posture failure on a table or role | Take the affected replicas out (readiness already does), restore the posture (A) |
| One actor reaching other Tenants | Contain the actor (B) |
| A defect in a route that lets a request name the Tenant it writes or reads | Contain the route (C), fix, then establish who used it |
| A consumer serving a context across Tenants | Reconcile and rebuild it ([projection drift repair](projection-drift-repair.md), C), and contain at the source (D) |
| Extent unknown and the boundary may still be open | Contain broadly (D) |

## Remediation

- **A. Restore the posture.** Rerun the migrate job (`organization-migrate -stage=post`): `rls.sql`
  recreates every policy and `FORCE`, and `grants.sql` the privileges, on every run. The replica
  returns to the load balancer when `/readyz` passes. Find out how the posture changed: no runtime
  role holds DDL, so a change came from the migration credential or a superuser.
- **B. Contain the actor.** A provider: revoke the grant (`POST /v1/provider-grants/{grant_id}/revoke`)
  or end the activation (`POST /v1/provider-activations/{activation_id}/end`). The authority read
  stops the next request. A Tenant administrator: revoke the administration grant
  (`POST /v1/tenants/{tenant_id}/administrators/{grant_id}/revoke`). A consumer: retire it
  (`POST /v1/projections/consumers/{consumer_id}/retire`; it stops being served, and is an outage for
  what it serves). Each is a provider command: `X-Administrative-Reason` and `Idempotency-Key`.
- **C. Contain the route.** Roll back the release. If the route is a Tenant administrator's command,
  pause Tenant administration meanwhile:
  `POST /v1/tenant-administration-pause` `{"paused":true}` (provider, reason, `Idempotency-Key`).
  Reads continue; lift it with `{"paused":false}`.
- **D. Contain the affected Tenants.** Suspend each:
  `POST /v1/tenants/{tenant_id}/suspend` `{"expected_version": n}`. That publishes the priority
  `tenant.security.suspended`, withdrawing every member at every consumer. It is a business decision,
  taken with the Tenant's owner and recorded in the incident.

## Verification

- `/readyz` passes on every replica and the startup log reads `tenant isolation verified`.
- The contained actor's next request is refused `403`.
- `GET /v1/privileged-access?actor_id=$ACTOR&from=<containment time>` lists no access after
  containment except the incident's own reads.
- A reconciliation of each consumer involved finds no `extra`.

## Escalation

- Security lead from the first minute. Security owns notification of affected customers and any
  regulator.
- A posture change nobody can explain: treat the migration credential as compromised; rotate it.

## Gaps

- The access record names a Tenant only when the access named one. A list across Tenants, or a
  record addressed by its own identifier, records none ([provider-access review](provider-access-review.md),
  Gaps): read those by `operation` and `correlation_id`.
- Tenant-scoped requests write no privileged-access row. Their trail is the request log, kept as
  long as the log platform keeps it.
- No route reads the posture report itself; it is in the service log.

## References

| # | Source |
| :-- | :-- |
| R1 | SAD-004 §9.1.1 (cross-tenant policy defect), §9.2 (blast radius), §9.3.3 (required runbook "suspected cross-tenant data exposure"), §9.3.4 (Security owns incident policy for cross-tenant exposure) |
| R2 | TDD-organization-control-001 §Policy, §Verifying the Posture at Runtime, §Privileged Access Review, §Operational Notes |
| R3 | NIST SP 800-61r3, §2.3, <https://doi.org/10.6028/NIST.SP.800-61r3>: "organizations should also develop and maintain procedures for particularly important processes that may be urgently needed during emergency situations" |
| R4 | PostgreSQL 17, *Row Security Policies*, <https://www.postgresql.org/docs/17/ddl-rowsecurity.html>, accessed 2026-10-09: "Table owners normally bypass row security as well, though a table owner can choose to be subject to row security with ALTER TABLE ... FORCE ROW LEVEL SECURITY." |

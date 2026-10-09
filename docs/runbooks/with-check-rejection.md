# Runbook: `WITH CHECK` rejection triage

Version 1.0.0. Owner: Core Platform Team. Last reviewed 2026-10-09.

The database refused a write because the row belonged to a Tenant the transaction was not bound
to. Every tenant-scoped policy carries `WITH CHECK` mirroring `USING`, so a bound caller cannot
insert or update a row into another Tenant (TDD-organization-control-001 §Policy). The write did
not happen. That code tried to make it is a defect or an attack, and TDD-001 §Operational Notes
treats it as a security finding at any count, not as a validation error.

## Trigger

- `IsolationWithCheckRejection` (critical):
  `increase(organization_isolation_refusals_total{control="with_check"}[15m]) > 0`.
- The ERROR log line `an isolation control refused a statement` with `control=with_check`, `method`,
  `route` and `error` (`new row violates row-level security policy for table "..."`).
- A `500` (`https://problems.scnehaux.com/internal`) whose `correlation_id` leads to that line.

## Impact

- The write was refused, so no row crossed a Tenant boundary through this statement.
- Something tried. Either a handler or service built a row with a Tenant other than the bound one,
  or a caller found a way to steer the Tenant a write names. The second is an attack on the
  isolation boundary, and the first is the defect that would make one possible.
- The policy is the second line of defence. The first is that a tenant route takes its Tenant from
  the token and never from the request (TDD-001 §Scope Resolution). A rejection means the first line
  failed for that request.

## Diagnosis

1. Read the log line and its `correlation_id`. Note the `route` pattern and the table the error
   names.
2. Who sent it. For a provider route, the privileged-access record names the actor and the reason:

   ```sh
   curl -sS "$OC/v1/privileged-access?correlation_id=$CORRELATION" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 WITH CHECK rejection"
   ```

   A tenant route writes no privileged-access row. Its caller is in the request log under the same
   correlation identifier: the token's `principal_id` and `tenant_id`.
3. Was it one request or a pattern? Query the counter over a day, and search the logs for the same
   actor or the same route. Several Tenants named by one actor is probing.
4. Which Tenant did the row name, and which one was bound? The error names only the table. Reproduce
   from the request (body and path) in a non-production environment, or read the code path from the
   route pattern: the Tenant a tenant-scoped service writes must come from `db.ScopeFrom`, and a
   provider's act inside one Tenant from the path, through `db.WithProviderInTenant`.

## Decision table

| Finding | Cause | Act |
| :-- | :-- | :-- |
| One request, a code path that builds the Tenant from the request | Defect: a requested scope used as the authoritative one | Contain the route (A), fix (C) |
| Repeated, one actor, different Tenants in the body or path | Probing the boundary | Contain the actor (B), then [suspected cross-tenant exposure](cross-tenant-exposure.md) |
| Since a deploy, every request on the route | Defect introduced by the release | Roll back (A), fix (C) |
| Readiness also failing on the isolation posture | The database's posture changed | [Suspected cross-tenant exposure](cross-tenant-exposure.md) |

## Remediation

- **A. Contain the route.** Roll back the release that introduced it. If the route is a Tenant
  administrator's command and no rollback is possible yet, pause Tenant administration:

  ```sh
  curl -sS -X POST "$OC/v1/tenant-administration-pause" -H "Authorization: Bearer $TOKEN" \
    -H "X-Administrative-Reason: INC-123 WITH CHECK rejection on POST /v1/workspaces" \
    -H "Idempotency-Key: $(uuidgen)" -H "Content-Type: application/json" -d '{"paused":true}'
  ```

  Every Tenant administrator's command then answers `503` and their reads continue. It is
  estate-wide, so it is a decision for the incident lead. Lift it with `{"paused":false}`.
- **B. Contain the actor.** A Tenant administrator: a provider revokes its grant,
  `POST /v1/tenants/{tenant_id}/administrators/{grant_id}/revoke` (`Idempotency-Key` required), and
  if the Membership itself must go, the Tenant administrator of that Tenant revokes it. A provider:
  revoke its grant (`POST /v1/provider-grants/{grant_id}/revoke`) or end its activation
  (`POST /v1/provider-activations/{activation_id}/end`). For a serious exposure, suspend the Tenant
  the actor acts in: `POST /v1/tenants/{tenant_id}/suspend` with `{"expected_version": n}`. That
  withdraws every member, so take it with the Tenant's owner.
- **C. Fix.** Make the write take its Tenant from the bound scope. Add a test that sends a request
  naming another Tenant as the runtime role and expects a refusal before the database
  (`TestScopeIgnoresRequestSuppliedTenant` is the model).

Never resolve one of these by widening a policy. The policy did its job.

## Verification

- `increase(organization_isolation_refusals_total{control="with_check"}[15m])` returns to 0 and the
  alert clears.
- The route refuses a request naming another Tenant with a `4xx` before any statement runs.
- For B: the actor's next request is refused `403`.

## Escalation

- Every occurrence: security lead notified. It is a security finding.
- Probing, or any indication that a read crossed a Tenant: Sev-1 under
  [suspected cross-tenant exposure](cross-tenant-exposure.md).

## Gaps

- The error names the table and not the Tenant either row named. Establishing both needs the request
  and the code path.
- The classification reads the PostgreSQL message beside SQLSTATE `42501`
  (`internal/httpapi/signals.go`); a reworded message stops the count without an error.

## References

| # | Source |
| :-- | :-- |
| R1 | TDD-organization-control-001 §Policy, §Scope Resolution, §The Single Binding Path, §Operational Notes |
| R2 | PostgreSQL 17, *Row Security Policies*, <https://www.postgresql.org/docs/17/ddl-rowsecurity.html>, accessed 2026-10-09: "The policy above implicitly provides a WITH CHECK clause identical to its USING clause, so that the constraint applies both to rows selected by a command (so a manager cannot SELECT, UPDATE, or DELETE existing rows belonging to a different manager) and to rows modified by a command (so rows belonging to a different manager cannot be created via INSERT or UPDATE)." |
| R3 | NIST SP 800-61r3, §2.3, <https://doi.org/10.6028/NIST.SP.800-61r3>: "organizations should also develop and maintain procedures for particularly important processes that may be urgently needed during emergency situations" |

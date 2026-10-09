# Runbook: stuck, unresolved and refused provisioning

Version 1.0.0. Owner: Core Platform Team. Last reviewed 2026-10-09.

A Tenant is not `active` until the external system that owns its isolation boundary confirms the
boundary exists (SAD-004 §5.1). This service records the desired state as a provisioning request,
publishes it, and correlates the realized status back by the request's correlation identifier
(TDD-organization-control-003 §Provisioning Correlation). This runbook covers the three runbooks
TDD-003 §Operational Notes requires: stuck provisioning, unresolved provisioning resolution, and
Tenant activation refused.

## Trigger

- `ProvisioningStuckWarning` (warning) and `ProvisioningStuckCritical` (critical): the oldest
  provisioning request in flight is older than 1 hour and 4 hours,
  `organization_provisioning_oldest_request_age_seconds{operation="provision",state="requested"}`.
- `ProvisioningUnresolved` (warning): a request timed out with no outcome,
  `organization_provisioning_requests{operation="provision",state="unresolved"} > 0`.
- `LifecycleTelemetryAbsent` (warning): none of these gauges for 10 minutes; the alerts above are
  blind.
- An operator's `POST /v1/tenants/{tenant_id}/activate` answers `412`.

The gauges count requests and name no Tenant. The Tenant is found in diagnosis.

## Impact

- The Tenant cannot be used. No Membership can be granted into a Tenant that is not `active`.
- `unresolved` is ambiguous: the boundary may have been built, or not. It is never retried
  automatically, because "retrying an operation whose outcome is unknown is how a Tenant gets
  provisioned twice" (TDD-003 §Provisioning Correlation).
- No access exposure. A Tenant waiting on provisioning has no members.

## Diagnosis

1. Find the Tenants. As a provider, list those not yet active:

   ```sh
   curl -sS "$OC/v1/tenants?status=provisioning&limit=100" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 stuck provisioning"
   curl -sS "$OC/v1/tenants?status=requested&limit=100" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 stuck provisioning"
   ```

2. Read each: `GET /v1/tenants/{tenant_id}`. `status`, `version`, and `provisioning`, the latest
   provisioning request: `{request_id, correlation_id, state, detail, requested_at, resolved_at}`.
3. Read the request's state against the Tenant's status:

   | Tenant `status` | `provisioning.state` | Meaning |
   | :-- | :-- | :-- |
   | `requested` | `requested` | Recorded and published, not yet dispatched here |
   | `provisioning` | `requested` | Dispatched, waiting on the provisioning system |
   | `provisioning` | `unresolved` | Timed out with no outcome: ambiguous |
   | `provisioning` | `realized` | Built; waiting on an operator's activation |
   | `failed` | `failed` | Refused by the provisioning system; `detail` says why |

4. Ask the provisioning system's owner about the request, giving the Tenant and the
   `correlation_id`. That identifier is the only handle both sides share.

## Decision table

| Finding | Act |
| :-- | :-- |
| `requested` Tenant, `requested` request | Dispatch (A) |
| `provisioning`, `requested`, the provisioning system still working | Wait. Past `ORGANIZATION_PROVISIONING_TIMEOUT` (default 30m) the sweep marks it `unresolved` (B) |
| `provisioning`, `requested`, the provisioning system has no record of it | Report `failed` on its confirmation (C), then retry (D) |
| `unresolved` | Establish the outcome with the provisioning system's owner, then report it (C) |
| `failed` | Fix the cause the `detail` names, then retry (D) |
| `realized` | Activate (E) |
| Activation answers `412` | Read the message (E) |

## Remediation

All are provider calls with `X-Administrative-Reason`. `POST /v1/tenants/{tenant_id}/provisioning`
and `/activate` are commands and require an `Idempotency-Key`. The reports and the sweep honour one
and do not require it.

- **A. Dispatch.** `POST /v1/tenants/{tenant_id}/provisioning` `{"expected_version": <version>}`.
  It records that the desired state has left and moves the Tenant to `provisioning`. `412` means no
  provisioning command is outstanding for this Tenant.
- **B. Age unanswered requests.** `POST /v1/provisioning/sweep-unresolved` `{"size": 100}` answers
  `{"affected": n}`. It sets requests older than the timeout to `unresolved`, in both directions
  (provisioning and offboarding's deprovisioning), and starts no retry.
- **C. Report the outcome** on the provisioning system's behalf, with its confirmation in the reason:

  ```sh
  curl -sS -X POST "$OC/v1/provisioning/realized" -H "Authorization: Bearer $TOKEN" \
    -H "X-Administrative-Reason: INC-123 infra confirmed boundary built" -H "Content-Type: application/json" \
    -d '{"correlation_id":"<correlation_id>","detail":"confirmed by <name>"}'
  ```

  Or `POST /v1/provisioning/failed` with the same body; `detail` is required on a failure.
  The response is `{provisioning_request_id, tenant_id, state, tenant, replay, resolved_at}`.
  `replay: true` means the same outcome was already recorded and nothing changed. An `unresolved`
  request accepts either outcome. Refusals:
  - `404`: no provisioning request has that correlation identifier. The two systems disagree about
    what was asked for; investigate before anything else.
  - `409`: the request already has the other outcome. An attempt has one result; a new attempt is a
    retry (D), with its own request.
  - `412`: the identifier matches requests for more than one Tenant. It is refused rather than
    guessed; resolve each Tenant with its owner.
  A realized outcome does not activate the Tenant. Activation stays a deliberate act (E).
- **D. Retry a failed provisioning.** `POST /v1/tenants/{tenant_id}/provisioning`
  `{"expected_version": <version>}` on a `failed` Tenant writes a new request with a new correlation
  identifier, copying the desired profile from the Tenant, and moves it back to `provisioning`. The
  failed request keeps its record. Never retry an `unresolved` one: report its outcome first (C).
- **E. Activate.** `POST /v1/tenants/{tenant_id}/activate` `{"expected_version": <version>}`.
  - `412` "provisioning has not been realized": the latest request is not `realized`. Go back to the
    table.
  - `412` "the sponsoring Organization is not active": restore or activate the Organization first
    (`GET /v1/organizations/{organization_id}`).
  - `409`: the version changed since you read it, or the transition is not allowed from the Tenant's
    status. Read the Tenant again.

## Verification

- `GET /v1/tenants/{tenant_id}` reads `status: active` with `provisioning.state: realized`.
- The gauges fall: no `requested` request older than an hour, no `unresolved` one, and the alerts
  clear.

## Escalation

- The provisioning system unreachable or not answering: its owner.
- `unresolved` the provisioning system's owner cannot settle: escalate to the design owner of
  TDD-organization-control-003. Never infer success.
- `404` on a report: both owners, because one side holds a request the other does not.

## Gaps

- Nothing runs the sweep on a schedule. `ORGANIZATION_PROVISIONING_RECONCILE_INTERVAL` is read and
  validated, and no process acts on it, so a request becomes `unresolved` only when someone calls
  `POST /v1/provisioning/sweep-unresolved`. TDD-003's critical "older than two reconcile intervals"
  is therefore not alerted.
- The stuck alerts read the age of a request in flight. A Tenant in `requested` whose dispatch was
  never sent shows as an old `requested` request as well; diagnosis tells the two apart.

## References

| # | Source |
| :-- | :-- |
| R1 | TDD-organization-control-003 §Tenant State Machine, §Tenant Activation, §Provisioning Correlation, §Operational Notes |
| R2 | SAD-004 §5.1, §7.5 |
| R3 | NIST SP 800-61r3, §2.3, <https://doi.org/10.6028/NIST.SP.800-61r3>: "Playbooks provide actionable steps or tasks for people to perform during various scenarios or situations." |

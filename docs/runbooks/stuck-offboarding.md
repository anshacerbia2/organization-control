# Runbook: stuck offboarding

Version 1.2.0. Owner: Core Platform Team. Last reviewed 2026-10-09.

An offboarding moves `freeze -> obligations -> release -> retired`, or ends `cancelled`. Each stage
is persisted, so a stopped offboarding resumes from where it is. This runbook finds why one has not
moved and moves it, or stops it (TDD-organization-control-004 §Offboarding Stages).

## Trigger

The signals TDD-004 §Operational Notes names, exported as gauges and alerted:

- `OffboardingObligationOverdue` (warning): `organization_offboarding_obligations_overdue > 0`, an
  open obligation past its `due_at`. `organization_offboarding_oldest_overdue_obligation_age_seconds`
  is how long past.
- `OffboardingReleaseAmbiguous` (warning): an offboarding held in `release` by an `unresolved`
  deprovisioning, `organization_provisioning_requests{operation="deprovision",state="unresolved"} > 0`.
  `OffboardingReleaseAmbiguousCritical` (critical) when it has been ambiguous over 24 hours,
  `organization_provisioning_oldest_request_age_seconds{operation="deprovision",state="unresolved"}`.
- `OffboardingProlongedWarning` (over 30 days) and `OffboardingProlongedCritical` (over 90 days):
  the oldest offboarding still in `freeze`, `obligations` or `release`,
  `organization_offboarding_oldest_in_progress_age_seconds`; `organization_offboarding_in_progress`
  counts them.
- `LifecycleTelemetryAbsent` (warning): none of these for 10 minutes. The alerts above are blind; run
  diagnosis step 1 by hand until it clears.
- An operator or a domain reports an offboarding that does not move.

The gauges name no offboarding and no Tenant. Find them with diagnosis step 1.

## Impact

- Access already stopped at `begin`: `tenant.security.suspended` was published then. A stuck
  offboarding is not an access exposure.
- It holds data and infrastructure past their release date, and holds obligations to other
  domains and to the customer.
- Release and retirement are irreversible. Nothing in this runbook skips a gate to move faster.

## Diagnosis

1. Find it, as a provider:

   ```sh
   curl -sS "$OC/v1/offboardings?stage=obligations&limit=100" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 stuck offboarding"
   curl -sS "$OC/v1/offboardings?tenant_id=$TENANT_ID" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 stuck offboarding"
   ```

2. Read it: `GET /v1/offboardings/{offboarding_id}`. The fields that decide:
   `stage`, `legal_hold`, `started_at`, `obligations_at`, `released_at`, `active_memberships`,
   `deprovisioning` (`state`, `detail`, `requested_at`, `resolved_at`), and for a cancellation
   `restore_pending`.

3. Read its obligations: `GET /v1/offboardings/{offboarding_id}/obligations`. `outstanding`, and
   `obligations` with `obligation_id`, `domain`, `type`, `state`, `due_at`, `detail`,
   `resolved_by`, `resolved_at`. Overdue open rows come first.

4. Read the Tenant for its `version`: `GET /v1/tenants/{tenant_id}`. Retirement and cancellation
   name it as `expected_version`.

## Decision table

| Stage | Finding | Act |
| :-- | :-- | :-- |
| `freeze` | `active_memberships` above 0 | Run freeze batches (A) |
| `freeze` | `active_memberships` 0 | Complete the freeze (A) |
| `obligations` | An `open` obligation | Chase the owning domain (B) |
| `obligations` | A `failed` obligation | It holds release like an open one. The domain fixes and reports again, or an accountable person waives it (B) |
| `obligations` | None outstanding, `legal_hold` true | Release is refused while the hold is set (C) |
| `obligations` | None outstanding, no hold | Release (D) |
| `release` | `deprovisioning.state` `requested` | In flight. Wait for the provisioning system. Past `ORGANIZATION_PROVISIONING_TIMEOUT` (default 30m), the scheduled sweep marks it `unresolved` (E) |
| `release` | `unresolved` | Ambiguous: the infrastructure may or may not be released (E) |
| `release` | `failed` | Refused by the provisioning system. Once its cause is fixed, send it again (H) |
| `release` | `realized` | Retire (F) |
| `freeze` or `obligations` | The offboarding was a mistake | Cancel (G) |
| `cancelled` | `restore_pending` above 0 | Send the cancel again (G) |

## Remediation

All are provider commands: `X-Administrative-Reason` and an `Idempotency-Key` are required.

- **A. Freeze.** Repeat until `affected` is 0, then complete:

  ```sh
  curl -sS -X POST "$OC/v1/offboardings/$ID/freeze" -H "Authorization: Bearer $TOKEN" \
    -H "X-Administrative-Reason: INC-123 resume freeze" -H "Idempotency-Key: $(uuidgen)" \
    -H "Content-Type: application/json" -d '{"size":100}'
  curl -sS -X POST "$OC/v1/offboardings/$ID/complete-freeze" -H "Authorization: Bearer $TOKEN" \
    -H "X-Administrative-Reason: INC-123 complete freeze" -H "Idempotency-Key: $(uuidgen)"
  ```

  `complete-freeze` answers `409` naming how many active Memberships remain.

- **B. Obligations.** The owning domain reports the outcome. On its behalf, with its confirmation in
  the reason:

  ```sh
  curl -sS -X POST "$OC/v1/obligations/$OBLIGATION_ID/resolve" -H "Authorization: Bearer $TOKEN" \
    -H "X-Administrative-Reason: INC-123 billing confirmed export" -H "Idempotency-Key: $(uuidgen)" \
    -H "Content-Type: application/json" -d '{"domain":"billing","state":"completed"}'
  ```

  - `state` is `completed`, `waived` or `failed`. `waived` and `failed` need a `detail`.
  - `domain` must be the one the obligation was raised against, or it is refused `403`.
  - Waive only on the decision of an accountable person, named in `detail`. `waived` exists so an
    audit can tell a decision from a satisfied obligation (TDD-004 §Security Notes).
  - A missing obligation is raised with `POST /v1/offboardings/{id}/obligations`
    `{"domain", "type", "due_at"}`.

- **C. Legal hold.** Release only on counsel's written instruction, cited in the reason:
  `POST /v1/offboardings/{id}/legal-hold` with `{"hold": false}`. A hold blocks release and
  retirement only.

- **D. Release.** `POST /v1/offboardings/{id}/release`, no body. It records the deprovisioning
  command and publishes it in one transaction. `412` names the outstanding obligations or the hold.
  From here the offboarding cannot be cancelled.

- **E. Deprovisioning outcome.** Do not retire on a guess. A timeout is not proof the target did
  nothing (SAD-004 §7.5).
  1. The scheduled sweep ages an unanswered request within one reconcile interval of the timeout. To
     do it now: `POST /v1/provisioning/sweep-unresolved` with `{"size": 100}`.
  2. Ask the infrastructure owner what happened, giving the offboarding's `correlation_id`, the
     Tenant, and `deprovisioning.requested_at`.
  3. The provisioning system reports, or you report on its behalf with its confirmation:
     `POST /v1/offboardings/{id}/deprovisioning` with `{"state":"realized"}`, or
     `{"state":"failed","detail":"..."}`. `204`. It records; it never advances the stage.
  4. A `failed` deprovisioning is sent again with H once the provisioning system's owner has fixed
     the cause the `detail` names. An `unresolved` one is never sent again: report its outcome
     first.

- **F. Retire.** `POST /v1/offboardings/{id}/retire` with `{"expected_version": <Tenant version>}`.
  All three gates are checked again at this moment: no obligation open or failed, no legal hold,
  deprovisioning `realized`. Irreversible.

- **G. Cancel.** Only in `freeze` or `obligations`:
  `POST /v1/offboardings/{id}/cancel` with `{"expected_version": <Tenant version>}`. The Tenant
  returns to the status recorded when the offboarding began, open obligations close as
  `cancelled`, and the frozen Memberships are restored after the Tenant commits. If
  `restore_pending` is above 0 afterwards, send the cancel again with a new key. An offboarding
  begun before TDD-004 1.8.0 shipped has no freeze record, and its cancellation is refused `409`.

- **H. Send a failed deprovisioning again.**

  ```sh
  curl -sS -X POST "$OC/v1/offboardings/$ID/deprovisioning/resend" -H "Authorization: Bearer $TOKEN" \
    -H "X-Administrative-Reason: INC-123 infra fixed quota, resend" -H "Idempotency-Key: $(uuidgen)"
  ```

  - Only in `release`, and only when the latest deprovisioning is `failed`. Anything else is `409`:
    `requested` is in flight, `realized` is done, and `unresolved` is ambiguous, so sending it again
    would retry an unknown outcome (SAD-004 §7.5).
  - The two release gates are checked again: a legal hold or an open or failed obligation refuses it
    `412`, as it would have refused the release.
  - It records a new deprovisioning request under the offboarding's `correlation_id` and publishes
    the `released` event again with a new event identifier, in one transaction. The failed request
    keeps its record. The stage does not move.
  - The response is the offboarding view; `deprovisioning.state` reads `requested` with a new
    `requested_at`.

## Verification

- `GET /v1/offboardings/{id}` shows the expected next `stage` and its entry instant
  (`obligations_at`, `released_at`, `retired_at`), or `cancelled` with `restore_pending` 0.
- For a retirement, `GET /v1/tenants/{tenant_id}` reads `status: retired`.

## Escalation

- An obligation the owning domain does not answer: that domain's owner, then the service owner.
- `unresolved` past 24 hours: the infrastructure owner, as critical.
- A request to lift a legal hold without counsel's instruction: refuse and escalate to legal.

## Gaps

- TDD-004's critical for an obligation "past the contract deadline" is not alerted: no contract
  deadline is recorded, only `due_at`.

## References

| # | Source |
| :-- | :-- |
| R1 | TDD-organization-control-004 §Offboarding, §Offboarding Stages, §Cancellation, §Legal Hold, §Sending a Failed Deprovisioning Again, §Operational Notes |
| R2 | TDD-organization-control-003 §Provisioning Correlation (the `unresolved` state) |
| R3 | NIST SP 800-61r3, §2.3, <https://doi.org/10.6028/NIST.SP.800-61r3>: "Formatting procedures within a playbook instead of another format can improve their usability." |

# Runbook: provider-access review

Version 1.0.0. Owner: Core Platform Team. Last reviewed 2026-10-07.

Provider authority crosses every Tenant. This runbook reviews who holds it, who used it, and why,
and removes what is no longer needed. It has two forms: a scheduled review, and a review after each
use of an emergency grant.

## Trigger

- **Scheduled:** every 90 days, the period this service already uses for emergency grant validation
  (TDD-organization-control-001 §Emergency Grant Validation). NIST leaves the frequency to the
  organization [R1]; Microsoft validates emergency accounts "at least every 90 days" [R4].
- **Each emergency use:** the service log line `a provider acted on an emergency grant` (WARN, with
  `principal_id`, `method`, `route`). Each one is reviewed [R4].
- **Overdue validation:** the daily `organization-migrate -stage=maintenance` logs
  `an emergency provider grant has not been used in 90 days ...` at WARN.
- **Lockout risk:** at startup in production, `fewer than two emergency provider grants in
  production; grant another with kind emergency` (WARN), per scope.
- **A person leaves or changes role.**

## Impact

- Provider authority reads and changes any Tenant, closes dead letters, and grants provider
  authority. An unneeded grant is standing access to the whole estate.
- Too few emergency grants is the opposite failure: the deployment cannot be administered when
  approval is unavailable (ADR-ORG-002 §5.2).

## Diagnosis

All reads are provider reads with `X-Administrative-Reason`, and each writes an access record.

1. **Grants.** Every grant, active and revoked:

   ```sh
   curl -sS "$OC/v1/provider-grants" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: Q4 provider-access review"
   ```

   Each grant: `grant_id`, `principal_id`, `scope` (`provider:organization-control` or
   `provider:identity-control`), `kind` (`eligible` or `emergency`), `granted_by`,
   `bootstrap_operator`, `reason`, `granted_at`, `active`, `revoked_*`.

2. **Activations.** Pending requests, activations in force, and the last hundred:

   ```sh
   curl -sS "$OC/v1/provider-activations" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: Q4 provider-access review"
   ```

   Check `approval_required`, `decided_by` (never the holder), `duration_seconds`, `reason`.

3. **Emergency grants of this service's scope:**

   ```sh
   curl -sS "$OC/v1/provider-grants:emergency-validation" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: Q4 provider-access review"
   ```

   `uses`, `last_used_at`, `due_at`, `overdue`. The Identity Control API reports the use of
   `provider:identity-control` emergency grants itself; this service cannot see it.

4. **What was done with the authority.** `audit.privileged_access` holds one row per provider-scoped
   transaction and per dead-letter resolution attempt and outcome: `actor_id`, `correlation_id`,
   `reason`, `occurred_at` (SQL, read-only):

   ```sql
   SELECT actor_id, count(*), min(occurred_at), max(occurred_at)
     FROM audit.privileged_access
    WHERE occurred_at > now() - interval '90 days'
    GROUP BY actor_id ORDER BY count(*) DESC;

   SELECT occurred_at, correlation_id, reason
     FROM audit.privileged_access
    WHERE actor_id = $PRINCIPAL AND occurred_at > now() - interval '90 days'
    ORDER BY occurred_at;
   ```

   Join `correlation_id` to the request logs for the method and route.

5. **Tenant administrators**, a provider-made grant inside one Tenant:
   `GET /v1/tenants/{tenant_id}/administrators`, for each Tenant in `GET /v1/tenants`.

## Decision table

| Finding | Act |
| :-- | :-- |
| An active grant whose holder no longer needs it, or has left | Revoke it (Remediation A) |
| An `eligible` grant never activated in the period | Ask the holder. Revoke unless there is a reason to keep it |
| An emergency grant `overdue` | Its holder validates it: sign in and make one request whose reason says it is a drill |
| An emergency use whose reason does not say drill, and no incident explains it | Treat as possible misuse (Remediation C) |
| Fewer than two active emergency grants for a scope | Grant another emergency grant to a second person (Remediation B) |
| An activation approved by its own holder | Cannot happen in production: a check constraint refuses it. If found, escalate as a defect |
| A blank or meaningless `reason` in `audit.privileged_access` | Cannot be blank (refused at the table). A meaningless one is raised with the actor |
| A Tenant administrator who should not be one | Revoke it: `POST /v1/tenants/{tenant_id}/administrators/{grant_id}/revoke` |

## Remediation

- **A. Revoke a grant.**

  ```sh
  curl -sS -X POST "$OC/v1/provider-grants/$GRANT_ID/revoke" -H "Authorization: Bearer $TOKEN" \
    -H "X-Administrative-Reason: Q4 review: holder moved team" -H "Idempotency-Key: $(uuidgen)"
  ```

  The grant stays as the record, with `revoked_at`, `revoked_by` and `revoke_reason`. Authority
  stops at the holder's next request. The last active `provider:organization-control` grant
  cannot be revoked; grant another first. A
  `provider:identity-control` revocation publishes `provider.security.revoked` on the priority lane.

- **B. Grant.** `POST /v1/provider-grants` with
  `{"principal_id": "...", "scope": "provider:organization-control", "kind": "eligible"}`
  (or `"emergency"`), `Idempotency-Key` required. One active grant per Principal and scope.

- **C. Suspected misuse.** Preserve the log lines and the `audit.privileged_access` rows. End any
  activation in force (`POST /v1/provider-activations/{activation_id}/end`), revoke the grant, and
  open a security incident. Decide whether the use was "a planned drill", "an actual emergency", or
  "misuse or unauthorized usage" [R4].

## Verification

- `GET /v1/provider-grants` shows each decision applied, with the review named in the reasons.
- Every scope has at least two active emergency grants, none overdue.
- Record the review: date, reviewer, grants kept, grants revoked, uses examined.

## Escalation

- Suspected misuse: security lead, at once.
- Unable to keep two emergency grants: the service owner. It is a lockout risk.

## Gaps

- No route reads `audit.privileged_access`; no runtime role can read it, which is the point. The
  review needs SQL on the migration credential.
- The 90-day cadence is set here. No design names one for the scheduled review.

## References

| # | Source |
| :-- | :-- |
| R1 | NIST SP 800-53 Rev. 5, AC-6(7) *Review of User Privileges*, <https://doi.org/10.6028/NIST.SP.800-53r5>: "(a) Review [Assignment: organization-defined frequency] the privileges assigned to [Assignment: organization-defined roles or classes of users] to validate the need for such privileges; and (b) Reassign or remove privileges, if necessary, to correctly reflect organizational mission and business needs." |
| R2 | NIST SP 800-53 Rev. 5, AC-6(9) *Log Use of Privileged Functions*: "Log the execution of privileged functions." and AU-6 a.: "Review and analyze system audit records [Assignment: organization-defined frequency] for indications of [Assignment: organization-defined inappropriate or unusual activity] and the potential impact of the inappropriate or unusual activity" |
| R3 | NIST SP 800-53 Rev. 5, AC-2 j.: "Review accounts for compliance with account management requirements [Assignment: organization-defined frequency]" |
| R4 | Microsoft, *Manage emergency access admin accounts*, <https://learn.microsoft.com/en-us/entra/identity/role-based-access-control/security-emergency-access>: "Validate account functionality at least every 90 days." and "After any use of an emergency access account, conduct a review to determine whether the use was authorized and whether the actions taken were appropriate." The review determines whether the account was used "For a planned drill to validate its suitability", "In response to an actual emergency where no administrator could use their regular accounts", or "As a result of misuse or unauthorized usage of the account". |
| R5 | TDD-organization-control-001 §Caller Authority, §Provider Activation, §Emergency Grant Validation, §Provider Authority Projection |

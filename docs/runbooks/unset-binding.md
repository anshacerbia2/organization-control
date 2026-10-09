# Runbook: unset-binding investigation

Version 1.0.0. Owner: Core Platform Team. Last reviewed 2026-10-09.

A statement reached a Row-Level Security policy on a connection whose scope was never bound. Every
policy reads its setting with `missing_ok` false, so the statement raised an error instead of quietly
returning no rows (TDD-organization-control-001 §Policy, §Session Binding). The request answered
`500`. The isolation held. What failed is the code path that should have bound a scope first.

## Trigger

- `IsolationUnsetBindingWarning` (warning): any unset-binding refusal in the last hour.
  `increase(organization_isolation_refusals_total{control="unset_binding"}[1h]) > 0`.
- `IsolationUnsetBindingCritical` (critical): five or more in an hour.
- The ERROR log line `an isolation control refused a statement` with `control=unset_binding`. It
  carries `method`, `route` (the route pattern, for example `POST /v1/memberships`) and `error`.
- A caller reports a `500` (`https://problems.scnehaux.com/internal`) whose `correlation_id` leads to
  that log line.

## Impact

- No data crossed a Tenant boundary. An unbound statement raises; it does not read.
- The route in the log line fails every time it takes that path. For a tenant route that is an
  outage for that operation. For a read the authentication path makes, it can refuse every caller.
- An unset binding is a defect in this service, never a caller error. TDD-001 §Operational Notes
  sets the thresholds: warning at any occurrence, critical at five in an hour.

## Diagnosis

1. Find the log lines. Filter on `msg="an isolation control refused a statement"` and
   `control="unset_binding"`. Note `route`, `error` and the request's `correlation_id`.
2. Read `error`. It names the cause:
   - `unrecognized configuration parameter "app...."` (SQLSTATE `42704`): the connection never set
     the parameter in any transaction. The statement ran outside every scope wrapper.
   - `invalid input syntax for type uuid: ""` or `... type boolean: ""` (SQLSTATE `22P02`): an
     earlier transaction on the same pooled connection set the parameter with `SET LOCAL`. It
     reverted to empty at commit, and this transaction never set it again. Also a statement outside
     a wrapper. The previous request's value never leaks: it is empty, not stale.
3. Find the code path. `internal/db` is the only package that binds a scope
   (`TestScopeBindingLivesInExactlyOnePackage`). A statement on an RLS table (schemas `tenant`,
   `workspace`, `membership`, `invitation`, `operation`) must run inside `db.WithTenantScope`,
   `db.WithTenantRead`, `db.WithProviderInTenant`, `db.WithProviderScope`, `db.WithSelfRead` or the
   consumer and resolution wrappers. Statements on a raw transactor are the suspects: the caller
   records (`authority.Reader`), the frontier and signals readers, and the claim store. None of them
   may touch an RLS table.
4. Check what changed. Was there a deploy since the alert last stayed quiet? `git log` the release
   for changes under `internal/` that add SQL, or that move a body from one wrapper to another.
5. Confirm the posture is intact, so the cause is the code and not the database:
   `curl -fsS $OC/readyz`. A posture problem fails readiness and is logged as `readiness failed`
   (TDD-001 §Verifying the Posture at Runtime). If readiness fails too, go to
   [suspected cross-tenant exposure](cross-tenant-exposure.md).

## Decision table

| Finding | Cause | Act |
| :-- | :-- | :-- |
| One route, every request, since a deploy | A statement added outside a wrapper, or a body moved to the wrong wrapper | Roll back the release (A), then fix (B) |
| One route, some requests | A path that skips its wrapper only on one branch | Fix (B); no rollback unless the route matters now |
| `route` empty or not an API route | Not a request: a background reader or the maintenance stage on an RLS table | Fix (B). The counter counts HTTP requests only (Gaps) |
| `readyz` fails as well | The database lost a policy or a role attribute | [Suspected cross-tenant exposure](cross-tenant-exposure.md) |

## Remediation

- **A. Roll back.** Redeploy the previous release. The isolation held, so a rollback is about the
  broken route, not about exposure.
- **B. Fix.** Move the statement into the wrapper its scope needs. Add a test that runs the route
  as the runtime role, the way `internal/*/..._integration_test.go` do. Run
  `go run ./tools/grantcheck`: a raw transaction outside a declared boundary is a `PROBLEM` there,
  which is how most of these are caught before a deploy.

Never "fix" one of these by reading the setting with `missing_ok` true or by giving a role
`BYPASSRLS`. Both turn this loud failure into a silent empty result, which TDD-001 §Operational
Notes rejects by name.

## Verification

- The counter stops rising: `increase(organization_isolation_refusals_total{control="unset_binding"}[1h])`
  returns to 0 and both alerts clear.
- The route answers its normal status for the request in the incident.

## Escalation

- Critical, or a route on the authentication path: Core Platform Team on call.
- Any sign that a value other than empty reached a policy (a request served another Tenant's row):
  [suspected cross-tenant exposure](cross-tenant-exposure.md), security lead notified.

## Gaps

- The counter counts refusals the HTTP surface answers `500`. A background read (the metric
  callbacks, the dispatcher's registration check, the maintenance stage) that hits an unset binding
  fails in its own caller and is not counted. For the gauges it shows as an absent series:
  `EnforcementTelemetryAbsent` or `LifecycleTelemetryAbsent`.
- The classification reads the PostgreSQL message text beside the SQLSTATE
  (`internal/httpapi/signals.go`). A PostgreSQL release that rewords the message stops the count
  without an error. `TestEveryRuleReadsASeriesThisPackageExports` holds the series name, not the
  message.

## References

| # | Source |
| :-- | :-- |
| R1 | TDD-organization-control-001 §Session Binding, §The Single Binding Path, §Verifying the Posture at Runtime, §Operational Notes |
| R2 | PostgreSQL 17, *Row Security Policies*, <https://www.postgresql.org/docs/17/ddl-rowsecurity.html>, accessed 2026-10-09: "Table owners normally bypass row security as well, though a table owner can choose to be subject to row security with ALTER TABLE ... FORCE ROW LEVEL SECURITY." |
| R3 | NIST SP 800-61r3, §2.3, <https://doi.org/10.6028/NIST.SP.800-61r3>: "Playbooks provide actionable steps or tasks for people to perform during various scenarios or situations." |

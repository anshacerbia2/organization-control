# Runbook: Organization Database restore

Version 1.1.0. Owner: Core Platform Team. Last reviewed 2026-10-09.

The Organization Database is the authority for Organizations, Tenants, Memberships, provider grants
and offboardings, and it holds the outbox every consumer's projection is built from. This runbook
restores it from the daily backup when its volume or its data is lost. A restore to the instant of the
backup is drilled on every change and daily (`scripts/dev-restore-drill.sh`, STD-GLB-002 §Restore
Evidence [R1]). A restore to an older point than the consumers' state is contained by pausing
Tenant administration and reconciled by §After a restore to an older point, which moves each
Membership a consumer holds at a higher version past it (SAD-004 §6.6, §9.1.1).

## Trigger

- `/readyz` fails and `docker compose logs postgres` shows the database will not start, or reports
  corrupted data.
- The volume `scnehaux-organization-control-dev_postgres` is gone, or `docker compose down -v` was
  run.
- The migrate job fails on a database it accepted before, and the cause is the data, not a release.

## Impact

| While the database is down | After a restore to an older point |
| :-- | :-- |
| Every mutation fails closed: no grant, revocation, Tenant change or provider administration (SAD-004 §9.1.2) | Every change after the backup is lost from authority: grants, revocations, Tenants, provider grants, offboarding steps |
| Consumers keep their projections within their freshness policy; identity-control reads the provider projection stale and honours the emergency grant | Consumers hold newer versions than authority. A revocation lost from authority still holds at the consumer, and a grant lost from authority is an `extra` there |
| Nothing is published | The outbox and its delivery state are as at the backup: events undelivered then are delivered again, and consumers discard what they already applied |

A restore to an older point is a security incident when any revocation, suspension or offboarding
happened after the backup.

## Remediation

1. **Stop the service.** `docker compose stop organization-control`. Its dispatcher stops with it.
2. **Keep what is left.** If the volume still exists, copy it before deleting anything:
   `docker run --rm -v scnehaux-organization-control-dev_postgres:/data -v "$PWD":/out alpine tar -C /data -czf /out/damaged-volume.tgz .`.
3. **Choose the backup.** The newest `globals-<date>.sql` and `organization_control-<date>.dump`
   pair from the same date. Write the date in the incident record: every change after it is lost.
4. **Put `.env` and `keys/` back** from the same backup. The migrate job sets the five login roles'
   passwords from `.env`, and the dispatcher signs with the workload key.
5. **Delete the damaged database.** `docker compose down -v`, deliberately, now that step 2 has kept
   a copy. `restore.sh` refuses a cluster that still holds `organization_control`.
6. **Restore, paused.** From `deploy/dev`:

   ```sh
   PAUSE_REASON="INC-123 restored from the <date> backup" \
   ./restore.sh <backup>/globals-<date>.sql <backup>/organization_control-<date>.dump
   ```

   It applies the roles, then the database with `pg_restore --create --exit-on-error` [R2], and stops
   at the first error. With `PAUSE_REASON` set it then records a pause of Tenant administration in the
   restored database, before anything can serve, so no Tenant administrator changes authority until
   §After a restore to an older point is done. Always set it: whether the backup is older than a
   consumer's state is learned only after the restore. If the backup predates the pause table
   (TDD-organization-control-001 1.22.0), `restore.sh` stops after the restore with the insert's
   error; go on, and pause through the API (step 8) the moment the service answers.
7. **Start.** `docker compose up -d --build`, then `curl -fsS http://127.0.0.1:8083/readyz`.
   `docker compose logs migrate` ends with `control database ready`: RLS, the privileges and the
   login roles are asserted on the restored database.
8. **Confirm the pause.** As a provider:

   ```sh
   curl -sS "$OC/v1/tenant-administration-pause" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 restore"
   ```

   It answers `{"paused", "pause_id", "reason", "actor_id", "correlation_id", "recorded_at"}`;
   `actor_id` is null on the pause `restore.sh` recorded. If `paused` is false, pause now:

   ```sh
   curl -sS -X POST "$OC/v1/tenant-administration-pause" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 restored from the <date> backup" \
     -H "Idempotency-Key: $(uuidgen)" -H "Content-Type: application/json" -d '{"paused":true}'
   ```

   While paused, every Tenant administrator's request other than `GET`, `HEAD` and `OPTIONS` answers
   `503` ("Tenant administration is paused ..."), read for each request. Their reads continue, and so
   does every provider act: the repairs below are provider acts.
9. **Reconcile** (§After a restore to an older point) before announcing recovery, whatever the
   backup's age. Then lift the pause (step 6 there).

## After a restore to an older point

Run under the pause, in this order. Every call is a provider call with the incident in
`X-Administrative-Reason`.

1. **Reconcile every consumer.** For each consumer in `GET /v1/projections/consumers?state=active`,
   get its current report and run [projection drift repair](projection-drift-repair.md), A. Read the
   findings as lost changes:
   - a `mismatch` whose `projected_version` is above `authoritative_version` is a change authority
     lost. The consumer holds that Membership active (a report lists only active ones);
   - an `extra` with `state` set and `projected_version` above `authoritative_version`: authority
     holds the Membership withdrawn at a lower version, the consumer active at a higher one;
   - an `extra` with `state` null is a grant authority lost entirely. The repair removes it at the
     consumer;
   - a `missing` may be a lost revocation or suspension the consumer holds at a higher version. The
     report cannot say, because it lists active Memberships only.
   Keep each report and response in the incident.
2. **Re-apply lost revocations and suspensions first,** through their routes. Then lost provider-grant
   revocations ([provider-access review](provider-access-review.md)), then offboardings
   ([stuck offboarding](stuck-offboarding.md)). Grants come last, and only when their requester
   confirms them again. Their sources are the incident record, the consumers' own logs and the
   requesters.
3. **Advance the versions** for each consumer, with the same report:

   ```sh
   curl -sS -X POST "$OC/v1/projections/advance-versions" -H "Authorization: Bearer $TOKEN" \
     -H "X-Administrative-Reason: INC-123 advance versions after restore" \
     -H "Idempotency-Key: $(uuidgen)" -H "Content-Type: application/json" -d @report.json
   ```

   The body is the consumer's report, `{"consumer_id", "mark", "rows"}`, as reconcile takes it. For
   each Membership the consumer reports at a version above authority's, and that authority still
   holds, it sets authority's version to the consumer's plus one, unchanged in state, and publishes
   authority's state at it: active as `membership.lifecycle.restored`, a withdrawal as
   `membership.security.suspended` or `.revoked` on the priority lane. The consumer applies it, and
   every later change, because they are now above what it holds. The answer is
   `{"consumer_id", "mark", "advanced": [{"membership_id", "tenant_id", "from_version", "to_version",
   "membership_status", "event_id"}]}`; `event_id` is null for one already past the report, which is
   left alone.
   - It never widens access: it touches only Memberships the consumer holds active, so what it
     publishes is the same active state or a withdrawal.
   - It runs one transaction per Tenant. If it fails part way, send the same report again with a new
     key: the Memberships already advanced are no longer behind.
4. **Read the consumer again.** A fresh report, reconciled, finds no `mismatch` or `extra` with the
   consumer ahead. Confirm each change re-applied in step 2 reached the consumer by its state, not
   only its version: the enforcement read `GET /v1/tenants/{tenant_id}/memberships/{membership_id}/enforcement`
   reads `enforced` for a re-applied revocation.
5. Repeat 1 to 4 for every consumer, so the known state is the current one [R4].
6. **Lift the pause:**
   `POST /v1/tenant-administration-pause` `{"paused":false}` with the incident and the outcome in the
   reason (`Idempotency-Key` required). Only a person lifts a pause; the record keeps who and why.

## Verification

- `GET /v1/provider-grants`, `GET /v1/organizations` and `GET /v1/offboardings` answer what the
  backup held, plus what step 2 of §After a restore to an older point re-applied.
- `GET /v1/projections/consumers` shows each consumer reporting again, and a reconciliation returns
  no finding.
- `GET /v1/tenant-administration-pause` reads `"paused": false`, lifted by a named provider, and a
  Tenant administrator's command succeeds.
- identity-control's provider projection reads fresh (its logs: `the provider projection is fresh`).

## Escalation

- Any lost revocation, suspension or offboarding: a security incident. SAD-004 §9.1.1 names a
  restore to an older point as blocking the whole Tenancy control plane until reconciled.
- A restore that `restore.sh` refuses or stops: keep the files and the output; do not edit the
  dump or run `pg_restore` by hand without `--exit-on-error`.

## Gaps

- **The RPO is 24 hours, not 1 minute.** The backup is a daily `pg_dump`. PAD-PLT-002 §6.2 targets
  1 minute, which needs continuous WAL archiving with point-in-time recovery on the production
  platform [R1] [R3]. Until it does, a restore loses up to a day.
- **Tenant security versions and provider grant versions are not advanced.** A consumer's report
  carries Memberships only, so nothing says which Tenant or grant version it holds. A Tenant
  suspension or restoration lost by the restore is handled by re-applying it (step 2). The options
  are in TDD-organization-control-002 §After a Restore to an Older Point, for the owner.
- **A Membership the consumer holds withdrawn at a higher version is not advanced.** It is not in
  the report. The consumer keeps the narrower state, a denial, until authority's version passes its
  own; re-applying the withdrawal (step 2) makes the two agree.
- **The same version, two states, is not detected.** A report gives versions only. The pause stops
  Tenant administrators changing authority meanwhile; a provider's change in that window is the
  remaining exposure.
- **Only the procedure is timed.** The drill measures it on CI data, not on a production-sized
  database, and it does not exercise `.env`, `keys/`, the server's cron, `PAUSE_REASON` or the
  version advance.

## References

| # | Source |
| :-- | :-- |
| R1 | STD-GLB-002 §Restore Evidence, scnehaux-architecture `02-standards/_global/STD-GLB-002-database.md`; TDD-organization-control-001 §Restore Evidence, §Pausing Tenant Administration; TDD-organization-control-002 §After a Restore to an Older Point; SAD-004 §6.6, §9.1.1 |
| R2 | PostgreSQL 17, *pg_restore*, <https://www.postgresql.org/docs/17/app-pgrestore.html>, accessed 2026-10-08: `--exit-on-error`: "Exit if an error is encountered while sending SQL commands to the database. The default is to continue and to display a count of errors at the end of the restoration." |
| R3 | PostgreSQL 17, *Continuous Archiving and Point-in-Time Recovery (PITR)*, <https://www.postgresql.org/docs/17/continuous-archiving.html>, accessed 2026-10-08: "pg_dump and pg_dumpall do not produce file-system-level backups and cannot be used as part of a continuous-archiving solution." |
| R4 | NIST SP 800-53 Rev. 5, CP-10, <https://raw.githubusercontent.com/usnistgov/oscal-content/main/nist.gov/SP800-53/rev5/json/NIST_SP-800-53_rev5_catalog.json>, accessed 2026-10-08: "Provide for the recovery and reconstitution of the system to a known state within [Assignment] after a disruption, compromise, or failure." §After a restore to an older point is what makes the known state the current one. |

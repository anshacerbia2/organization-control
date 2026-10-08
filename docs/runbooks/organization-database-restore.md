# Runbook: Organization Database restore

Version 1.0.0. Owner: Core Platform Team. Last reviewed 2026-10-08.

The Organization Database is the authority for Organizations, Tenants, Memberships, provider grants
and offboardings, and it holds the outbox every consumer's projection is built from. This runbook
restores it from the daily backup when its volume or its data is lost. A restore to the instant of the
backup is drilled on every change and daily (`scripts/dev-restore-drill.sh`, STD-GLB-002 §Restore
Evidence [R1]). A restore to an older point than the consumers' state is not reconciled by anything
built, and §After a restore to an older point says what an operator does meanwhile.

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
6. **Restore.** From `deploy/dev`:
   `./restore.sh <backup>/globals-<date>.sql <backup>/organization_control-<date>.dump`. It applies
   the roles, then the database with `pg_restore --create --exit-on-error` [R2], and stops at the
   first error.
7. **Start.** `docker compose up -d --build`, then `curl -fsS http://127.0.0.1:8083/readyz`.
   `docker compose logs migrate` ends with `control database ready`: RLS, the privileges and the
   login roles are asserted on the restored database.
8. **If the backup is older than the last change,** go on to §After a restore to an older point
   before announcing recovery.

## After a restore to an older point

1. **Reconcile every consumer at once.** For each consumer in `GET /v1/projections/consumers`, run
   [projection drift repair](projection-drift-repair.md) with its current report. Read the findings
   as lost changes:
   - a `mismatch` whose consumer version is higher is a change authority lost. If the consumer holds
     it `revoked` or `suspended`, authority now holds an older, wider state: a security incident;
   - an `extra` is a grant authority lost, or a consumer ahead of authority.
2. **Re-apply lost revocations and suspensions first,** through their routes, each with the
   incident in `X-Administrative-Reason`. Then lost provider-grant revocations
   ([provider-access review](provider-access-review.md)), then offboardings
   ([stuck offboarding](stuck-offboarding.md)). Grants come last, and only when their requester
   confirms them again.
3. **Read the consumer again** and confirm each re-applied change reached it, so the known state
   is the current one [R4]. A re-applied change
   whose version the consumer already holds is discarded there (§Gaps), so check the state, not only
   the version.

## Verification

- `GET /v1/provider-grants`, `GET /v1/organizations` and `GET /v1/offboardings` answer what the
  backup held, plus what step 2 of §After a restore to an older point re-applied.
- `GET /v1/projections/consumers` shows each consumer reporting again, and a reconciliation returns
  no finding.
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
- **No reconciliation of security versions after a restore is built** (SAD-004 §6.6). Authority's
  next version of a Membership can equal one a consumer already holds for a lost change, and the
  consumer then discards the new event as already applied. No route moves a version forward.
- **No route pauses Tenant administrators' mutations** while §After a restore to an older point
  runs.
- **Only the procedure is timed.** The drill measures it on CI data, not on a production-sized
  database, and it does not exercise `.env`, `keys/` or the server's cron.

## References

| # | Source |
| :-- | :-- |
| R1 | STD-GLB-002 §Restore Evidence, scnehaux-architecture `02-standards/_global/STD-GLB-002-database.md`; TDD-organization-control-001 §Restore Evidence; SAD-004 §6.6 |
| R2 | PostgreSQL 17, *pg_restore*, <https://www.postgresql.org/docs/17/app-pgrestore.html>, accessed 2026-10-08: `--exit-on-error`: "Exit if an error is encountered while sending SQL commands to the database. The default is to continue and to display a count of errors at the end of the restoration." |
| R3 | PostgreSQL 17, *Continuous Archiving and Point-in-Time Recovery (PITR)*, <https://www.postgresql.org/docs/17/continuous-archiving.html>, accessed 2026-10-08: "pg_dump and pg_dumpall do not produce file-system-level backups and cannot be used as part of a continuous-archiving solution." |
| R4 | NIST SP 800-53 Rev. 5, CP-10, <https://raw.githubusercontent.com/usnistgov/oscal-content/main/nist.gov/SP800-53/rev5/json/NIST_SP-800-53_rev5_catalog.json>, accessed 2026-10-08: "Provide for the recovery and reconstitution of the system to a known state within [Assignment] after a disruption, compromise, or failure." §After a restore to an older point is what makes the known state the current one. |

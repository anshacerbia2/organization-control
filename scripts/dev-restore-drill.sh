#!/usr/bin/env bash
# The Organization Database restore drill (STD-GLB-002 §Restore Evidence, SAD-004 §6.6,
# ROADMAP.md §Gates, docs/runbooks/organization-database-restore.md). deploy-dev runs it last, after the
# wiring proof has filled the three stacks:
#
#   1. begins an offboarding on a new Tenant, so offboarding state exists to restore, then reads the
#      provider grants, Organizations and offboardings through the API, and stops the service so
#      nothing writes;
#   2. fingerprints the database: its schema with owners, grants and RLS policies, its migration
#      version, every table's row count and content checksum, every sequence, and the cluster's roles;
#   3. backs it up with deploy/dev/backup.sh, the script README.md §Backups gives the operator's cron;
#   4. deletes the database volume with docker compose down --volumes, the loss a backup is for;
#   5. restores into the new, empty volume with deploy/dev/restore.sh, and fingerprints it again
#      before any migration job runs; a second restore.sh over it must refuse, and two probes show
#      the checks are load-bearing: one row removed in a rolled-back transaction changes the
#      fingerprint, and the same dump restored into a cluster without its roles stops;
#   6. starts the stack with docker compose up -d --build, waits for /readyz, and reads again;
#   7. writes restore-evidence.json and fails on any difference, on an empty outbox, delivery,
#      receipt, consumer registry, Membership, Tenant or offboarding table, or on a recovery slower
#      than the 15-minute RTO of PAD-PLT-002 §6.2;
#   8. runs the restore bump restore.sh skipped for the comparison (ADR-ORG-003 §5.5, STD-GLB-002
#      3.3.0 §A restore to an older point): every Tenant version and security version at 10^9 or
#      above, each withdrawn Tenant published at its new version and no active one, every active
#      consumer asked to reconcile, and identity-control applying a bumped Tenant version.
#
# Between steps 3 and 5 it also drills backup.sh's retention (STD-GLB-009 rule 9 and compliance check
# 7, 1.10.0) in a directory of its own, seeded with 16 older complete sets, one lone file and one
# .partial: a backup there keeps exactly the newest 14 complete sets, the one it wrote among them, and
# leaves the lone file and the .partial alone; a backup run while the database is down, after step 4,
# fails and deletes nothing.
#
# It deletes the database, so it refuses to run outside CI. The environment is the wiring proof's:
# IDENTITY_CALLER_PASSWORD, IDENTITY_CALLER_KEY_FILE, KC_BASE_URL and IDENTITY_OPERATOR_TOTP_FILE.
#
#   bash scripts/dev-restore-drill.sh <evidence directory> <identity-control checkout> <wiring state>
set -euo pipefail

usage="usage: dev-restore-drill.sh <evidence directory> <identity-control checkout> <wiring state>"
out="${1:?$usage}"
identity_repo="${2:?$usage}"
state="${3:?$usage}"
if [ "${CI:-}" != "true" ]; then
	echo "refusing: the drill deletes the database volume, so it runs in CI only" >&2
	exit 1
fi

root="$(cd "$(dirname "$0")/.." && pwd)"
deploy="$root/deploy/dev"
database=organization_control
service=organization-control
volume=scnehaux-organization-control-dev_postgres
ready_url="${ORGANIZATION_API_URL:-http://127.0.0.1:8083}/readyz"
rto_seconds=900
# TDD-organization-control-001 §Restore Evidence and SAD-004 §6.6: the outbox and its per-consumer
# delivery, the receipts, the consumer registry with its marks, and the authority itself. Each must
# hold rows in the source, or the drill would be comparing empty tables.
required=(platform.outbox platform.outbox_delivery platform.delivery_receipt platform.subscription
	projection.consumer membership.membership membership.membership_event tenant.tenant
	organization.provider_grant operation.offboarding)

mkdir -p "$out"
out="$(cd "$out" && pwd)"
identity_repo="$(cd "$identity_repo" && pwd)"
state="$(cd "$(dirname "$state")" && pwd)/$(basename "$state")"
# Stands in for storage outside the Docker volume. The workflow never uploads it: the files hold role
# password hashes.
backup="$out/backup"

compose() { (cd "$deploy" && docker compose "$@"); }
now() { date +%s.%N; }
elapsed() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.3f", b - a }'; }

fingerprint() {
	compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -qtA -U postgres -d "$database" \
		< "$root/scripts/restore-fingerprint.sql" | jq -S . > "$out/fingerprint-$1.json"
	compose exec -T postgres pg_dump -U postgres --schema-only --create --restrict-key=drill "$database" \
		> "$out/schema-$1.sql"
}

# read_api <before|after> [-Seed]
read_api() {
	pwsh -NoProfile -File "$root/scripts/dev-restore-read.ps1" -IdentityRepo "$identity_repo" -State "$state" \
		-Out "$out/read-$1.json" "${@:2}"
}

# same <name> <file a> <file b>: true when equal; otherwise false, with the diff kept beside the evidence.
same() {
	if diff -u "$2" "$3" > "$out/diff-$1.txt"; then
		rm -f "$out/diff-$1.txt"
		echo true
	else
		echo false
	fi
}

section() { jq -S ".$2" "$out/fingerprint-$1.json" > "$out/$2-$1.json"; }

started_at="$(date -u +%FT%TZ)"

echo "1. begin an offboarding, read through the API, then stop the service"
read_api before -Seed
compose stop "$service"

echo "2. fingerprint the source"
fingerprint source

echo "3. back up with deploy/dev/backup.sh"
t="$(now)"
bash "$deploy/backup.sh" "$backup" > "$out/backup-files.txt"
backup_seconds="$(elapsed "$t" "$(now)")"
globals="$(sed -n 1p "$out/backup-files.txt")"
dump="$(sed -n 2p "$out/backup-files.txt")"
dump_bytes="$(stat -c %s "$dump")"

echo "3a. retention: backup.sh keeps the newest 14 complete sets"
retention="$out/retention"
mkdir -p "$retention"
# complete_sets <dir>: the <date> of every complete set, newest first, as backup.sh counts them.
complete_sets() {
	for file in "$1"/globals-*.sql; do
		[ -f "$file" ] || continue
		day="${file##*/globals-}"
		day="${day%.sql}"
		if [ -f "$1/$database-$day.dump" ]; then echo "$day"; fi
	done | sort -r
}
for n in $(seq 1 16); do
	day="$(date -u -d "today - $n days" +%F)"
	echo "seeded" > "$retention/globals-$day.sql"
	echo "seeded" > "$retention/$database-$day.dump"
done
lone="globals-$(date -u -d "today - 30 days" +%F).sql"
stale="$database-$(date -u -d "today - 40 days" +%F).dump.partial"
echo "seeded" > "$retention/$lone"
echo "seeded" > "$retention/$stale"
bash "$deploy/backup.sh" "$retention" > "$out/retention-files.txt" 2> "$out/retention-pruned.txt"
written_day="$(sed -n 1p "$out/retention-files.txt" | sed -E 's/.*globals-([0-9-]+)\.sql$/\1/')"
mapfile -t kept < <(complete_sets "$retention")
expected_pruned="$(for n in 14 15 16; do date -u -d "today - $n days" +%F; done | sort -r | tr '\n' ' ')"
actually_pruned="$(sed -nE 's/^pruned the set of ([0-9-]+):.*/\1/p' "$out/retention-pruned.txt" | sort -r | tr '\n' ' ')"
retention_kept=false
[ "${#kept[@]}" -eq 14 ] && retention_kept=true
retention_newest_kept=false
[ "${kept[0]:-}" = "$written_day" ] && [ "$written_day" = "$(date -u +%F)" ] && retention_newest_kept=true
retention_pruned_oldest=false
[ "$actually_pruned" = "$expected_pruned" ] && retention_pruned_oldest=true
retention_incomplete_left=false
[ -f "$retention/$lone" ] && [ -f "$retention/$stale" ] && retention_incomplete_left=true

echo "4. lose the database: docker compose down --volumes"
volume_before="$(docker volume inspect -f '{{.CreatedAt}}' "$volume")"
compose down --volumes
if docker volume inspect "$volume" >/dev/null 2>&1; then
	echo "::error::the database volume survived docker compose down --volumes"
	exit 1
fi

echo "4a. retention: a backup run while the database is down fails and deletes nothing"
sets_before="$(complete_sets "$retention" | tr '\n' ' ')"
retention_failure_refused=false
if bash "$deploy/backup.sh" "$retention" > "$out/retention-failed.txt" 2>&1; then
	echo "::error::backup.sh succeeded with no database to read"
else
	retention_failure_refused=true
fi
retention_failure_deleted_nothing=false
[ "$(complete_sets "$retention" | tr '\n' ' ')" = "$sets_before" ] && [ -f "$retention/$lone" ] &&
	[ -f "$retention/$stale" ] && retention_failure_deleted_nothing=true

echo "5. restore into an empty volume with deploy/dev/restore.sh"
t0="$(now)"
# RESTORE_BUMP=0: the comparison below needs the restored tables as backed up; step 8 bumps them.
RESTORE_BUMP=0 bash "$deploy/restore.sh" "$globals" "$dump"
restore_seconds="$(elapsed "$t0" "$(now)")"
volume_after="$(docker volume inspect -f '{{.CreatedAt}}' "$volume")"
fingerprint restored

# The comparison is load-bearing: one row removed from the probe table, inside a transaction that rolls
# back, must change that table's count and checksum.
probe_table=platform.delivery_receipt
compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -qtA -U postgres -d "$database" \
	-c "BEGIN" -c "SET LOCAL session_replication_role = replica" \
	-c "DELETE FROM $probe_table WHERE ctid = (SELECT ctid FROM $probe_table LIMIT 1)" \
	-f - -c "ROLLBACK" < "$root/scripts/restore-fingerprint.sql" | jq -S . > "$out/fingerprint-probe.json"
probe_entry() { jq -c --arg t "$probe_table" '.tables[] | select(.table == $t) | [.rows, .checksum]' "$1"; }
one_row_detected=false
if [ "$(probe_entry "$out/fingerprint-probe.json")" != "$(probe_entry "$out/fingerprint-restored.json")" ] &&
	[ "$(jq '.tables' "$out/fingerprint-probe.json")" != "$(jq '.tables' "$out/fingerprint-source.json")" ]; then
	one_row_detected=true
fi
rm -f "$out/fingerprint-probe.json"

# Roles first is load-bearing: the same dump restored into a cluster without them must stop on its
# first owner or grantee.
probe_container="restore-drill-probe-$$"
image="$(docker inspect -f '{{.Config.Image}}' "$(compose ps -q postgres)")"
docker run -d --rm --name "$probe_container" -e POSTGRES_PASSWORD="$(openssl rand -hex 16)" "$image" >/dev/null
for _ in $(seq 1 60); do
	docker exec "$probe_container" pg_isready -q -h 127.0.0.1 -U postgres && break
	sleep 1
done
without_roles_refused=false
if docker exec -i "$probe_container" pg_restore -U postgres -d postgres --create --exit-on-error \
	< "$dump" > "$out/restore-without-roles.txt" 2>&1; then
	echo "::error::the dump restored into a cluster without its roles; the roles-first order tests nothing"
else
	grep -qE 'role "[^"]+" does not exist' "$out/restore-without-roles.txt" && without_roles_refused=true
fi
docker rm -f "$probe_container" >/dev/null

refuses_occupied=false
if RESTORE_BUMP=0 bash "$deploy/restore.sh" "$globals" "$dump" > "$out/second-restore.txt" 2>&1; then
	echo "::error::a second restore.sh ran over the restored database; it must refuse"
else
	grep -qF "refusing:" "$out/second-restore.txt" && refuses_occupied=true
fi

echo "6. start the stack on the restored database"
t="$(now)"
compose up -d --build
ready=false
for _ in $(seq 1 180); do
	if curl -fsS "$ready_url" >/dev/null 2>&1; then
		ready=true
		break
	fi
	sleep 1
done
ready_seconds="$(elapsed "$t" "$(now)")"
migrate_ready=false
compose logs --no-color migrate | grep -F "control database ready" >/dev/null && migrate_ready=true
read_ok=false
if [ "$ready" = true ] && read_api after; then
	read_ok=true
fi
recovery_seconds="$(elapsed "$t0" "$(now)")"

echo "7. compare"
for side in source restored; do
	for part in migration tables sequences roles; do section "$side" "$part"; done
done
schema_equal="$(same schema "$out/schema-source.sql" "$out/schema-restored.sql")"
migration_equal="$(same migration "$out/migration-source.json" "$out/migration-restored.json")"
tables_equal="$(same tables "$out/tables-source.json" "$out/tables-restored.json")"
sequences_equal="$(same sequences "$out/sequences-source.json" "$out/sequences-restored.json")"
roles_equal="$(same roles "$out/roles-source.json" "$out/roles-restored.json")"
read_equal=false
if [ "$read_ok" = true ]; then
	read_equal="$(same read "$out/read-before.json" "$out/read-after.json")"
fi
volume_new=false
[ "$volume_before" != "$volume_after" ] && volume_new=true

required_json="$(printf '%s\n' "${required[@]}" | jq -R . | jq -s .)"
critical="$(jq --argjson names "$required_json" \
	'[.tables[] | select(.table as $t | $names | index($t)) | {table, rows}]' "$out/fingerprint-source.json")"
critical_filled="$(jq --argjson names "$required_json" \
	'($names | length) == ([.[] | select(.rows > 0)] | length)' <<< "$critical")"
within_rto="$(awk -v s="$recovery_seconds" -v b="$rto_seconds" 'BEGIN { print (s <= b) ? "true" : "false" }')"

echo "8. the restore bump (ADR-ORG-003 §5.5)"
psql_org() {
	compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -qtA -U postgres -d "$database" -c "$1"
}
compose stop "$service"
compose --profile restore-bump run --rm restore-bump > "$out/restore-bump.txt" 2>&1 || true
compose start "$service"
bump_versions=false
[ "$(psql_org "SELECT count(*) > 0 AND bool_and(version >= 1000000000 AND tenant_security_version >= 1000000000) FROM tenant.tenant")" = t ] &&
	bump_versions=true
withdrawn_published="$(psql_org "SELECT count(*) FROM platform.outbox
	WHERE envelope->'data'->>'tenant_status' IN ('suspended', 'offboarding', 'retired')
	  AND (envelope->'data'->>'tenant_security_version')::bigint >= 1000000000")"
active_published="$(psql_org "SELECT count(*) FROM platform.outbox
	WHERE envelope->'data'->>'tenant_status' = 'active'
	  AND (envelope->'data'->>'tenant_security_version')::bigint >= 1000000000")"
bump_withdrawals_only=false
[ "$withdrawn_published" -gt 0 ] && [ "$active_published" -eq 0 ] && bump_withdrawals_only=true
bump_reconcile_requested=false
[ "$(psql_org "SELECT count(*) > 0 AND bool_and(reconciliation_requested_at IS NOT NULL) FROM projection.consumer WHERE retired_at IS NULL")" = t ] &&
	bump_reconcile_requested=true
# The consumer applies it: identity-control holds a Tenant at a bumped version, delivered by the
# restarted dispatcher. Its apply rule is a higher version only, so this is the version the bump wrote.
bump_applied=false
for _ in $(seq 1 90); do
	applied="$(cd "$identity_repo/deploy/dev" && docker compose exec -T postgres psql -X -qtA -U postgres -d identity_control \
		-c "SELECT count(*) FROM identity.tenant_desired WHERE tenant_version >= 1000000000" 2>/dev/null || echo 0)"
	if [ "${applied:-0}" -gt 0 ] 2>/dev/null; then
		bump_applied=true
		break
	fi
	sleep 2
done

jq -n \
	--arg service organization-control --arg database "$database" --arg started_at "$started_at" \
	--arg commit "${GITHUB_SHA:-}" \
	--arg run "${GITHUB_SERVER_URL:-}/${GITHUB_REPOSITORY:-}/actions/runs/${GITHUB_RUN_ID:-}" \
	--arg postgres "$(compose exec -T postgres postgres --version)" \
	--argjson backup_seconds "$backup_seconds" --argjson dump_bytes "$dump_bytes" \
	--argjson restore_seconds "$restore_seconds" --argjson ready_seconds "$ready_seconds" \
	--argjson recovery_seconds "$recovery_seconds" --argjson rto_seconds "$rto_seconds" \
	--argjson within_rto "$within_rto" --argjson volume_new "$volume_new" \
	--argjson schema_equal "$schema_equal" --argjson migration_equal "$migration_equal" \
	--argjson tables_equal "$tables_equal" --argjson sequences_equal "$sequences_equal" \
	--argjson roles_equal "$roles_equal" --argjson ready "$ready" --argjson migrate_ready "$migrate_ready" \
	--argjson read_equal "$read_equal" --argjson critical "$critical" --argjson critical_filled "$critical_filled" \
	--argjson refuses_occupied "$refuses_occupied" --argjson one_row_detected "$one_row_detected" \
	--argjson without_roles_refused "$without_roles_refused" --arg probe_table "$probe_table" \
	--argjson retention_kept "$retention_kept" --argjson retention_newest_kept "$retention_newest_kept" \
	--argjson retention_pruned_oldest "$retention_pruned_oldest" \
	--argjson retention_incomplete_left "$retention_incomplete_left" \
	--argjson retention_failure_refused "$retention_failure_refused" \
	--argjson retention_failure_deleted_nothing "$retention_failure_deleted_nothing" \
	--arg kept_sets "${kept[*]}" --arg pruned_sets "$actually_pruned" \
	--argjson bump_versions "$bump_versions" --argjson bump_withdrawals_only "$bump_withdrawals_only" \
	--argjson bump_reconcile_requested "$bump_reconcile_requested" --argjson bump_applied "$bump_applied" \
	--argjson withdrawn_published "$withdrawn_published" --argjson active_published "$active_published" \
	--slurpfile source "$out/fingerprint-source.json" \
	'{
	  standard: "STD-GLB-002 §Restore Evidence",
	  service: $service, database: $database, started_at: $started_at, commit: $commit, run: $run,
	  postgres: $postgres,
	  backup: {procedure: "deploy/dev/backup.sh", roles: "pg_dumpall --globals-only",
	           database: "pg_dump --format=custom", seconds: $backup_seconds, dump_bytes: $dump_bytes},
	  loss: {procedure: "docker compose down --volumes", new_volume: $volume_new},
	  restore: {procedure: "deploy/dev/restore.sh", seconds: $restore_seconds,
	            refuses_a_cluster_holding_the_database: $refuses_occupied},
	  integrity: {
	    migration: $source[0].migration,
	    tables: ($source[0].tables | length),
	    rows: ([$source[0].tables[].rows] | add),
	    schema_equal: $schema_equal, migration_equal: $migration_equal, tables_equal: $tables_equal,
	    sequences_equal: $sequences_equal, roles_equal: $roles_equal,
	    critical_tables: $critical, critical_tables_filled: $critical_filled
	  },
	  load_bearing: {one_row_removed_from: $probe_table, detected: $one_row_detected,
	                 restore_without_roles_refused: $without_roles_refused},
	  service_check: {migrate_job_privileges_asserted: $migrate_ready, ready: $ready,
	                  ready_seconds: $ready_seconds, read: "GET /v1/provider-grants, /v1/organizations?limit=100, /v1/offboardings?limit=100",
	                  read_equal: $read_equal},
	  rto: {target_seconds: $rto_seconds, recovery_seconds: $recovery_seconds, within: $within_rto,
	        measured: "from restore.sh on an empty volume to the verified API read"},
	  retention: {rule: "STD-GLB-009 rule 9: the newest 14 complete sets, by count", procedure: "deploy/dev/backup.sh",
	             seeded_complete_sets: 16, kept: ($kept_sets | split(" ") | map(select(. != ""))),
	             pruned: ($pruned_sets | split(" ") | map(select(. != ""))),
	             fourteen_kept: $retention_kept, newest_kept: $retention_newest_kept,
	             oldest_pruned: $retention_pruned_oldest, incomplete_left: $retention_incomplete_left,
	             failed_backup_refused: $retention_failure_refused,
	             failed_backup_deleted_nothing: $retention_failure_deleted_nothing},
	  rpo: {target_seconds: 60, backup_interval_seconds: 86400, met: false,
	        why: "A daily pg_dump loses up to 24 hours. A 1-minute RPO needs continuous WAL archiving with point-in-time recovery on the production platform; a drill of a logical dump never proves it."},
	  restore_bump: {decision: "ADR-ORG-003 §5.5", procedure: "organization-migrate -stage=restore-bump, run by deploy/dev/restore.sh",
	                 margin: 1000000000, versions_bumped: $bump_versions, withdrawn_published: $withdrawn_published,
	                 active_published: $active_published, withdrawals_only: $bump_withdrawals_only,
	                 every_consumer_asked_to_reconcile: $bump_reconcile_requested,
	                 identity_control_applied_a_bumped_tenant: $bump_applied},
	  older_point: {drilled: false,
	                why: "The drill restores to the instant of its own backup, then bumps. Memberships a consumer holds at a higher version are advanced from its report, which internal/membership integration tests prove (SAD-004 §6.6)."}
	}' > "$out/restore-evidence.json"

cat "$out/restore-evidence.json"

checks=(schema_equal migration_equal tables_equal sequences_equal roles_equal critical_filled
	volume_new refuses_occupied one_row_detected without_roles_refused ready migrate_ready read_equal within_rto
	retention_kept retention_newest_kept retention_pruned_oldest retention_incomplete_left
	retention_failure_refused retention_failure_deleted_nothing
	bump_versions bump_withdrawals_only bump_reconcile_requested bump_applied)
failures=0
for check in "${checks[@]}"; do
	if [ "${!check}" = true ]; then
		echo "  ok    $check"
	else
		echo "  FAIL  $check"
		failures=$((failures + 1))
	fi
done
for diff in "$out"/diff-*.txt; do
	[ -e "$diff" ] || continue
	echo "--- $(basename "$diff")"
	head -n 60 "$diff"
done

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "### Organization Database restore drill"
		echo ""
		echo "| Measure | Value |"
		echo "| :-- | :-- |"
		echo "| Outbox, deliveries, receipts, consumers | $(jq -r '[.integrity.critical_tables[] | select(.table | test("outbox$|outbox_delivery$|delivery_receipt$|consumer$")) | "\(.table) \(.rows)"] | join(", ")' "$out/restore-evidence.json") |"
		echo "| Tables, rows | $(jq '.integrity.tables' "$out/restore-evidence.json"), $(jq '.integrity.rows' "$out/restore-evidence.json") |"
		echo "| Backup | ${backup_seconds} s, ${dump_bytes} bytes |"
		echo "| Restore into an empty volume | ${restore_seconds} s |"
		echo "| Recovery, to the verified read | ${recovery_seconds} s, against an RTO of ${rto_seconds} s |"
		echo "| Schema, migration, tables, sequences, roles equal | $schema_equal, $migration_equal, $tables_equal, $sequences_equal, $roles_equal |"
		echo "| API read equal | $read_equal |"
		echo "| Retention, 16 older sets seeded | ${#kept[@]} complete sets kept, pruned: ${actually_pruned:-none}; a backup with the database down refused: $retention_failure_refused, deleted nothing: $retention_failure_deleted_nothing |"
		echo "| Restore bump (ADR-ORG-003 §5.5) | versions bumped: $bump_versions; withdrawn published: $withdrawn_published, active: $active_published; consumers asked to reconcile: $bump_reconcile_requested; identity-control applied a bumped Tenant: $bump_applied |"
		echo "| RPO | not met: a daily dump loses up to 24 h, against 1 min (STD-GLB-002 §Restore Evidence) |"
	} >> "$GITHUB_STEP_SUMMARY"
fi

if [ "$failures" -gt 0 ]; then
	echo "::error::the restore drill failed $failures check(s); see restore-evidence.json"
	exit 1
fi
echo "the Organization Database restores"

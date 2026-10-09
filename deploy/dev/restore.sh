#!/bin/sh
# Restores the Organization Database into an empty stack (README.md §Backups, STD-GLB-009 rule 9).
#
#   ./restore.sh /mnt/imam-storage/backups/organization-control/globals-2026-10-08.sql \
#                /mnt/imam-storage/backups/organization-control/organization_control-2026-10-08.dump
#   docker compose up -d --build
#
# 1. Starts postgres alone and waits until it accepts TCP connections, which the image's first-start
#    initialisation does not.
# 2. Refuses a cluster that already holds organization_control. Replacing a live database is a
#    decision an operator makes by deleting the volume (README.md §Never do), never a side effect.
# 3. Applies the roles from the pg_dumpall file. A database dump holds no roles, and every owner and
#    grantee in it must exist before it is restored. The one error allowed is the bootstrap
#    superuser's "role already exists", which PostgreSQL documents as harmless; any other stops here.
# 4. Restores the database whole with pg_restore --create --exit-on-error: the first error stops it,
#    rather than a count of errors at the end.
# 5. With PAUSE_REASON set, records a pause of Tenant administration before anything can serve
#    (TDD-organization-control-001 §Pausing Tenant Administration). A restore to an older point is
#    reconciled before Tenant administrators change anything (SAD-004 §9.1.1), and a pause made
#    through the API after the service starts would leave a window in which they can. A provider
#    lifts it with POST /v1/tenant-administration-pause {"paused": false} when the reconciliation in
#    docs/runbooks/organization-database-restore.md is done:
#
#      PAUSE_REASON="INC-123 restored from the 2026-10-08 backup" ./restore.sh <globals> <dump>
#
#    The drill (scripts/dev-restore-drill.sh) leaves it unset, because it compares every table with
#    the source's and a pause row would be a difference it made itself.
#
# The migrate job that `docker compose up` runs next finds the schema at its revision, applies
# anything newer, and re-asserts RLS, the privileges and the five login roles' passwords from .env.
# Put .env and keys/ back before that. deploy-dev runs this script in its restore drill
# (scripts/dev-restore-drill.sh).
set -eu

globals="${1:?usage: restore.sh <globals.sql> <database.dump>}"
dump="${2:?usage: restore.sh <globals.sql> <database.dump>}"
database=organization_control
for file in "$globals" "$dump"; do
	[ -r "$file" ] || { echo "cannot read $file" >&2; exit 1; }
done
globals="$(cd "$(dirname "$globals")" && pwd)/$(basename "$globals")"
dump="$(cd "$(dirname "$dump")" && pwd)/$(basename "$dump")"
cd "$(dirname "$0")"

echo "[1/4] postgres"
docker compose up -d postgres
i=0
until docker compose exec -T postgres pg_isready -q -h 127.0.0.1 -U postgres; do
	i=$((i + 1))
	[ "$i" -lt 60 ] || { echo "postgres did not accept connections within 60s" >&2; exit 1; }
	sleep 1
done

echo "[2/4] an empty cluster"
held="$(docker compose exec -T postgres psql -X -qtA -U postgres -d postgres \
	-c "SELECT count(*) FROM pg_database WHERE datname = '$database'")"
if [ "$held" != "0" ]; then
	echo "refusing: this cluster already holds $database; restore into an empty volume" >&2
	exit 1
fi

echo "[3/4] roles"
errors="$(docker compose exec -T -e PGOPTIONS='-c client_min_messages=warning' postgres \
	psql -X -q -U postgres -d postgres < "$globals" 2>&1 >/dev/null)" || true
unexpected="$(printf '%s\n' "$errors" | grep -E 'ERROR|FATAL|PANIC' | grep -v 'role "postgres" already exists' || true)"
if [ -n "$unexpected" ]; then
	printf '%s\n' "$unexpected" >&2
	exit 1
fi

echo "[4/4] $database"
docker compose exec -T postgres pg_restore -U postgres -d postgres --create --exit-on-error < "$dump"

if [ -n "${PAUSE_REASON:-}" ]; then
	echo "[pause] Tenant administration"
	docker compose exec -T postgres psql -X -q -v ON_ERROR_STOP=1 -U postgres -d "$database" \
		-v reason="$PAUSE_REASON" <<'SQL'
INSERT INTO organization.tenant_administration_pause (pause_id, paused, reason)
VALUES (gen_random_uuid(), true, :'reason');
SQL
fi

echo "restored $database; next: docker compose up -d --build"

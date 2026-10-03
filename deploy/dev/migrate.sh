#!/bin/sh
# The Control Database pipeline, in a container: the same sources, in the same order, as the README's
# §Building the database and CI (STD-GLB-009 §Development Server Deployment).
#
#   1. databases                         organization_control and Atlas's scratch org_atlas_dev
#   2. organization-migrate -stage=pre   the cluster roles
#   3. atlas migrate apply               the owned schemas
#   4. organization-migrate -stage=post  the platform schema, RLS, privileges, and their checks
#   5. the five runtime login roles      scripts/login-roles.sql, the file CI's fixture runs
#   6. assertions of the privilege shape
#
# Idempotent, so it runs on every `docker compose up`. POSTGRES_SUPER_URL is the server's superuser
# without a database: roles are a cluster-level act.
set -eu

: "${POSTGRES_SUPER_URL:?POSTGRES_SUPER_URL is required}"
for name in ORGANIZATION_RUNTIME_PASSWORD ORGANIZATION_PROVIDER_PASSWORD ORGANIZATION_DISPATCH_PASSWORD \
	ORGANIZATION_RESOLUTION_PASSWORD ORGANIZATION_CONSUMER_PASSWORD; do
	eval "value=\${$name:-}"
	if [ -z "$value" ]; then
		echo "$name is required" >&2
		exit 1
	fi
done
base="${POSTGRES_SUPER_URL%/}"
database=organization_control
dev=org_atlas_dev

sql() { psql -v ON_ERROR_STOP=1 -qtA "$@"; }

echo "[1/6] databases"
for name in "$database" "$dev"; do
	if [ "$(sql -d "$base/postgres" -c "SELECT 1 FROM pg_database WHERE datname = '$name'")" != "1" ]; then
		sql -d "$base/postgres" -c "CREATE DATABASE \"$name\""
		echo "      created $name"
	fi
done

echo "[2/6] organization-migrate -stage=pre"
ORGANIZATION_MIGRATION_DATABASE_URL="$base/$database?sslmode=disable" organization-migrate -stage=pre

echo "[3/6] atlas migrate apply"
DATABASE_URL="$base/$database?sslmode=disable" ATLAS_DEV_URL="$base/$dev?sslmode=disable" atlas migrate apply --env local

echo "[4/6] organization-migrate -stage=post"
ORGANIZATION_MIGRATION_DATABASE_URL="$base/$database?sslmode=disable" organization-migrate -stage=post

echo "[5/6] runtime login roles"
# Passwords travel as psql variables, quoted by psql, never spliced into the SQL text.
psql -v ON_ERROR_STOP=1 -q -d "$base/$database" \
	-v runtime_password="$ORGANIZATION_RUNTIME_PASSWORD" \
	-v provider_password="$ORGANIZATION_PROVIDER_PASSWORD" \
	-v dispatch_password="$ORGANIZATION_DISPATCH_PASSWORD" \
	-v resolution_password="$ORGANIZATION_RESOLUTION_PASSWORD" \
	-v consumer_password="$ORGANIZATION_CONSUMER_PASSWORD" \
	-f /work/login-roles.sql

echo "[6/6] privilege shape"
check() {
	got="$(sql -d "$base/$database" -c "$2")"
	if [ "$got" != "$3" ]; then
		echo "      FAIL $1 (got $got, want $3)" >&2
		exit 1
	fi
	echo "      ok   $1"
}
check "no runtime login role bypasses RLS" "SELECT count(*) FROM pg_roles WHERE rolname LIKE 'organization\_%\_app' ESCAPE '\\' AND rolbypassrls" 0
check "five runtime login roles" "SELECT count(*) FROM pg_roles WHERE rolname IN ('organization_app','organization_provider_app','organization_dispatch_app','organization_resolution_app','organization_consumer_app') AND rolcanlogin" 5
check "the tenant runtime cannot create in organization" "SELECT has_schema_privilege('organization_app','organization','CREATE')" f
check "no runtime role owns a table" "SELECT count(*) = 0 FROM pg_tables WHERE tableowner LIKE 'organization\_%' ESCAPE '\\' AND tableowner <> 'organization_migrator'" t

echo "control database ready"

#!/bin/sh
# The Organization Database's backup (README.md §Backups, STD-GLB-009 rule 9): the cluster's roles and the
# database, read from the running postgres container into a directory outside the Docker volume.
#
#   ./backup.sh /mnt/imam-storage/backups/organization-control
#
# Writes globals-<date>.sql (pg_dumpall --globals-only) and organization_control-<date>.dump
# (pg_dump --format=custom). Each is written under a .partial name and renamed once complete, so a
# failed run never leaves a file that looks like a backup. Both hold role password hashes, so they are
# readable by their owner alone. Prints the two paths, roles first: restore.sh takes them in that order.
#
# pg_dump "makes consistent backups even if the database is being used concurrently", so the service
# keeps running. It connects over the container's local socket, which the image trusts: no password is
# typed or stored here. deploy-dev runs this script in its restore drill (scripts/dev-restore-drill.sh).
set -eu

dir="${1:?usage: backup.sh <directory outside the Docker volume>}"
database=organization_control

umask 077
mkdir -p "$dir"
dir="$(cd "$dir" && pwd)"
cd "$(dirname "$0")"

stamp="$(date -u +%F)"
globals="$dir/globals-$stamp.sql"
dump="$dir/$database-$stamp.dump"

docker compose exec -T postgres pg_dumpall -U postgres --globals-only > "$globals.partial"
docker compose exec -T postgres pg_dump -U postgres --format=custom "$database" > "$dump.partial"
mv "$globals.partial" "$globals"
mv "$dump.partial" "$dump"

echo "$globals"
echo "$dump"

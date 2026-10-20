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
# Retention is by count (STD-GLB-009 rule 9, 1.10.0): once both files of this run's set are in place,
# it keeps the newest 14 complete sets in the directory and deletes the older ones. A set is complete
# when both files of one <date> exist under their final names. Only complete sets are counted or
# deleted: a lone file, or a .partial left by a failed run, is not a backup this script made whole,
# and is left for the operator. A run that fails stops before the prune (set -e), so a failed or
# silent backup never deletes a good one, which an age-based `find -mtime +N -delete` would do once
# backups stop for N days. The set this run wrote is the newest, and is never deleted. Each deleted
# set is named on stderr; stdout stays the two paths.
#
# pg_dump "makes consistent backups even if the database is being used concurrently", so the service
# keeps running. It connects over the container's local socket, which the image trusts: no password is
# typed or stored here. deploy-dev runs this script in its restore drill (scripts/dev-restore-drill.sh).
set -eu

dir="${1:?usage: backup.sh <directory outside the Docker volume>}"
database=organization_control
keep=14

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

# The complete sets, newest first. <date> is date -u +%F, so the names sort by time.
complete_sets() {
	for file in "$dir"/globals-*.sql; do
		[ -f "$file" ] || continue
		day="${file##*/globals-}"
		day="${day%.sql}"
		case "$day" in
		[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) ;;
		*) continue ;;
		esac
		if [ -f "$dir/$database-$day.dump" ]; then echo "$day"; fi
	done | sort -r
}
count=0
for day in $(complete_sets); do
	count=$((count + 1))
	if [ "$count" -le "$keep" ] || [ "$day" = "$stamp" ]; then
		continue
	fi
	rm -f "$dir/globals-$day.sql" "$dir/$database-$day.dump"
	echo "pruned the set of $day: the newest $keep complete sets are kept" >&2
done

echo "$globals"
echo "$dump"

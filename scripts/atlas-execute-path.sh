#!/usr/bin/env bash
# Proves what the not_affected statements for /usr/local/bin/atlas in .grype.yaml rest on
# (STD-GLB-009 1.8.0 §Container Images rule 7): as the migrate image runs it, Atlas listens on
# nothing and connects to nothing but the database. image-scan runs it on every change and daily.
#
#   scripts/atlas-execute-path.sh <migrate-image> [VARIABLE ...]
#
# <migrate-image> is the migrate target, already built. Each VARIABLE is one more variable the
# image's entrypoint requires besides POSTGRES_SUPER_URL; it gets a random value.
#
# It starts the Control Database that deploy/dev/compose.yaml pins, on a Docker network with no
# egress. It then runs the image's own entrypoint, the whole pipeline, with Atlas and only Atlas
# wrapped in strace. The database is named by its address, so a DNS query would show as a connection
# to the resolver. The run fails on:
#
#   - a listen() or accept(), or a bind() of an inet socket: Atlas served something;
#   - an inet connect() to anything but the database's address and port: Atlas called out;
#   - a pipeline that fails, or a trace with no connection to the database: the trace saw nothing.
#
# A control run then empties ATLAS_NO_UPDATE_NOTIFIER, which Atlas reads as unset. It must show the
# release check as a connection to the resolver. If it does not, the trace is blind, and the run
# fails.
#
# It starts a database container: run it in CI or on a workstation, not on a shared host.
set -euo pipefail

image="${1:?usage: scripts/atlas-execute-path.sh <migrate-image> [VARIABLE ...]}"
shift

db_image="$(awk '/^  postgres:/ {found = 1} found && $1 == "image:" {print $2; exit}' deploy/dev/compose.yaml)"
case "$db_image" in
  *@sha256:*) ;;
  *) echo "::error::deploy/dev/compose.yaml names no digest-pinned image for its postgres service"
     exit 1 ;;
esac

# CI's temporary directory, or one under the checkout, which .gitignore covers. Never /tmp.
WORK="${RUNNER_TEMP:-$PWD/.image-scan}/atlas-execute-path"
rm -rf "$WORK"
mkdir -p "$WORK/pipeline" "$WORK/control"
# The image's unprivileged user writes the traces.
chmod 0777 "$WORK/pipeline" "$WORK/control"

name="atlas-execute-path-$$"
cleanup() {
  docker rm -f "$name-db" >/dev/null 2>&1 || true
  docker network rm "$name" >/dev/null 2>&1 || true
  docker rmi -f "$name" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# The image under test, with Atlas behind strace. strace is added for this run only and never ships.
# The binary ships execute-only, and the kernel keeps a tracer out of the memory of a process whose
# file it cannot read, so strace could not decode an address; here it is made readable.
user="$(docker image inspect -f '{{.Config.User}}' "$image")"
docker build -q -t "$name" - >/dev/null <<EOF
FROM $image
USER root
RUN apk add --no-cache strace \\
 && mv /usr/local/bin/atlas /usr/local/bin/atlas.real \\
 && chmod 0755 /usr/local/bin/atlas.real \\
 && printf '%s\\n' '#!/bin/sh' 'exec strace -f -qq -e trace=socket,connect,bind,listen,accept,accept4 -o "/trace/atlas.\$\$" /usr/local/bin/atlas.real "\$@"' >/usr/local/bin/atlas \\
 && chmod 0755 /usr/local/bin/atlas
USER ${user:-root}
EOF

docker network create --internal "$name" >/dev/null
password="$(openssl rand -hex 16)"
docker run -d --name "$name-db" --network "$name" -e POSTGRES_PASSWORD="$password" "$db_image" >/dev/null
for attempt in $(seq 90); do
  # Over TCP, because while initdb runs the image's temporary server listens on its socket alone.
  if docker exec "$name-db" pg_isready -q -h 127.0.0.1 -U postgres; then
    break
  fi
  if [ "$attempt" = 90 ]; then
    echo "::error::the database did not come up"
    docker logs "$name-db"
    exit 1
  fi
  sleep 1
done
db_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$name-db")"

env_args=(-e "POSTGRES_SUPER_URL=postgres://postgres:$password@$db_ip:5432")
for variable in "$@"; do
  env_args+=(-e "$variable=$(openssl rand -hex 16)")
done

echo "::group::the pipeline, with Atlas traced"
if ! docker run --rm --network "$name" -v "$WORK/pipeline:/trace" "${env_args[@]}" "$name"; then
  echo "::endgroup::"
  echo "::error::the migrate image's pipeline failed, so its trace proves nothing"
  exit 1
fi
echo "::endgroup::"

echo "::group::the control: Atlas with its release check on"
docker run --rm --network "$name" -v "$WORK/control:/trace" -e ATLAS_NO_UPDATE_NOTIFIER= \
  --entrypoint atlas "$name" version
echo "::endgroup::"

python3 - "$WORK" "$db_ip" <<'PY'
import glob, re, sys

work, db = sys.argv[1], sys.argv[2]
PORT = re.compile(r"htons\((\d+)\)")
ADDRESS = re.compile(r'inet_addr\("([^"]+)"\)|inet_pton\(AF_INET6, "([^"]+)"')


def trace(kind):
    files = sorted(glob.glob(f"{work}/{kind}/atlas.*"))
    return files, [line.rstrip("\n") for path in files for line in open(path, encoding="utf-8", errors="replace")]


def inet_connects(lines):
    for line in lines:
        if re.search(r"\bconnect\(\d+, \{sa_family=AF_INET6?,", line):
            port = PORT.search(line)
            address = ADDRESS.search(line)
            yield line, (address.group(1) or address.group(2)) if address else "?", int(port.group(1)) if port else -1


errors = []
files, lines = trace("pipeline")
if not files:
    errors.append("the pipeline never ran Atlas, so nothing was traced")
for line in lines:
    if re.search(r"\b(listen|accept4?)\(", line):
        errors.append(f"Atlas listened or accepted: {line}")
    if re.search(r"\bbind\(\d+, \{sa_family=AF_INET6?,", line):
        errors.append(f"Atlas bound an inet socket: {line}")
database = 0
for line, address, port in inet_connects(lines):
    if (address, port) == (db, 5432):
        database += 1
    else:
        errors.append(f"Atlas connected to {address}:{port}, which is not the database: {line}")
if files and not database:
    errors.append("the trace holds no connection to the database, so it saw nothing")

_, control = trace("control")
resolver = sum(1 for _, _, port in inet_connects(control) if port == 53)
if not resolver:
    errors.append("the control run showed no DNS query with the release check on: the trace is blind")

for message in errors:
    print(f"::error::{message}")
if errors:
    sys.exit(1)
print(f"the pipeline: Atlas ran {len(files)} time(s) and made {database} connection(s), all to the database at "
      f"{db}:5432, with no listen(), accept() or inet bind()")
print(f"the control: with ATLAS_NO_UPDATE_NOTIFIER empty, Atlas made {resolver} connection(s) to the resolver")
PY

#!/usr/bin/env bash
# STD-GLB-009 3.0.0 §5 item 9 (ADR-GLB-020): a repository that builds an image has a .dockerignore
# that excludes the three kinds of path of §Container Images rule 12. This fails when the file is
# missing, or when a path of one of the kinds reaches the build context.
#
# The check uses Docker's own matching, not gitignore's: it makes a scratch build context that holds
# the repository's .dockerignore and one probe file for each kind, builds a Dockerfile that copies the
# whole context out, and fails if any probe came out. Nothing is tagged, so no image is left behind.
#
# Run from the repository root. This file is byte-identical in identity-kernel, identity-control and
# organization-control; a change goes to all three.
set -euo pipefail

cd "$(dirname "$0")/.."

[ -f .dockerignore ] || { echo "check-dockerignore: .dockerignore is missing (STD-GLB-009 §Container Images rule 12)" >&2; exit 1; }

# The scratch directories sit in the repository, never in /tmp (a RAM disk). Both are removed on exit.
scratch="$(mktemp -d "$PWD/.dockerignore-check.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
ctx="$scratch/ctx"
out="$scratch/out"
mkdir -p "$ctx" "$out"
cp .dockerignore "$ctx/.dockerignore"

# One probe for each kind of rule 12: secrets and key material, local state, version control and CI.
probes=(
  keys/probe
  deploy/dev/keys/probe
  .env
  deploy/dev/.env
  probe.pem
  deploy/dev/compose.override.yaml
  .token
  .kernel/probe
  coverage/probe
  .git/probe
  .github/probe
)
for p in "${probes[@]}"; do
  mkdir -p "$ctx/$(dirname "$p")"
  echo probe >"$ctx/$p"
done
echo keep >"$ctx/kept"

# busybox:1.37, pinned by digest (resolved from Docker Hub on 2026-10-21).
dockerfile='FROM busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
COPY . /ctx
FROM scratch
COPY --from=0 /ctx /'

DOCKER_BUILDKIT=1 docker build --quiet --output "type=local,dest=$out" -f - "$ctx" <<<"$dockerfile" >/dev/null

# The control: a file no rule names must arrive, or the build proved nothing.
[ -f "$out/kept" ] || { echo "check-dockerignore: the control file did not reach the context; the check is broken" >&2; exit 1; }

fail=0
for p in "${probes[@]}"; do
  if [ -e "$out/$p" ]; then
    echo "check-dockerignore: $p reaches the build context; .dockerignore does not match it" >&2
    fail=1
  fi
done
[ "$fail" -eq 0 ] || exit 1
echo "check-dockerignore: ok, no probe of rule 12 reaches the build context"

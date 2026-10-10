#!/usr/bin/env bash
# Scans this repository's container images for known vulnerabilities (STD-GLB-009 1.8.0
# §Container Images, which lands with scnehaux-architecture #86). CI runs it on every change and daily;
# it runs no service and changes nothing.
#
#   scripts/image-scan.sh <image>...
#
# Each argument is an image this repository built, already in the local Docker daemon. Every image
# a deploy/dev compose file names by digest is scanned as well, from its registry. For each image:
#
#   - a report of every finding, ignored ones included, which never fails the run (rule 4);
#   - the gate, which fails on a High or Critical vulnerability that has a fixed version.
#
# .grype.yaml holds the ignore rules (rule 5). scripts/image-scan-rules.py checks them first (§5
# Enforcement item 4): every rule names the vulnerability and the package's name, version and type, a
# package found inside a file also its full path with no wildcard, and its reason has the parts of
# rule 5 in order with a justification rule 7 accepts; a review date that has passed, or one beyond
# rule 5's limits, fails the run.
#
# The scanner runs from an image pinned by digest (rule 6): never a tag, never a fetched script. Its
# database and the image archives go under RUNNER_TEMP in CI and under ./.image-scan elsewhere, never
# in /tmp.
set -euo pipefail

# anchore/grype:v0.120.1, resolved from Docker Hub on 2026-10-07.
GRYPE_IMAGE="${GRYPE_IMAGE:-anchore/grype@sha256:e4a44ef45d285b829ce6efe2642980329661bd2d18eab5fc539138d4adaebbbe}"
CONFIG=.grype.yaml
WORK="${RUNNER_TEMP:+$RUNNER_TEMP/image-scan}"
WORK="${WORK:-$PWD/.image-scan}"
mkdir -p "$WORK/images" "$WORK/db" "$WORK/tmp"

python3 "$(dirname "$0")/image-scan-rules.py" "$CONFIG"

targets=()
for image in "$@"; do
  archive="$(echo "$image" | tr '/:@' '___').tar"
  docker save "$image" -o "$WORK/images/$archive"
  targets+=("docker-archive:/images/$archive")
done
while read -r ref; do
  targets+=("registry:$ref")
done < <(grep -hoE '^[[:space:]]*image:[[:space:]]*[^[:space:]]+@sha256:[0-9a-f]{64}' deploy/dev/compose*.yaml 2>/dev/null |
  awk '{print $2}' | sort -u)

grype() {
  docker run --rm --user "$(id -u):$(id -g)" \
    -e GRYPE_DB_CACHE_DIR=/db -e GRYPE_CHECK_FOR_APP_UPDATE=false \
    -v "$WORK/db:/db" -v "$WORK/tmp:/tmp" -v "$WORK/images:/images:ro" -v "$PWD/$CONFIG:/config.yaml:ro" \
    "$GRYPE_IMAGE" "$@" --config /config.yaml
}

status=0
for target in "${targets[@]}"; do
  echo "::group::$target: every finding"
  # --show-suppressed: a finding an ignore rule hides from the gate stays in the report, marked.
  grype "$target" -o table --show-suppressed || echo "::warning::the report for $target did not complete"
  echo "::endgroup::"
  echo "== $target: the gate, High or Critical with a fix"
  # Grype exits 2 when it found a vulnerability at or above --fail-on, and 1 when the scan failed.
  code=0
  grype "$target" --only-fixed --fail-on high -o table || code=$?
  case "$code" in
    0) ;;
    2) echo "::error::$target holds a High or Critical vulnerability with a fix (STD-GLB-009 §Container Images rule 4)"
       status=1 ;;
    *) echo "::error::the scan of $target did not complete (exit $code)"
       status=1 ;;
  esac
done
exit "$status"

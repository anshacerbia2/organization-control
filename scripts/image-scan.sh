#!/usr/bin/env bash
# Scans this repository's container images for known vulnerabilities (STD-GLB-009 1.4.0
# §Container Images). CI runs it on every change and daily; it runs no service and changes nothing.
#
#   scripts/image-scan.sh <image>...
#
# Each argument is an image this repository built, already in the local Docker daemon. Every image
# a deploy/dev compose file names by digest is scanned as well, from its registry. For each image:
#
#   - a report of every finding, which never fails the run (rule 4: what has no fix is reported);
#   - the gate, which fails on a High or Critical vulnerability that has a fixed version.
#
# .grype.yaml holds the ignore rules (rule 5). Each rule's reason begins "review-by YYYY-MM-DD:",
# at most 90 days ahead, and the run fails once that date has passed.
#
# The scanner runs from an image pinned by digest (rule 6): never a tag, never a fetched script.
set -euo pipefail

# anchore/grype:v0.120.1, resolved from Docker Hub on 2026-10-07.
GRYPE_IMAGE="${GRYPE_IMAGE:-anchore/grype@sha256:e4a44ef45d285b829ce6efe2642980329661bd2d18eab5fc539138d4adaebbbe}"
CONFIG=.grype.yaml
WORK="${RUNNER_TEMP:-$(mktemp -d)}/image-scan"
mkdir -p "$WORK/images" "$WORK/db" "$WORK/tmp"

python3 - "$CONFIG" <<'PY'
import datetime, re, sys
path = sys.argv[1]
text = open(path, encoding="utf-8").read()
today = datetime.date.today()
rules = re.findall(r"^  - ", text, re.M)
# A reason is written inline or as a folded block (reason: >-), and begins with its review date.
reasons = re.findall(r"^\s+reason:\s*(?:>-?[ \t]*\n\s*)?['\"]?review-by (\d{4}-\d{2}-\d{2}):", text, re.M)
failed = False
if len(rules) != len(reasons):
    print(f"::error file={path}::{len(rules)} ignore rules and {len(reasons)} reasons beginning 'review-by YYYY-MM-DD:'; every rule needs one")
    failed = True
for raw in reasons:
    due = datetime.date.fromisoformat(raw)
    if due < today:
        print(f"::error file={path}::an ignore rule's review date {raw} has passed; fix the finding or decide it again")
        failed = True
    elif (due - today).days > 90:
        print(f"::error file={path}::an ignore rule's review date {raw} is more than 90 days ahead")
        failed = True
print(f"{len(rules)} ignore rules, each with a review date that has not passed" if not failed else "")
sys.exit(1 if failed else 0)
PY

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
  grype "$target" -o table || echo "::warning::the report for $target did not complete"
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

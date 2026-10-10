#!/usr/bin/env bash
# Scans this repository's container images for known vulnerabilities (STD-GLB-009 1.8.0
# §Container Images). CI runs it on every change and daily; it runs no service and changes nothing.
#
#   scripts/image-scan.sh <image>...
#
# Each argument is an image this repository built, already in the local Docker daemon. Every image
# a deploy/dev compose file names by digest is scanned as well, from its registry. For each image:
#
#   - a report of every finding, ignored ones included, which never fails the run (rule 4);
#   - the gate, which fails on a High or Critical vulnerability that has a fixed version.
#
# .grype.yaml holds the ignore rules (rule 5), each a VEX statement about one file. Before it scans,
# the run reads them and fails (STD-GLB-009 1.8.0 §5 Enforcement item 4) on a rule:
#
#   - whose review date has passed;
#   - whose reason lacks rule 5's parts in order: review-by YYYY-MM-DD, affected or not_affected/
#     with a justification rule 7 accepts, detected YYYY-MM-DD, the images, the statement;
#   - whose review date is more than 90 days ahead, or, for affected, more than 90 days after
#     detected, the longest time rule 8 sets; or whose detected date is after today or after it;
#   - without vulnerability, package.name, package.version or package.location, or whose location
#     is relative or a wildcard. Every package Grype reports is found in a file, a Go module in its
#     binary and an Alpine package in /lib/apk/db/installed, so every rule names one.
#
# Whether a statement is true, and whether rule 8's time was read right, are left to review.
#
# The scanner runs from an image pinned by digest (rule 6): never a tag, never a fetched script.
set -euo pipefail

# anchore/grype:v0.120.1, resolved from Docker Hub on 2026-10-07.
GRYPE_IMAGE="${GRYPE_IMAGE:-anchore/grype@sha256:e4a44ef45d285b829ce6efe2642980329661bd2d18eab5fc539138d4adaebbbe}"
CONFIG=.grype.yaml
# CI's temporary directory, or one under the checkout, which .gitignore covers. Never /tmp: on some
# hosts it is memory, and the vulnerability database alone is hundreds of megabytes.
WORK="${RUNNER_TEMP:-$PWD/.image-scan}/image-scan"
mkdir -p "$WORK/images" "$WORK/db" "$WORK/tmp"

python3 - "$CONFIG" <<'PY'
import datetime, re, sys

path = sys.argv[1]
today = datetime.date.today()
GLOB = set("*?[]{}")
JUSTIFICATIONS = {"component_not_present", "vulnerable_code_not_present", "vulnerable_code_not_in_execute_path"}
REASON = re.compile(r"review-by (\d{4}-\d{2}-\d{2}): (affected|not_affected/([a-z_]+)): "
                    r"detected (\d{4}-\d{2}-\d{2}): (.+?): (.+)")

# The ignore list, read in the shape this file is written in: each rule a "  - " item, its keys at
# four spaces, the package's at six, and a reason inline or as a folded block (reason: >-). A line
# this cannot place fails the run, so the checks never pass on a rule they did not read.
rules, rule, section, folding, errors = [], None, None, None, []
in_ignore = False
for number, raw in enumerate(open(path, encoding="utf-8").read().splitlines(), 1):
    line = raw.rstrip()
    indent = len(line) - len(line.lstrip(" "))
    if folding is not None:
        if not line or indent > folding:
            rule["reason"] = (rule.get("reason", "") + " " + line.strip()).strip()
            continue
        folding = None
    if not line or line.lstrip().startswith("#"):
        continue
    if indent == 0:
        in_ignore = line == "ignore:"
        continue
    if not in_ignore:
        continue
    if line.startswith("  - "):
        rule, section = {"line": number}, None
        rules.append(rule)
        line, indent = "    " + line[4:], 4
    if rule is None:
        errors.append(f"line {number}: not inside an ignore rule")
        continue
    key, _, value = line.strip().partition(":")
    value = value.strip().strip("'\"")
    if indent == 4:
        section = key if key == "package" and not value else None
        if key == "reason" and value in (">", ">-"):
            rule["reason"], folding = "", 4
        else:
            rule[key] = value
    elif indent == 6 and section == "package":
        rule["package." + key] = value
    else:
        errors.append(f"line {number}: cannot place this line in an ignore rule")

for rule in rules:
    where = f"the ignore rule at line {rule['line']} ({rule.get('vulnerability', '?')})"
    for key in ("vulnerability", "package.name", "package.version"):
        if not rule.get(key):
            errors.append(f"{where} has no {key}; a rule names the vulnerability, the package and its version (rule 5)")
    location = rule.get("package.location", "")
    if not location:
        errors.append(f"{where} has no package.location; every package Grype reports is found in a file, and the rule names it (rule 5)")
    elif not location.startswith("/") or GLOB & set(location):
        errors.append(f"{where} has package.location '{location}'; it must be the file's full path, without a wildcard (rule 5)")
    match = REASON.fullmatch(rule.get("reason", ""))
    if not match:
        errors.append(f"{where}: its reason must read 'review-by YYYY-MM-DD: <status>: detected YYYY-MM-DD: <images>: <statement>' (rule 5)")
        continue
    review_raw, status, justification, detected_raw = match.group(1, 2, 3, 4)
    if justification is not None and justification not in JUSTIFICATIONS:
        errors.append(f"{where}: not_affected/{justification} is not a justification rule 7 accepts: {', '.join(sorted(JUSTIFICATIONS))}")
    try:
        review = datetime.date.fromisoformat(review_raw)
        detected = datetime.date.fromisoformat(detected_raw)
    except ValueError as exc:
        errors.append(f"{where}: {exc}")
        continue
    if review < today:
        errors.append(f"{where}: its review date {review} has passed; fix the finding or decide it again")
    elif (review - today).days > 90:
        errors.append(f"{where}: its review date {review} is more than 90 days ahead (rule 5)")
    if detected > today or detected > review:
        errors.append(f"{where}: detected {detected} is after today or after its review date")
    if status == "affected" and (review - detected).days > 90:
        errors.append(f"{where}: an affected finding is due within rule 8's time, at most 90 days after detected {detected}")

for message in errors:
    print(f"::error file={path}::{message}")
if not rules:
    print(f"{path}: no ignore rules")
elif not errors:
    print(f"{len(rules)} ignore rules, each in rule 5's form, with a review date that has not passed")
sys.exit(1 if errors else 0)
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

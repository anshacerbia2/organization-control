#!/usr/bin/env python3
"""Checks the ignore rules in .grype.yaml against STD-GLB-009 1.8.0 §Container Images rule 5.

    python3 scripts/image-scan-rules.py .grype.yaml [YYYY-MM-DD]

scripts/image-scan.sh runs it before any scan. The date defaults to today (UTC). It fails (exit 1) on:

  - a rule without `vulnerability`, `package.name`, `package.version` or `package.type`;
  - a rule for a package found inside a file (any type but an operating-system package database:
    apk, deb, rpm) without `package.location`, and any `package.location` with a wildcard
    (`*`, `?`, `[`, `{`), because Grype reads it as a glob;
  - a `reason` that is not `review-by YYYY-MM-DD: <status>: detected YYYY-MM-DD: <images>: <statement>`,
    where `<status>` is `affected` or `not_affected/<justification>`, and the justification is
    `component_not_present`, `vulnerable_code_not_present` or `vulnerable_code_not_in_execute_path`
    (rule 7);
  - a review date that has passed;
  - a review date more than 90 days ahead, or before the detection date;
  - a detection date in the future;
  - for `affected`, a review date more than 90 days after the detection date, the longest remediation
    time rule 8 allows.

Whether a statement is true, and whether rule 8's time was read correctly, are left to review.

It reads the one form .grype.yaml is written in (a top-level `ignore:` list of mappings, `package:`
nested, `reason: >-` folded or a plain scalar), with the standard library only, and refuses anything
else rather than guessing.
"""
import datetime
import re
import sys

OS_TYPES = {"apk", "deb", "rpm"}
JUSTIFICATIONS = {
    "component_not_present",
    "vulnerable_code_not_present",
    "vulnerable_code_not_in_execute_path",
}
RULE_KEYS = {"vulnerability", "package", "reason"}
PACKAGE_KEYS = {"name", "version", "type", "location"}
TOP_KEYS = {"check-for-app-update", "ignore"}
REASON = re.compile(
    r"^review-by (\d{4}-\d{2}-\d{2}): (affected|not_affected/([a-z_]+)): "
    r"detected (\d{4}-\d{2}-\d{2}): (\S.*?): (\S.*)$"
)
GLOB = re.compile(r"[*?\[\]{}]")
REVIEW_LIMIT = 90


class FormatError(Exception):
    pass


def scalar(raw):
    raw = raw.strip()
    if len(raw) >= 2 and raw[0] == raw[-1] and raw[0] in "'\"":
        return raw[1:-1]
    return raw


def parse(text):
    """Returns (top-level keys, rules), each rule a dict with `package` a dict. Line numbers kept."""
    lines = text.split("\n")
    top, rules = {}, []
    i = 0
    in_ignore = False

    def indent(s):
        return len(s) - len(s.lstrip(" "))

    while i < len(lines):
        line = lines[i]
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            i += 1
            continue
        if "\t" in line[: indent(line) + 1]:
            raise FormatError(f"line {i + 1}: indentation with a tab")
        ind = indent(line)
        if ind == 0:
            key, sep, value = stripped.partition(":")
            if not sep or key not in TOP_KEYS:
                raise FormatError(f"line {i + 1}: unexpected top-level line {stripped!r}")
            top[key] = scalar(value)
            in_ignore = key == "ignore"
            if in_ignore and value.strip() not in ("", "[]"):
                raise FormatError(f"line {i + 1}: write ignore rules as a block list, or [] for none")
            i += 1
            continue
        if not in_ignore or ind != 2 or not stripped.startswith("- "):
            raise FormatError(f"line {i + 1}: expected an ignore rule starting '  - ', found {stripped!r}")
        rule = {"line": i + 1, "package": {}}
        # The first key sits after the dash; the rest at indent 4.
        first = stripped[2:]
        pending = [(4, first, i)]
        i += 1
        while i < len(lines):
            nxt = lines[i]
            if nxt.strip() and not nxt.strip().startswith("#") and indent(nxt) <= 2:
                break
            if nxt.strip() and not nxt.strip().startswith("#"):
                pending.append((indent(nxt), nxt.strip(), i))
            elif not nxt.strip():
                pending.append((None, "", i))
            i += 1
        j = 0
        section = None
        while j < len(pending):
            ind2, content, ln = pending[j]
            if ind2 is None:
                j += 1
                continue
            key, sep, value = content.partition(":")
            if not sep:
                raise FormatError(f"line {ln + 1}: expected 'key: value', found {content!r}")
            key, value = key.strip(), value.strip()
            if ind2 == 4:
                section = None
                if key not in RULE_KEYS:
                    raise FormatError(f"line {ln + 1}: unexpected rule key {key!r}")
                if key in rule and key != "package":
                    raise FormatError(f"line {ln + 1}: {key!r} given twice")
                if key == "package":
                    if value:
                        raise FormatError(f"line {ln + 1}: write package as a nested mapping")
                    section = "package"
                    j += 1
                    continue
                if key == "reason" and value in (">", ">-"):
                    parts = []
                    j += 1
                    while j < len(pending) and (pending[j][0] is None or pending[j][0] > 4):
                        if pending[j][0] is not None:
                            parts.append(pending[j][1])
                        j += 1
                    rule["reason"] = " ".join(parts)
                    continue
                if value.startswith((">", "|")):
                    raise FormatError(f"line {ln + 1}: block scalar {value!r}; write reason as '>-'")
                rule[key] = scalar(value)
            elif ind2 == 6 and section == "package":
                if key not in PACKAGE_KEYS:
                    raise FormatError(f"line {ln + 1}: unexpected package key {key!r}")
                if key in rule["package"]:
                    raise FormatError(f"line {ln + 1}: package {key!r} given twice")
                rule["package"][key] = scalar(value)
            else:
                raise FormatError(f"line {ln + 1}: unexpected indentation for {content!r}")
            j += 1
        rules.append(rule)
    return top, rules


def check(rule, today):
    errors = []
    pkg = rule["package"]
    for field in ("vulnerability",):
        if not rule.get(field):
            errors.append(f"no {field}")
    for field in ("name", "version", "type"):
        if not pkg.get(field):
            errors.append(f"no package.{field}")
    ptype = pkg.get("type", "")
    location = pkg.get("location")
    if ptype and ptype not in OS_TYPES and not location:
        errors.append(f"a {ptype} package is found inside a file: give package.location, its full path in the image")
    if location is not None and (not location.startswith("/") or GLOB.search(location)):
        errors.append(f"package.location {location!r} must be a full path with no wildcard")
    reason = rule.get("reason", "")
    m = REASON.match(reason)
    if not m:
        errors.append(
            "reason must read 'review-by YYYY-MM-DD: affected|not_affected/<justification>: "
            "detected YYYY-MM-DD: <images>: <statement>'"
        )
        return errors
    review_raw, status, justification, detected_raw = m.group(1), m.group(2), m.group(3), m.group(4)
    try:
        review = datetime.date.fromisoformat(review_raw)
        detected = datetime.date.fromisoformat(detected_raw)
    except ValueError as e:
        errors.append(f"a date does not parse: {e}")
        return errors
    if justification is not None and justification not in JUSTIFICATIONS:
        errors.append(
            f"justification {justification!r} is not accepted; use one of {', '.join(sorted(JUSTIFICATIONS))} "
            "(rule 7), or 'affected'"
        )
    if review < today:
        errors.append(f"review date {review_raw} has passed: fix the finding or decide it again")
    if (review - today).days > REVIEW_LIMIT:
        errors.append(f"review date {review_raw} is more than {REVIEW_LIMIT} days ahead")
    if detected > today:
        errors.append(f"detected {detected_raw} is in the future")
    if review < detected:
        errors.append(f"review date {review_raw} is before detected {detected_raw}")
    if status == "affected" and (review - detected).days > REVIEW_LIMIT:
        errors.append(
            f"an affected finding is fixed within rule 8's time, at most {REVIEW_LIMIT} days after detected "
            f"{detected_raw}; review date {review_raw} is later"
        )
    return errors


def main(argv):
    if len(argv) not in (2, 3):
        print(__doc__.split("\n\n")[1], file=sys.stderr)
        return 2
    path = argv[1]
    today = datetime.date.fromisoformat(argv[2]) if len(argv) == 3 else datetime.datetime.now(datetime.timezone.utc).date()
    try:
        _, rules = parse(open(path, encoding="utf-8").read())
    except FormatError as e:
        print(f"::error file={path}::{e}")
        return 1
    failed = False
    for rule in rules:
        for err in check(rule, today):
            print(f"::error file={path},line={rule['line']}::{rule.get('vulnerability', '?')} "
                  f"({rule['package'].get('name', '?')}): {err}")
            failed = True
    if not failed:
        print(f"{len(rules)} ignore rules, each in the form of STD-GLB-009 1.8.0 rule 5 and not past review")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

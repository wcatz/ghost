"""Pure transforms between the model's findings.json and the GitHub
Reviews API payload.

No network, no filesystem, no GitHub calls — everything here is a function
of its arguments so it can be unit-tested off-CI. post_review.py does the
I/O.
"""

import re

VERDICTS = ("blocker", "should-fix", "nit", "clean")
SEVERITIES = ("blocker", "should-fix", "nit")
BLOCKING = ("blocker", "should-fix")


class ValidationError(ValueError):
    """The model produced a document we refuse to act on."""


def _require(cond, msg):
    if not cond:
        raise ValidationError(msg)


def validate(doc):
    """Raise ValidationError unless doc matches the findings.json contract."""
    _require(isinstance(doc, dict), "top level must be an object")
    _require(doc.get("verdict") in VERDICTS,
             f"verdict must be one of {VERDICTS}, got {doc.get('verdict')!r}")
    _require(isinstance(doc.get("summary"), str) and doc["summary"].strip(),
             "summary must be a non-empty string")
    findings = doc.get("findings")
    _require(isinstance(findings, list), "findings must be a list")

    for i, f in enumerate(findings):
        where = f"findings[{i}]"
        _require(isinstance(f, dict), f"{where} must be an object")
        for key in ("file", "title", "body"):
            _require(isinstance(f.get(key), str) and f[key].strip(),
                     f"{where}.{key} must be a non-empty string")
        path = f["file"]
        _require(not path.startswith("/") and ".." not in path.split("/"),
                 f"{where}.file must be a repo-relative path, got {path!r}")
        _require(f.get("severity") in SEVERITIES,
                 f"{where}.severity must be one of {SEVERITIES}")
        # bool is a subclass of int; reject it explicitly.
        line = f.get("line")
        _require(isinstance(line, int) and not isinstance(line, bool) and line > 0,
                 f"{where}.line must be a positive integer")
        end = f.get("end_line")
        if end is not None:
            _require(isinstance(end, int) and not isinstance(end, bool),
                     f"{where}.end_line must be an integer")
            _require(end >= line, f"{where}.end_line must be >= line")
        sug = f.get("suggestion")
        if sug is not None:
            _require(isinstance(sug, str), f"{where}.suggestion must be a string")
    return doc


_HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@")


def parse_hunks(diff_text):
    """Map repo-relative path -> set of RIGHT-side line numbers in the diff.

    GitHub only accepts review comments anchored to a line that appears in
    the diff, which means added ('+') or context (' ') lines. Deleted files
    (+++ /dev/null) contribute nothing.
    """
    hunks = {}
    path = None
    new_line = 0

    for raw in diff_text.splitlines():
        if raw.startswith("diff --git "):
            path, new_line = None, 0
            continue
        if raw.startswith("--- "):
            continue
        if raw.startswith("+++ "):
            target = raw[4:].strip()
            path = None if target == "/dev/null" else re.sub(r"^b/", "", target)
            continue
        m = _HUNK_RE.match(raw)
        if m:
            new_line = int(m.group(1))
            continue
        if path is None or new_line == 0:
            continue
        if raw.startswith("+") or raw.startswith(" ") or raw == "":
            hunks.setdefault(path, set()).add(new_line)
            new_line += 1
        # '-' lines consume no RIGHT-side number; '\' (no newline) is inert.
    return hunks

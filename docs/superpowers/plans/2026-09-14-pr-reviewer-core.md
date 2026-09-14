# PR Reviewer Core (Phases 1–3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A PR reviewer that posts line-anchored, severity-gated review threads from opencode CLI output, sweeps stale threads, and converges instead of oscillating.

**Architecture:** The model emits a strict-schema findings document on stdout — it is given no write tools — and every downstream decision is deterministic Python in `.github/scripts/`, unit-tested off-CI. The workflows stay thin: assemble context, run the model, hand JSON to a tested script. Three convergence inputs (incremental diff, existing thread state, base-ref conventions) are gathered in the prepare step before `.git` is deleted.

**Tech Stack:** GitHub Actions, opencode CLI (`opencode/big-pickle`, free tier), Python 3 stdlib only (`unittest`, `json`, `re`), `gh` CLI, GitHub GraphQL + Reviews REST API.

**Spec:** `docs/superpowers/specs/2026-09-14-pr-reviewer-design.md`

**Scope:** Phases 1–3 only. Phases 4–7 (summary comment, suggestions, linter cross-check, chat) get their own plans.

## Pre-verified before this plan was handed off (2026-09-14)

Do not re-derive these; they were run against the real repo, not asserted.

| Claim | Evidence |
|---|---|
| actionlint catches the `administration:` key that broke #421 | v1.7.12, exit 1, `unknown permission scope "administration"` at `gate.yml:46` |
| actionlint is clean on `main`'s current workflows | exit 0 |
| actionlint over all of #421's workflows finds exactly 1 issue | the `gate.yml` one that took CI down |
| actionlint FAILS LOUDLY when auto-discovery finds nothing | `no project was found in any parent directories of "..."`, exit 3 (`command.go` ExitStatusFailure). An earlier claim in this table that it silently exits 0 was WRONG — it came from grepping the output for `^\.github` and reading "no matching lines" as "no findings" without checking the exit code. Explicit paths are still used in Task 1, but for a different reason: they pin the scanned set and make the count line positive evidence of what ran |
| The Tasks 2–4b Python is correct as written | all 32 tests pass, `Ran 32 tests ... OK` |
| `extract_findings` recovers JSON from prose, fences, escaped quotes, multi-object streams | 11 dedicated tests, incl. failure cases (pure prose, truncated JSON, empty) |
| The transform layer is hardened against adversarial input | 12 adversarial tests added after review found 5 reproduced defects: uncaught `RecursionError`, marker forgery via 5 different fields, ```suggestion fence breakout, a stray prose quote discarding a valid reply, `+++` header confusion, and unusable git-quoted paths. Suite 32 -> 44 |
| The stdout channel carries real model output on an x86_64 runner | #421's review of 2026-09-14T04:59:07Z: 4,241 chars, first line `verdict: nit` |
| The model's write path is unverified (NOT disproven) | `--auto` defaults false; a local probe was invalid — aarch64 host vs the pinned linux-x64 build |
| `parse_hunks` handles real diffs | 3 real diffs (80 KB / 7 KB / 95 KB), 33 files, no `a/`–`b/` prefix leaks, no non-positive lines |
| `ci.yml` runs under `bash -e`, NOT `-eo pipefail` | no `shell:` or `defaults.run.shell` in the file; GitHub uses `-eo pipefail` only when `shell: bash` is set explicitly. Verified both forms against the count line: `-e` → exit 0, `-eo pipefail` → exit 2 |
| `parse_hunks` never anchors past EOF | 32 files cross-checked against their content at `main`: every anchorable line number exists |

Two facts that shape the plan and were also checked:

- `main`'s `gate.yml` does **not** contain the `administration: read` bug — it
  was introduced only on #421's branch, so abandoning that branch retires it.
  No fix task is needed.
- `main`'s `best-practices-loop.yml` still calls the Zen REST endpoint
  (`opencode.ai/zen/v1/chat/completions`), the exact transport #421 proved
  cannot reach the free tier. That workflow is therefore broken on `main` too.
  Out of scope here; it needs its own PR.

---

## Design decisions locked in before Task 1

**Marker placement (decouples phase 3 from phase 4).** The spec put the
`reviewed:<sha>` marker on the sticky summary comment, which phase 4 creates.
This plan puts it on the **review body** the reviewer already posts in phase 1:
`<!-- ghost-review:<sha> -->`. Phase 3's incremental diff reads it from the most
recent bot review. No dependency on phase 4.

**`event: COMMENT`, never `REQUEST_CHANGES`, from the reviewer.** The reviewer
posts findings; `sweeper.yml` owns the merge signal after it knows thread state.
This matches the validated split in `pr-loop.yml` and keeps the reviewer's token
needs minimal.

**Base-ref context must be extracted before `rm -rf .git`.** The prepare step
deletes `.git` so the model cannot reach head-ref instruction files. Conventions
from `main` therefore have to be written to plain files first, and they must NOT
be named `CLAUDE.md`/`AGENTS.md` — the injection sweep would delete them, and
opencode would auto-load them outside our prompt control. They go to
`.review-context/conventions.md`.

## File structure

| File | Responsibility |
|---|---|
| `.github/scripts/review_findings.py` | Schema validation, diff-hunk parsing, severity partition, anchor resolution, review-payload construction. Pure functions, no I/O against GitHub. |
| `.github/scripts/test_review_findings.py` | `unittest` suite for validation, hunks, partition, payload. |
| `.github/scripts/test_extract.py` | `unittest` suite for recovering the JSON from the model's stdout. |
| `.github/scripts/post_review.py` | Thin I/O wrapper: reads files, calls `review_findings`, POSTs via `gh api`. |
| `.github/workflows/reviewer.yml` | Trigger, context assembly, model invocation, post. |
| `.github/workflows/sweeper.yml` | Stale-thread resolution + `REQUEST_CHANGES` signal (restored from `pr-loop.yml`). |
| `.github/workflows/ci.yml` | Add `actionlint` to the existing `lint` job. |

---

## Task 1: actionlint guard

The invalid `administration:` permissions key and the column-0 YAML break both
shipped to CI in PR #421. This catches that class statically, and it goes first
so every later task is protected by it.

**Files:**
- Modify: `.github/workflows/ci.yml` (the `lint` job)

- [x] **Step 1: Write a deliberately invalid workflow to prove the check catches it**

Create `/tmp/bad-workflow-test.yml`:

```yaml
name: bad
on: pull_request
permissions:
  contents: read
jobs:
  x:
    runs-on: ubuntu-latest
    permissions:
      administration: read
    steps:
      - run: echo hi
```

- [x] **Step 2: Run actionlint against it locally to confirm it fails**

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 /tmp/bad-workflow-test.yml
```

Expected: non-zero exit, with an error naming `administration` as an unknown
permission scope. If actionlint does NOT flag it, stop and report — the guard
does not cover the case it was chosen for, and the task needs rethinking.

Already verified on 2026-09-14 with actionlint v1.7.12; the exact output was:

```
bad-workflow-test.yml:9:7: unknown permission scope "administration". all
available permission scopes are "actions", "artifact-metadata", "attestations",
"checks", "contents", "deployments", "discussions", "id-token", "issues",
"models", "packages", "pages", "pull-requests", "repository-projects",
"security-events", "statuses" [permissions]
```

Run against PR #421's workflow set it produces exactly one finding — the
`gate.yml:46` `administration: read` that took CI down. `main`'s current
workflows produce zero.

- [x] **Step 3: Add the actionlint step to the lint job**

In `.github/workflows/ci.yml`, inside the `lint` job's `steps:`, after checkout:

```yaml
      - name: actionlint
        # Workflow-file validity is not covered by golangci-lint. PR #421
        # shipped both an invalid `administration:` permissions key (which
        # makes Actions reject the whole file at parse time — 0 jobs, no
        # log) and a column-0 comment that broke YAML indentation. Both are
        # statically detectable; neither was caught before merge.
        #
        # Pinned release + SHA256, matching the repo's supply-chain
        # convention for third-party binaries.
        run: |
          AL_VERSION=1.7.12
          AL_SHA256=8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8
          # --retry: `lint` is a required check, so a transient Releases
          # 5xx would otherwise redden every open PR with no code defect.
          curl --retry 3 --retry-connrefused -fsSL --proto '=https' --tlsv1.2 \
            -o /tmp/actionlint.tar.gz \
            "https://github.com/rhysd/actionlint/releases/download/v${AL_VERSION}/actionlint_${AL_VERSION}_linux_amd64.tar.gz"
          echo "${AL_SHA256}  /tmp/actionlint.tar.gz" | sha256sum -c -
          tar -xzf /tmp/actionlint.tar.gz -C /tmp actionlint
          # Count and scan the SAME set by construction. nullglob makes a
          # non-matching pattern expand to nothing instead of a literal, so
          # a repo with only .yml (like this one today) and one with .yaml
          # both work, and the guard below cannot disagree with what is
          # actually scanned.
          shopt -s nullglob
          FILES=(.github/workflows/*.yml .github/workflows/*.yaml)
          shopt -u nullglob
          # Explicit paths, not bare `actionlint`: this pins the scanned set
          # to .github/workflows/*.{yml,yaml} rather than relying on
          # actionlint's own project auto-discovery, and the count line below
          # is positive evidence of what was actually scanned.
          [ "${#FILES[@]}" -gt 0 ] || { echo "::error::actionlint found no workflow files to scan"; exit 1; }
          echo "actionlint scanning ${#FILES[@]} workflow file(s)"
          /tmp/actionlint -color "${FILES[@]}"
```

- [x] **Step 4: Verify it runs clean against the current workflows**

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -color
```

Expected: exit 0. If existing workflows have pre-existing findings, fix only
genuine errors; add `# actionlint-ignore` or a `.actionlint.yaml` exclusion for
style-only noise rather than expanding scope.

- [x] **Step 5: Commit**

```bash
git add .github/workflows/ci.yml
git commit -s -m "ci: add actionlint to the lint job"
```

---

## Task 2: findings schema validation

**Files:**
- Create: `.github/scripts/review_findings.py`
- Create: `.github/scripts/test_review_findings.py`

- [ ] **Step 1: Write the failing tests**

Create `.github/scripts/test_review_findings.py`:

```python
import unittest

from review_findings import ValidationError, validate


def _doc(**over):
    doc = {
        "verdict": "should-fix",
        "summary": "Adds a thing.",
        "findings": [
            {
                "file": "internal/memory/store.go",
                "line": 42,
                "severity": "should-fix",
                "title": "Unchecked error",
                "body": "The error from Exec is discarded.",
            }
        ],
    }
    doc.update(over)
    return doc


class TestValidate(unittest.TestCase):
    def test_accepts_a_minimal_valid_document(self):
        validate(_doc())

    def test_accepts_clean_verdict_with_no_findings(self):
        validate(_doc(verdict="clean", findings=[]))

    def test_rejects_unknown_verdict(self):
        with self.assertRaises(ValidationError):
            validate(_doc(verdict="looks-fine"))

    def test_rejects_missing_summary(self):
        doc = _doc()
        del doc["summary"]
        with self.assertRaises(ValidationError):
            validate(doc)

    def test_rejects_unknown_severity(self):
        with self.assertRaises(ValidationError):
            validate(_doc(findings=[{
                "file": "a.go", "line": 1, "severity": "critical",
                "title": "t", "body": "b",
            }]))

    def test_rejects_non_integer_line(self):
        with self.assertRaises(ValidationError):
            validate(_doc(findings=[{
                "file": "a.go", "line": "42", "severity": "nit",
                "title": "t", "body": "b",
            }]))

    def test_rejects_end_line_before_line(self):
        with self.assertRaises(ValidationError):
            validate(_doc(findings=[{
                "file": "a.go", "line": 10, "end_line": 4,
                "severity": "nit", "title": "t", "body": "b",
            }]))

    def test_rejects_findings_not_a_list(self):
        with self.assertRaises(ValidationError):
            validate(_doc(findings={"file": "a.go"}))

    def test_rejects_absolute_and_traversing_paths(self):
        for bad in ("/etc/passwd", "../outside.go"):
            with self.assertRaises(ValidationError):
                validate(_doc(findings=[{
                    "file": bad, "line": 1, "severity": "nit",
                    "title": "t", "body": "b",
                }]))


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd .github/scripts && python3 -m unittest test_review_findings -v
```

Expected: FAIL with `ModuleNotFoundError: No module named 'review_findings'`.

- [ ] **Step 3: Write the implementation**

Create `.github/scripts/review_findings.py`:

```python
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
```

- [ ] **Step 4: Run to verify it passes**

```bash
cd .github/scripts && python3 -m unittest test_review_findings -v
```

Expected: 9 tests, all PASS.

- [ ] **Step 5: Commit**

```bash
git add .github/scripts/review_findings.py .github/scripts/test_review_findings.py
git commit -s -m "ci: add findings.json schema validation"
```

---

## Task 3: diff-hunk parsing

An anchor outside the diff makes the Reviews API reject the **entire** review,
so out-of-range findings must be detected and dropped rather than posted.

**Files:**
- Modify: `.github/scripts/review_findings.py`
- Modify: `.github/scripts/test_review_findings.py`

- [ ] **Step 1: Write the failing tests**

Append to `.github/scripts/test_review_findings.py` (and add `parse_hunks` to
the import line at the top):

```python
DIFF = """diff --git a/a.go b/a.go
index 111..222 100644
--- a/a.go
+++ b/a.go
@@ -1,3 +1,5 @@
 package main
+
+func added() {}
 
 func kept() {}
diff --git a/b.go b/b.go
index 333..444 100644
--- a/b.go
+++ b/b.go
@@ -10,2 +10,3 @@ func x() {
 	a := 1
+	b := 2
 	_ = a
diff --git a/gone.go b/gone.go
deleted file mode 100644
index 555..000
--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package main
-func dead() {}
"""


class TestParseHunks(unittest.TestCase):
    def test_maps_added_and_context_lines_per_file(self):
        hunks = parse_hunks(DIFF)
        self.assertEqual(hunks["a.go"], {1, 2, 3, 4, 5})
        self.assertEqual(hunks["b.go"], {10, 11, 12})

    def test_ignores_deleted_files(self):
        self.assertNotIn("gone.go", parse_hunks(DIFF))

    def test_does_not_confuse_the_minus_header_with_a_removed_line(self):
        # '--- a/a.go' starts with '-' but is a header, not a deletion.
        self.assertIn(1, parse_hunks(DIFF)["a.go"])

    def test_empty_diff_yields_no_hunks(self):
        self.assertEqual(parse_hunks(""), {})
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd .github/scripts && python3 -m unittest test_review_findings -v
```

Expected: FAIL with `ImportError: cannot import name 'parse_hunks'`.

- [ ] **Step 3: Write the implementation**

Append to `.github/scripts/review_findings.py`:

```python
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
```

- [ ] **Step 4: Run to verify it passes**

```bash
cd .github/scripts && python3 -m unittest test_review_findings -v
```

Expected: 13 tests, all PASS.

- [ ] **Step 5: Commit**

```bash
git add .github/scripts/review_findings.py .github/scripts/test_review_findings.py
git commit -s -m "ci: parse diff hunks to validate review anchors"
```

---

## Task 4: severity partition and review-payload construction

**Files:**
- Modify: `.github/scripts/review_findings.py`
- Modify: `.github/scripts/test_review_findings.py`

- [ ] **Step 1: Write the failing tests**

Append to `.github/scripts/test_review_findings.py` (add `build_review` and
`partition` to the imports):

```python
class TestPartition(unittest.TestCase):
    def test_splits_blocking_from_nits(self):
        findings = [
            {"severity": "blocker"}, {"severity": "should-fix"},
            {"severity": "nit"},
        ]
        blocking, nits = partition(findings)
        self.assertEqual([f["severity"] for f in blocking],
                         ["blocker", "should-fix"])
        self.assertEqual([f["severity"] for f in nits], ["nit"])


class TestBuildReview(unittest.TestCase):
    def setUp(self):
        self.hunks = {"a.go": {1, 2, 3, 4, 5}}

    def _f(self, **over):
        f = {"file": "a.go", "line": 3, "severity": "should-fix",
             "title": "Unchecked error", "body": "Exec's error is dropped."}
        f.update(over)
        return f

    def test_anchors_a_single_line_finding(self):
        doc = {"verdict": "should-fix", "summary": "s",
               "findings": [self._f()]}
        payload, dropped = build_review(doc, "abc123", self.hunks)
        self.assertEqual(payload["event"], "COMMENT")
        self.assertEqual(payload["commit_id"], "abc123")
        self.assertEqual(len(payload["comments"]), 1)
        c = payload["comments"][0]
        self.assertEqual((c["path"], c["line"], c["side"]), ("a.go", 3, "RIGHT"))
        self.assertNotIn("start_line", c)
        self.assertEqual(dropped, [])

    def test_anchors_a_multi_line_finding_with_start_line(self):
        doc = {"verdict": "should-fix", "summary": "s",
               "findings": [self._f(line=2, end_line=4)]}
        payload, _ = build_review(doc, "abc123", self.hunks)
        c = payload["comments"][0]
        self.assertEqual((c["start_line"], c["line"]), (2, 4))
        self.assertEqual(c["start_side"], "RIGHT")

    def test_nits_never_become_comments(self):
        doc = {"verdict": "nit", "summary": "s",
               "findings": [self._f(severity="nit")]}
        payload, _ = build_review(doc, "abc123", self.hunks)
        self.assertEqual(payload["comments"], [])
        self.assertIn("Unchecked error", payload["body"])

    def test_out_of_diff_anchor_is_dropped_not_posted(self):
        doc = {"verdict": "should-fix", "summary": "s",
               "findings": [self._f(line=99)]}
        payload, dropped = build_review(doc, "abc123", self.hunks)
        self.assertEqual(payload["comments"], [])
        self.assertEqual(len(dropped), 1)
        self.assertIn("could not be anchored", payload["body"])

    def test_unknown_file_is_dropped(self):
        doc = {"verdict": "should-fix", "summary": "s",
               "findings": [self._f(file="nope.go")]}
        _, dropped = build_review(doc, "abc123", self.hunks)
        self.assertEqual(len(dropped), 1)

    def test_suggestion_renders_a_fenced_block(self):
        doc = {"verdict": "should-fix", "summary": "s",
               "findings": [self._f(suggestion="\tif err != nil {\n")]}
        payload, _ = build_review(doc, "abc123", self.hunks)
        self.assertIn("```suggestion", payload["comments"][0]["body"])

    def test_body_carries_the_reviewed_sha_marker(self):
        doc = {"verdict": "clean", "summary": "All good.", "findings": []}
        payload, _ = build_review(doc, "deadbeef", self.hunks)
        self.assertIn("<!-- ghost-review:deadbeef -->", payload["body"])
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd .github/scripts && python3 -m unittest test_review_findings -v
```

Expected: FAIL with `ImportError: cannot import name 'build_review'`.

- [ ] **Step 3: Write the implementation**

Append to `.github/scripts/review_findings.py`:

```python
MARKER = "<!-- ghost-review:{sha} -->"

_LABEL = {"blocker": "🔴 blocker", "should-fix": "🟠 should-fix", "nit": "🔵 nit"}


def partition(findings):
    """Split findings into (blocking, nits) per the severity policy."""
    blocking = [f for f in findings if f.get("severity") in BLOCKING]
    nits = [f for f in findings if f.get("severity") == "nit"]
    return blocking, nits


def _render_comment(f):
    parts = [f"**{_LABEL[f['severity']]} — {f['title']}**", "", f["body"]]
    if f.get("suggestion") is not None:
        parts += ["", "```suggestion", f["suggestion"].rstrip("\n"), "```"]
    return "\n".join(parts)


def _anchor(f, hunks):
    """Return a Reviews-API comment dict, or None if it is not in the diff."""
    lines = hunks.get(f["file"])
    if not lines:
        return None
    start = f["line"]
    end = f.get("end_line") or start
    if start not in lines or end not in lines:
        return None
    comment = {
        "path": f["file"],
        "line": end,
        "side": "RIGHT",
        "body": _render_comment(f),
    }
    if end != start:
        comment["start_line"] = start
        comment["start_side"] = "RIGHT"
    return comment


def build_review(doc, commit_id, hunks):
    """Return (review_payload, dropped_findings).

    Blocking findings become inline comments; nits and un-anchorable
    findings go in the review body so nothing is silently lost.
    """
    blocking, nits = partition(doc["findings"])

    comments, dropped = [], []
    for f in blocking:
        anchored = _anchor(f, hunks)
        if anchored is None:
            dropped.append(f)
        else:
            comments.append(anchored)

    body = [doc["summary"], ""]
    body.append(f"**Verdict:** `{doc['verdict']}` — "
                f"{len(comments)} inline finding(s), {len(nits)} nit(s).")

    if nits:
        body += ["", "<details><summary>Nits "
                 f"({len(nits)}) — non-blocking</summary>", ""]
        for f in nits:
            loc = f"`{f['file']}:{f['line']}`"
            body.append(f"- {loc} **{f['title']}** — {f['body']}")
        body += ["", "</details>"]

    if dropped:
        body += ["", f"**{len(dropped)} finding(s) could not be anchored** "
                 "to a line in this diff and are reported here instead:", ""]
        for f in dropped:
            loc = f"`{f['file']}:{f['line']}`"
            body.append(f"- {_LABEL[f['severity']]} {loc} "
                        f"**{f['title']}** — {f['body']}")

    body += ["", MARKER.format(sha=commit_id)]

    payload = {
        "commit_id": commit_id,
        "body": "\n".join(body),
        "event": "COMMENT",
        "comments": comments,
    }
    return payload, dropped
```

- [ ] **Step 4: Run to verify it passes**

```bash
cd .github/scripts && python3 -m unittest test_review_findings -v
```

Expected: 21 tests, all PASS.

- [ ] **Step 5: Commit**

```bash
git add .github/scripts/review_findings.py .github/scripts/test_review_findings.py
git commit -s -m "ci: build severity-gated review payloads from findings"
```

---

## Task 4b: extract the findings document from the model reply

**The model is given no write tools.** Its only output channel is stdout.

Three things support this, stated at the confidence they actually have:

1. **The stdout path is proven in CI.** PR #421's green runs produced real
   reviews through it — e.g. the 2026-09-14T04:59:07Z review, 4,241 characters
   beginning `verdict: nit`, generated by `opencode run --format json` on an
   `ubuntu-latest` runner and scraped from the event stream. Whatever else is
   uncertain, this channel demonstrably carries model output in CI.
2. **There is no evidence the write path works.** `opencode run` documents
   `--auto` ("auto-approve permissions that are not explicitly denied") as
   defaulting to `false`, so a write tool needs an approval a non-interactive
   runner cannot give. #421 scraped stdout rather than reading a file, which is
   consistent with the same conclusion having been reached there.
3. **Write-less is the better boundary regardless.** Granting `--auto` would
   hand write and shell capability to a model whose input is an
   attacker-controllable diff, undoing the isolation the rest of this design
   pays for.

A local attempt to test the write path directly was **inconclusive and should
not be cited**: it ran on an `aarch64` workstation against the ARM build, while
this plan pins `opencode-linux-x64.tar.gz`. It returned nothing in 7 minutes for
both a write request and a plain-text request, which given the architecture
mismatch says nothing about the x86_64 runner path.

The mistake in #421 was not choosing stdout — it was scraping it badly.
`parts[-1]` takes a single event and silently loses a reply split across
several.

So the JSON has to be recovered from prose. This task makes that robust and
tested rather than a regex guess, with schema validation (Task 2) as the gate
behind it.

**Files:**
- Modify: `.github/scripts/review_findings.py`
- Create: `.github/scripts/test_extract.py`

- [ ] **Step 1: Write the failing tests**

Create `.github/scripts/test_extract.py`:

```python
import unittest
from review_findings import ValidationError, extract_findings

DOC = '{"verdict":"clean","summary":"ok","findings":[]}'


class TestExtract(unittest.TestCase):
    def test_bare_json(self):
        self.assertEqual(extract_findings(DOC)["verdict"], "clean")

    def test_json_with_prose_before_and_after(self):
        t = f"Let me review this.\n\n{DOC}\n\nHope that helps!"
        self.assertEqual(extract_findings(t)["verdict"], "clean")

    def test_fenced_json_block(self):
        t = f"Here is the result:\n\n```json\n{DOC}\n```\n"
        self.assertEqual(extract_findings(t)["verdict"], "clean")

    def test_prefers_the_last_verdict_object(self):
        t = f'draft: {{"verdict":"nit","summary":"x","findings":[]}}\nfinal: {DOC}'
        self.assertEqual(extract_findings(t)["verdict"], "clean")

    def test_ignores_non_verdict_objects(self):
        t = f'{{"note":"thinking"}} {DOC} {{"unrelated":true}}'
        self.assertEqual(extract_findings(t)["verdict"], "clean")

    def test_handles_braces_inside_strings(self):
        d = '{"verdict":"nit","summary":"use {} not new Object()","findings":[]}'
        self.assertEqual(extract_findings(d)["summary"], "use {} not new Object()")

    def test_handles_escaped_quotes(self):
        d = '{"verdict":"nit","summary":"say \\"hi\\"","findings":[]}'
        self.assertEqual(extract_findings(d)["summary"], 'say "hi"')

    def test_nested_objects_in_findings(self):
        d = ('{"verdict":"should-fix","summary":"s","findings":'
             '[{"file":"a.go","line":1,"severity":"nit","title":"t","body":"b"}]}')
        self.assertEqual(len(extract_findings(d)["findings"]), 1)

    def test_raises_on_pure_prose(self):
        with self.assertRaises(ValidationError):
            extract_findings("I reviewed the PR and found three issues.")

    def test_raises_on_truncated_json(self):
        with self.assertRaises(ValidationError):
            extract_findings('{"verdict":"clean","summary":"ok"')

    def test_raises_on_empty(self):
        with self.assertRaises(ValidationError):
            extract_findings("")


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd .github/scripts && python3 -m unittest test_extract -v
```

Expected: FAIL with `ImportError: cannot import name 'extract_findings'`.

- [ ] **Step 3: Write the implementation**

Append to `.github/scripts/review_findings.py`:

```python
def extract_findings(text):
    """Pull the findings document out of a model's free-form reply.

    The model has no write tools by design — its only output channel is
    stdout — so the JSON has to be recovered from whatever prose, fenced
    blocks, or event wrappers surround it. Scans for balanced top-level
    JSON objects and returns the LAST one that carries a 'verdict' key,
    which survives a model that reasons in prose before answering, wraps
    the answer in ```json, or restates a partial object mid-explanation.

    Raises ValidationError when nothing usable is present, so a garbled
    reply fails the job loudly instead of posting a degraded review.
    """
    import json as _json

    candidates = []
    depth = 0
    start = None
    in_str = False
    esc = False

    for i, ch in enumerate(text):
        if in_str:
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == '"':
                in_str = False
            continue
        if ch == '"':
            in_str = True
        elif ch == "{":
            if depth == 0:
                start = i
            depth += 1
        elif ch == "}":
            if depth > 0:
                depth -= 1
                if depth == 0 and start is not None:
                    candidates.append(text[start:i + 1])
                    start = None

    for blob in reversed(candidates):
        try:
            doc = _json.loads(blob)
        except ValueError:
            continue
        if isinstance(doc, dict) and "verdict" in doc:
            return doc

    raise ValidationError(
        "no JSON object with a 'verdict' key found in the model reply "
        f"({len(candidates)} balanced object(s) scanned, "
        f"{len(text)} chars)")
```

- [ ] **Step 4: Run to verify it passes**

```bash
cd .github/scripts && python3 -m unittest discover -p 'test_*.py' -v
```

Expected: 44 tests, all PASS.

- [ ] **Step 5: Commit**

```bash
git add .github/scripts/review_findings.py .github/scripts/test_extract.py
git commit -s -m "ci: extract the findings document from model stdout"
```

---

## Task 5: run the script tests in CI

Tests that only run locally are tests that rot.

**Files:**
- Modify: `.github/workflows/ci.yml` (the `lint` job)

- [ ] **Step 1: Add the step**

In `.github/workflows/ci.yml`, in the `lint` job after the actionlint step:

```yaml
      - name: Review-script unit tests
        # stdlib-only unittest; ubuntu-latest ships python3, so no setup
        # step and no dependency install.
        working-directory: .github/scripts
        run: python3 -m unittest discover -p 'test_*.py' -v
```

- [ ] **Step 2: Verify locally the same command passes**

```bash
cd .github/scripts && python3 -m unittest discover -p 'test_*.py' -v
```

Expected: 44 tests, OK.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/ci.yml
git commit -s -m "ci: run review-script unit tests in the lint job"
```

---

## Task 6: the posting wrapper

**Files:**
- Create: `.github/scripts/post_review.py`

- [ ] **Step 1: Write the script**

Create `.github/scripts/post_review.py`:

```python
#!/usr/bin/env python3
"""Read the model's raw reply + pr.diff, build a review payload, POST it.

Exits non-zero on a malformed document — a bad review is never posted in
degraded form. Usage:

    post_review.py <repo> <pr_number> <commit_id> <reply.txt> <pr.diff>
"""

import json
import subprocess
import sys

from review_findings import (ValidationError, build_review, extract_findings,
                             parse_hunks, validate)


def main(argv):
    if len(argv) != 6:
        print(__doc__, file=sys.stderr)
        return 2
    repo, number, commit_id, reply_path, diff_path = argv[1:]

    with open(reply_path, encoding="utf-8", errors="replace") as fh:
        reply = fh.read()

    try:
        doc = extract_findings(reply)
        validate(doc)
    except ValidationError as exc:
        print(f"::error::unusable model reply: {exc}", file=sys.stderr)
        # Surface a bounded excerpt so the failure is diagnosable from the
        # Actions log without re-running the model.
        print(f"::group::model reply (first 2000 chars)\n{reply[:2000]}\n::endgroup::",
              file=sys.stderr)
        return 1

    with open(diff_path, encoding="utf-8") as fh:
        hunks = parse_hunks(fh.read())

    payload, dropped = build_review(doc, commit_id, hunks)
    for f in dropped:
        print(f"::warning::finding not anchorable to the diff "
              f"({f['file']}:{f['line']}): {f['title']}")

    proc = subprocess.run(
        ["gh", "api", f"repos/{repo}/pulls/{number}/reviews",
         "--method", "POST", "--input", "-"],
        input=json.dumps(payload), text=True, capture_output=True, check=False,
    )
    if proc.returncode != 0:
        print(f"::error::posting the review failed: {proc.stderr.strip()}",
              file=sys.stderr)
        return 1

    print(f"posted review: {len(payload['comments'])} inline finding(s), "
          f"{len(dropped)} unanchored")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
```

- [ ] **Step 2: Verify it rejects a malformed document without calling gh**

```bash
cd .github/scripts
echo '{"verdict":"nope","summary":"x","findings":[]}' > /tmp/bad.txt
echo '' > /tmp/empty.diff
python3 post_review.py wcatz/ghost 1 abc /tmp/bad.txt /tmp/empty.diff; echo "exit=$?"
```

Expected: `::error::unusable model reply: verdict must be one of
('blocker', 'should-fix', 'nit', 'clean'), got 'nope'` and `exit=1`.

- [ ] **Step 3: Verify it rejects non-JSON**

```bash
cd .github/scripts
printf 'I reviewed the PR and found some issues.\n' > /tmp/prose.txt
python3 post_review.py wcatz/ghost 1 abc /tmp/prose.txt /tmp/empty.diff; echo "exit=$?"
```

Expected: `::error::unusable model reply: no JSON object with a 'verdict' key
found in the model reply (0 '{' position(s) tried, 41 chars)` and `exit=1`,
followed by the bounded reply excerpt. (41, not 40: `printf` writes a
trailing newline.)

- [ ] **Step 4: Commit**

```bash
git add .github/scripts/post_review.py
git commit -s -m "ci: add the review posting wrapper"
```

---

## Task 7: reviewer.yml — triggers, identity, context

Cherry-picks the correct parts of PR #421 and applies the two fixes its own
last commit introduced.

**Files:**
- Create: `.github/workflows/reviewer.yml`

- [ ] **Step 1: Create the workflow through the context step**

```yaml
name: Reviewer

on:
  pull_request:
    paths-ignore:
      - '**/*.md'
      - 'docs/**'
    types: [opened, reopened, synchronize]

permissions:
  contents: read
  pull-requests: write

concurrency:
  group: reviewer-${{ github.event.pull_request.number }}
  cancel-in-progress: true

jobs:
  review:
    runs-on: ubuntu-latest
    timeout-minutes: 25
    steps:
      - name: Mint bot identity token
        id: apptoken
        continue-on-error: true
        uses: actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1 # v3.2.0
        with:
          client-id: ${{ secrets.GH_APP_ID }}
          private-key: ${{ secrets.GH_APP_PRIVATE_KEY }}

      - name: Warn on degraded identity
        if: ${{ steps.apptoken.outcome == 'failure' && steps.apptoken.outputs.token == '' }}
        run: echo "::warning::App token unavailable — reviews will post as github-actions"

      - uses: actions/checkout@d23441a48e516b6c34aea4fa41551a30e30af803 # v6
        with:
          fetch-depth: 1
          # The model step must not reach any write-scoped credential, not
          # via env and not via .git/config http.extraheader.
          persist-credentials: false

      - name: Install opencode
        # Binary from GitHub Releases with SHA256 verification — no
        # unpinned curl|bash. The PATH export is REQUIRED: the old
        # curl|bash installer wrote $GITHUB_PATH itself, and dropping it
        # is what gave PR #421 "opencode: command not found" (exit 127).
        run: |
          mkdir -p "$HOME/.opencode/bin"
          OC_VERSION=1.18.30
          OC_SHA256=55007246858165496ff85ba1c2b648f7421e8e2013bf4189a680c9ff8e699d17
          OC_URL="https://github.com/anomalyco/opencode/releases/download/v${OC_VERSION}/opencode-linux-x64.tar.gz"
          curl -fsSL --proto '=https' --tlsv1.2 -o /tmp/opencode.tar.gz "$OC_URL"
          echo "${OC_SHA256}  /tmp/opencode.tar.gz" | sha256sum -c -
          tar -xzf /tmp/opencode.tar.gz -C "$HOME/.opencode/bin" opencode
          chmod +x "$HOME/.opencode/bin/opencode"
          "$HOME/.opencode/bin/opencode" --version
          echo "$HOME/.opencode/bin" >> "$GITHUB_PATH"

      - name: Prepare review context
        env:
          GH_TOKEN: ${{ github.token }}
          PR_NUMBER: ${{ github.event.pull_request.number }}
        run: |
          cd "${GITHUB_WORKSPACE}"
          gh pr checkout "$PR_NUMBER"
          git config --local --unset-all http.extraheader 2>/dev/null || true
          git config --local --remove-section 'http.https://github.com/' 2>/dev/null || true

          gh pr view "$PR_NUMBER" --json title --jq .title > pr.title
          gh pr view "$PR_NUMBER" --json headRefOid --jq .headRefOid > pr.head
          gh pr diff "$PR_NUMBER" > pr.diff

          # Conventions come from the BASE ref, which is not
          # attacker-controllable, unlike the PR head tree. They are
          # extracted to files here because .git is deleted below, and are
          # deliberately NOT named CLAUDE.md/AGENTS.md: the injection sweep
          # would delete them, and opencode would auto-load them outside
          # our prompt's control.
          mkdir -p .review-context
          git fetch --depth=1 origin "${GITHUB_BASE_REF}" 2>/dev/null || true
          for f in best_practices.md CLAUDE.md; do
            git show "origin/${GITHUB_BASE_REF}:${f}" 2>/dev/null \
              >> .review-context/conventions.md || true
          done
          [ -s .review-context/conventions.md ] \
            || echo "(no repo conventions found on the base ref)" \
               > .review-context/conventions.md

          # The head tree is attacker-controllable and opencode auto-loads
          # instruction files from it hierarchically. Strip recursively,
          # then delete .git so they cannot be recovered via git show.
          find . -path ./.review-context -prune -o \
            \( -name AGENTS.md -o -name CLAUDE.md -o -name .mcp.json \
            -o -name opencode.json -o -name opencode.jsonc \
            -o -name .opencode \) -exec rm -rf {} + 2>/dev/null || true
          rm -rf .git
```

- [ ] **Step 2: Validate the workflow file**

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -color
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/reviewer.yml'))"
```

Expected: exit 0 from both.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/reviewer.yml
git commit -s -m "ci: add reviewer workflow context assembly"
```

---

## Task 8: reviewer.yml — model step and posting

**Files:**
- Modify: `.github/workflows/reviewer.yml`

- [ ] **Step 1: Append the model step**

```yaml
      - name: Review with opencode big-pickle
        timeout-minutes: 15
        run: |
          cd "${GITHUB_WORKSPACE}"
          # Hard-fail rather than hand a credential to a model fed
          # untrusted input. A set-but-EMPTY var is harmless, so the test
          # is on the value.
          for v in GITHUB_TOKEN GH_TOKEN ACTIONS_RUNTIME_TOKEN \
                   ACTIONS_ID_TOKEN_REQUEST_TOKEN AWS_WEB_IDENTITY_TOKEN_FILE; do
            if [ -n "${!v}" ]; then
              echo "::error::credential \$$v present in the model step — refusing to run"
              exit 1
            fi
          done

          # Capture stdout to a file; the model writes NOTHING to disk (no
          # --auto, so it has no approved write tool). post_review.py
          # recovers the JSON from the reply via extract_findings().
          #
          # --format json emits raw JSON events rather than a single blob,
          # so the reply text is spread across events; extract_findings
          # scans the whole stream for balanced objects, which is why it
          # tolerates that shape where #421's parts[-1] did not.
          opencode run -m opencode/big-pickle --format json \
            "You are a senior Go code reviewer for the ghost codebase (Go 1.26+, modernc.org/sqlite, no CGO).

          Read ./pr.title, ./pr.diff, and ./.review-context/conventions.md. The diff is authoritative for changed lines; do NOT re-derive it. Read the changed files in this checkout to verify each finding. Do NOT run commands and do NOT narrate your process.

          Cover: correctness bugs, race conditions, error-handling gaps, and breaking changes to MCP tool contracts or the SQLite schema. Do NOT report style preferences — golangci-lint handles those.

          Reply with ONLY a JSON document matching exactly this schema — no preamble, no explanation, no markdown fence. Do NOT attempt to write any file; you have no write access. Print the JSON and nothing else:

          {\"verdict\": \"blocker|should-fix|nit|clean\",
           \"summary\": \"one paragraph, plain English, what this PR does\",
           \"findings\": [{\"file\": \"repo/relative/path.go\", \"line\": 42, \"end_line\": 44, \"severity\": \"blocker|should-fix|nit\", \"title\": \"short label\", \"body\": \"what is wrong, why it matters, the concrete fix\"}]}

          Rules: 'line' must be a line that appears in ./pr.diff as added or context — never a line outside it. 'end_line' is optional and must be >= 'line'. Use severity 'nit' for anything that does not affect correctness. If you find nothing, emit verdict 'clean' with an empty findings array. Verify each finding against the code before asserting it; omit anything you cannot verify." \
            > model-reply.txt

          [ -s model-reply.txt ] || { echo "::error::model produced no output at all"; exit 1; }
          echo "model reply: $(wc -c < model-reply.txt) bytes"

      - name: Post review
        env:
          GH_TOKEN: ${{ steps.apptoken.outputs.token || github.token }}
          PR_NUMBER: ${{ github.event.pull_request.number }}
        run: |
          cd "${GITHUB_WORKSPACE}"
          python3 "${GITHUB_WORKSPACE}/.github/scripts/post_review.py" \
            "${GITHUB_REPOSITORY}" "${PR_NUMBER}" "$(cat pr.head)" \
            model-reply.txt pr.diff
```

Note: `.github/scripts/` survives the injection sweep (it matches none of the
swept names) and `rm -rf .git` does not remove tracked working-tree files.

- [ ] **Step 2: Validate**

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -color
```

Expected: exit 0.

- [ ] **Step 3: Commit and open the PR to exercise it live**

```bash
git add .github/workflows/reviewer.yml
git commit -s -m "ci: run the model and post the structured review"
git push -u origin HEAD
gh pr create --fill
```

- [ ] **Step 4: Verify against the live run**

```bash
gh pr checks --watch
gh api "repos/wcatz/ghost/pulls/$(gh pr view --json number --jq .number)/comments" \
  --jq 'length'
```

Expected: the `Reviewer` check succeeds and the inline-comment count is > 0.
If it is 0, read the step log: either the model emitted no findings (check the
review body's verdict) or every anchor was dropped (check for
`::warning::finding not anchorable`).

---

## Task 9: restore the sweeper

**Files:**
- Create: `.github/workflows/sweeper.yml` (from `git show main:.github/workflows/pr-loop.yml`)

- [ ] **Step 1: Copy the validated workflow**

```bash
git show main:.github/workflows/pr-loop.yml > .github/workflows/sweeper.yml
```

- [ ] **Step 2: Retarget it at the new reviewer**

In `.github/workflows/sweeper.yml`:

- change `name: Review Loop` to `name: Sweeper`
- change `workflows: ["PR Agent"]` to `workflows: ["Reviewer"]`
- change the concurrency group prefix `review-loop-` to `sweeper-`

- [ ] **Step 3: Widen the author filter**

The existing filter only matches `github-actions`; the reviewer now posts as
`review-sweeper[bot]` when the App token is available. Replace both
`select((.comments.nodes[0].author.login // "") | startswith("github-actions"))`
occurrences with:

```jq
select((.comments.nodes[0].author.login // "")
  | startswith("github-actions") or startswith("review-sweeper"))
```

- [ ] **Step 4: Fix the pre-existing dangling variable**

`pr-loop.yml` contains `rm -f "$NODES_FILE"` but never defines `NODES_FILE` —
under `bash -e` this is a harmless no-op, but it is dead code that reads as a
bug. Delete that line; `PAGES_FILE` is already cleaned by the `trap`.

- [ ] **Step 5: Validate**

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -color
```

Expected: exit 0.

- [ ] **Step 6: Commit and verify live**

```bash
git add .github/workflows/sweeper.yml
git commit -s -m "ci: restore the review thread sweeper"
git push
gh pr checks --watch
```

Expected: after the Reviewer run completes, the `Sweeper` run reports either
`No live findings` or `Requested changes: N live finding(s)`. Confirm with:

```bash
gh pr view --json reviewDecision --jq .reviewDecision
```

Expected: `REVIEW_REQUIRED` or `CHANGES_REQUESTED` depending on findings.

---

## Task 10: thread state as prompt input

This is the fix for the round-29 oscillation.

**Files:**
- Modify: `.github/workflows/reviewer.yml`

- [ ] **Step 1: Add the fetch to the prepare step**

Insert into the `Prepare review context` step **immediately after the
`.review-context/conventions.md` block** (which creates the directory) and
before the `find`/`rm -rf .git` block:

```bash
          # Existing bot threads become prompt input so the model does not
          # re-raise what is already open or already dismissed. Without
          # this the reviewer re-derives findings from scratch every push
          # and oscillates (PR #421: 32 reviews, verdict flipping between
          # clean and should-fix on identical code).
          gh api graphql -f query='
            query($owner:String!,$name:String!,$number:Int!){
              repository(owner:$owner,name:$name){
                pullRequest(number:$number){
                  reviewThreads(first:100){
                    nodes{
                      isResolved
                      comments(first:1){nodes{author{login} path line body}}
                    }}}}}' \
            -f owner="${GITHUB_REPOSITORY%%/*}" \
            -f name="${GITHUB_REPOSITORY##*/}" \
            -F number="$PR_NUMBER" \
            --jq '[.data.repository.pullRequest.reviewThreads.nodes[]
                   | select((.comments.nodes[0].author.login // "")
                       | startswith("github-actions") or startswith("review-sweeper"))
                   | {resolved: .isResolved,
                      path: .comments.nodes[0].path,
                      line: .comments.nodes[0].line,
                      title: (.comments.nodes[0].body | split("\n")[0])}]' \
            > .review-context/prior-threads.json 2>/dev/null \
            || echo '[]' > .review-context/prior-threads.json
```

- [ ] **Step 2: Reference it in the prompt**

In the `Review with opencode big-pickle` step, add this sentence to the prompt
immediately after the "Read ./pr.title, ./pr.diff, ..." sentence:

```
Also read ./.review-context/prior-threads.json — findings you already raised on this PR. Entries with \"resolved\": false are STILL OPEN: do not raise them again. Entries with \"resolved\": true were dismissed: do not raise them again unless the code changed in a way that makes the original concern newly valid.
```

- [ ] **Step 3: Validate and push**

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -color
git add .github/workflows/reviewer.yml
git commit -s -m "ci: feed prior thread state back into the reviewer"
git push
```

- [ ] **Step 4: Verify convergence empirically**

Push an empty commit to re-trigger a review against unchanged code:

```bash
git commit -s --allow-empty -m "chore: trigger re-review"
git push
gh pr checks --watch
```

Assert these three things, in this order — the first two are mechanical and
must hold; the third is the behavioural goal and is softer:

1. `prior-threads.json` is non-empty in the prepare-step log. If it is `[]`,
   the GraphQL query or the author filter is wrong — fix that before judging
   the model.
2. The prompt in the model step's log contains the `prior-threads.json`
   sentence. If not, the edit did not land.
3. The second review posts **fewer** inline comments than the first, ideally
   zero.

Do not treat a non-zero count in (3) as a hard failure while (1) and (2) hold.
Task 11 has not landed yet, so this run still reviews the **full** PR diff, and
convergence here rests entirely on the model honouring one prompt clause — a
free-tier model partially ignoring it is expected, and the incremental diff in
Task 11 is the structural fix that does not depend on the model's cooperation.
Record the before/after counts; they are the baseline for judging Task 11.

---

## Task 11: incremental diff

**Files:**
- Modify: `.github/workflows/reviewer.yml`

- [ ] **Step 1: Resolve the last reviewed SHA and scope the diff**

Replace the single `gh pr diff "$PR_NUMBER" > pr.diff` line in the prepare step
with:

```bash
          # Review only what changed since the last review. The marker is
          # written into the review body by build_review(); reading it back
          # keeps this independent of the summary comment (phase 4).
          LAST_SHA="$(gh api "repos/${GITHUB_REPOSITORY}/pulls/${PR_NUMBER}/reviews" \
            --paginate --jq '[.[] | .body
              | capture("<!-- ghost-review:(?<sha>[0-9a-f]{7,40}) -->")?.sha]
              | last // empty' 2>/dev/null || true)"

          if [ -n "$LAST_SHA" ] && git cat-file -e "${LAST_SHA}^{commit}" 2>/dev/null; then
            echo "incremental review: ${LAST_SHA}..HEAD"
            git diff "${LAST_SHA}" HEAD > pr.diff
          else
            echo "full review (no usable prior marker)"
            gh pr diff "$PR_NUMBER" > pr.diff
          fi

          # An incremental diff can be empty when a push changed nothing
          # reviewable (e.g. a rebase with no content change). Skip rather
          # than burn a model run on an empty diff.
          if [ ! -s pr.diff ]; then
            echo "no reviewable change since ${LAST_SHA}; skipping"
            echo "skip=true" >> "$GITHUB_OUTPUT"
          fi
```

Add `id: context` to the `Prepare review context` step so the output is
addressable.

Note: `gh pr checkout` fetches the head branch but the prior SHA may not be
present at `fetch-depth: 1`. Add this immediately after `gh pr checkout`:

```bash
          git fetch --unshallow origin 2>/dev/null || git fetch --deepen=50 origin || true
```

- [ ] **Step 2: Gate the model and post steps on the skip flag**

Add to both the `Review with opencode big-pickle` and `Post review` steps:

```yaml
        if: steps.context.outputs.skip != 'true'
```

- [ ] **Step 3: Validate**

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -color
```

Expected: exit 0.

- [ ] **Step 4: Commit and verify both paths**

```bash
git add .github/workflows/reviewer.yml
git commit -s -m "ci: review only the diff since the last review"
git push
gh pr checks --watch
```

Expected in the log: **either** `incremental review: <sha>..HEAD` **or**
`full review (no usable prior marker)`. Both are correct outcomes — do not
debug the second one on its own.

`gh pr checkout` fetches the head branch; the prior head commit is only
reachable if it is still an ancestor. After a force-push or rebase it may exist
nowhere locally, `git cat-file -e` fails, and the full-diff fallback is the
intended behaviour. Only investigate if you see `full review` on a run where
the PR was updated by an ordinary fast-forward push — that would mean the
marker itself is not being written or not being parsed. Check with:

```bash
gh api "repos/wcatz/ghost/pulls/$(gh pr view --json number --jq .number)/reviews" \
  --jq '.[].body | capture("<!-- ghost-review:(?<sha>[0-9a-f]+) -->")?.sha'
```

Expected: one SHA per prior review. An empty result means `build_review()` is
not emitting the marker — a Task 4 regression, not a Task 11 bug.

Then verify the skip path:

```bash
git commit -s --allow-empty -m "chore: verify empty-diff skip"
git push
gh pr checks --watch
```

Expected: `no reviewable change since <sha>; skipping`, and the model step
shows as skipped.

---

## Task 12: ground-truth fixture test

Everything so far verifies plumbing. This verifies the reviewer actually finds
bugs, which is the only claim that matters.

**Files:**
- Create: `.github/scripts/fixtures/known_bad.go`
- Create: `docs/superpowers/plans/2026-09-14-pr-reviewer-fixture-results.md`

- [ ] **Step 1: Write the fixture with three planted, verifiable bugs**

Create `.github/scripts/fixtures/known_bad.go`:

```go
//go:build ignore

// Fixture for reviewer ground-truth testing. Not compiled into the module
// (build tag `ignore`). Each function contains exactly one planted defect;
// the reviewer is expected to find all three.
package fixtures

import (
	"database/sql"
	"sync"
)

// PLANTED BUG 1: the error from Exec is discarded, so a failed write is
// reported as success.
func saveMemory(db *sql.DB, id, content string) error {
	db.Exec("INSERT INTO memories (id, content) VALUES (?, ?)", id, content)
	return nil
}

// PLANTED BUG 2: counter is mutated from multiple goroutines with no
// synchronisation — a data race.
func countAll(items []string) int {
	counter := 0
	var wg sync.WaitGroup
	for range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counter++
		}()
	}
	wg.Wait()
	return counter
}

// PLANTED BUG 3: rows is never closed, leaking a connection on every call.
func listIDs(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT id FROM memories")
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}
```

- [ ] **Step 2: Open a throwaway PR containing only the fixture**

```bash
git checkout -b test/reviewer-fixture
git add .github/scripts/fixtures/known_bad.go
git commit -s -m "test: add reviewer ground-truth fixture"
git push -u origin HEAD
gh pr create --title "test: reviewer ground-truth fixture" \
  --body "Throwaway PR to measure reviewer recall against planted bugs. Do not merge."
```

- [ ] **Step 3: Wait for the review and score it**

```bash
gh pr checks --watch
PR=$(gh pr view --json number --jq .number)
gh api "repos/wcatz/ghost/pulls/$PR/comments" \
  --jq '.[] | "\(.path):\(.line) \(.body | split("\n")[0])"'
```

Expected: three inline comments on `known_bad.go`, one anchored inside each
function. Record which of the three planted bugs were found.

- [ ] **Step 4: Record the result**

Create `docs/superpowers/plans/2026-09-14-pr-reviewer-fixture-results.md` with
the date, the opencode model version, recall (found / 3), any false positives,
and the raw comment bodies. This is the baseline that later prompt changes are
measured against — without a recorded number, "the prompt got better" is
unfalsifiable.

- [ ] **Step 5: Close the fixture PR and delete the branch**

```bash
gh pr close "$PR" --delete-branch
```

- [ ] **Step 6: Commit the results doc on the main working branch**

```bash
git checkout -
git add docs/superpowers/plans/2026-09-14-pr-reviewer-fixture-results.md
git commit -s -m "docs: record reviewer ground-truth baseline"
```

---

## Task 13: retire the old pipeline

Only after Tasks 1–12 are green on live traffic.

**Files:**
- Delete: `.github/workflows/pr-agent.yml`
- Delete: `.github/workflows/pr-loop.yml`
- Delete: `.pr_agent.toml`
- Modify: `README.md`

- [ ] **Step 1: Confirm the new pipeline has actually reviewed a real PR**

```bash
gh api "repos/wcatz/ghost/pulls/$(gh pr view --json number --jq .number)/comments" \
  --jq '[.[] | select(.user.login | startswith("review-sweeper") or startswith("github-actions"))] | length'
```

Expected: > 0. If this is 0, do not proceed — there is no evidence the
replacement works.

- [ ] **Step 2: Remove the old files**

```bash
git rm .github/workflows/pr-agent.yml .github/workflows/pr-loop.yml .pr_agent.toml
```

- [ ] **Step 3: Update the README pipeline section**

Replace any description of the PR-Agent pipeline with the new one: `reviewer.yml`
produces severity-gated inline threads from the model's structured findings
document; `sweeper.yml`
resolves stale threads and signals `REQUEST_CHANGES`;
`required_conversation_resolution` on `main` is the merge gate.

- [ ] **Step 4: Verify no dangling references**

```bash
grep -rn "pr_agent\|pr-agent\|pr-loop" --include='*.yml' --include='*.md' \
  . | grep -v '^./docs/superpowers/' | grep -v '^./.git/'
```

Expected: no output. Historical plan/spec docs under `docs/superpowers/` keep
their references intentionally.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -s -m "ci: retire the PR-Agent pipeline"
```

- [ ] **Step 6: Close the superseded PRs**

```bash
gh pr close 421 --comment "Superseded by the reviewer/sweeper pipeline; see docs/superpowers/specs/2026-09-14-pr-reviewer-design.md"
gh pr close 420 --comment "The PR-Agent action is removed; bump no longer applies."
```

---

## Definition of done

- [x] `actionlint` and the Python unit tests run in the `lint` job on every PR — `lint` pass on PR #422 head `71438eb`, run 34855752142. Note `ci.yml` triggers on `pull_request: branches: [main]`, so a PR stacked onto a feature branch does not run it; #423's workflow changes get their first CI lint when GitHub retargets that PR to `main`.
- [x] A real PR receives severity-gated inline threads from `review-sweeper[bot]` — PR #423 run 34855768054 (2 inline findings), PR #425 run 34861183180 (1 blocker thread)
- [x] `nit` findings appear in a collapsed block and do not block merge — verified live on PR #425, run 34861183180: 2 nits in the collapsed body block, 0 inline threads for them (see `2026-09-14-pr-reviewer-fixture-results.md`)
- [x] `blocker`/`should-fix` findings block merge via `required_conversation_resolution` —
  measured on #423 at head `2ca846f`, base `main`, with every check green
  (`lint`, `build-and-test`, `gate`, `review`, CodeQL all SUCCESS):
  `mergeable=MERGEABLE`, `mergeStateStatus=BLOCKED`, `unresolved=4`. The four
  unresolved `review-sweeper[bot]` threads are the only remaining gate.
  Take the reading *after* the required checks report: missing checks also
  produce `BLOCKED`, so a `BLOCKED` observed while `build-and-test` and
  `lint` are still absent proves nothing about the conversation gate. On a
  feature-branch base no protection applies at all, so this is only ever
  observable on a PR whose base is `main`.
- [ ] `sweeper.yml` resolves stale threads and posts `REQUEST_CHANGES` on live ones — **not verifiable yet.** `workflow_run` workflows only execute from the default branch, so the sweeper cannot run until this lands on `main`.
- [x] A re-run against unchanged code posts zero duplicate findings — PR #423 run 34857252809: `posted review: 0 inline finding(s)` on the second pass
- [x] An empty incremental diff skips the model run — run 34859894426: `no reviewable change since c0623bd...; skipping`; Review/Mint/Post all `skipped`, job green
- [x] Fixture recall is recorded in the results doc — `2026-09-14-pr-reviewer-fixture-results.md`: 3/3 recall, 0 fixture false positives, 4/4 true positives on real code, 1 false positive rejected with log evidence, plus the severity-partition run
- [x] `pr-agent.yml`, `pr-loop.yml`, and `.pr_agent.toml` are gone; #420 and #421 closed — deleted in `443ad53`. The `pr_agent_job` check still reports on #422 because the deletion lives on the workflows branch and the workflow file is still on `main`; it stops running once this merges.

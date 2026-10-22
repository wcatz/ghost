"""The Sweeper workflow's own shell, run over fixture answers.

The sweeper publishes the merge gate, so what its shell does is what a
merge is gated on. The decision itself lives in ONE tested script
(review_gate.py); this suite holds the wiring - that the shell asks the
script, and that the gate it publishes is the one the script decided.

The shell is read out of the workflow file and run as the workflow runs
it, over a fake gh and a fake curl: the gh answers the API calls from
fixtures and records the gate that was published, and the curl hands
the shell the trusted scripts out of this repository.
"""

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))

# A commit, and another one.
HEAD = "8877fc9b" + "0" * 32
OTHER = "11223344" + "0" * 32

# The identities the reviewer posts as, and the author of a pull
# request.
BOT = "review-sweeper[bot]"
AUTHOR = "the-author"

# The title the reviewer publishes when the models were unavailable.
PENDING = "REVIEW-UNAVAILABLE: the free models were unreachable"


def workflow_path():
    """The path of the Sweeper workflow."""
    return os.path.join(HERE, os.pardir, "workflows", "sweeper.yml")


def step_shell(name):
    """The shell of one workflow step, read out of the workflow file.

    The step is found by its name and its `run:` block is taken
    verbatim, so the suite runs the shell the workflow runs rather than a
    copy of it.
    """
    with open(workflow_path(), encoding="utf-8") as handle:
        lines = handle.read().splitlines()
    start = None
    for index, line in enumerate(lines):
        if line.strip() == "- name: %s" % name:
            start = index
            break
    if start is None:
        raise AssertionError("the workflow has no step named %r" % name)
    for index in range(start, len(lines)):
        line = lines[index]
        if not line.strip().startswith("run:"):
            continue
        indent = len(line) - len(line.lstrip())
        body = []
        for follow in lines[index + 1:]:
            if follow.strip() and len(follow) - len(follow.lstrip()) <= indent:
                break
            body.append(follow)
        while body and not body[-1].strip():
            body.pop()
        if not body:
            raise AssertionError("the step %r has an empty run block" % name)
        cut = len(body[0]) - len(body[0].lstrip())
        return "\n".join(line[cut:] if line.strip() else ""
                         for line in body) + "\n"
    raise AssertionError("the step %r has no run block" % name)


def step_event_env(name, event):
    """The event-derived env of one workflow step, as the runner sets it.

    Each `KEY: ${{ github.event.a.b }}` line of the step's `env:` block is
    resolved against the event payload, so the suite reads the field the
    workflow reads rather than setting the variable itself. A field the
    payload lacks resolves to the empty string, as on the runner.
    """
    with open(workflow_path(), encoding="utf-8") as handle:
        lines = handle.read().splitlines()
    start = None
    for index, line in enumerate(lines):
        if line.strip() == "- name: %s" % name:
            start = index
            break
    if start is None:
        raise AssertionError("the workflow has no step named %r" % name)
    pattern = re.compile(r"^\s*([A-Z_]+):\s*\$\{\{\s*github\.event\.([\w.]+)\s*\}\}\s*$")
    resolved = {}
    for line in lines[start + 1:]:
        if line.strip().startswith("run:") or line.strip().startswith("- name:"):
            break
        match = pattern.match(line)
        if not match:
            continue
        value = event
        for part in match.group(2).split("."):
            value = value.get(part) if isinstance(value, dict) else None
        resolved[match.group(1)] = "" if value is None else str(value)
    return resolved


def _write(path, text):
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(text)


def _marker(sha):
    """The marker the reviewer writes."""
    return "<!-- ghost-review:%s -->" % sha


def _review(login, body):
    """One review, as the reviews API returns it."""
    user_type = "Bot" if login in (BOT, "github-actions[bot]") else "User"
    return {"user": {"login": login, "type": user_type},
            "body": body, "state": "COMMENTED"}


def _fake_gh(folder):
    """A fake gh: the API answers come from the fixtures."""
    path = os.path.join(folder, "gh")
    _write(path,
           "#!/bin/sh\n"
           "# The gate the shell published, for the test to read.\n"
           "if printf '%s\\n' \"$*\" | grep -q -- '--method POST'; then\n"
           "  cat > \"$GATE_FILE\"\n"
           "  exit 0\n"
           "fi\n"
           "case \"$*\" in\n"
           "  *headRefOid*) printf '%s\\n' \"$HEAD_SHA\" ;;\n"
           "  *reviewThreads*) cat \"$FIXTURE_THREADS\" ;;\n"
           "  *check-runs*) cat \"$FIXTURE_PENDING\" ;;\n"
           "  *actions/runs*) python3 -c \"\n"
           "import json, os, re, sys\n"
           "url = ' '.join(sys.argv[1:])\n"
           "data = json.load(open(os.environ['FIXTURE_RUNS']))\n"
           "m = re.search(r'head_sha=([^& ]+)', url)\n"
           "if m:\n"
           "    sha = m.group(1)\n"
           "    data = {'workflow_runs': [r for r in data.get('workflow_runs', []) if r.get('head_sha') == sha]}\n"
           "print(json.dumps(data))\n"
           "\" \"$*\" ;;\n"
           "  *reviews*) cat \"$FIXTURE_REVIEWS\" ;;\n"
           "  *) echo \"fake gh: unexpected call: $*\" >&2; exit 1 ;;\n"
           "esac\n")
    os.chmod(path, 0o755)
    return path


def _fake_curl(folder):
    """A fake curl: the trusted scripts come from this repository."""
    path = os.path.join(folder, "curl")
    _write(path,
           "#!/bin/sh\n"
           "out=\"\"\n"
           "for argument in \"$@\"; do\n"
           "  out=\"$argument\"\n"
           "done\n"
           "cp \"$SCRIPTS_SRC/$(basename \"$out\")\" \"$out\" || exit 1\n")
    os.chmod(path, 0o755)
    return path


def run_step(folder, event="workflow_run", conclusion="success",
             run_head=HEAD, head=HEAD, reviews=None, pending=None,
             threads=None, runs=None, wf_path=None, wf_event=None):
    """The sweep step's shell, run as the workflow runs it.

    The gate the shell published is returned, or None when it published
    none.
    """
    runner = os.path.join(folder, "runner-temp")
    os.makedirs(runner, exist_ok=True)
    _fake_gh(folder)
    _fake_curl(folder)

    reviews_path = os.path.join(folder, "reviews.json")
    _write(reviews_path, json.dumps(reviews if reviews is not None else []))
    pending_path = os.path.join(folder, "pending.txt")
    _write(pending_path, "\n".join(pending or []) + ("\n" if pending else ""))
    threads_path = os.path.join(folder, "threads.json")
    _write(threads_path, json.dumps(threads if threads is not None else
                                    {"nodes": [], "pageInfo":
                                     {"hasNextPage": False, "endCursor": None}}))
    runs_path = os.path.join(folder, "runs.json")
    if runs is None:
        # A successful Reviewer run for the head, so an issue_comment
        # re-evaluation can succeed.
        runs = {"workflow_runs": [{"name": "Reviewer",
                                   "path": ".github/workflows/reviewer.yml",
                                   "event": "pull_request",
                                   "head_sha": head,
                                   "conclusion": "success",
                                   "created_at": "2026-01-01T00:00:00Z"}]}
    _write(runs_path, json.dumps(runs))
    gate_path = os.path.join(folder, "gate.json")

    step = os.path.join(folder, "sweep.sh")
    _write(step, step_shell("Sweep stale findings, signal on live ones"))
    environment = dict(os.environ)
    environment.update({
        "PATH": folder + os.pathsep + environment.get("PATH", ""),
        "RUNNER_TEMP": runner,
        "GITHUB_REPOSITORY": "wcatz/ghost",
        "GITHUB_SHA": "0" * 40,
        "GITHUB_EVENT_NAME": event,
        "GH_TOKEN": "test-token",
        "APP_TOKEN": "",
        "NUMBER": "42",
        "HEAD_SHA": head,
        "FIXTURE_REVIEWS": reviews_path,
        "FIXTURE_PENDING": pending_path,
        "FIXTURE_THREADS": threads_path,
        "FIXTURE_RUNS": runs_path,
        "GATE_FILE": gate_path,
        "SCRIPTS_SRC": HERE,
    })
    # The run fields come from an event payload shaped like the one the
    # runner hands the workflow, through the step's own env lines.
    payload = {}
    if event == "workflow_run":
        payload = {"workflow_run": {
            "conclusion": conclusion,
            "head_sha": run_head,
            "path": wf_path if wf_path is not None else ".github/workflows/reviewer.yml",
            "event": wf_event if wf_event is not None else "pull_request",
        }}
    derived = step_event_env("Sweep stale findings, signal on live ones", payload)
    for key in ("WF_CONCLUSION", "WF_HEAD", "WF_PATH", "WF_EVENT"):
        if key not in derived:
            raise AssertionError("the sweep step does not set %s from the event" % key)
    environment.update(derived)
    result = subprocess.run(["bash", "-e", step], env=environment,
                            capture_output=True, text=True, check=False)
    gate = None
    if os.path.exists(gate_path):
        with open(gate_path, encoding="utf-8") as handle:
            gate = json.load(handle)
    return result, gate


def _bot_review(head=HEAD):
    """A review the review bot posted for the head."""
    return [_review(BOT, _marker(head))]


class TestTheSweepStep(unittest.TestCase):
    """The gate the sweeper publishes, for each arm."""

    def _gate(self, **kwargs):
        with tempfile.TemporaryDirectory() as folder:
            result, gate = run_step(folder, **kwargs)
        return result, gate

    def test_a_reviewed_head_with_no_findings_passes_the_gate(self):
        result, gate = self._gate(reviews=_bot_review())
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIsNotNone(gate)
        self.assertEqual(gate["conclusion"], "success")
        self.assertEqual(gate["output"]["title"], "No unresolved reviewer findings")

    def test_a_workflow_run_with_correct_path_and_event_passes(self):
        # A workflow_run event with the correct path and event passes the
        # path check, so the gate is evaluated normally.
        result, gate = self._gate(
            reviews=_bot_review(),
            event="workflow_run",
            conclusion="success",
            run_head=HEAD,
            wf_path=".github/workflows/reviewer.yml",
            wf_event="pull_request",
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "success")

    def test_a_workflow_run_with_wrong_path_fails_closed(self):
        # A run that shares the name but has a different path fails closed.
        result, gate = self._gate(
            reviews=_bot_review(),
            event="workflow_run",
            conclusion="success",
            run_head=HEAD,
            wf_path="/some/other/path.yml",
            wf_event="pull_request",
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "failure")

    def test_a_workflow_run_with_path_at_ref_strips_suffix(self):
        # The workflow reads github.event.workflow_run.path, which can
        # carry an @ref suffix. The suffix must be stripped before
        # comparing against the trusted path.
        result, gate = self._gate(
            reviews=_bot_review(),
            event="workflow_run",
            conclusion="success",
            run_head=HEAD,
            wf_path=".github/workflows/reviewer.yml@refs/heads/main",
            wf_event="pull_request",
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "success")

    def test_a_workflow_run_with_no_path_in_the_payload_fails_closed(self):
        # The path comes from the payload through the step's env line; a
        # payload without it leaves the variable empty, and the gate fails.
        result, gate = self._gate(
            reviews=_bot_review(),
            event="workflow_run",
            conclusion="success",
            run_head=HEAD,
            wf_path="",
            wf_event="pull_request",
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "failure")

    def test_a_comment_finds_a_reviewer_run_whose_path_carries_a_ref(self):
        # The runs API can report a path with an @ref suffix; the lookup
        # compares the part before it.
        runs = {"workflow_runs": [
            {"name": "Reviewer",
             "path": ".github/workflows/reviewer.yml@refs/heads/main",
             "event": "pull_request", "head_sha": HEAD,
             "conclusion": "success", "created_at": "2026-01-01T00:00:00Z"},
        ]}
        result, gate = self._gate(event="issue_comment", conclusion="",
                                  run_head="", reviews=_bot_review(),
                                  runs=runs)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "success")

    def test_a_comment_cannot_publish_a_success_gate(self):
        # A comment is not a review: the head was never judged, so the
        # gate fails closed.
        result, gate = self._gate(event="issue_comment", conclusion="",
                                  run_head="", reviews=[])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIsNotNone(gate)
        self.assertEqual(gate["conclusion"], "failure")

    def test_a_comment_may_re_evaluate_a_head_a_review_judged(self):
        result, gate = self._gate(event="issue_comment", conclusion="",
                                  run_head="", reviews=_bot_review())
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "success")

    def test_a_comment_cannot_revive_a_head_whose_latest_run_failed(self):
        # The head was judged by an earlier run, but a later re-run was
        # cancelled: the comment must not turn that into a green gate.
        runs = {"workflow_runs": [
            {"name": "Reviewer",
             "path": ".github/workflows/reviewer.yml",
             "event": "pull_request",
             "head_sha": HEAD,
             "conclusion": "success",
             "created_at": "2026-01-01T00:00:00Z"},
            {"name": "Reviewer",
             "path": ".github/workflows/reviewer.yml",
             "event": "pull_request",
             "head_sha": HEAD,
             "conclusion": "cancelled",
             "created_at": "2026-01-02T00:00:00Z"},
        ]}
        result, gate = self._gate(event="issue_comment", conclusion="",
                                  run_head="", reviews=_bot_review(),
                                  runs=runs)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "failure")

    def test_a_comment_with_no_reviewer_run_fails_closed(self):
        runs = {"workflow_runs": []}
        result, gate = self._gate(event="issue_comment", conclusion="",
                                  run_head="", reviews=_bot_review(),
                                  runs=runs)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "failure")

    def test_the_newest_matching_run_is_the_one_that_counts(self):
        # A success for another head, an older success and a newer
        # cancelled run for this head, and a same-named run at a
        # different path must not hide the cancelled run.
        runs = {"workflow_runs": [
            {"name": "Reviewer",
             "path": ".github/workflows/reviewer.yml",
             "event": "pull_request",
             "head_sha": OTHER,
             "conclusion": "success",
             "created_at": "2026-01-03T00:00:00Z"},
            {"name": "Reviewer",
             "path": ".github/workflows/reviewer.yml",
             "event": "pull_request",
             "head_sha": HEAD,
             "conclusion": "success",
             "created_at": "2026-01-01T00:00:00Z"},
            {"name": "Reviewer",
             "path": "/some/other/reviewer.yml",
             "event": "pull_request",
             "head_sha": HEAD,
             "conclusion": "success",
             "created_at": "2026-01-04T00:00:00Z"},
            {"name": "Reviewer",
             "path": ".github/workflows/reviewer.yml",
             "event": "pull_request",
             "head_sha": HEAD,
             "conclusion": "cancelled",
             "created_at": "2026-01-02T00:00:00Z"},
        ]}
        result, gate = self._gate(event="issue_comment", conclusion="",
                                  run_head="", reviews=_bot_review(),
                                  runs=runs)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "failure")

    def test_all_workflows_use_the_exact_trusted_identities(self):
        # Every author-login filter in a workflow that gates on the review
        # bot's threads is an exact comparison against the trusted set, and
        # each workflow names the whole set.
        sys.path.insert(0, HERE)
        from review_marker import TRUSTED_IDENTITIES
        exact = re.compile(r'\(\.[\w.\[\]]*\.login // ""\) == "([^"]*)"')
        for name in ("reviewer.yml", "sweeper.yml", "best-practices-loop.yml"):
            with open(os.path.join(HERE, os.pardir, "workflows", name),
                      encoding="utf-8") as handle:
                lines = handle.read().splitlines()
            named = set()
            for number, line in enumerate(lines, 1):
                # Every login a jq filter reads counts, whatever path it
                # takes to reach it. GraphQL selections ({login}) and the
                # workflow's own `if:` expressions are not filters.
                if ".login" not in line or "github.event." in line:
                    continue
                named.update(exact.findall(line))
                rest = exact.sub("", line)
                self.assertNotIn(
                    ".login", rest,
                    "%s:%d filters a login other than by exact comparison: %s"
                    % (name, number, line.strip()))
            self.assertEqual(
                named, set(TRUSTED_IDENTITIES),
                "%s does not compare against exactly the trusted identities" % name)

    def test_a_skipped_run_leaves_the_gate_failed(self):
        result, gate = self._gate(conclusion="skipped", reviews=[])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "failure")
        self.assertIn("skipped", gate["output"]["title"])

    def test_a_cancelled_run_leaves_the_gate_failed(self):
        result, gate = self._gate(conclusion="cancelled", reviews=[])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "failure")

    def test_a_marker_the_author_posted_is_not_a_judgement(self):
        # The author's review names the head, and the head was never
        # judged by the review bot.
        result, gate = self._gate(reviews=[_review(AUTHOR, _marker(HEAD))])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "failure")

    def test_a_pending_gate_is_left_alone_while_nothing_judged_the_head(self):
        result, gate = self._gate(reviews=[], pending=[PENDING])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIsNone(gate)

    def test_a_judged_head_completes_a_pending_gate(self):
        result, gate = self._gate(reviews=_bot_review(), pending=[PENDING])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "success")

    def test_a_run_that_judged_another_commit_leaves_the_gate_failed(self):
        result, gate = self._gate(run_head=OTHER, reviews=_bot_review())
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "failure")

    def test_a_live_finding_fails_the_gate(self):
        threads = {"nodes": [{"id": "T1", "isResolved": False,
                               "isOutdated": False,
                               "comments": {"nodes": [{"author":
                                                       {"login": BOT}}]}}],
                   "pageInfo": {"hasNextPage": False, "endCursor": None}}
        result, gate = self._gate(reviews=_bot_review(), threads=threads)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(gate["conclusion"], "failure")
        self.assertIn("1", gate["output"]["title"])

    def test_the_gate_is_published_on_the_head_the_pull_request_is_on(self):
        with tempfile.TemporaryDirectory() as folder:
            _result, gate = run_step(folder, head=HEAD, reviews=_bot_review())
        self.assertEqual(gate["head_sha"], HEAD)


if __name__ == "__main__":
    unittest.main()

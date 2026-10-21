"""The Reviewer workflow's own shell, run over fixture outputs.

The classify step once printed its classification to the job log instead
of the step output, so the step that requires a valid review result
always saw an empty classification and every review failed with "no
valid free or paid review result was produced: a review result was
produced" - until it was caught on the pull request's own run. A step
whose shell is never run cannot fail that way again: this suite reads
the step's shell out of the workflow file and runs it over fixtures.

It also holds the three places that ask whether the review model acted
to the same question. The selection step read only the stderr once,
while the free review loop and the paid fallback read the event stream
as well, and a model that produced something in its stream was read as
one that produced nothing.
"""

import os
import shutil
import subprocess
import sys
import tempfile
import unittest

import review_outcome

HERE = os.path.dirname(os.path.abspath(__file__))

# What the provider said when the models were unreachable.
DROPPED_SOCKET = "The socket connection was closed unexpectedly"

# The provider's own error event, and the refusal opencode prints when
# the model reaches for a tool: the first is the provider's words, the
# second is the model's.
PROVIDER_ERROR = ('{"type":"error","error":{"type":"provider_error",'
                  '"message":"rate limit exceeded (HTTP 429)"}}')
REFUSAL_AND_OUTAGE = ("permission requested: bash; auto-rejecting\n"
                      "rate limit exceeded (HTTP 429)\n")
PAID_SILENCE = "the paid model did not answer"

# The event stream opencode writes: one JSON object per line.
TEXT_EVENT = '{"type":"text","part":{"type":"text","text":"a review"}}'


def workflow_path():
    """The path of the Reviewer workflow."""
    return os.path.join(HERE, os.pardir, "workflows", "reviewer.yml")


def script_path(name):
    """The path of one script beside this test."""
    return os.path.join(HERE, name)


def step_shell(name):
    """The shell of one workflow step, read out of the workflow file.

    The step is found by its name and its `run:` block is taken verbatim,
    so the suite runs the shell the workflow runs rather than a copy of
    it.
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


def _write(path, text):
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(text)


def marker_for(events="", stderr=""):
    """The marker the trusted rule writes for these fixtures.

    The marker is written by the rule itself, so the word the workflow
    reads and the word the rule writes cannot drift apart.
    """
    with tempfile.TemporaryDirectory() as folder:
        events_path = os.path.join(folder, "events.jsonl")
        stderr_path = os.path.join(folder, "stderr.txt")
        marker = os.path.join(folder, "model-acted.txt")
        _write(events_path, events)
        _write(stderr_path, stderr)
        subprocess.run([sys.executable, "-I", script_path("model_acted.py"),
                        "--events", events_path, "--stderr", stderr_path,
                        "--marker", marker],
                       capture_output=True, text=True, check=True)
        if not os.path.exists(marker):
            return ""
        with open(marker, encoding="utf-8") as handle:
            return handle.read()


def _read(path):
    """What a file says, or nothing when it is not there."""
    if not os.path.exists(path):
        return ""
    with open(path, encoding="utf-8") as handle:
        return handle.read()


def _fake_opencode(folder):
    """A fake opencode on the PATH: `models` finds no catalog, and `run`
    answers with what the fixture says."""
    path = os.path.join(folder, "opencode")
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(
            "#!/bin/sh\n"
            "case \"$1\" in\n"
            "  models)\n"
            "    exit 1\n"
            "    ;;\n"
            "  run)\n"
            "    if [ -n \"${FAKE_OPENCODE_STDOUT:-}\" ]; then\n"
            "      cat \"$FAKE_OPENCODE_STDOUT\"\n"
            "    fi\n"
            "    if [ -n \"${FAKE_OPENCODE_STDERR:-}\" ]; then\n"
            "      cat \"$FAKE_OPENCODE_STDERR\" >&2\n"
            "    fi\n"
            "    exit \"${FAKE_OPENCODE_STATUS:-1}\"\n"
            "    ;;\n"
            "  *)\n"
            "    exit 1\n"
            "    ;;\n"
            "esac\n")
    os.chmod(path, 0o755)
    return path


def run_selection_step(folder, helper=True, stdout="", stderr=""):
    """The model selection step's shell, run as the workflow runs it.

    The step asks the trusted rule whether the model acted, so the rule
    is either beside the trusted scripts or absent, and the marker says
    which. The runner's temporary directory is returned: it holds the
    marker and the probe failures the classification reads.
    """
    runner = os.path.join(folder, "runner-temp")
    scripts = os.path.join(runner, "base-scripts")
    os.makedirs(scripts)
    shutil.copyfile(script_path("review_outcome.py"),
                    os.path.join(scripts, "review_outcome.py"))
    if helper:
        shutil.copyfile(script_path("model_acted.py"),
                        os.path.join(scripts, "model_acted.py"))
    _fake_opencode(folder)
    stdout_path = os.path.join(folder, "opencode.out")
    stderr_path = os.path.join(folder, "opencode.err")
    _write(stdout_path, stdout)
    _write(stderr_path, stderr)
    output = os.path.join(folder, "github-output")
    _write(output, "")
    step = os.path.join(folder, "select.sh")
    _write(step, step_shell("Select review model"))
    environment = dict(os.environ)
    environment.update({
        "HOME": folder,
        "PATH": folder + os.pathsep + environment.get("PATH", ""),
        "RUNNER_TEMP": runner,
        "GITHUB_OUTPUT": output,
        "MODELS": "opencode/big-pickle",
        "FAKE_OPENCODE_STDOUT": stdout_path,
        "FAKE_OPENCODE_STDERR": stderr_path,
        "FAKE_OPENCODE_STATUS": "1",
    })
    result = subprocess.run(["bash", step], env=environment,
                            capture_output=True, text=True, check=False)
    return runner, result


def run_classify(folder, runner=None, free_status="failed",
                 paid_status="failed", free_failures="", paid_reason="",
                 marker=None, free_validation="", paid_validation=""):
    """The classify step's shell, run over fixtures.

    The runner's temporary directory is the one a call site left behind
    when it is given, so the classification reads the marker and the
    probe failures a call site really wrote.
    """
    if runner is None:
        runner = os.path.join(folder, "classify-runner-temp")
        scripts = os.path.join(runner, "base-scripts")
        os.makedirs(scripts)
        shutil.copyfile(script_path("review_outcome.py"),
                        os.path.join(scripts, "review_outcome.py"))
        _write(os.path.join(runner, "free-probe-failure.txt"),
               free_failures)
        _write(os.path.join(runner, "free-review-failure.txt"),
               free_failures)
        _write(os.path.join(runner, "paid-review-failure.txt"),
               paid_reason)
        _write(os.path.join(runner, "free-review-validation.txt"),
               free_validation)
        _write(os.path.join(runner, "paid-review-validation.txt"),
               paid_validation)
        if marker is not None:
            _write(os.path.join(runner, "model-acted.txt"), marker)
    output = os.path.join(folder, "classify-output")
    _write(output, "")
    step = os.path.join(folder, "classify.sh")
    _write(step, step_shell("Classify the review result"))
    environment = dict(os.environ)
    environment.update({
        "HOME": folder,
        "RUNNER_TEMP": runner,
        "GITHUB_OUTPUT": output,
        "FREE_STATUS": free_status,
        "PAID_STATUS": paid_status,
        "SELECT_AGENT_FATAL": "",
        "FREE_AGENT_FATAL": "",
        "PAID_AGENT_FATAL": "",
    })
    result = subprocess.run(["bash", step], env=environment,
                            capture_output=True, text=True, check=False)
    with open(output, encoding="utf-8") as handle:
        return handle.read(), result.stdout


def classification_of(output):
    """The classification the step wrote to its output."""
    for line in output.splitlines():
        if line.startswith("classification="):
            return line.split("=", 1)[1]
    return ""


def cause_of(output):
    """The cause the step wrote to its output."""
    lines = output.splitlines()
    for index, line in enumerate(lines):
        if line.startswith("cause<<"):
            return lines[index + 1]
    return ""


class TestTheClassifyStep(unittest.TestCase):
    """The step that decides what a review run was."""

    def _run(self, free_status="failed", paid_status="failed",
             free_failures="", paid_reason="", marker=None,
             free_validation="", paid_validation=""):
        with tempfile.TemporaryDirectory() as folder:
            return run_classify(
                folder, free_status=free_status, paid_status=paid_status,
                free_failures=free_failures, paid_reason=paid_reason,
                marker=marker, free_validation=free_validation,
                paid_validation=paid_validation)

    def _classification(self, output):
        return classification_of(output)

    def _cause(self, output):
        return cause_of(output)

    def test_an_outage_leaves_the_gate_pending(self):
        output, _log = self._run(
            free_failures="opencode/big-pickle: %s\n" % DROPPED_SOCKET,
            paid_reason="the paid model did not answer")
        self.assertEqual(self._classification(output), "unavailable")

    def test_the_classification_reaches_the_step_output(self):
        # The bug this suite exists for: the classification was printed
        # to the job log, so the step that reads it saw nothing.
        output, _log = self._run(
            free_status="success", paid_status="",
            free_failures="", paid_reason="")
        self.assertEqual(self._classification(output), "success")
        self.assertIn("classification=", output)

    def test_the_cause_reaches_the_step_output(self):
        output, _log = self._run(
            free_failures="opencode/big-pickle: %s\n" % DROPPED_SOCKET,
            paid_reason="the paid model did not answer")
        self.assertIn("cause<<", output)
        self.assertTrue(self._cause(output))

    def test_a_successful_review_is_a_success(self):
        output, _log = self._run(free_status="success")
        self.assertEqual(self._classification(output), "success")

    def test_a_model_that_acted_is_a_failure(self):
        output, _log = self._run(
            free_failures="opencode/big-pickle: %s\n" % DROPPED_SOCKET,
            paid_reason="the paid model did not answer",
            marker=marker_for(events=TEXT_EVENT))
        self.assertEqual(self._classification(output), "failure")
        self.assertIn("acted", self._cause(output))

    def test_a_rule_that_could_not_be_consulted_is_not_an_outage(self):
        # The rule that says whether the model acted is read from the
        # default branch. A base branch that predates it cannot answer,
        # and a run nobody read cannot be claimed as an outage.
        output, _log = self._run(
            free_failures="opencode/big-pickle: %s\n" % DROPPED_SOCKET,
            paid_reason="the paid model did not answer",
            marker="unknown\n")
        self.assertEqual(self._classification(output), "failure")

    def test_a_rule_that_could_not_be_consulted_claims_nothing(self):
        # The cause is a label a reader trusts, and it may not name a
        # model that never acted.
        output, _log = self._run(
            free_failures="opencode/big-pickle: %s\n" % DROPPED_SOCKET,
            paid_reason="the paid model did not answer",
            marker="unknown\n")
        self.assertEqual(self._cause(output), review_outcome.CAUSE_NO_RESULT)

    def test_a_review_that_came_back_is_a_success_whatever_the_rule_said(self):
        output, _log = self._run(free_status="success", marker="unknown\n")
        self.assertEqual(self._classification(output), "success")

    def test_a_validation_failure_is_a_failure(self):
        output, _log = self._run(
            free_failures="", paid_reason="the paid model did not answer",
            free_validation="reply validation failed: no verdict")
        self.assertEqual(self._classification(output), "failure")


class TestThePlacesThatAsk(unittest.TestCase):
    """The three places that ask whether the review model acted."""

    STEPS = ("Select review model",
             "Review with opencode (free)",
             "Review with opencode (paid fallback)")

    def test_every_place_asks_about_the_stream_and_the_stderr(self):
        for name in self.STEPS:
            shell = step_shell(name)
            with self.subTest(step=name):
                self.assertIn("model_acted.py", shell)
                self.assertIn("--events", shell)
                self.assertIn("--stderr", shell)

    def test_the_pattern_is_not_written_out_in_the_workflow(self):
        # The rule is one script; a copy of the pattern in the workflow
        # is a rule that can drift away from the tested one.
        with open(workflow_path(), encoding="utf-8") as handle:
            workflow = handle.read()
        self.assertNotIn("MODEL_ACTED_STDERR", workflow)


class TestACallSite(unittest.TestCase):
    """A call site that asks the rule, with the rule and without it.

    The call site is the model selection step, run as the workflow runs
    it over a fake opencode: the rule is either beside the trusted
    scripts or absent, and the marker says which.
    """

    def test_a_call_site_without_the_rule_says_the_run_is_unknown(self):
        with tempfile.TemporaryDirectory() as folder:
            runner, _result = run_selection_step(
                folder, helper=False, stdout=PROVIDER_ERROR + "\n")
            marker = _read(os.path.join(runner, "model-acted.txt"))
            self.assertEqual(marker.strip(), "unknown")

    def test_a_call_site_without_the_rule_never_reads_as_an_outage(self):
        with tempfile.TemporaryDirectory() as folder:
            runner, _result = run_selection_step(
                folder, helper=False, stdout=PROVIDER_ERROR + "\n")
            output, _log = run_classify(folder, runner=runner)
            self.assertEqual(classification_of(output), "failure")
            self.assertNotEqual(classification_of(output), "unavailable")

    def test_a_refused_tool_attempt_on_stderr_alone_is_a_failure(self):
        # No events at all: the model's refusal is on the stderr, and it
        # is the refusal that makes the run a verdict rather than an
        # outage.
        with tempfile.TemporaryDirectory() as folder:
            runner, _result = run_selection_step(
                folder, helper=True, stderr=REFUSAL_AND_OUTAGE)
            marker = _read(os.path.join(runner, "model-acted.txt"))
            self.assertEqual(marker.strip(), "acted")
            output, _log = run_classify(folder, runner=runner)
            self.assertEqual(classification_of(output), "failure")

    def test_the_same_run_without_the_refusal_is_an_outage(self):
        # The contrast: the same provider failure reads as an outage
        # when the model produced nothing at all.
        with tempfile.TemporaryDirectory() as folder:
            runner, _result = run_selection_step(
                folder, helper=True, stderr=REFUSAL_AND_OUTAGE)
            failures = _read(os.path.join(runner, "free-probe-failure.txt"))
            output, _log = run_classify(folder, free_failures=failures,
                                        paid_reason=PAID_SILENCE)
            self.assertEqual(classification_of(output), "unavailable")


if __name__ == "__main__":
    unittest.main()

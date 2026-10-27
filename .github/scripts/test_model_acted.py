"""The rule that says whether the review model ACTED: the model produced
something of its own, so the run carries a verdict nobody can use rather
than an outage of the review models.

The rule is asked in three places in the Reviewer workflow - the model
selection step, the free review loop and the paid fallback - and the
three answers have to be the same answer. The rule therefore lives in
ONE script (model_acted.py), and this suite is what holds it to that.

Two things are held here. The event stream is the primary evidence: an
event that is not an error event is the model's own output, and a
stream nobody can read counts as produced, because the rule fails
closed. And the stderr is read NARROWLY: it is where opencode echoes a
refused tool call, and it is also where an outage prints a stack trace,
so a bare word such as "permission" or "bash" must not turn a genuine
outage into a model that acted.
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

import model_acted
from model_acted import acted, stderr_acted, stream_acted

# The event stream opencode writes: one JSON object per line.
TEXT_EVENT = '{"type":"text","part":{"type":"text","text":"a review"}}'
TOOL_EVENT = ('{"type":"tool_use","part":{"type":"tool","tool":"bash",'
              '"state":{"status":"error","error":"The user declined this '
              'tool call"}}}')
ERROR_EVENT = ('{"type":"error","error":{"type":"aborted","message":"Session '
               'interrupted: shutdown"}}')

# What opencode prints when it refuses the model's own tool call.
ASK_NOTICE = "permission requested: bash; auto-rejecting"
REFUSAL = ("Permission denied for tool bash: the model tried to run go "
           "test ./...")

# What an outage's stderr carries instead: a stack trace that mentions a
# permission or a shell without either being a refusal.
PERMISSION_TRACE = ("Traceback (most recent call last):\n"
                    "  File \"/usr/lib/python3/run.py\", line 4, in <module>\n"
                    "PermissionError: [Errno 13] Permission denied: "
                    "'/root/.bashrc'\n")
SHELL_TRACE = "/bin/bash: line 3: go: command not found\n"
EPERM_TRACE = "Traceback (most recent call last):\nOperation not permitted\n"

# What opencode writes as it begins to shut down: an aborted error, a
# declined tool call, and the auto-reject notice on the stderr. A run
# killed by its own timeout leaves this shape while the provider is still
# thinking, so it is not evidence that the model produced anything. The
# same shape from a run that ended on its own IS a real declined tool
# call, and stays evidence.
SHUTDOWN_STREAM = (
    '{"type":"step_start","part":{"type":"step-start"}}' + "\n" +
    '{"type":"error","error":{"type":"aborted","message":"Session '
    'interrupted: shutdown"}}' + "\n" +
    '{"type":"tool_use","part":{"type":"tool","tool":"bash","state":'
    '{"status":"error","error":"The user declined this tool call"}}}'
)
SHUTDOWN_STDERR = "permission requested: bash; auto-rejecting\n"

# A timeout can end the stream mid-line. A final line with no newline is
# an interrupted write, not a complete event.
TRUNCATED_STREAM = ('{"type":"step_start","part":{"type":"step-start"}}'
                    + "\n" +
                    '{"type":"text","part":{"type":"text","text":"a rev')

# A tool call that ran to completion before the timeout is still the
# model's own work.
COMPLETED_TOOL_EVENT = ('{"type":"tool_use","part":{"type":"tool",'
                        '"tool":"read","state":{"status":"completed"}}}')


class TestTheEventStream(unittest.TestCase):
    def test_an_empty_stream_produced_nothing(self):
        self.assertFalse(stream_acted(""))
        self.assertFalse(stream_acted("\n \n"))

    def test_a_text_event_is_the_models_own_output(self):
        self.assertTrue(stream_acted(TEXT_EVENT))

    def test_a_tool_event_is_the_models_own_output(self):
        self.assertTrue(stream_acted(TOOL_EVENT))

    def test_only_error_events_produced_nothing(self):
        self.assertFalse(stream_acted(ERROR_EVENT))
        self.assertFalse(stream_acted(ERROR_EVENT + "\n" + ERROR_EVENT))

    def test_a_step_that_opened_and_never_answered_produced_nothing(self):
        # A run killed at its own timeout leaves only the step bookkeeping
        # behind: the model produced nothing of its own.
        start = '{"type":"step_start","part":{"type":"step-start"}}'
        finish = '{"type":"step_finish","part":{"type":"step-finish"}}'
        self.assertFalse(stream_acted(start))
        self.assertFalse(stream_acted(start + "\n" + ERROR_EVENT))
        self.assertFalse(stream_acted(start + "\n" + finish))

    def test_a_step_does_not_hide_what_the_model_produced(self):
        start = '{"type":"step_start","part":{"type":"step-start"}}'
        self.assertTrue(stream_acted(start + "\n" + TEXT_EVENT))
        self.assertTrue(stream_acted(start + "\n" + TOOL_EVENT))
        self.assertTrue(stream_acted(start + "\n" + '{"type":"reasoning"}'))
        self.assertTrue(stream_acted(start + "\nnot json"))

    def test_a_stream_that_does_not_parse_counts_as_produced(self):
        # The rule fails closed: a stream nobody can read is a stream
        # nobody can say was empty.
        self.assertTrue(stream_acted("not json at all"))
        self.assertTrue(stream_acted('{"type":"text"' + "\n"))

    def test_a_stream_that_is_not_an_object_counts_as_produced(self):
        self.assertTrue(stream_acted("[1, 2]"))
        self.assertTrue(stream_acted('"a string"'))

    def test_an_event_without_a_type_counts_as_produced(self):
        self.assertTrue(stream_acted('{"part":{"type":"text"}}'))


class TestTheStderr(unittest.TestCase):
    def test_an_ask_notice_is_a_refusal(self):
        self.assertTrue(stderr_acted(ASK_NOTICE))

    def test_a_refusal_naming_the_tool_is_a_refusal(self):
        self.assertTrue(stderr_acted(REFUSAL))

    def test_a_declined_tool_call_is_a_refusal(self):
        self.assertTrue(stderr_acted("The user declined this tool call"))

    def test_a_stack_trace_naming_a_permission_is_not_a_refusal(self):
        # The broad pattern matched the bare word, so an outage whose
        # stderr carried a stack trace was read as a model that acted
        # and the gate failed instead of staying pending.
        self.assertFalse(stderr_acted(PERMISSION_TRACE))

    def test_a_stack_trace_naming_a_shell_is_not_a_refusal(self):
        self.assertFalse(stderr_acted(SHELL_TRACE))

    def test_a_refusal_to_permit_is_a_refusal(self):
        # The one phrase that is not a refusal wording: it can only
        # force ACTED, so it fails the gate rather than leaving it
        # pending. That is the safe direction.
        self.assertTrue(stderr_acted("bash: not permitted"))
        self.assertTrue(stderr_acted(EPERM_TRACE))

    def test_an_empty_stderr_refused_nothing(self):
        self.assertFalse(stderr_acted(""))


class TestAFileThatCannotBeRead(unittest.TestCase):
    """A run whose stream or stderr cannot be READ is not a run that
    produced nothing: an unreadable run is an unknown, and the rule
    fails closed on an unknown."""

    def _answer(self, events_path, stderr_path):
        result = subprocess.run(
            [sys.executable, "-I", _script("model_acted.py"),
             "--events", events_path, "--stderr", stderr_path],
            capture_output=True, text=True, check=False)
        self.assertEqual(result.returncode, 0)
        return result.stdout.strip()

    def test_a_directory_instead_of_the_stream_is_acted(self):
        with tempfile.TemporaryDirectory() as folder:
            events_path = os.path.join(folder, "events.jsonl")
            os.mkdir(events_path)
            stderr_path = os.path.join(folder, "stderr.txt")
            with open(stderr_path, "w", encoding="utf-8") as handle:
                handle.write("")
            self.assertEqual(self._answer(events_path, stderr_path), "true")

    def test_a_stream_nobody_can_read_is_acted(self):
        with tempfile.TemporaryDirectory() as folder:
            events_path = _unreadable(self, folder, ERROR_EVENT + "\n")
            stderr_path = os.path.join(folder, "stderr.txt")
            with open(stderr_path, "w", encoding="utf-8") as handle:
                handle.write("")
            self.assertEqual(self._answer(events_path, stderr_path), "true")

    def test_a_stderr_nobody_can_read_is_acted(self):
        with tempfile.TemporaryDirectory() as folder:
            events_path = os.path.join(folder, "events.jsonl")
            with open(events_path, "w", encoding="utf-8") as handle:
                handle.write(ERROR_EVENT + "\n")
            stderr_path = _unreadable(self, folder, "the provider said nothing\n")
            self.assertEqual(self._answer(events_path, stderr_path), "true")

    def test_an_absent_file_is_nothing(self):
        with tempfile.TemporaryDirectory() as folder:
            absent = os.path.join(folder, "absent.jsonl")
            stderr_path = os.path.join(folder, "stderr.txt")
            with open(stderr_path, "w", encoding="utf-8") as handle:
                handle.write("")
            self.assertEqual(self._answer(absent, stderr_path), "false")

    def test_an_empty_file_is_nothing(self):
        with tempfile.TemporaryDirectory() as folder:
            events_path = os.path.join(folder, "events.jsonl")
            stderr_path = os.path.join(folder, "stderr.txt")
            for path in (events_path, stderr_path):
                with open(path, "w", encoding="utf-8") as handle:
                    handle.write("")
            self.assertEqual(self._answer(events_path, stderr_path), "false")


class TestTheAnswer(unittest.TestCase):
    def test_nothing_at_all_is_not_acted(self):
        self.assertFalse(acted("", ""))

    def test_the_stream_alone_is_enough(self):
        self.assertTrue(acted(TEXT_EVENT, ""))

    def test_the_stderr_alone_is_enough(self):
        self.assertTrue(acted("", ASK_NOTICE))

    def test_an_outage_that_mentions_neither_is_not_acted(self):
        self.assertFalse(acted(ERROR_EVENT, PERMISSION_TRACE))


class TestATimedOutRun(unittest.TestCase):
    """A run killed by its own timeout is an outage, not a verdict.

    opencode writes the shutdown shape - an aborted error, a declined
    tool call and the auto-reject notice - as it begins to shut down. A
    timed-out run carries that shape even though the provider was still
    thinking, so it must not be read as the model acting. The same shape
    from a run that ended on its own is a real declined tool call.
    """

    def test_the_shutdown_shape_is_not_the_model_acting(self):
        self.assertFalse(stream_acted(SHUTDOWN_STREAM, timed_out=True))

    def test_the_shutdown_shape_on_stderr_is_not_a_refusal(self):
        self.assertFalse(stderr_acted(SHUTDOWN_STDERR, timed_out=True))

    def test_a_timeout_that_produced_only_the_shutdown_did_not_act(self):
        self.assertFalse(acted(SHUTDOWN_STREAM, SHUTDOWN_STDERR,
                               timed_out=True))

    def test_the_same_shutdown_shape_without_a_timeout_is_acted(self):
        # Without the timeout, the declined tool call is the model's own
        # attempt and the run is a verdict.
        self.assertTrue(acted(SHUTDOWN_STREAM, SHUTDOWN_STDERR))

    def test_a_truncated_final_line_is_an_interrupted_write(self):
        self.assertFalse(stream_acted(TRUNCATED_STREAM, timed_out=True))

    def test_a_truncated_final_line_without_a_timeout_still_counts(self):
        self.assertTrue(stream_acted(TRUNCATED_STREAM))

    def test_output_before_the_timeout_is_still_evidence(self):
        stream = SHUTDOWN_STREAM + "\n" + TEXT_EVENT
        self.assertTrue(stream_acted(stream, timed_out=True))

    def test_a_completed_tool_before_the_timeout_is_still_evidence(self):
        stream = COMPLETED_TOOL_EVENT + "\n" + SHUTDOWN_STREAM
        self.assertTrue(stream_acted(stream, timed_out=True))


class TestTheCommand(unittest.TestCase):
    def _run(self, events="", stderr="", exit_status=""):
        with tempfile.TemporaryDirectory() as folder:
            events_path = os.path.join(folder, "events.jsonl")
            stderr_path = os.path.join(folder, "stderr.txt")
            marker = os.path.join(folder, "model-acted.txt")
            with open(events_path, "w", encoding="utf-8") as handle:
                handle.write(events)
            with open(stderr_path, "w", encoding="utf-8") as handle:
                handle.write(stderr)
            argv = [sys.executable, "-I", _script("model_acted.py"),
                    "--events", events_path, "--stderr", stderr_path,
                    "--marker", marker]
            if exit_status != "":
                argv += ["--exit-status", str(exit_status)]
            result = subprocess.run(argv, capture_output=True, text=True,
                                    check=False)
            return result, _marker(marker)

    def test_the_command_answers_true_for_a_model_that_produced_something(self):
        result, marker = self._run(events=TEXT_EVENT)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout.strip(), "true")

    def test_the_command_answers_false_for_a_model_that_produced_nothing(self):
        result, marker = self._run(events=ERROR_EVENT)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout.strip(), "false")

    def test_the_command_marks_a_run_where_the_model_acted(self):
        _result, marker = self._run(events=TEXT_EVENT)
        self.assertEqual(marker.strip(), model_acted.MARKER_ACTED)

    def test_the_command_leaves_the_marker_alone_otherwise(self):
        _result, marker = self._run(events=ERROR_EVENT)
        self.assertEqual(marker.strip(), "")

    def test_a_missing_file_is_not_an_answer_of_its_own(self):
        result = subprocess.run(
            [sys.executable, "-I", _script("model_acted.py"),
             "--events", os.path.join("nowhere", "events.jsonl")],
            capture_output=True, text=True, check=False)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout.strip(), "false")

    def test_the_command_reads_the_timeout_status(self):
        result, marker = self._run(events=SHUTDOWN_STREAM,
                                   stderr=SHUTDOWN_STDERR,
                                   exit_status=124)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout.strip(), "false")
        self.assertEqual(marker.strip(), "")

    def test_the_same_shutdown_shape_on_a_run_that_ended_is_acted(self):
        result, marker = self._run(events=SHUTDOWN_STREAM,
                                   stderr=SHUTDOWN_STDERR,
                                   exit_status=1)
        self.assertEqual(result.stdout.strip(), "true")
        self.assertEqual(marker.strip(), model_acted.MARKER_ACTED)

    def test_the_command_reports_the_event_that_decided(self):
        result, _marker = self._run(events=TEXT_EVENT)
        report = _report(result)
        self.assertIs(report["acted"], True)
        self.assertIn("text", report["stream"])

    def test_the_command_reports_a_timeout_and_its_evidence(self):
        result, _marker = self._run(events=SHUTDOWN_STREAM,
                                    stderr=SHUTDOWN_STDERR,
                                    exit_status=124)
        report = _report(result)
        self.assertIs(report["acted"], False)
        self.assertIs(report["timed_out"], True)
        self.assertIn("shutdown", report["stream"])

    def test_the_command_reports_the_refusal_phrase(self):
        result, _marker = self._run(stderr=ASK_NOTICE)
        report = _report(result)
        self.assertIs(report["acted"], True)
        self.assertIn("permission requested", report["stderr"])

    def test_the_command_reports_an_unreadable_stream(self):
        with tempfile.TemporaryDirectory() as folder:
            events_path = os.path.join(folder, "events.jsonl")
            os.mkdir(events_path)
            stderr_path = os.path.join(folder, "stderr.txt")
            with open(stderr_path, "w", encoding="utf-8") as handle:
                handle.write("")
            result = subprocess.run(
                [sys.executable, "-I", _script("model_acted.py"),
                 "--events", events_path, "--stderr", stderr_path],
                capture_output=True, text=True, check=False)
        report = _report(result)
        self.assertIs(report["acted"], True)
        self.assertIn("could not be read", report["stream"])


def _report(result):
    """The evidence line the command wrote, as a dict."""
    for line in result.stderr.splitlines():
        if line.startswith("model-acted: "):
            return json.loads(line[len("model-acted: "):])
    return {}


def _script(name):
    """The path of one script beside this test."""
    return os.path.join(os.path.dirname(os.path.abspath(__file__)), name)


def _marker(path):
    """What the marker file says, or nothing when it was never written."""
    if not os.path.exists(path):
        return ""
    with open(path, encoding="utf-8") as handle:
        return handle.read()


def _unreadable(case, folder, content):
    """A file this process cannot read, or the test is skipped.

    The content is what the file would say if it COULD be read, and it
    says nothing that would force acted on its own: a file that turns
    out to be readable must fail the test, not pass it for the wrong
    reason.
    """
    path = os.path.join(folder, "unreadable.txt")
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(content)
    os.chmod(path, 0)
    try:
        with open(path, encoding="utf-8") as handle:
            handle.read()
    except OSError:
        return path
    os.chmod(path, 0o600)
    case.skipTest("this host reads a mode-000 file")


if __name__ == "__main__":
    unittest.main()

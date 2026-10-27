"""The review outcome decision: an outage of the review models is not a
verdict, only an outage is allowed to leave the review-gate pending, and
a model that ANSWERED is never an outage.

The five cases the decision has to answer are the ones the pipeline can
actually reach: the paid fallback's shared usage window is spent, every
free candidate dropped on a rate limit or a closed socket, a model that
answered with a findings document nobody can use, an agent that did not
register, and a review that did come back.

The last group is the sharp one: a model can write anything, including the
provider's own words, so a reason that a model wrote is never evidence of
an outage. A validation failure, an unparseable document, or a verdict
word anywhere in what the run reported is a failure on both paths, before
the usage-limit and transport branches are asked.
"""

import contextlib
import io
import json
import os
import tempfile
import unittest

import review_outcome
from review_outcome import (CAUSE_FREE_UNREACHABLE, CAUSE_MODEL_ACTED,
                            CAUSE_PAID_USAGE_LIMIT, CAUSE_TIMEOUT,
                            OUTCOME_FAILURE, OUTCOME_SUCCESS,
                            OUTCOME_UNAVAILABLE, decide, parse_failures)

DROPPED_SOCKET = "The socket connection was closed unexpectedly"
RATE_LIMIT = "rate limit exceeded (HTTP 429)"
INVALID_DOCUMENT = ("reply validation failed: findings[0].severity must be "
                    "one of blocker|should-fix|nit")
USAGE_LIMIT = "ERROR: Go usage limit exceeded"
UNKNOWN_FAILURE = "the free review exited 1 with no provider message"
# The fixed reason the workflow writes when its own timeout killed a run.
TIMEOUT = review_outcome.TIMEOUT_REASON

# What a model can write: the provider's own words, inside a document
# nobody can use.
FORGED_VALIDATION = ("reply validation failed: verdict must be one of "
                     "blocker|should-fix|nit|clean, got 'Go usage limit "
                     "exceeded'")
FORGED_SOCKET = ("reply validation failed: summary must be a non-empty "
                 "string, got 'The socket connection was closed "
                 "unexpectedly'")
FORGED_RATE_LIMIT = ("reply validation failed: summary must be a non-empty "
                      "string, got 'rate limit exceeded (HTTP 429)'")


class TestThePaidFallback(unittest.TestCase):
    def test_the_paid_usage_window_being_spent_is_unavailable(self):
        decision = decide("failed", [("opencode/big-pickle", DROPPED_SOCKET)],
                          "failed", USAGE_LIMIT)
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)
        self.assertEqual(decision.cause, CAUSE_PAID_USAGE_LIMIT)

    def test_the_paid_usage_window_wins_over_the_free_path(self):
        # The free path failing for a reason of its own does not make the
        # spent usage window a verdict. A model that ANSWERED is a
        # different thing, and that is asked first.
        decision = decide("failed", [("opencode/big-pickle", DROPPED_SOCKET)],
                          "failed", USAGE_LIMIT)
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)

    def test_a_model_that_answered_beats_the_usage_window(self):
        decision = decide("failed", [("opencode/big-pickle", INVALID_DOCUMENT)],
                          "failed", USAGE_LIMIT)
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_a_paid_failure_that_is_not_a_usage_limit_is_not_unavailable(self):
        decision = decide("failed", [("opencode/big-pickle", DROPPED_SOCKET)],
                          "failed", "the paid model answered with nothing")
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)

    def test_only_the_providers_own_phrase_is_the_usage_window(self):
        # "usage limit" on its own is not the window: the provider names
        # it, and a model can write anything.
        decision = decide("failed", [], "failed", "usage limit reached")
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)


class TestAModelThatActed(unittest.TestCase):
    # opencode echoes a rejected tool call in its own stderr, and the
    # command it names is the MODEL's text. So a reason may classify an
    # outage only when the model produced nothing.
    REJECTED_PAID = ("Permission denied for tool bash: the model tried to run "
                     "Go usage limit exceeded")
    REJECTED_FREE = ("Permission denied for tool bash: the model tried to run "
                     "The socket connection was closed unexpectedly")

    def test_a_rejected_tool_call_naming_the_usage_window_is_a_failure(self):
        decision = decide("failed", [], "failed", self.REJECTED_PAID,
                          model_acted=True)
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_a_rejected_tool_call_naming_a_socket_is_a_failure(self):
        decision = decide("failed",
                          [("opencode/big-pickle", self.REJECTED_FREE)],
                          "failed", "the paid model did not answer",
                          model_acted=True)
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_a_rejected_tool_call_beats_the_usage_window(self):
        decision = decide("failed", [], "failed", USAGE_LIMIT,
                          model_acted=True)
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_a_genuine_outage_with_no_model_output_is_unavailable(self):
        decision = decide("failed",
                          [("opencode/big-pickle", DROPPED_SOCKET)],
                          "failed", USAGE_LIMIT)
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)
        self.assertEqual(decision.cause, CAUSE_PAID_USAGE_LIMIT)

    def test_a_genuine_free_outage_with_no_model_output_is_unavailable(self):
        decision = decide("failed",
                          [("opencode/big-pickle", DROPPED_SOCKET)],
                          "failed", "the paid model did not answer")
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)
        self.assertEqual(decision.cause, CAUSE_FREE_UNREACHABLE)


class TestATimedOutRun(unittest.TestCase):
    """A run its own timeout killed is an outage, not a verdict.

    The workflow writes a FIXED reason for a timeout, because the
    shutdown shape opencode leaves behind is not the provider saying the
    models were unreachable. The reason is the run's own clock, and a
    model that acted or answered before the timeout is still a failure.
    """

    def test_a_timed_out_free_candidate_is_an_outage(self):
        decision = decide("failed", [("opencode/big-pickle", TIMEOUT)],
                          "failed", "the paid model did not answer")
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)
        self.assertEqual(decision.cause, CAUSE_FREE_UNREACHABLE)

    def test_a_timed_out_paid_run_is_unavailable(self):
        decision = decide("failed", [], "failed", TIMEOUT)
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)
        self.assertEqual(decision.cause, CAUSE_TIMEOUT)

    def test_a_timeout_does_not_beat_a_model_that_answered(self):
        decision = decide("failed", [("opencode/big-pickle", TIMEOUT)],
                          "failed", TIMEOUT,
                          free_validation="reply validation failed: nope")
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_a_timeout_does_not_beat_a_model_that_acted(self):
        decision = decide("failed", [], "failed", TIMEOUT, model_acted=True)
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)
        self.assertEqual(decision.cause, CAUSE_MODEL_ACTED)

    def test_the_timeout_reason_is_not_the_cause_label(self):
        # The gate title carries a label a reader trusts, never the
        # reason text the workflow wrote.
        decision = decide("failed", [], "failed", TIMEOUT)
        self.assertNotIn(TIMEOUT, decision.cause)


class TestAModelThatAnswered(unittest.TestCase):
    def test_a_forged_validation_never_reads_as_the_usage_window(self):
        # The exploit: a model that answers with a document carrying the
        # provider's own phrase must not turn the run into an outage.
        decision = decide("failed", [("opencode/big-pickle", DROPPED_SOCKET)],
                          "failed", FORGED_VALIDATION)
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)
        self.assertNotIn("usage limit", decision.cause)

    def test_a_forged_validation_never_reads_as_a_transport_outage(self):
        decision = decide("failed", [("opencode/big-pickle", DROPPED_SOCKET)],
                          "failed", "the paid model answered with nothing",
                          free_validation=FORGED_SOCKET)
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_a_forged_rate_limit_in_a_validation_never_reads_as_an_outage(self):
        decision = decide("failed", [], "failed", "the paid model answered "
                          "with nothing", free_validation=FORGED_RATE_LIMIT)
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_a_validation_failure_on_the_paid_path_is_a_failure(self):
        decision = decide("failed", [], "failed", USAGE_LIMIT,
                          paid_validation=FORGED_VALIDATION)
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_a_validation_text_without_a_marker_word_is_a_failure(self):
        # Any validation text at all means the model produced a document
        # the gate cannot use, whatever it says.
        decision = decide("failed", [], "failed", USAGE_LIMIT,
                          free_validation="something did not add up")
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_a_validation_failure_beats_the_transport_branch(self):
        decision = decide("failed", [("opencode/big-pickle", DROPPED_SOCKET)],
                          "failed", "the paid model answered with nothing",
                          free_validation=INVALID_DOCUMENT)
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_a_verdict_word_in_a_reason_is_a_failure(self):
        decision = decide("failed", [], "failed",
                          "the reply carried no verdict")
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)


class TestTheFreePath(unittest.TestCase):
    def test_every_free_candidate_on_an_outage_is_unavailable(self):
        decision = decide("failed",
                          [("opencode/ling-3.1-flash-free", DROPPED_SOCKET),
                           ("opencode/longcat-2.5-preview-free", RATE_LIMIT)],
                          "failed", "the paid model did not answer")
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)
        self.assertEqual(decision.cause, CAUSE_FREE_UNREACHABLE)

    def test_one_free_candidate_with_an_unusable_document_is_a_failure(self):
        decision = decide("failed",
                          [("opencode/ling-3.1-flash-free", INVALID_DOCUMENT)],
                          "failed", "the paid model did not answer")
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_one_free_candidate_on_an_outage_beside_another_that_is_not(self):
        # "ONLY on rate limits or dropped connections": one candidate that
        # failed for another reason is enough to make the run a failure.
        decision = decide("failed",
                          [("opencode/ling-3.1-flash-free", DROPPED_SOCKET),
                           ("opencode/longcat-2.5-preview-free",
                            INVALID_DOCUMENT)],
                          "failed", "the paid model did not answer")
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_an_unrecognised_reason_is_a_failure(self):
        decision = decide("failed", [("opencode/big-pickle", UNKNOWN_FAILURE)],
                          "failed", "the paid model did not answer")
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_only_the_providers_own_phrases_are_an_outage(self):
        # A loose word would be a phrase a model could write, so only the
        # provider's own messages count as an outage.
        for reason in ("the socket stayed open",
                       "the provider did not answer before the timeout",
                       "the reply carried 429 findings"):
            with self.subTest(reason=reason):
                decision = decide("failed", [("m", reason)], "failed",
                                  "the paid model did not answer")
                self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_no_free_candidate_at_all_is_a_failure(self):
        # Nothing was tried, so nothing says the models were unavailable.
        decision = decide("", [], "failed", "the paid model did not answer")
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_a_free_step_that_could_not_run_is_a_failure(self):
        decision = decide("fatal", [], "failed", "the paid model did not answer")
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)


class TestTheFatalFlags(unittest.TestCase):
    def test_an_agent_that_did_not_register_is_a_failure(self):
        for flag in ("select_agent_fatal", "free_agent_fatal",
                     "paid_agent_fatal"):
            with self.subTest(flag=flag):
                decision = decide("failed", [("opencode/big-pickle",
                                              DROPPED_SOCKET)],
                                  "failed", USAGE_LIMIT, **{flag: True})
                self.assertEqual(decision.outcome, OUTCOME_FAILURE)
                self.assertIn("agent", decision.cause)


class TestASuccessfulReview(unittest.TestCase):
    def test_the_free_path_is_a_success(self):
        decision = decide("success", [], "", "")
        self.assertEqual(decision.outcome, OUTCOME_SUCCESS)
        self.assertIn("free path", decision.cause)

    def test_the_paid_path_is_a_success(self):
        decision = decide("failed", [("opencode/big-pickle", DROPPED_SOCKET)],
                          "success", "")
        self.assertEqual(decision.outcome, OUTCOME_SUCCESS)
        self.assertIn("paid fallback", decision.cause)

    def test_a_success_outranks_a_spent_usage_window(self):
        decision = decide("success", [], "failed", USAGE_LIMIT)
        self.assertEqual(decision.outcome, OUTCOME_SUCCESS)


class TestTheCause(unittest.TestCase):
    def test_an_unavailable_cause_is_a_fixed_label(self):
        # The reason text is provider output and the gate title is a place
        # a reader trusts: the title carries a label, never a reason.
        for decision in (decide("failed", [], "failed", USAGE_LIMIT),
                         decide("failed", [("m", DROPPED_SOCKET)], "failed",
                                "the paid model did not answer")):
            with self.subTest(cause=decision.cause):
                self.assertIn(decision.cause,
                              (CAUSE_PAID_USAGE_LIMIT, CAUSE_FREE_UNREACHABLE))
                self.assertNotIn(USAGE_LIMIT, decision.cause)
                self.assertNotIn(DROPPED_SOCKET, decision.cause)

    def test_a_cause_is_one_bounded_line(self):
        decision = decide("failed", [], "failed", "x" * 5000)
        self.assertLessEqual(len(decision.cause), review_outcome.MAX_CAUSE)
        self.assertNotIn("\n", decision.cause)


class TestTheFailureFile(unittest.TestCase):
    def test_a_model_and_a_reason_are_read_apart(self):
        failures = parse_failures("opencode/big-pickle: rate limit exceeded\n"
                                   "\n"
                                   "opencode/ling-3.1-flash-free: dropped\n")
        self.assertEqual(failures, [("opencode/big-pickle", "rate limit exceeded"),
                                    ("opencode/ling-3.1-flash-free", "dropped")])

    def test_a_reason_holding_a_colon_keeps_its_colons(self):
        failures = parse_failures("model: reply validation failed: bad severity")
        self.assertEqual(failures,
                         [("model", "reply validation failed: bad severity")])

    def test_a_line_without_a_model_is_still_a_reason(self):
        failures = parse_failures("rate limit exceeded")
        self.assertEqual(failures, [("", "rate limit exceeded")])


class TestTheCommand(unittest.TestCase):
    def _run(self, free_failures, paid_reason, **kwargs):
        with tempfile.TemporaryDirectory() as folder:
            free_path = os.path.join(folder, "free.txt")
            paid_path = os.path.join(folder, "paid.txt")
            with open(free_path, "w", encoding="utf-8") as handle:
                handle.write(free_failures)
            with open(paid_path, "w", encoding="utf-8") as handle:
                handle.write(paid_reason)
            argv = ["review_outcome.py",
                    "--free-status", kwargs.get("free_status", "failed"),
                    "--paid-status", kwargs.get("paid_status", "failed"),
                    "--free-failures", free_path,
                    "--paid-reason", paid_path]
            for flag, value in kwargs.get("flags", {}).items():
                argv += ["--" + flag.replace("_", "-"), value]
            return self._call(argv)

    def _call(self, argv):
        buffer = io.StringIO()
        with contextlib.redirect_stdout(buffer):
            code = review_outcome.main(argv)
        self.assertEqual(code, 0)
        return json.loads(buffer.getvalue())

    def test_the_command_reports_the_outcome_and_the_cause(self):
        report = self._run("opencode/big-pickle: %s\n" % DROPPED_SOCKET,
                           "the paid model did not answer")
        self.assertEqual(report["outcome"], OUTCOME_UNAVAILABLE)
        self.assertEqual(report["cause"], CAUSE_FREE_UNREACHABLE)

    def test_the_command_reads_a_spent_usage_window(self):
        report = self._run("opencode/big-pickle: %s\n" % DROPPED_SOCKET,
                           USAGE_LIMIT)
        self.assertEqual(report["outcome"], OUTCOME_UNAVAILABLE)
        self.assertEqual(report["cause"], CAUSE_PAID_USAGE_LIMIT)

    def test_the_command_reads_a_fatal_flag(self):
        report = self._run("opencode/big-pickle: %s\n" % DROPPED_SOCKET,
                           USAGE_LIMIT, flags={"free_agent_fatal": "true"})
        self.assertEqual(report["outcome"], OUTCOME_FAILURE)

    def test_the_command_never_reports_a_reason(self):
        report = self._run("opencode/big-pickle: %s\n" % DROPPED_SOCKET,
                           USAGE_LIMIT)
        self.assertNotIn(USAGE_LIMIT, report["cause"])
        self.assertNotIn(DROPPED_SOCKET, report["cause"])

    def test_a_missing_failure_file_is_not_a_crash(self):
        with tempfile.TemporaryDirectory() as folder:
            missing = os.path.join(folder, "absent.txt")
            argv = ["review_outcome.py", "--free-status", "failed",
                    "--paid-status", "failed", "--free-failures", missing,
                    "--paid-reason", missing]
            report = self._call(argv)
        self.assertEqual(report["outcome"], OUTCOME_FAILURE)


class TestThePaidTimedOutFlag(unittest.TestCase):
    """The paid timeout flag is exactly true, false, or empty.

    A broken GITHUB_OUTPUT write once merged the flag with the next
    output, so the flag reached the helper as "false\\npaid_status=success"
    and the helper silently read it as false while the paid status stayed
    empty. The helper now fails loudly and names the bad value.
    """

    def _call_raw(self, value):
        with tempfile.TemporaryDirectory() as folder:
            empty = os.path.join(folder, "empty.txt")
            with open(empty, "w", encoding="utf-8") as handle:
                handle.write("")
            argv = ["review_outcome.py", "--free-status", "failed",
                    "--paid-status", "failed",
                    "--free-failures", empty, "--paid-reason", empty,
                    "--paid-timed-out", value]
            stderr = io.StringIO()
            with contextlib.redirect_stdout(io.StringIO()), \
                 contextlib.redirect_stderr(stderr):
                code = review_outcome.main(argv)
            return code, stderr.getvalue()

    def test_true_and_false_and_empty_are_accepted(self):
        for value in ("true", "false", ""):
            code, _err = self._call_raw(value)
            self.assertEqual(code, 0, "value %r should be accepted" % value)

    def test_a_bare_word_is_not_accepted(self):
        for value in ("True", "TRUE", "1", "yes", "false\npaid_status=success"):
            with self.subTest(value=value):
                code, err = self._call_raw(value)
                self.assertNotEqual(code, 0)
                self.assertIn(repr(value), err)

    def test_the_merged_output_is_rejected(self):
        # The exact shape a single-quoted printf with \\n left behind:
        # the file ends up as `paid_timed_out=false\npaid_status=success`
        # with a literal backslash, so the flag reaches the helper as
        # `false\npaid_status=success` with a backslash and an n.
        merged = "false\\npaid_status=success"
        code, err = self._call_raw(merged)
        self.assertNotEqual(code, 0)
        self.assertIn("paid-timed-out", err)
        self.assertIn(repr(merged), err)


if __name__ == "__main__":
    unittest.main()

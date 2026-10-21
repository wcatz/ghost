"""The review outcome decision: an outage of the review models is not a
verdict, and only an outage is allowed to leave the review-gate pending.

The five cases the decision has to answer are the ones the pipeline can
actually reach: the paid fallback's shared usage window is spent, every
free candidate dropped on a rate limit or a closed socket, a model that
answered with a findings document nobody can use, an agent that did not
register, and a review that did come back.
"""

import contextlib
import io
import json
import os
import tempfile
import unittest

import review_outcome
from review_outcome import (OUTCOME_FAILURE, OUTCOME_SUCCESS,
                            OUTCOME_UNAVAILABLE, decide, parse_failures)

DROPPED_SOCKET = "The socket connection was closed unexpectedly"
RATE_LIMIT = "rate limit exceeded (HTTP 429)"
INVALID_DOCUMENT = ("reply validation failed: findings[0].severity must be "
                    "one of blocker|should-fix|nit")
USAGE_LIMIT = "ERROR: Go usage limit exceeded"
UNKNOWN_FAILURE = "the free review exited 1 without a valid findings document"


class TestThePaidFallback(unittest.TestCase):
    def test_the_paid_usage_window_being_spent_is_unavailable(self):
        decision = decide("failed", [("opencode/big-pickle", DROPPED_SOCKET)],
                          "failed", USAGE_LIMIT)
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)
        self.assertIn("Go usage limit exceeded", decision.cause)

    def test_the_paid_usage_window_wins_over_the_free_path(self):
        # The free path failing for a reason of its own does not make the
        # spent usage window a verdict.
        decision = decide("failed", [("opencode/big-pickle", INVALID_DOCUMENT)],
                          "failed", USAGE_LIMIT)
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)

    def test_a_paid_failure_beside_a_free_outage_is_still_unavailable(self):
        # The free path was an outage and the paid step produced no
        # result: nothing here is a verdict.
        decision = decide("failed", [("opencode/big-pickle", DROPPED_SOCKET)],
                          "failed", "the paid model answered with nothing")
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)


class TestTheFreePath(unittest.TestCase):
    def test_every_free_candidate_on_an_outage_is_unavailable(self):
        decision = decide("failed",
                          [("opencode/ling-3.1-flash-free", DROPPED_SOCKET),
                           ("opencode/longcat-2.5-preview-free", RATE_LIMIT)],
                          "failed", "the paid model did not answer")
        self.assertEqual(decision.outcome, OUTCOME_UNAVAILABLE)
        self.assertIn("free candidate", decision.cause)

    def test_one_free_candidate_with_an_unusable_document_is_a_failure(self):
        decision = decide("failed",
                          [("opencode/ling-3.1-flash-free", INVALID_DOCUMENT)],
                          "failed", "the paid model did not answer")
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)
        self.assertIn("findings document", decision.cause)

    def test_one_free_candidate_on_an_outage_beside_another_that_is_not(self):
        # "ONLY on rate limits or dropped connections": one candidate that
        # failed for another reason is enough to make the run a failure.
        decision = decide("failed",
                          [("opencode/ling-3.1-flash-free", DROPPED_SOCKET),
                           ("opencode/longcat-2.5-preview-free", INVALID_DOCUMENT)],
                          "failed", "the paid model did not answer")
        self.assertEqual(decision.outcome, OUTCOME_FAILURE)

    def test_an_unrecognised_reason_is_a_failure(self):
        decision = decide("failed", [("opencode/big-pickle", UNKNOWN_FAILURE)],
                          "failed", "the paid model did not answer")
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
        self.assertTrue(report["cause"])

    def test_the_command_reads_a_spent_usage_window(self):
        report = self._run("opencode/big-pickle: %s\n" % DROPPED_SOCKET,
                           USAGE_LIMIT)
        self.assertEqual(report["outcome"], OUTCOME_UNAVAILABLE)
        self.assertIn("Go usage limit exceeded", report["cause"])

    def test_the_command_reads_a_fatal_flag(self):
        report = self._run("opencode/big-pickle: %s\n" % DROPPED_SOCKET,
                           USAGE_LIMIT, flags={"free_agent_fatal": "true"})
        self.assertEqual(report["outcome"], OUTCOME_FAILURE)

    def test_a_missing_failure_file_is_not_a_crash(self):
        with tempfile.TemporaryDirectory() as folder:
            missing = os.path.join(folder, "absent.txt")
            argv = ["review_outcome.py", "--free-status", "failed",
                    "--paid-status", "failed", "--free-failures", missing,
                    "--paid-reason", missing]
            report = self._call(argv)
        self.assertEqual(report["outcome"], OUTCOME_FAILURE)


class TestTheCause(unittest.TestCase):
    def test_a_cause_is_one_bounded_line(self):
        decision = decide("failed", [], "failed", "x" * 5000)
        self.assertLessEqual(len(decision.cause), review_outcome.MAX_CAUSE)
        self.assertNotIn("\n", decision.cause)


if __name__ == "__main__":
    unittest.main()

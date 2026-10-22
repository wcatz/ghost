"""What the `review-gate` check run says about a pull request.

The Sweeper publishes the gate, and the merge is gated on it. So the
gate has to be a judgement about the pull request, never about the
event that woke the sweeper up.

A comment on the pull request, a reviewer run that was skipped, and a
run that judged a commit other than the current head each leave the
gate failed, and those arms are held here. Whose review may carry the
marker is held by test_review_marker.py.
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

from review_gate import decide, leave_pending

HERE = os.path.dirname(os.path.abspath(__file__))

# A commit, and another one.
HEAD = "8877fc9b" + "0" * 32
OTHER = "11223344" + "0" * 32

# The title the reviewer publishes when the models were unavailable.
PENDING = "REVIEW-UNAVAILABLE: the free models were unreachable"


class TestTheArms(unittest.TestCase):
    """Every arm of the decision, one at a time."""

    def test_a_successful_run_that_judged_the_head_passes_the_gate(self):
        decision = decide("workflow_run", "success", HEAD, HEAD, True, [], 0, 0)
        self.assertEqual(decision.publish, "success")

    def test_a_run_that_judged_nothing_leaves_the_gate_failed(self):
        decision = decide("workflow_run", "success", HEAD, HEAD, False, [], 0, 0)
        self.assertEqual(decision.publish, "failure")

    def test_a_cancelled_run_leaves_the_gate_failed(self):
        decision = decide("workflow_run", "cancelled", HEAD, HEAD, False, [], 0, 0)
        self.assertEqual(decision.publish, "failure")

    def test_a_failed_run_leaves_the_gate_failed(self):
        decision = decide("workflow_run", "failure", HEAD, HEAD, False, [], 0, 0)
        self.assertEqual(decision.publish, "failure")

    def test_a_skipped_run_leaves_the_gate_failed(self):
        # The reviewer skips a fork or a bot pull request, so nothing
        # judged it and the gate must not read as "nothing wrong".
        decision = decide("workflow_run", "skipped", HEAD, HEAD, False, [], 0, 0)
        self.assertEqual(decision.publish, "failure")
        self.assertIn("skipped", decision.title)

    def test_a_skipped_run_leaves_the_gate_failed_even_with_no_findings(self):
        decision = decide("workflow_run", "skipped", HEAD, HEAD, False, [], 0, 0)
        self.assertNotEqual(decision.publish, "success")

    def test_a_run_that_judged_another_commit_leaves_the_gate_failed(self):
        # The run that succeeded judged an older commit, so the commit
        # the pull request is on now was never judged.
        decision = decide("workflow_run", "success", OTHER, HEAD, True, [], 0, 0)
        self.assertEqual(decision.publish, "failure")

    def test_a_run_that_did_not_name_its_commit_leaves_the_gate_failed(self):
        # An empty run_head is treated as judging another commit: the run
        # did not name what it judged, so it cannot be trusted for this
        # head.
        decision = decide("workflow_run", "success", "", HEAD, True, [], 0, 0)
        self.assertEqual(decision.publish, "failure")

    def test_a_comment_cannot_publish_a_success_gate(self):
        decision = decide("issue_comment", "", "", HEAD, False, [], 0, 0)
        self.assertEqual(decision.publish, "failure")

    def test_a_comment_may_re_evaluate_a_head_that_was_judged(self):
        # A comment re-evaluates a head a successful run judged.
        decision = decide("issue_comment", "success", "", HEAD, True, [], 0, 0)
        self.assertEqual(decision.publish, "success")

    def test_a_comment_cannot_revive_a_head_whose_latest_run_failed(self):
        # The head was judged by an earlier run, but a later re-run was
        # cancelled: the comment must not turn that into a green gate.
        decision = decide("issue_comment", "cancelled", "", HEAD, True, [], 0, 0)
        self.assertEqual(decision.publish, "failure")

    def test_a_comment_with_no_reviewer_run_fails_closed(self):
        # No Reviewer run was found for this head, so it was never judged
        # by a run we can verify.
        decision = decide("issue_comment", "", "", HEAD, True, [], 0, 0)
        self.assertEqual(decision.publish, "failure")

    def test_a_comment_cannot_publish_a_success_gate_with_live_findings(self):
        decision = decide("issue_comment", "", "", HEAD, False, [], 2, 0)
        self.assertEqual(decision.publish, "failure")

    def test_live_findings_fail_the_gate(self):
        decision = decide("workflow_run", "success", HEAD, HEAD, True, [], 2, 0)
        self.assertEqual(decision.publish, "failure")
        self.assertIn("2", decision.title)

    def test_a_pending_gate_is_left_alone_while_nothing_judged_the_head(self):
        decision = decide("workflow_run", "success", HEAD, HEAD, False,
                           [PENDING], 0, 0)
        self.assertEqual(decision.publish, "none")

    def test_a_judged_head_completes_a_pending_gate(self):
        decision = decide("workflow_run", "success", HEAD, HEAD, True,
                           [PENDING], 0, 0)
        self.assertEqual(decision.publish, "success")

    def test_a_comment_cannot_complete_a_pending_gate(self):
        # A comment is not a review, so it cannot say the head was
        # judged, and it cannot clear a gate that is waiting for one.
        decision = decide("issue_comment", "", "", HEAD, False, [PENDING], 0, 0)
        self.assertEqual(decision.publish, "none")

    def test_the_pending_gate_is_only_left_alone_on_review_unavailable(self):
        decision = decide("workflow_run", "success", HEAD, HEAD, False,
                           ["some other pending gate"], 0, 0)
        self.assertEqual(decision.publish, "failure")

    def test_a_failed_run_reports_its_own_failure(self):
        # The run's own verdict comes first: a run that did not
        # complete is the thing to report, not the pending gate.
        decision = decide("workflow_run", "failure", HEAD, HEAD, False,
                           [PENDING], 0, 0)
        self.assertEqual(decision.publish, "failure")

    def test_the_decision_asks_for_the_threads_before_it_can_publish(self):
        decision = decide("workflow_run", "success", HEAD, HEAD, True, [],
                          None, None)
        self.assertEqual(decision.publish, "more")


class TestSweeping(unittest.TestCase):
    """Which arms may resolve outdated threads."""

    def test_a_successful_run_may_sweep_outdated_threads(self):
        decision = decide("workflow_run", "success", HEAD, HEAD, True, [], 0, 3)
        self.assertTrue(decision.sweep)

    def test_a_comment_does_not_sweep_outdated_threads(self):
        decision = decide("issue_comment", "", "", HEAD, True, [], 0, 3)
        self.assertFalse(decision.sweep)

    def test_a_run_that_judged_nothing_does_not_sweep(self):
        decision = decide("workflow_run", "skipped", HEAD, HEAD, False, [], 0, 3)
        self.assertFalse(decision.sweep)

    def test_a_run_that_judged_another_commit_does_not_sweep(self):
        decision = decide("workflow_run", "success", OTHER, HEAD, True, [], 0, 3)
        self.assertFalse(decision.sweep)


class TestThePendingGate(unittest.TestCase):
    """The gate that is left pending on REVIEW-UNAVAILABLE."""

    def test_a_pending_gate_is_left_alone_while_nothing_judged_the_head(self):
        self.assertTrue(leave_pending([PENDING], False))

    def test_a_judged_head_completes_a_pending_gate(self):
        self.assertFalse(leave_pending([PENDING], True))

    def test_no_pending_gate_is_left_alone(self):
        self.assertFalse(leave_pending([], False))

    def test_another_pending_gate_is_left_alone(self):
        self.assertFalse(leave_pending(["some other gate"], False))


class TestTheCommand(unittest.TestCase):
    """The command the sweeper calls."""

    def _run(self, *arguments):
        return subprocess.run(
            [sys.executable, "-I", os.path.join(HERE, "review_gate.py")]
            + list(arguments),
            capture_output=True, text=True, check=False)

    def test_the_command_answers_with_the_gate_to_publish(self):
        result = self._run("--mode", "gate", "--event", "workflow_run",
                            "--conclusion", "success", "--run-head", HEAD,
                            "--head", HEAD, "--judged", "true",
                            "--live", "0", "--stale", "0")
        self.assertEqual(result.returncode, 0)
        answer = json.loads(result.stdout)
        self.assertEqual(answer["publish"], "success")
        self.assertTrue(answer["title"])

    def test_the_command_answers_the_question_that_needs_no_threads(self):
        result = self._run("--mode", "pre", "--event", "workflow_run",
                            "--conclusion", "success", "--run-head", HEAD,
                            "--head", HEAD, "--judged", "false",
                            "--pending-title", PENDING)
        self.assertEqual(result.returncode, 0)
        answer = json.loads(result.stdout)
        self.assertEqual(answer["publish"], "none")

    def test_the_command_asks_for_the_threads_when_the_head_was_judged(self):
        result = self._run("--mode", "pre", "--event", "workflow_run",
                            "--conclusion", "success", "--run-head", HEAD,
                            "--head", HEAD, "--judged", "true")
        self.assertEqual(result.returncode, 0)
        answer = json.loads(result.stdout)
        self.assertEqual(answer["publish"], "more")


if __name__ == "__main__":
    unittest.main()

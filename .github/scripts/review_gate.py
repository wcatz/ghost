#!/usr/bin/env python3
"""Decide what the `review-gate` check run says about a pull request.

The Sweeper owns the merge gate: it publishes the `review-gate` check
run on the pull request's head, and a merge is gated on that check. So
what the gate says has to be a judgement about the pull request, and
never about the event that woke the sweeper up.

The gate is published from one question — WAS THIS HEAD JUDGED? — and
the answer is read from the review the bot itself posted for that
commit, never from the event. A run that did not complete, a run that
was skipped, a run that judged another commit, and a head no review
judged all leave the gate FAILED with a title that says which. A
comment may ask the question again; it cannot answer it.

The decision is asked in two steps, because the threads are only worth
counting once the head is known to have been judged: the first answer
needs no thread counts and says whether the gate may be published at
all, the second one adds the counts and says what it says. A gate that
is pending on REVIEW-UNAVAILABLE is left alone before the threads are
counted, so an outage of the review models keeps its own gate.

This module is the ONE place that decides, and it is unit-tested
(test_review_gate.py). No network and no GitHub calls: the answer is a
function of the facts the caller hands over.
"""

import argparse
import json
import sys
from typing import NamedTuple

# What the gate publishes. "success" and "failure" are the two
# verdicts; "none" means it publishes nothing at all, because a gate
# that is waiting for a review stays waiting; "more" means the answer
# needs the reviewer's threads before it can be given.
PUBLISH_SUCCESS = "success"
PUBLISH_FAILURE = "failure"
PUBLISH_NONE = "none"
PUBLISH_MORE = "more"

# The title the reviewer publishes when the review models were
# unavailable. A gate carrying it is waiting for a review, not a
# verdict, and it stays that way until a review judges the head.
PENDING_MARKER = "REVIEW-UNAVAILABLE"

# The reasons the decision reached, for the job log. A label, never a
# sentence: the log is read by people looking for the cause.
REASON_RUN_SKIPPED = "the review did not run"
REASON_RUN_NOT_COMPLETE = "the review did not complete"
REASON_RUN_JUDGED_ANOTHER = "the review judged another commit"
REASON_NOT_JUDGED = "no review judged this head"
REASON_PENDING = "the gate is pending on REVIEW-UNAVAILABLE"
REASON_NEEDS_THREADS = "the reviewer threads decide"
REASON_LIVE_FINDINGS = "there are live reviewer findings"
REASON_NO_FINDINGS = "there are no live reviewer findings"


class Decision(NamedTuple):
    """What the gate says, and whether outdated threads may be swept."""
    publish: str
    title: str
    summary: str
    sweep: bool
    reason: str


def leave_pending(pending_titles, judged):
    """True when a gate pending on REVIEW-UNAVAILABLE must be left alone.

    An outage of the review models is not a verdict. The gate the
    reviewer publishes for it stays pending until a review judges the
    head, and publishing a completed gate over it would replace "wait
    for a review" with a verdict nobody reached.
    """
    for title in pending_titles or []:
        if PENDING_MARKER in str(title or ""):
            return not judged
    return False


def decide(event, conclusion, run_head, head, judged, pending_titles,
           live=None, stale=None):
    """Decide what the gate says about a pull request.

    `event` is what woke the sweeper ("workflow_run" or
    "issue_comment"), `conclusion` is how the reviewer run ended (empty
    for a comment), and `run_head` is the commit that run reviewed
    (empty for a comment). `head` is the commit the pull request is on
    NOW, `judged` says whether a review the bot itself posted says
    that commit was reviewed, `pending_titles` are the titles of the
    gates still pending on this head, and `live` and `stale` are the
    reviewer's threads that are still open against it and the ones that
    went outdated. `live` of None means they have not been counted
    yet, and the answer is then only the one that does not need them.
    """
    titles = [str(title or "") for title in (pending_titles or [])]

    # For a workflow_run, the run's own verdict comes first: a run that
    # did not complete judged nothing, and it is reported before the
    # pending gate is consulted.
    if event == "workflow_run":
        if conclusion == "skipped":
            return Decision(
                PUBLISH_FAILURE,
                "The review did not run (skipped)",
                "The Reviewer workflow skipped this pull request, so no "
                "review ran and this commit was never judged. The gate "
                "fails closed until a maintainer reviews it by hand.",
                False, REASON_RUN_SKIPPED)
        if conclusion != "success":
            return Decision(
                PUBLISH_FAILURE,
                "The review did not complete (%s)" % conclusion,
                "The Reviewer workflow ended as `%s`, so this commit was "
                "never judged. Re-run the Reviewer or push a new commit."
                % conclusion,
                False, REASON_RUN_NOT_COMPLETE)
        # A run that succeeded on another commit says nothing about the
        # commit the pull request is on now. An empty run_head is treated
        # the same way: the run did not name the commit it judged, so it
        # cannot be trusted to have judged this one.
        if not run_head or run_head != head:
            return Decision(
                PUBLISH_FAILURE,
                "The review judged an earlier commit",
                "The last Reviewer run judged commit %s, not the current "
                "head %s, so this commit was never judged. Push a new "
                "commit or re-run the Reviewer." % (run_head or "unknown",
                                                    head),
                False, REASON_RUN_JUDGED_ANOTHER)

    # A gate that is waiting for a review stays waiting while nothing
    # judged the head. This is asked before the threads are counted, so
    # an outage keeps its own gate.
    if leave_pending(titles, judged):
        return Decision(PUBLISH_NONE,
                        "The review-gate is pending on %s" % PENDING_MARKER,
                        "No review judged this commit yet, so the gate stays "
                        "pending.",
                        False, REASON_PENDING)

    # For an issue_comment, the looked-up run's verdict applies now: a
    # comment is not a run and cannot vouch for one.
    if event == "issue_comment":
        if not conclusion:
            return Decision(
                PUBLISH_FAILURE,
                "The review did not judge this head",
                "No Reviewer run was found for this commit, so it was never "
                "judged. Push a new commit, or re-run the Reviewer for this "
                "head.",
                False, REASON_NOT_JUDGED)
        if conclusion == "skipped":
            return Decision(
                PUBLISH_FAILURE,
                "The review did not run (skipped)",
                "The Reviewer workflow skipped this pull request, so no "
                "review ran and this commit was never judged. The gate "
                "fails closed until a maintainer reviews it by hand.",
                False, REASON_RUN_SKIPPED)
        if conclusion != "success":
            return Decision(
                PUBLISH_FAILURE,
                "The review did not complete (%s)" % conclusion,
                "The Reviewer workflow ended as `%s`, so this commit was "
                "never judged. Re-run the Reviewer or push a new commit."
                % conclusion,
                False, REASON_RUN_NOT_COMPLETE)

    # Nothing judged this head: the gate fails closed. A comment is
    # not a review, so it cannot make this answer a different one.
    if not judged:
        return Decision(
            PUBLISH_FAILURE,
            "The review did not judge this head",
            "No review of the review bot judged this commit, so the gate "
            "fails closed. Push a new commit, or re-run the Reviewer for "
            "this head.",
            False, REASON_NOT_JUDGED)

    # The head was judged, so the reviewer's findings are the verdict.
    # Outdated threads may be swept only when the run that judged this
    # head is the one that finished: a comment is not a review, and a
    # run that judged nothing re-flagged nothing.
    sweep = event == "workflow_run" and conclusion == "success"

    # The verdict needs the reviewer's threads. Until they are counted,
    # the answer is that they decide.
    if live is None:
        return Decision(PUBLISH_MORE, "", "", sweep, REASON_NEEDS_THREADS)

    if live > 0:
        return Decision(
            PUBLISH_FAILURE,
            "%d unresolved reviewer finding(s)" % live,
            "The reviewer has %d finding(s) that are still open against "
            "the current diff. Address the code, or resolve the thread if "
            "the finding is wrong. The gate re-evaluates on the next "
            "review." % live,
            sweep, REASON_LIVE_FINDINGS)
    return Decision(
        PUBLISH_SUCCESS,
        "No unresolved reviewer findings",
        "Every reviewer finding on this PR is either resolved or outdated "
        "against the current diff.",
        sweep, REASON_NO_FINDINGS)


def _flag(value):
    """True when a flag carries the word true."""
    return str(value or "").strip().lower() in ("true", "1", "yes")


def _count(value):
    """A count, or nothing when it is not one."""
    try:
        return int(str(value).strip())
    except (TypeError, ValueError):
        return None


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", default="gate",
                        choices=["gate", "pre"],
                        help="the question to answer: the one that needs "
                             "the reviewer's threads, or the one that does "
                             "not")
    parser.add_argument("--event", default="",
                        help="what woke the sweeper up")
    parser.add_argument("--conclusion", default="",
                        help="how the reviewer run ended")
    parser.add_argument("--run-head", default="",
                        help="the commit the reviewer run reviewed")
    parser.add_argument("--head", default="",
                        help="the commit the pull request is on now")
    parser.add_argument("--judged", default="",
                        help="true when a review the bot posted says this "
                             "head was reviewed")
    parser.add_argument("--pending-title", action="append", default=[],
                        help="the title of a gate still pending on this "
                             "head (repeatable)")
    parser.add_argument("--live", default=None,
                        help="the reviewer's threads still open")
    parser.add_argument("--stale", default=None,
                        help="the reviewer's threads that went outdated")
    args = parser.parse_args(argv[1:])

    judged = _flag(args.judged)
    if args.mode == "pre":
        decision = decide(args.event, args.conclusion, args.run_head,
                          args.head, judged, args.pending_title)
    else:
        decision = decide(args.event, args.conclusion, args.run_head,
                          args.head, judged, args.pending_title,
                          _count(args.live), _count(args.stale))
    print(json.dumps({
        "publish": decision.publish,
        "title": decision.title,
        "summary": decision.summary,
        "sweep": decision.sweep,
        "reason": decision.reason,
    }))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

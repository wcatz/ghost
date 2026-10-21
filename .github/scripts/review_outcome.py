#!/usr/bin/env python3
"""Classify why a review run produced no result.

The Reviewer workflow has two paths to a review: a free path (a list of
anonymous opencode models, tried in turn) and a paid fallback (one
opencode-go model, sharing one usage window with every other paid model
on the account). When neither produced a result the workflow failed, and
the sweeper published a FAILING `review-gate` check run - so an outage of
the review models read exactly like a review that could not be trusted,
and the gate stayed red until somebody re-ran the job by hand.

This module is the ONE place that decides which of three outcomes a run
had:

  success     - a result was produced, on either path
  unavailable - no result, because the models were unavailable: an
                outage, not a verdict
  failure     - everything else, exactly as it behaved before

The input is what the two paths reported: the free path's status and its
per-candidate failure reasons, the paid step's status and reason, the
fatal flags the steps raise, and the validation errors each path
produced. The output is the outcome and a FIXED LABEL naming the cause,
which the workflow prints in the job log and puts in the title of the
pending review-gate.

TWO RULES MAKE THE ANSWER WORTH TRUSTING.

The reasons are the PROVIDER's own words - the opencode process's error
events and its stderr - and never the model's. A model can write
anything, including the provider's own phrases, so a reason a model wrote
is not evidence of an outage. The model's own document is read apart, and
only as the sign that a model ANSWERED.

And the CAUSE is a label, never a reason. The gate title is a place a
reader trusts, and it says which of two things happened, not what a
provider said about it.

No network, no filesystem, no GitHub calls: everything here is a function
of its arguments, so it is unit-tested off-CI (test_review_outcome.py).
"""

import argparse
import json
import re
import sys
from typing import NamedTuple

OUTCOME_SUCCESS = "success"
OUTCOME_UNAVAILABLE = "unavailable"
OUTCOME_FAILURE = "failure"

# The causes are FIXED LABELS. A reason is provider output, and the gate
# title is a place a reader trusts: the title says which of two things
# happened, never what a provider said about it.
CAUSE_PAID_USAGE_LIMIT = "paid fallback usage limit"
CAUSE_FREE_UNREACHABLE = "free models unreachable"
CAUSE_MODEL_ANSWERED = "a model answered with a document nobody can use"
CAUSE_MODEL_ACTED = "the model acted on the pull request"
CAUSE_AGENT_MISSING = "the reviewer agent did not register"
CAUSE_FREE_STEP_FATAL = "the free review step could not run"
CAUSE_NO_RESULT = "no valid free or paid review result was produced"
CAUSE_FREE_RESULT = "a review result was produced by the free path"
CAUSE_PAID_RESULT = "a review result was produced by the paid fallback"

# The cause is carried in a check run title (255 characters) and in a job
# log line, so it is one bounded line.
MAX_CAUSE = 200

# The signs that a MODEL ANSWERED: a validation failure, an unusable
# findings document, a verdict word. A model that answered has produced a
# verdict, whatever the transport did on the way, and a verdict is not an
# outage - so these force a failure on BOTH paths, before the
# usage-limit and transport branches are asked. They are checked against
# every text the run reported, so a reason that quotes a model's own
# words cannot smuggle an outage past them.
_ANSWERED_MARKERS = (
    "validation failed",
    "reply validation failed",
    "unparseable",
    "could not parse",
    "invalid findings",
    "unusable findings",
    "no valid findings",
    "findings document",
    "verdict",
    "severity must be",
    "summary must be",
    "no json object",
)

# The provider's own words for an outage, and nothing looser: a phrase a
# model could write belongs in _ANSWERED_MARKERS or nowhere, because a
# reason a model wrote is not evidence of an outage.
_OUTAGE_PATTERNS = (
    # The paid fallback's shared usage window, in the provider's words.
    "go usage limit exceeded",
    # The free tier's rate limit, in the provider's words.
    "rate limit exceeded",
    "too many requests",
    "http 429",
    # A dropped or reset connection, in the provider's words.
    "the socket connection was closed unexpectedly",
    "socket hang up",
    "connection reset by peer",
    "connection refused",
    "connection closed",
    "broken pipe",
    "econnreset",
    "econnrefused",
    "econnaborted",
    "etimedout",
)


class Decision(NamedTuple):
    """The outcome of a review run and the label naming its cause."""
    outcome: str
    cause: str


def one_line(text, limit=MAX_CAUSE):
    """Collapse text to one bounded line."""
    collapsed = " ".join(str(text or "").split())
    if len(collapsed) > limit:
        collapsed = collapsed[:limit].rstrip() + "..."
    return collapsed


def parse_failures(text):
    """Read the `<model>: <reason>` lines the free path writes.

    The model name is only ever reported beside its reason, so a line
    that carries no name is still a reason.
    """
    failures = []
    for line in str(text or "").splitlines():
        line = line.strip()
        if not line:
            continue
        model, separator, reason = line.partition(": ")
        if not separator:
            model, reason = "", line
        failures.append((model, reason))
    return failures


def model_answered(text):
    """True when the text shows a MODEL answered.

    A validation failure, an unusable findings document or a verdict word
    all mean the same thing: the model produced something, and what it
    produced has to be judged rather than waited for.
    """
    lowered = str(text or "").lower()
    if not lowered:
        return False
    return any(marker in lowered for marker in _ANSWERED_MARKERS)


def is_outage(reason):
    """True when the reason names an outage of the model service."""
    text = str(reason or "").lower()
    if not text:
        return False
    if model_answered(text):
        return False
    return any(pattern in text for pattern in _OUTAGE_PATTERNS)


def is_usage_limit(reason):
    """True when the paid fallback failed on the shared usage window.

    The paid model is an opencode-go one, so its usage limit is the
    account's Go window: the one window every paid model shares, which
    comes back by itself later. The provider names it exactly, and the
    phrase is matched exactly - a looser match would be a phrase a model
    could write.
    """
    return "go usage limit exceeded" in str(reason or "").lower()


def decide(free_status, free_failures, paid_status, paid_reason,
           free_validation="", paid_validation="", model_acted=False,
           select_agent_fatal=False, free_agent_fatal=False,
           paid_agent_fatal=False):
    """Classify a review run.

    `free_failures` is a list of (model, reason) pairs: every free
    candidate that failed, with the provider's reason. `free_status` and
    `paid_status` are the two steps' own statuses, and `paid_reason` is
    the provider's reason when the paid step failed. The two validation
    texts are the MODEL's own document errors, and they are read only as
    the sign that a model answered. `model_acted` says the model did
    something - it produced output, or it tried a tool - which is
    reported by the workflow from the event stream and the process's own
    stderr.
    """
    free_status = str(free_status or "").strip()
    paid_status = str(paid_status or "").strip()
    paid_reason = str(paid_reason or "")
    free_validation = str(free_validation or "")
    paid_validation = str(paid_validation or "")
    free_failures = list(free_failures or [])

    if free_status == "success":
        return Decision(OUTCOME_SUCCESS, CAUSE_FREE_RESULT)
    if paid_status == "success":
        return Decision(OUTCOME_SUCCESS, CAUSE_PAID_RESULT)

    # The model ACTED, or ANSWERED: it produced something, so the run
    # has a verdict to judge rather than an outage to wait for. This is
    # asked first, on both paths, because a model can write anything,
    # including the provider's own words - and because the process's
    # stderr carries a rejected tool call, whose command is the model's
    # own text.
    if model_acted:
        return Decision(OUTCOME_FAILURE, CAUSE_MODEL_ACTED)
    if free_validation.strip() or paid_validation.strip():
        # Any validation text at all means the model produced a document
        # the gate cannot use, whatever the text says.
        return Decision(OUTCOME_FAILURE, CAUSE_MODEL_ANSWERED)
    # A reason that carries a model's words forces a failure too: the
    # marker list fails closed on purpose, because a phrase the model
    # chose is not evidence of an outage.
    reported = [paid_reason] + [reason for _model, reason in free_failures]
    if any(model_answered(text) for text in reported):
        return Decision(OUTCOME_FAILURE, CAUSE_MODEL_ANSWERED)

    if select_agent_fatal or free_agent_fatal or paid_agent_fatal:
        return Decision(OUTCOME_FAILURE, CAUSE_AGENT_MISSING)

    # The paid fallback's usage window is spent. That is an outage
    # whatever the free path did.
    if paid_status != "success" and is_usage_limit(paid_reason):
        return Decision(OUTCOME_UNAVAILABLE, CAUSE_PAID_USAGE_LIMIT)

    # A free step that could not RUN at all - a missing trusted script, a
    # credential in the model step, no workspace - is a failure of the
    # pipeline rather than of the models.
    if free_status == "fatal":
        return Decision(OUTCOME_FAILURE, CAUSE_FREE_STEP_FATAL)

    # Every free candidate failed on a rate limit or a dropped connection,
    # and the paid step produced no result either.
    if (paid_status != "success" and free_failures
            and all(is_outage(reason) for _model, reason in free_failures)):
        return Decision(OUTCOME_UNAVAILABLE, CAUSE_FREE_UNREACHABLE)

    return Decision(OUTCOME_FAILURE, CAUSE_NO_RESULT)


def _read(path):
    """Read a file, or nothing when it is absent."""
    if not path:
        return ""
    try:
        with open(path, encoding="utf-8", errors="replace") as handle:
            return handle.read()
    except OSError:
        return ""


def _flag(value):
    """True when a workflow flag carries the word true."""
    return str(value or "").strip().lower() in ("true", "1", "yes")


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--free-status", default="",
                        help="the free step's status")
    parser.add_argument("--paid-status", default="",
                        help="the paid step's status")
    parser.add_argument("--free-failures", action="append", default=[],
                        help="a file of `<model>: <reason>` lines, the "
                             "provider's reasons only (repeatable)")
    parser.add_argument("--paid-reason", default="",
                        help="a file holding the paid step's reason, the "
                             "provider's words only")
    parser.add_argument("--free-validation", default="",
                        help="a file holding the free step's validation "
                             "error: the model's own document, read only as "
                             "the sign that a model answered")
    parser.add_argument("--paid-validation", default="",
                        help="a file holding the paid step's validation "
                             "error")
    parser.add_argument("--model-acted", default="",
                        help="true when the model produced output or tried "
                             "a tool: a reason may classify an outage only "
                             "when the model produced nothing")
    parser.add_argument("--select-agent-fatal", default="")
    parser.add_argument("--free-agent-fatal", default="")
    parser.add_argument("--paid-agent-fatal", default="")
    args = parser.parse_args(argv[1:])

    failures = []
    for path in args.free_failures:
        failures.extend(parse_failures(_read(path)))

    decision = decide(args.free_status, failures, args.paid_status,
                      _read(args.paid_reason),
                      free_validation=_read(args.free_validation),
                      paid_validation=_read(args.paid_validation),
                      model_acted=_flag(args.model_acted),
                      select_agent_fatal=_flag(args.select_agent_fatal),
                      free_agent_fatal=_flag(args.free_agent_fatal),
                      paid_agent_fatal=_flag(args.paid_agent_fatal))
    print(json.dumps({"outcome": decision.outcome, "cause": decision.cause}))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

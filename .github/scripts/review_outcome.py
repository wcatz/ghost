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
per-candidate failure reasons, the paid step's status and reason, and the
fatal flags the steps raise. The output is the outcome and the cause,
which the workflow prints in the job log and puts in the title of the
pending review-gate.

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

# The cause is carried in a check run title (255 characters) and in a job
# log line, and it can quote a provider's own error text, so it is one
# bounded line.
MAX_CAUSE = 200

# The free path's reasons that name an OUTAGE of the model service: a
# rate limit, or a connection the provider dropped. The paid fallback's
# reason for the same thing is its own usage window (is_usage_limit).
_OUTAGE_WORDS = (
    "rate limit", "ratelimit", "rate_limit", "too many requests",
    "socket", "econnreset", "econnrefused", "econnaborted", "etimedout",
    "connection reset", "connection refused", "connection closed",
    "connection aborted", "connection error", "broken pipe",
    "transport", "network error", "dns",
    "timed out", "timeout", "deadline exceeded",
    "temporarily unavailable", "service unavailable", "overloaded",
    "bad gateway", "gateway timeout", "internal server error",
    "upstream",
)

# The HTTP status codes that mean the provider refused the call rather
# than the model answering badly. Matched as whole numbers so a byte
# count that happens to contain one is not read as a status code.
_OUTAGE_CODES = re.compile(r"\b(?:429|500|502|503|504)\b")

# The reasons that name a failure of the REVIEW rather than an outage of
# the models. They win over the outage words: a reason carrying one of
# them is a failure even when it also mentions a transport error, because
# a model that answered with a document nobody can use produced a
# verdict the reviewer cannot trust.
_VERDICT_WORDS = (
    "invalid findings", "unusable findings", "unparseable", "could not parse",
    "validation failed", "reply validation failed", "findings document",
    "agent not found", "did not register",
    "no such file", "enoent", "base-scripts",
)


class Decision(NamedTuple):
    """The outcome of a review run and the reason for it."""
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


def is_outage(reason):
    """True when the reason names an outage of the model service."""
    text = str(reason or "").lower()
    if not text:
        return False
    if any(word in text for word in _VERDICT_WORDS):
        return False
    if any(word in text for word in _OUTAGE_WORDS):
        return True
    return bool(_OUTAGE_CODES.search(text))


def is_usage_limit(reason):
    """True when the paid fallback failed on the shared usage window.

    The paid model is an opencode-go one, so its usage limit is the
    account's Go window: the one window every paid model shares, which
    comes back by itself later.
    """
    return "usage limit" in str(reason or "").lower()


def _failure_cause(free_failures, paid_reason):
    """Name what the failure was, as far as the reasons name it."""
    cause = "no valid free or paid review result was produced"
    for _model, reason in free_failures:
        if any(word in str(reason or "").lower() for word in _VERDICT_WORDS):
            return one_line(cause + " (a free candidate answered with an "
                                "unusable findings document)")
    if paid_reason:
        return one_line(cause + " (the paid fallback failed: "
                        + one_line(paid_reason, 120) + ")")
    return cause


def decide(free_status, free_failures, paid_status, paid_reason,
           select_agent_fatal=False, free_agent_fatal=False,
           paid_agent_fatal=False):
    """Classify a review run.

    `free_failures` is a list of (model, reason) pairs: every free
    candidate that failed, with the reason it failed. `free_status` and
    `paid_status` are the two steps' own statuses, and `paid_reason` is
    what the paid step said when it failed.
    """
    free_status = str(free_status or "").strip()
    paid_status = str(paid_status or "").strip()
    paid_reason = str(paid_reason or "")
    free_failures = list(free_failures or [])

    if free_status == "success":
        return Decision(OUTCOME_SUCCESS, "a review result was produced by the free path")
    if paid_status == "success":
        return Decision(OUTCOME_SUCCESS, "a review result was produced by the paid fallback")

    if select_agent_fatal or free_agent_fatal or paid_agent_fatal:
        return Decision(OUTCOME_FAILURE,
                        "the reviewer agent did not register, so no review "
                        "was run with the read-only tool set")

    # The paid fallback's usage window is spent. That is an outage
    # whatever the free path did.
    if paid_status != "success" and is_usage_limit(paid_reason):
        return Decision(OUTCOME_UNAVAILABLE,
                        "the paid fallback failed: " + one_line(paid_reason))

    # A free step that could not RUN at all - a missing trusted script, a
    # credential in the model step, no workspace - is a failure of the
    # pipeline rather than of the models.
    if free_status == "fatal":
        return Decision(OUTCOME_FAILURE, "the free review step could not run")

    # Every free candidate failed on a rate limit or a dropped connection,
    # and the paid step produced no result either.
    if (paid_status != "success" and free_failures
            and all(is_outage(reason) for _model, reason in free_failures)):
        return Decision(
            OUTCOME_UNAVAILABLE,
            "every free candidate failed on a rate limit or a dropped "
            "connection")

    return Decision(OUTCOME_FAILURE, _failure_cause(free_failures, paid_reason))


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
                        help="a file of `<model>: <reason>` lines "
                             "(repeatable)")
    parser.add_argument("--paid-reason", default="",
                        help="a file holding the paid step's reason")
    parser.add_argument("--select-agent-fatal", default="")
    parser.add_argument("--free-agent-fatal", default="")
    parser.add_argument("--paid-agent-fatal", default="")
    args = parser.parse_args(argv[1:])

    failures = []
    for path in args.free_failures:
        failures.extend(parse_failures(_read(path)))

    decision = decide(args.free_status, failures, args.paid_status,
                      _read(args.paid_reason),
                      select_agent_fatal=_flag(args.select_agent_fatal),
                      free_agent_fatal=_flag(args.free_agent_fatal),
                      paid_agent_fatal=_flag(args.paid_agent_fatal))
    print(json.dumps({"outcome": decision.outcome, "cause": decision.cause}))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

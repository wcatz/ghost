#!/usr/bin/env python3
"""Decide whether the review model ACTED during a review run.

The Reviewer workflow has to tell two failures apart: the review models
were UNAVAILABLE - an outage, and the review-gate is left pending - and
the model did something of its own, which is a verdict nobody can use
and a gate that fails.

The workflow asked that question in three places: the model selection
step, the free review loop and the paid fallback. Each place carried its
own copy of the answer - a jq test over the run's event stream and a
grep of the run's stderr - and nothing kept the copies in step. The
copies drifted: the selection step read only the stderr, so a model that
produced something in its event stream was read as one that produced
nothing. The pattern was broad enough to match a bare word such as
"permission" or "bash", so an outage whose stderr carried a stack trace
was read as a model that acted, and the gate failed instead of staying
pending. And no test could reach any of it: only the outcome helper's
`decide` was tested, with the answer passed in.

This module is the ONE place that answers the question, so the three
steps ask the same thing and the answer is unit-tested
(test_model_acted.py).

THE ANSWER IS TRUE WHEN THE MODEL PRODUCED ANYTHING OF ITS OWN.

The event stream is the primary evidence. An event that is not an error
event is the model's own output - a text part, a tool call, a step - and
a stream that does not parse counts as produced too, so the rule fails
closed: a stream nobody can read is a stream nobody can say was empty.

The stderr is the secondary evidence, and it is read narrowly. opencode
echoes a refused tool call there, and the command it names is the
model's own text, so a refusal is evidence that the model acted. But the
stderr is also where an outage prints a stack trace, and a bare word
turns up in one often enough that a genuine outage would be read as a
model that acted. The phrases below are the refusals themselves, each of
them more than one word, so an outage that merely MENTIONS a permission
or a shell is not mistaken for a refusal. The one phrase that is not a
refusal wording - "not permitted" - errs the other way: it can only
force ACTED, so it fails closed.

A file that cannot be READ is not nothing either. The workflow
pre-creates the empty files, so an absent file and an empty file are
the run's own silence; a file that exists and cannot be opened (a
directory, a file nobody can read) is an unknown, and an unknown is
acted.

A TIMEOUT IS AN OUTAGE, NOT A VERDICT. `timeout --foreground` exits 124
when it killed the run, and opencode writes the same shutdown shape as
it begins to shut down: an aborted error, a tool call it declined, and
the auto-reject notice on the stderr. A run killed while the provider
was still thinking leaves that shape without the model having produced
anything, so on a timeout the declined tool call and the stderr
refusal notice are NOT evidence. A final event line the timeout cut
short - no newline, and not valid JSON - is an interrupted write rather
than an event. Output that arrived before the timeout still counts:
text, reasoning, a completed tool call.

The answer is not enough to check a failing run against. The command
prints one structured line on its own stderr naming the evidence that
decided it - the event type, the refusal phrase, whether the run timed
out, and a file that could not be read - so the workflow can keep it in
the job log instead of discarding it. The line carries labels and event
types, never the provider's reason text or the model's document.

No network and no GitHub calls: the answer is a function of the two
texts it is given and the exit status.
"""

import argparse
import json
import re
import sys

# The refusals opencode prints when the MODEL's own tool attempt was
# turned down. Each one is a PHRASE and never a bare word: "permission"
# and "bash" turn up in a provider's stack trace, and an outage that
# mentions either must not be read as a model that acted.
_STDERR_REFUSALS = (
    "permission requested",        # opencode's ask notice
    "permission denied for tool",  # the refusal, naming the tool
    "rejected permission",         # the same refusal, the other word order
    "auto-reject",                 # "auto-rejecting", "auto-rejected"
    "auto rejected",               # "auto rejected"
    "tool call",                   # "The user declined this tool call"
    "tool_use",                    # the event type, in the process's words
    "tool attempt",
    # Not a refusal wording, and the one phrase that errs the wrong way
    # on purpose: it can only force ACTED, so a stack trace that
    # mentions it fails the gate rather than leaving it pending.
    "not permitted",
)

# What the marker file says when the model produced something of its own.
# The workflow reads the marker, and writes "unknown" beside it when the
# rule could not be consulted at all.
MARKER_ACTED = "acted"


class _Unreadable:
    """The marker for a file that exists and could not be READ.

    It is a type of its own rather than a string, so no text a file
    holds can be mistaken for it.
    """

    def __repr__(self):
        return "<unreadable>"


# The answer for a file nobody could open. A run whose stream or stderr
# cannot be read is an unknown, and an unknown is ACTED: the rule fails
# closed rather than reading an unreadable run as one that produced
# nothing.
UNREADABLE = _Unreadable()


# Events opencode writes around the model's work without carrying any of
# it: a step opens before the first token and closes after the last. A
# run killed by its own timeout while the provider is still thinking
# leaves only these behind, and they name nothing the model produced.
# Text, reasoning and tool events are the model's own and stay evidence,
# and so does any event type this list does not name.
_LIFECYCLE_EVENTS = ("step_start", "step_finish")

# The word opencode puts in a declined tool call's state. The same shape
# appears when a run shuts down, which is why the exit status decides
# whether it is the model's attempt or the shutdown's own artifact.
_DECLINED = "declined"

# The characters a genuine event type is made of. Anything else is not
# a type name, and is not logged.
_TYPE_RE = re.compile(r"[A-Za-z0-9._-]{1,40}")

# `timeout --foreground` exits 124 when it killed the run. The workflow
# passes the status through, because the status is the one fact that
# tells a real declined tool call apart from the shutdown shape.
TIMEOUT_STATUS = 124


def _visible(value):
    """One short printable form of a value, safe in a log line."""
    text = "".join(ch if ch.isprintable() else "?" for ch in str(value))
    if len(text) > 60:
        text = text[:57] + "..."
    return text


def _is_declined_tool(event):
    """True when the event is a tool call the session declined."""
    part = event.get("part")
    if not isinstance(part, dict) or part.get("type") != "tool":
        return False
    state = part.get("state")
    if not isinstance(state, dict) or state.get("status") != "error":
        return False
    return _DECLINED in str(state.get("error") or "").lower()


def _event_decision(line, timed_out):
    """The evidence one line is, or None when it carries nothing.

    Step bookkeeping and error events are never the model's own output.
    On a timeout the declined tool call is the shutdown shape rather
    than the model's attempt, so it is not evidence either.
    """
    try:
        event = json.loads(line)
    except ValueError:
        return "an event line did not parse"
    if not isinstance(event, dict):
        return "an event line was not an object"
    kind = event.get("type")
    if kind in ("error",) + _LIFECYCLE_EVENTS:
        return None
    if timed_out and _is_declined_tool(event):
        return None
    # The event type is the only provider/model-supplied value the
    # report carries, so it is reduced to a type-shaped token: anything
    # that is not a type name becomes an unnamed type, and no model text
    # reaches the job log.
    if isinstance(kind, str) and _TYPE_RE.fullmatch(kind):
        return "event %s" % kind
    return "an event whose type is not a type name"


def _stream_decision(text, timed_out):
    """(acted, evidence) for the event stream."""
    if text is UNREADABLE:
        return True, "the event stream could not be read"
    raw = str(text or "")
    if not raw.strip():
        return False, "the event stream was empty"
    lines = raw.split("\n")
    # A final line with no newline may be a write the timeout cut short.
    trailing = "" if raw.endswith("\n") else lines.pop()
    for line in lines:
        if not line.strip():
            continue
        reason = _event_decision(line, timed_out)
        if reason is not None:
            return True, reason
    if trailing.strip():
        try:
            json.loads(trailing)
        except ValueError:
            if timed_out:
                return False, ("the timeout cut the final event line short, "
                               "so it is not a complete event")
            return True, "the final event line did not parse"
        reason = _event_decision(trailing, timed_out)
        if reason is not None:
            return True, reason
    if timed_out:
        return False, ("the shutdown shape a timeout leaves is not the "
                       "model acting")
    return False, "no event carried model output"


def _stderr_decision(text, timed_out):
    """(acted, evidence) for the process's own stderr."""
    if text is UNREADABLE:
        return True, "the stderr could not be read"
    lowered = str(text or "").lower()
    if not lowered:
        return False, "the stderr was empty"
    if timed_out:
        # The auto-reject notice is part of the shutdown shape, so it is
        # not the model reaching for a tool.
        return False, ("the refusal notice opencode writes as it shuts "
                       "down is not the model acting")
    for phrase in _STDERR_REFUSALS:
        if phrase in lowered:
            return True, "stderr refusal phrase %s" % _visible(phrase)
    return False, "the stderr named no refusal"


def stream_acted(text, timed_out=False):
    """True when the event stream holds anything the model produced."""
    return _stream_decision(text, timed_out)[0]


def stderr_acted(text, timed_out=False):
    """True when the stderr names a refusal of the model's own tool."""
    return _stderr_decision(text, timed_out)[0]


def assess(events_text, stderr_text, timed_out=False):
    """The answer and the evidence that decided it."""
    stream, stream_evidence = _stream_decision(events_text, timed_out)
    errors, stderr_evidence = _stderr_decision(stderr_text, timed_out)
    return stream or errors, {"stream": stream_evidence,
                              "stderr": stderr_evidence}


def acted(events_text, stderr_text, timed_out=False):
    """True when the model produced anything of its own."""
    return assess(events_text, stderr_text, timed_out)[0]


def _read(path):
    """Read a file, or nothing when it is absent.

    An ABSENT file is nothing: the workflow pre-creates the empty files,
    and a run that wrote nothing wrote nothing. A file that exists and
    cannot be READ is UNREADABLE, not nothing: an unreadable run is an
    unknown, and the rule fails closed on an unknown.
    """
    if not path:
        return ""
    try:
        with open(path, encoding="utf-8", errors="replace") as handle:
            return handle.read()
    except FileNotFoundError:
        return ""
    except OSError:
        return UNREADABLE


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--events", default="",
                        help="the opencode event stream of the run")
    parser.add_argument("--stderr", default="",
                        help="the opencode process's own stderr")
    parser.add_argument("--marker", default="",
                        help="the file the answer is appended to, when "
                             "the model produced something of its own")
    parser.add_argument("--exit-status", default="",
                        help="the opencode process's exit status; "
                             "%d means its own timeout killed it"
                             % TIMEOUT_STATUS)
    args = parser.parse_args(argv[1:])

    try:
        exit_status = int(args.exit_status)
    except (TypeError, ValueError):
        exit_status = None
    timed_out = exit_status == TIMEOUT_STATUS

    decision, evidence = assess(_read(args.events), _read(args.stderr),
                                timed_out)
    report = {"acted": decision, "timed_out": timed_out,
              "exit_status": exit_status}
    report.update(evidence)
    # One line, prefixed so it can never begin a workflow command, and
    # JSON-encoded so a line break in an event type cannot start one.
    print("model-acted: " + json.dumps(report, sort_keys=True),
          file=sys.stderr)

    if decision:
        if args.marker:
            with open(args.marker, "a", encoding="utf-8") as handle:
                handle.write(MARKER_ACTED + "\n")
        print("true")
    else:
        print("false")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

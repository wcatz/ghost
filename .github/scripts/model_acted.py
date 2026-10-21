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

No network and no GitHub calls: the answer is a function of the two
texts it is given.
"""

import argparse
import json
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


def stream_acted(text):
    """True when the event stream holds anything the model produced.

    An event that is not an error event is the model's own output, and a
    stream that does not parse counts as produced: the rule fails
    closed. A stream that could not be READ counts as produced too.
    """
    if text is UNREADABLE:
        return True
    lines = [line for line in str(text or "").splitlines() if line.strip()]
    if not lines:
        return False
    for line in lines:
        try:
            event = json.loads(line)
        except ValueError:
            return True
        if not isinstance(event, dict) or event.get("type") != "error":
            return True
    return False


def stderr_acted(text):
    """True when the stderr names a refusal of the model's own tool.

    A stderr that could not be READ counts as a refusal: it can only
    force ACTED.
    """
    if text is UNREADABLE:
        return True
    lowered = str(text or "").lower()
    if not lowered:
        return False
    return any(phrase in lowered for phrase in _STDERR_REFUSALS)


def acted(events_text, stderr_text):
    """True when the model produced anything of its own."""
    return stream_acted(events_text) or stderr_acted(stderr_text)


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
    args = parser.parse_args(argv[1:])

    if acted(_read(args.events), _read(args.stderr)):
        if args.marker:
            with open(args.marker, "a", encoding="utf-8") as handle:
                handle.write(MARKER_ACTED + "\n")
        print("true")
    else:
        print("false")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

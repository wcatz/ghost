#!/usr/bin/env python3
"""Read the model's raw reply + pr.diff, build a review payload, POST it.

Exits non-zero on a malformed document — a bad review is never posted in
degraded form. Usage:

    post_review.py <repo> <pr_number> <commit_id> <reply.txt> <pr.diff>
"""

import json
import re
import subprocess
import sys

from review_findings import (MARKER, ValidationError, build_review,
                             extract_findings_from_reply, parse_hunks,
                             validate)

MAX_COMMENTS = 30
MAX_BODY_CHARS = 60000
# Excerpt budget for a failed reply. An opencode event stream starts with
# tool noise, so the head alone never shows the model's answer; the tail is
# where a truncated or off-contract answer actually is.
EXCERPT_HEAD = 1000
EXCERPT_TAIL = 1500
EXCERPT_WHOLE = 2000


def _log_safe(text, limit=None, multiline=False):
    """Neutralise model-derived text before it reaches the Actions log.

    Actions interprets ::command:: at the start of a line, so a newline
    inside model text lets it forge annotations (::error::), mask
    arbitrary later output (::add-mask::), or suppress the job's own
    commands entirely (::stop-commands::). Verified reachable from a
    VALID findings document via a finding title.
    """
    if multiline:
        # The excerpt is legitimately multi-line, so instead of collapsing
        # it we prefix every line with '> ' so no line can begin with '::'.
        # Truncate first so the cap applies to the excerpt, not the
        # already-prefixed (longer) text.
        if limit is not None:
            text = text[:limit]
        text = text.replace("\r\n", "\n").replace("\r", "\n")
        return "\n".join(f"> {line}" for line in text.split("\n"))

    # Single-line use: collapse every whitespace run (including \n and \r)
    # to a single space so the result can never contain a line break.
    text = re.sub(r"\s+", " ", text).strip()
    if limit is not None:
        text = text[:limit]
    return text


def main(argv):
    if len(argv) != 6:
        print(__doc__, file=sys.stderr)
        return 2
    repo, number, commit_id, reply_path, diff_path = argv[1:]

    with open(reply_path, encoding="utf-8", errors="replace") as fh:
        reply = fh.read()

    try:
        doc = extract_findings_from_reply(reply)
        validate(doc)
    except ValidationError as exc:
        print(f"::error::unusable model reply: {exc}", file=sys.stderr)
        # Surface a bounded excerpt so the failure is diagnosable from the
        # Actions log without re-running the model. Prefix every line so a
        # newline in the model's reply cannot forge or suppress commands.
        # A reply short enough to show whole is shown whole, unchanged.
        if len(reply) <= EXCERPT_WHOLE:
            excerpt = _log_safe(reply, limit=EXCERPT_WHOLE, multiline=True)
            print(f"::group::model reply (first {EXCERPT_WHOLE} chars)\n"
                  f"{excerpt}\n::endgroup::", file=sys.stderr)
        else:
            # Slice before _log_safe: its own `limit` truncates from the
            # head, so the tail has to be cut here to be a tail at all.
            head = _log_safe(reply[:EXCERPT_HEAD], multiline=True)
            tail = _log_safe(reply[-EXCERPT_TAIL:], multiline=True)
            # The separators are '> '-prefixed like the excerpt itself so
            # they can never be read as workflow commands either.
            print(f"::group::model reply ({len(reply)} chars: first "
                  f"{EXCERPT_HEAD} and last {EXCERPT_TAIL})\n"
                  f"> --- first {EXCERPT_HEAD} chars ---\n{head}\n"
                  f"> --- last {EXCERPT_TAIL} chars ---\n{tail}\n"
                  f"::endgroup::", file=sys.stderr)
        return 1

    # Same decoding policy as the reply above: a pr.diff containing invalid
    # UTF-8 must not crash with a raw traceback.
    with open(diff_path, encoding="utf-8", errors="replace") as fh:
        hunks = parse_hunks(fh.read())

    payload, dropped = build_review(doc, commit_id, hunks)
    for f in dropped:
        print(f"::warning::finding not anchorable to the diff "
              f"({_log_safe(f['file'])}:{f['line']}): {_log_safe(f['title'])}")

    # build_review is pure and untouched; bound the payload here so a model
    # emitting hundreds of findings or a huge body degrades gracefully
    # instead of the whole review being rejected by GitHub (413/422).
    #
    # build_review always appends the <!-- ghost-review:<sha> --> marker as
    # the body's last line — review_findings.py's whole '<!--' validation
    # rule set exists to protect that marker from being spoofed, and a
    # consumer may rely on it to know a commit was already reviewed. Strip
    # it here and re-append it after every mutation below so neither the
    # comment-cap note nor body truncation can push it off the end or cut
    # through it.
    marker = MARKER.format(sha=commit_id)
    body = payload["body"]
    if body.endswith(marker):
        body = body[:-len(marker)].rstrip("\n")

    if len(payload["comments"]) > MAX_COMMENTS:
        omitted = len(payload["comments"]) - MAX_COMMENTS
        payload["comments"] = payload["comments"][:MAX_COMMENTS]
        body += (f"\n\n_{omitted} inline comment(s) omitted — the "
                 f"{MAX_COMMENTS}-comment cap was hit._")
        print(_log_safe(f"::warning::comment cap hit: {omitted} inline "
                         f"comment(s) omitted (cap {MAX_COMMENTS})"))

    suffix = "\n\n_[review body truncated]_"
    # Reserve room for the trailing marker so it always survives truncation.
    budget = MAX_BODY_CHARS - len("\n\n" + marker)
    if len(body) > budget:
        body = body[:budget - len(suffix)] + suffix
        print(_log_safe("::warning::review body truncated to "
                         f"{MAX_BODY_CHARS} chars"))

    payload["body"] = body + "\n\n" + marker

    proc = _post(repo, number, payload)
    if proc.returncode != 0 and payload["comments"]:
        # GitHub rejects the WHOLE review (422) when any one inline anchor
        # is refused, e.g. "line must be part of the diff" after the head
        # moved under a long-running review. Losing the review entirely
        # leaves the gate red with nothing to act on, so fall back once:
        # fold every inline finding into the body and post without anchors.
        print("::warning::inline review rejected "
              f"({_log_safe(_api_error(proc), limit=300)}); "
              "retrying with findings in the review body")
        payload = _fold_comments_into_body(payload, marker)
        proc = _post(repo, number, payload)
    if proc.returncode != 0:
        # GitHub's Reviews API can return a 422 whose body echoes back
        # field-level validation errors, and every field in our payload is
        # model-derived — so hostile content can round-trip through the
        # API and reach this line, same class as the other two sites, with
        # GitHub as the courier. This is also the line that reports a
        # genuine posting failure, so it is neutralised to a single line
        # rather than dropped, and bounded so a pathological response
        # can't flood the log.
        print(f"::error::posting the review failed: "
              f"{_log_safe(_api_error(proc), limit=2000)}",
              file=sys.stderr)
        return 1

    print(f"posted review: {len(payload['comments'])} inline finding(s), "
          f"{len(dropped)} unanchored")
    return 0


def _post(repo, number, payload):
    return subprocess.run(
        ["gh", "api", f"repos/{repo}/pulls/{number}/reviews",
         "--method", "POST", "--input", "-"],
        input=json.dumps(payload), text=True, capture_output=True, check=False,
    )


def _api_error(proc):
    """The real failure reason. `gh api` prints only "gh: Unprocessable
    Entity (HTTP 422)" on stderr; GitHub's validation errors (which field
    was refused, and why) are the JSON body it prints on stdout."""
    parts = (getattr(proc, "stderr", ""), getattr(proc, "stdout", ""))
    return " ".join(x.strip() for x in parts if isinstance(x, str) and x.strip())


def _fold_comments_into_body(payload, marker):
    """Move inline comments into the review body, keeping the trailing
    <!-- ghost-review:<sha> --> marker as the body's last line."""
    body = payload["body"]
    if body.endswith(marker):
        body = body[:-len(marker)].rstrip("\n")
    lines = ["", "**Inline findings** (posted in the body because GitHub "
             "rejected their line anchors):"]
    for c in payload["comments"]:
        lines.append(f"- `{c.get('path')}:{c.get('line')}` {c.get('body', '')}")
    body = body + "\n" + "\n".join(lines)
    budget = MAX_BODY_CHARS - len("\n\n" + marker)
    if len(body) > budget:
        body = body[:budget - 30] + "\n\n_[review body truncated]_"
    out = dict(payload)
    out["comments"] = []
    out["body"] = body + "\n\n" + marker
    return out


if __name__ == "__main__":
    sys.exit(main(sys.argv))

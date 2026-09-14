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
                             extract_findings, parse_hunks, validate)

MAX_COMMENTS = 30
MAX_BODY_CHARS = 60000


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
        doc = extract_findings(reply)
        validate(doc)
    except ValidationError as exc:
        print(f"::error::unusable model reply: {exc}", file=sys.stderr)
        # Surface a bounded excerpt so the failure is diagnosable from the
        # Actions log without re-running the model. Prefix every line so a
        # newline in the model's reply cannot forge or suppress commands.
        excerpt = _log_safe(reply, limit=2000, multiline=True)
        print(f"::group::model reply (first 2000 chars)\n{excerpt}\n::endgroup::",
              file=sys.stderr)
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

    proc = subprocess.run(
        ["gh", "api", f"repos/{repo}/pulls/{number}/reviews",
         "--method", "POST", "--input", "-"],
        input=json.dumps(payload), text=True, capture_output=True, check=False,
    )
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
              f"{_log_safe(proc.stderr.strip(), limit=2000)}",
              file=sys.stderr)
        return 1

    print(f"posted review: {len(payload['comments'])} inline finding(s), "
          f"{len(dropped)} unanchored")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

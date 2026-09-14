#!/usr/bin/env python3
"""Read the model's raw reply + pr.diff, build a review payload, POST it.

Exits non-zero on a malformed document — a bad review is never posted in
degraded form. Usage:

    post_review.py <repo> <pr_number> <commit_id> <reply.txt> <pr.diff>
"""

import json
import subprocess
import sys

from review_findings import (ValidationError, build_review, extract_findings,
                             parse_hunks, validate)


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
        # Actions log without re-running the model.
        print(f"::group::model reply (first 2000 chars)\n{reply[:2000]}\n::endgroup::",
              file=sys.stderr)
        return 1

    with open(diff_path, encoding="utf-8") as fh:
        hunks = parse_hunks(fh.read())

    payload, dropped = build_review(doc, commit_id, hunks)
    for f in dropped:
        print(f"::warning::finding not anchorable to the diff "
              f"({f['file']}:{f['line']}): {f['title']}")

    proc = subprocess.run(
        ["gh", "api", f"repos/{repo}/pulls/{number}/reviews",
         "--method", "POST", "--input", "-"],
        input=json.dumps(payload), text=True, capture_output=True, check=False,
    )
    if proc.returncode != 0:
        print(f"::error::posting the review failed: {proc.stderr.strip()}",
              file=sys.stderr)
        return 1

    print(f"posted review: {len(payload['comments'])} inline finding(s), "
          f"{len(dropped)} unanchored")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

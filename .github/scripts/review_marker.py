#!/usr/bin/env python3
"""Read the review marker out of a pull request's reviews, from the bot only.

The reviewer writes `<!-- ghost-review:<sha> -->` into the body of every
review it posts (`review_findings.build_review` appends it). Two
workflows read it back: the Reviewer decides how much of a pull request
still needs reviewing, and the Sweeper decides whether a commit was
judged at all.

A marker counts only when the review carrying it was posted by the
review bot itself — the app the workflow mints, or the workflow token
it falls back to — and the review author must be exactly
`github-actions[bot]` or `review-sweeper[bot]` with user type `Bot`.
Nobody else says what has been reviewed.

This module is the ONE place that answers the question, so both readers
ask it the same way, and it is unit-tested (test_review_marker.py). No
network and no GitHub calls: the answer is a function of the reviews
the caller hands over.
"""

import argparse
import json
import re
import sys

# The marker the reviewer writes, as a READER has to recognise it. It
# is written out here rather than imported, because the sweeper reads
# this one script on its own; the test that follows ties the two
# together - the marker this module finds is the marker
# review_findings.build_review writes.
MARKER_RE = re.compile(r"<!--\s*ghost-review:([0-9a-f]{7,40})\s*-->")

# The identities the reviewer posts as: the app the workflow mints
# (review-sweeper[bot]) and the workflow token it falls back to
# (github-actions[bot]).
#
# The set is EXACT. A login that merely starts with one of these
# prefixes (such as "github-actions-evil[bot]") is not the review bot.
# A GitHub login cannot carry a bracket, so "[bot]" belongs to a bot
# account and to no human, but the prefix alone is not enough — the
# whole login must match.
#
# The user type must be "Bot" as well. A same-repository workflow
# running with the workflow token posts as github-actions[bot], so the
# gate trusts write access, not authorship alone; the type is what
# separates a bot account from a human who took its name.
TRUSTED_IDENTITIES = frozenset({"github-actions[bot]", "review-sweeper[bot]"})


def posted_by_review_bot(user):
    """True when a review was posted by the review bot itself."""
    if not isinstance(user, dict):
        return False
    login = str(user.get("login") or "")
    if login not in TRUSTED_IDENTITIES:
        return False
    # The user type must be exactly "Bot". A missing type is not
    # enough: a review with no type cannot be trusted, because the
    # login alone is not proof of a bot account.
    if str(user.get("type") or "") != "Bot":
        return False
    return True


def parse_reviews(text):
    """The reviews in an API answer.

    `gh api --paginate` prints one JSON array per page, so the answer
    is a sequence of arrays rather than one array. Every document in it
    is read, in the order the API returned them.
    """
    reviews = []
    decoder = json.JSONDecoder()
    text = str(text or "")
    index = 0
    while index < len(text):
        while index < len(text) and text[index].isspace():
            index += 1
        if index >= len(text):
            break
        try:
            document, index = decoder.raw_decode(text, index)
        except ValueError:
            # An answer nobody can read names no commit: the rule fails
            # closed, so an unreadable answer is no marker at all.
            break
        if isinstance(document, list):
            reviews.extend(item for item in document if isinstance(item, dict))
        elif isinstance(document, dict):
            reviews.append(document)
    return reviews


def marker_in(body):
    """The commit a review body names, or "" when it names none.

    The FIRST marker in the body is the one that counts.
    """
    match = MARKER_RE.search(str(body or ""))
    return match.group(1) if match else ""


def bot_markers(reviews):
    """The commits the trusted reviews name, in the order they came."""
    markers = []
    for review in reviews or []:
        if not isinstance(review, dict) or not posted_by_review_bot(review.get("user")):
            continue
        sha = marker_in(review.get("body"))
        if sha:
            markers.append(sha)
    return markers


def last_marker(reviews):
    """The commit the newest trusted review says was reviewed."""
    markers = bot_markers(reviews)
    return markers[-1] if markers else ""


def judged(reviews, head):
    """True when a trusted review says THIS commit was reviewed."""
    head = str(head or "").lower()
    if not head:
        return False
    for sha in bot_markers(reviews):
        if head.startswith(sha.lower()):
            return True
    return False


def _read(path):
    """The text of a file, or of stdin, or nothing."""
    if not path or path == "-":
        try:
            return sys.stdin.read()
        except OSError:
            return ""
    try:
        with open(path, encoding="utf-8", errors="replace") as handle:
            return handle.read()
    except OSError:
        return ""


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--reviews", default="-",
                        help="the reviews API answer: a file, or - for stdin")
    parser.add_argument("--head", default="",
                        help="the commit to look for")
    args = parser.parse_args(argv[1:])

    reviews = parse_reviews(_read(args.reviews))
    print(json.dumps({
        "marker": last_marker(reviews),
        "judged": judged(reviews, args.head),
    }))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

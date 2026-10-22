"""The review marker, and the accounts whose marker counts.

The reviewer writes `<!-- ghost-review:<sha> -->` into the body of every
review it posts. Two workflows read it back: the Reviewer decides how
much of a pull request still needs reviewing, and the Sweeper decides
whether a commit was judged at all. A marker counts only when the
review carrying it was posted by the review bot itself — the app the
workflow mints, or the workflow token it falls back to — and the
review author is exactly `github-actions[bot]` or `review-sweeper[bot]`
with user type `Bot`. The rule is read from the TRUSTED copy of the
scripts, so a pull request cannot edit the question.

These tests hold that rule: a marker counts only when the review
carrying it was posted by the review bot itself.
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

import review_findings
from review_marker import judged, last_marker, marker_in, parse_reviews

HERE = os.path.dirname(os.path.abspath(__file__))

# The two identities the reviewer posts as: the app the workflow mints,
# and the workflow token it falls back to.
BOT = "review-sweeper[bot]"
WORKFLOW = "github-actions[bot]"

# A commit, and the short form of it.
HEAD = "8877fc9b" + "0" * 32


def _review(login, body, user_type="UNSET"):
    """One review, as the reviews API returns it."""
    user = {"login": login}
    if user_type == "UNSET":
        if login in (BOT, WORKFLOW):
            user["type"] = "Bot"
        else:
            user["type"] = "User"
    elif user_type is not None:
        user["type"] = user_type
    return {"user": user, "body": body, "state": "COMMENTED"}


def _marker(sha):
    """The marker the reviewer writes."""
    return "<!-- ghost-review:%s -->" % sha


def workflow_text(name):
    """The text of one workflow file."""
    path = os.path.join(HERE, os.pardir, "workflows", name)
    with open(path, encoding="utf-8") as handle:
        return handle.read()


class TestTheMarker(unittest.TestCase):
    """The marker the writer writes is the marker the reader finds."""

    def test_the_marker_the_writer_produces_is_the_one_the_reader_finds(self):
        payload, _dropped = review_findings.build_review(
            {"verdict": "clean", "summary": "Nothing to report.", "findings": []},
            "abc1234", [])
        self.assertEqual(marker_in(payload["body"]), "abc1234")

    def test_a_body_without_a_marker_names_nothing(self):
        self.assertEqual(marker_in("a review that names no commit"), "")

    def test_the_first_marker_in_a_body_is_the_one(self):
        # The first marker in a body is the one that counts.
        self.assertEqual(marker_in(_marker("aaaaaaa") + "\n"
                                   + _marker("bbbbbbb")), "aaaaaaa")

    def test_a_marker_naming_something_that_is_not_a_commit_names_nothing(self):
        self.assertEqual(marker_in("<!-- ghost-review:not-a-commit -->"), "")


class TestWhoseMarkerCounts(unittest.TestCase):
    """Only the review bot's own marker counts."""

    def test_a_marker_posted_by_the_author_is_ignored(self):
        reviews = [_review(BOT, _marker("1111111")),
                   _review("the-author", _marker("2222222"))]
        self.assertEqual(last_marker(reviews), "1111111")

    def test_the_newest_trusted_marker_wins(self):
        reviews = [_review(BOT, _marker("1111111")),
                   _review(WORKFLOW, _marker("2222222"))]
        self.assertEqual(last_marker(reviews), "2222222")

    def test_another_bot_is_not_the_review_bot(self):
        reviews = [_review("coderabbit[bot]", _marker("3333333"))]
        self.assertEqual(last_marker(reviews), "")

    def test_a_human_account_named_like_the_bot_is_not_the_bot(self):
        # A GitHub login cannot carry a bracket, so the bot suffix is
        # what separates the bot from an account that took its name.
        reviews = [_review("review-sweeper", _marker("4444444"))]
        self.assertEqual(last_marker(reviews), "")

    def test_a_look_alike_app_slug_is_not_the_bot(self):
        # An app slug such as "github-actions-evil" is not the review
        # bot, even though it starts with a trusted prefix. The type is
        # Bot, so the rejection is by the login, not the type.
        reviews = [_review("github-actions-evil[bot]", _marker("5555555"),
                            user_type="Bot")]
        self.assertEqual(last_marker(reviews), "")

    def test_an_exact_login_with_a_user_type_is_not_the_bot(self):
        # The login matches, but the type is "User", so the review is
        # not the bot's.
        reviews = [_review(BOT, _marker("7777777"), user_type="User")]
        self.assertEqual(last_marker(reviews), "")

    def test_a_review_with_no_type_is_not_the_bot(self):
        # A review that carries no user type cannot be trusted.
        reviews = [_review(BOT, _marker("8888888"), user_type=None)]
        self.assertEqual(last_marker(reviews), "")

    def test_a_bot_account_whose_type_is_not_bot_is_not_the_bot(self):
        # The user type must be "Bot": a human account cannot post as
        # the review bot.
        reviews = [_review(BOT, _marker("6666666"), user_type="User")]
        self.assertEqual(last_marker(reviews), "")

    def test_a_review_without_a_marker_names_nothing(self):
        reviews = [_review(BOT, "a review that names no commit")]
        self.assertEqual(last_marker(reviews), "")

    def test_no_review_at_all_names_nothing(self):
        self.assertEqual(last_marker([]), "")


class TestJudged(unittest.TestCase):
    """Which commits a trusted review says were reviewed."""

    def test_a_head_the_bot_reviewed_is_judged(self):
        reviews = [_review(BOT, _marker(HEAD))]
        self.assertTrue(judged(reviews, HEAD))

    def test_a_head_only_the_author_claims_is_not_judged(self):
        reviews = [_review("the-author", _marker(HEAD))]
        self.assertFalse(judged(reviews, HEAD))

    def test_an_older_bot_review_does_not_judge_a_new_head(self):
        reviews = [_review(BOT, _marker("1111111"))]
        self.assertFalse(judged(reviews, HEAD))

    def test_an_abbreviated_marker_names_its_commit(self):
        reviews = [_review(BOT, _marker(HEAD[:7]))]
        self.assertTrue(judged(reviews, HEAD))

    def test_no_head_judges_nothing(self):
        self.assertFalse(judged([_review(BOT, _marker(HEAD))], ""))


class TestTheAnswer(unittest.TestCase):
    """The API answer, in the shapes it arrives in."""

    def test_pages_are_read_in_order(self):
        # `gh api --paginate` prints one array per page.
        first = json.dumps([_review(BOT, _marker("1111111"))])
        second = json.dumps([_review(BOT, _marker("2222222"))])
        reviews = parse_reviews(first + "\n" + second)
        self.assertEqual(last_marker(reviews), "2222222")

    def test_one_array_is_read(self):
        reviews = parse_reviews(json.dumps([_review(BOT, _marker("1111111"))]))
        self.assertEqual(last_marker(reviews), "1111111")

    def test_an_answer_that_does_not_parse_names_nothing(self):
        self.assertEqual(last_marker(parse_reviews("[{")), "")

    def test_an_empty_answer_names_nothing(self):
        self.assertEqual(last_marker(parse_reviews("")), "")


class TestTheCommand(unittest.TestCase):
    """The command the workflows call."""

    def test_the_command_answers_with_the_marker_and_the_judgement(self):
        with tempfile.TemporaryDirectory() as folder:
            path = os.path.join(folder, "reviews.json")
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(json.dumps([_review(BOT, _marker(HEAD))]))
            result = subprocess.run(
                [sys.executable, "-I",
                 os.path.join(HERE, "review_marker.py"),
                 "--reviews", path, "--head", HEAD],
                capture_output=True, text=True, check=False)
        self.assertEqual(result.returncode, 0)
        answer = json.loads(result.stdout)
        self.assertEqual(answer["marker"], HEAD)
        self.assertTrue(answer["judged"])

    def test_the_command_answers_for_an_author_marker_too(self):
        with tempfile.TemporaryDirectory() as folder:
            path = os.path.join(folder, "reviews.json")
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(json.dumps([_review("the-author", _marker(HEAD))]))
            result = subprocess.run(
                [sys.executable, "-I",
                 os.path.join(HERE, "review_marker.py"),
                 "--reviews", path, "--head", HEAD],
                capture_output=True, text=True, check=False)
        self.assertEqual(result.returncode, 0)
        answer = json.loads(result.stdout)
        self.assertEqual(answer["marker"], "")
        self.assertFalse(answer["judged"])


class TestTheWorkflows(unittest.TestCase):
    """The workflows ask the rule, and carry no copy of it."""

    def test_the_reviewer_asks_the_rule_which_commit_was_reviewed(self):
        self.assertTrue(
            "base-scripts/review_marker.py" in workflow_text("reviewer.yml"),
            "the reviewer does not ask the rule which commit was reviewed")

    def test_the_sweeper_asks_the_rule_which_commit_was_reviewed(self):
        self.assertTrue(
            "review_marker.py" in workflow_text("sweeper.yml"),
            "the sweeper does not ask the rule which commit was reviewed")

    def test_no_workflow_carries_its_own_marker_filter(self):
        # A copy of the filter in a workflow is a filter that can drift
        # away from the rule the tests hold.
        for name in ("reviewer.yml", "sweeper.yml"):
            with self.subTest(workflow=name):
                workflow = workflow_text(name)
                self.assertFalse(
                    "capture(\"<!-- ghost-review:" in workflow
                    or "contains(\\\"<!-- ghost-review:" in workflow,
                    "the workflow carries its own copy of the marker filter")


if __name__ == "__main__":
    unittest.main()

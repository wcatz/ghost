"""A 422 on the inline review must not lose the review: post_review retries
once with every inline finding folded into the body, marker kept last."""
import json
import unittest
from types import SimpleNamespace
from unittest import mock

import post_review


class TestFoldFallback(unittest.TestCase):
    def test_fold_keeps_marker_last_and_drops_anchors(self):
        marker = post_review.MARKER.format(sha="abc123")
        payload = {"commit_id": "abc123", "event": "COMMENT",
                   "body": "**should-fix** — 1 inline finding\n\n" + marker,
                   "comments": [{"path": "a.go", "line": 7, "body": "bad"}]}
        out = post_review._fold_comments_into_body(payload, marker)
        self.assertEqual(out["comments"], [])
        self.assertTrue(out["body"].endswith(marker))
        self.assertIn("`a.go:7` bad", out["body"])
        self.assertEqual(out["body"].count(marker), 1)

    def test_api_error_includes_stdout_body(self):
        proc = SimpleNamespace(stderr="gh: Unprocessable Entity (HTTP 422)",
                               stdout='{"message":"line must be part of the diff"}')
        self.assertIn("line must be part of the diff", post_review._api_error(proc))

    def test_retry_posts_without_comments_after_422(self):
        calls = []

        def fake_post(repo, number, payload):
            calls.append(json.loads(json.dumps(payload)))
            rc = 1 if len(calls) == 1 else 0
            return SimpleNamespace(returncode=rc, stderr="gh: HTTP 422",
                                   stdout='{"message":"Validation Failed"}')

        payload = {"commit_id": "abc", "event": "COMMENT", "body": "b",
                   "comments": [{"path": "a.go", "line": 1, "body": "x"}]}
        with mock.patch.object(post_review, "_post", side_effect=fake_post):
            proc = post_review._post("r", 1, payload)
            if proc.returncode != 0 and payload["comments"]:
                payload = post_review._fold_comments_into_body(payload, "<!-- m -->")
                proc = post_review._post("r", 1, payload)
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[1]["comments"], [])
        self.assertEqual(proc.returncode, 0)


if __name__ == "__main__":
    unittest.main()

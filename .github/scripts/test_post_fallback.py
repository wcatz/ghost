"""A 422 on the inline review must not lose findings or hide them from the
merge gate: post_review retries once without anchors and posts each finding
as a file-level review comment (a review thread), failing closed."""
import json
import os
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

import post_review

DIFF = """diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,1 +1,2 @@
 package a
+var x = 1
"""
REPLY = {"verdict": "blocker", "summary": "s", "findings": [
    {"file": "a.go", "line": 2, "severity": "blocker", "title": "t", "body": "b"}]}


def _proc(rc, err=""):
    return SimpleNamespace(returncode=rc, stderr=err, stdout="")


class TestFileLevelFallback(unittest.TestCase):
    def _run(self, posts, file_posts):
        with tempfile.TemporaryDirectory() as d:
            reply, diff = os.path.join(d, "r.txt"), os.path.join(d, "p.diff")
            with open(reply, "w") as fh:
                json.dump(REPLY, fh)
            with open(diff, "w") as fh:
                fh.write(DIFF)
            with mock.patch.object(post_review, "_post", side_effect=posts) as p, \
                 mock.patch.object(post_review, "_post_file_comment",
                                   side_effect=file_posts) as fp:
                rc = post_review.main(["x", "o/r", "1", "sha1", reply, diff])
            return rc, p, fp

    def test_422_retries_and_posts_file_level_threads(self):
        rc, p, fp = self._run([_proc(1, "gh: HTTP 422"), _proc(0)], [_proc(0)])
        self.assertEqual(rc, 0)
        self.assertEqual(p.call_count, 2)
        self.assertEqual(p.call_args_list[1].args[2]["comments"], [])
        self.assertEqual(fp.call_count, 1)

    def test_file_level_failure_fails_closed_before_marker(self):
        rc, p, _ = self._run([_proc(1, "gh: HTTP 422"), _proc(0)], [_proc(1, "HTTP 422")])
        self.assertEqual(rc, 1)
        # the marker-bearing review is never posted after a lost finding
        self.assertEqual(p.call_count, 1)

    def test_non_422_is_not_retried(self):
        rc, p, fp = self._run([_proc(1, "gh: HTTP 401")], [])
        self.assertEqual(rc, 1)
        self.assertEqual(p.call_count, 1)
        self.assertEqual(fp.call_count, 0)

    def test_api_error_includes_stdout_body(self):
        proc = SimpleNamespace(stderr="gh: Unprocessable Entity (HTTP 422)",
                               stdout='{"message":"line must be part of the diff"}')
        self.assertIn("line must be part of the diff", post_review._api_error(proc))


if __name__ == "__main__":
    unittest.main()

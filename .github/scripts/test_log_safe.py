import io
import json
import os
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import Mock, patch

from post_review import MAX_BODY_CHARS, MAX_COMMENTS, _log_safe, main
from review_findings import MARKER


class TestLogSafeSingleLine(unittest.TestCase):
    def test_collapses_newlines_crs_and_tabs_to_single_spaces(self):
        text = "a\nb\r\nc\td\re"
        self.assertEqual(_log_safe(text), "a b c d e")

    def test_result_never_contains_cr_or_lf(self):
        text = "line one\nline two\r\nline three"
        out = _log_safe(text)
        self.assertNotIn("\n", out)
        self.assertNotIn("\r", out)

    def test_strips_and_truncates_to_limit(self):
        text = "  hello   world  "
        self.assertEqual(_log_safe(text, limit=5), "hello")


class TestLogSafeMultiLine(unittest.TestCase):
    def test_prefixes_every_line_so_none_can_start_with_double_colon(self):
        text = "::error::forged\nnormal line\n::stop-commands::x"
        out = _log_safe(text, multiline=True)
        lines = out.split("\n")
        self.assertTrue(all(line.startswith("> ") for line in lines))
        self.assertFalse(any(line.startswith("::") for line in out.splitlines()))

    def test_cr_only_line_breaks_are_also_neutralised(self):
        text = "::error::forged\r::add-mask::x"
        out = _log_safe(text, multiline=True)
        self.assertFalse(any(line.startswith("::") for line in out.splitlines()))

    def test_truncation_applies_before_prefixing(self):
        # 10 raw chars ("0123456789") truncated to 5 -> "01234", then
        # prefixed as a single line "> 01234" (no unprefixed remainder).
        text = "0123456789"
        out = _log_safe(text, limit=5, multiline=True)
        self.assertEqual(out, "> 01234")


class TestInjectionReproducer(unittest.TestCase):
    """The exact scenario from the security review: a VALID findings
    document whose finding title carries newline-delimited workflow
    commands, reaching the '::warning::finding not anchorable' site."""

    def _run(self, title):
        doc = {
            "verdict": "should-fix",
            "summary": "ok",
            "findings": [{
                "file": "x.go", "line": 999, "severity": "should-fix",
                "title": title, "body": "some body",
            }],
        }
        diff_text = (
            "diff --git a/a.go b/a.go\nindex 111..222 100644\n"
            "--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,1 @@\n package main\n"
        )
        with tempfile.TemporaryDirectory() as d:
            reply_path = os.path.join(d, "reply.txt")
            diff_path = os.path.join(d, "pr.diff")
            with open(reply_path, "w", encoding="utf-8") as fh:
                fh.write(json.dumps(doc))
            with open(diff_path, "w", encoding="utf-8") as fh:
                fh.write(diff_text)

            with patch("post_review.subprocess.run") as run:
                run.return_value.returncode = 0
                run.return_value.stderr = ""
                buf = io.StringIO()
                with redirect_stdout(buf):
                    rc = main(["post_review.py", "org/repo", "1", "abc123",
                               reply_path, diff_path])
        return rc, buf.getvalue()

    def test_newline_injection_yields_exactly_one_command_line(self):
        title = ("Injected title\n::error::FORGED ANNOTATION\n"
                 "::add-mask::topsecret\n::stop-commands::xyz")
        rc, out = self._run(title)
        self.assertEqual(rc, 0)
        lines = out.splitlines()
        cmd_lines = [l for l in lines if l.startswith("::")]
        self.assertEqual(len(cmd_lines), 1)
        self.assertTrue(cmd_lines[0].startswith("::warning::"))
        for forged in ("::add-mask::", "::error::", "::stop-commands::"):
            self.assertFalse(any(l.startswith(forged) for l in lines),
                              f"{forged} reached line start")

    def test_cr_only_injection_yields_exactly_one_command_line(self):
        title = ("Injected title\r::error::FORGED ANNOTATION\r"
                 "::add-mask::topsecret\r::stop-commands::xyz")
        rc, out = self._run(title)
        self.assertEqual(rc, 0)
        lines = out.splitlines()
        cmd_lines = [l for l in lines if l.startswith("::")]
        self.assertEqual(len(cmd_lines), 1)
        self.assertTrue(cmd_lines[0].startswith("::warning::"))

    def test_malformed_reply_group_excerpt_forges_no_command(self):
        # The second vulnerable site: a garbled (invalid) model reply is
        # echoed as a bounded excerpt inside ::group::/::endgroup:: on
        # stderr. A newline-embedded workflow command in that raw text
        # must not reach the start of a line.
        bad_reply = ("not valid json, and here is an attack\n"
                     "::error::FORGED\n::add-mask::topsecret\n"
                     "::stop-commands::xyz\n")
        diff_text = (
            "diff --git a/a.go b/a.go\nindex 111..222 100644\n"
            "--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,1 @@\n package main\n"
        )
        with tempfile.TemporaryDirectory() as d:
            reply_path = os.path.join(d, "reply.txt")
            diff_path = os.path.join(d, "pr.diff")
            with open(reply_path, "w", encoding="utf-8") as fh:
                fh.write(bad_reply)
            with open(diff_path, "w", encoding="utf-8") as fh:
                fh.write(diff_text)

            with patch("post_review.subprocess.run"):
                buf = io.StringIO()
                with redirect_stderr(buf):
                    rc = main(["post_review.py", "org/repo", "1", "abc123",
                               reply_path, diff_path])
        self.assertEqual(rc, 1)
        lines = buf.getvalue().splitlines()
        cmd_lines = [l for l in lines if l.startswith("::")]
        # Only this script's own ::error::, ::group:: and ::endgroup:: may
        # start a line — none of the model's forged commands.
        self.assertEqual(len(cmd_lines), 3)
        self.assertTrue(cmd_lines[0].startswith("::error::unusable model reply"))
        self.assertEqual(cmd_lines[1], "::group::model reply (first 2000 chars)")
        self.assertEqual(cmd_lines[2], "::endgroup::")
        for forged in ("::error::FORGED", "::add-mask::", "::stop-commands::"):
            self.assertFalse(any(l.startswith(forged) for l in lines),
                              f"{forged} reached line start")
        # The forged text must still be present, just neutralised.
        self.assertTrue(any("> ::error::FORGED" in l for l in lines))


class TestCaps(unittest.TestCase):
    def _run_with(self, findings, diff_text):
        doc = {"verdict": "blocker", "summary": "s", "findings": findings}
        with tempfile.TemporaryDirectory() as d:
            reply_path = os.path.join(d, "reply.txt")
            diff_path = os.path.join(d, "pr.diff")
            with open(reply_path, "w", encoding="utf-8") as fh:
                fh.write(json.dumps(doc))
            with open(diff_path, "w", encoding="utf-8") as fh:
                fh.write(diff_text)

            captured = {}

            def fake_run(*args, **kwargs):
                captured["payload"] = json.loads(kwargs["input"])
                run_result = Mock()
                run_result.returncode = 0
                run_result.stderr = ""
                return run_result

            with patch("post_review.subprocess.run", side_effect=fake_run):
                buf = io.StringIO()
                with redirect_stdout(buf):
                    rc = main(["post_review.py", "org/repo", "1", "abc123",
                               reply_path, diff_path])
        return rc, buf.getvalue(), captured["payload"]

    def test_comment_cap_fires_and_annotates(self):
        findings = []
        diff_parts = []
        for i in range(MAX_COMMENTS + 10):
            fname = f"f{i}.go"
            diff_parts.append(
                f"diff --git a/{fname} b/{fname}\nindex 111..222 100644\n"
                f"--- a/{fname}\n+++ b/{fname}\n@@ -1,1 +1,1 @@\n+line{i}\n")
            findings.append({"file": fname, "line": 1, "severity": "blocker",
                              "title": f"issue {i}", "body": "body"})
        rc, out, payload = self._run_with(findings, "".join(diff_parts))
        self.assertEqual(rc, 0)
        self.assertEqual(len(payload["comments"]), MAX_COMMENTS)
        self.assertIn("omitted", payload["body"])
        self.assertTrue(any("comment cap hit" in l for l in out.splitlines()))
        # The ghost-review marker must survive the cap, and stay last.
        marker = MARKER.format(sha="abc123")
        self.assertTrue(payload["body"].endswith(marker))

    def test_body_length_cap_fires_and_annotates(self):
        findings = [{
            "file": "a.go", "line": 1, "severity": "nit",
            "title": "issue", "body": "x" * (MAX_BODY_CHARS + 5000),
        }]
        diff_text = (
            "diff --git a/a.go b/a.go\nindex 111..222 100644\n"
            "--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,1 @@\n+line\n")
        rc, out, payload = self._run_with(findings, diff_text)
        self.assertEqual(rc, 0)
        self.assertLessEqual(len(payload["body"]), MAX_BODY_CHARS)
        self.assertIn("[review body truncated]", payload["body"])
        self.assertTrue(any("review body truncated" in l for l in out.splitlines()))
        # The ghost-review marker must survive truncation, and stay last —
        # a consumer's re.search (first match) or a naive endswith check
        # must still find the real sha, not be told the commit is unreviewed.
        marker = MARKER.format(sha="abc123")
        self.assertTrue(payload["body"].endswith(marker))


if __name__ == "__main__":
    unittest.main()

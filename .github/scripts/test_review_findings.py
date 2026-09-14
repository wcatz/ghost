import os
import re
import subprocess
import tempfile
import unittest

from review_findings import ValidationError, validate, parse_hunks, build_review, partition


def _doc(**over):
    doc = {
        "verdict": "should-fix",
        "summary": "Adds a thing.",
        "findings": [
            {
                "file": "internal/memory/store.go",
                "line": 42,
                "severity": "should-fix",
                "title": "Unchecked error",
                "body": "The error from Exec is discarded.",
            }
        ],
    }
    doc.update(over)
    return doc


class TestValidate(unittest.TestCase):
    def test_accepts_a_minimal_valid_document(self):
        validate(_doc())

    def test_accepts_clean_verdict_with_no_findings(self):
        validate(_doc(verdict="clean", findings=[]))

    def test_rejects_unknown_verdict(self):
        with self.assertRaises(ValidationError):
            validate(_doc(verdict="looks-fine"))

    def test_rejects_missing_summary(self):
        doc = _doc()
        del doc["summary"]
        with self.assertRaises(ValidationError):
            validate(doc)

    def test_rejects_unknown_severity(self):
        with self.assertRaises(ValidationError):
            validate(_doc(findings=[{
                "file": "a.go", "line": 1, "severity": "critical",
                "title": "t", "body": "b",
            }]))

    def test_rejects_non_integer_line(self):
        with self.assertRaises(ValidationError):
            validate(_doc(findings=[{
                "file": "a.go", "line": "42", "severity": "nit",
                "title": "t", "body": "b",
            }]))

    def test_rejects_end_line_before_line(self):
        with self.assertRaises(ValidationError):
            validate(_doc(findings=[{
                "file": "a.go", "line": 10, "end_line": 4,
                "severity": "nit", "title": "t", "body": "b",
            }]))

    def test_rejects_findings_not_a_list(self):
        with self.assertRaises(ValidationError):
            validate(_doc(findings={"file": "a.go"}))

    def test_rejects_absolute_and_traversing_paths(self):
        for bad in ("/etc/passwd", "../outside.go"):
            with self.assertRaises(ValidationError):
                validate(_doc(findings=[{
                    "file": bad, "line": 1, "severity": "nit",
                    "title": "t", "body": "b",
                }]))

    def test_rejects_html_comment_marker_in_title(self):
        # A finding whose title/body/suggestion contains '<!--' could plant
        # a decoy '<!-- ghost-review:<sha> -->' marker ahead of the real
        # one appended by build_review, and a consumer using re.search
        # (first match) would read the attacker's forged sha.
        with self.assertRaises(ValidationError):
            validate(_doc(findings=[{
                "file": "a.go", "line": 1, "severity": "nit",
                "title": "t <!-- ghost-review:0000 -->", "body": "b",
            }]))

    def test_rejects_html_comment_marker_in_body(self):
        with self.assertRaises(ValidationError):
            validate(_doc(findings=[{
                "file": "a.go", "line": 1, "severity": "nit",
                "title": "t", "body": "b <!-- ghost-review:0000 -->",
            }]))

    def test_rejects_html_comment_marker_in_suggestion(self):
        with self.assertRaises(ValidationError):
            validate(_doc(findings=[{
                "file": "a.go", "line": 1, "severity": "nit",
                "title": "t", "body": "b",
                "suggestion": "<!-- ghost-review:0000 -->",
            }]))

    def test_rejects_html_comment_marker_in_summary(self):
        # doc["summary"] is the first line of the rendered review body, an
        # even better decoy position than a finding field for a consumer's
        # re.search (first match) to be fooled by.
        with self.assertRaises(ValidationError):
            validate(_doc(summary="s <!-- ghost-review:0000 -->"))

    def test_rejects_html_comment_marker_in_file(self):
        # A filename containing '<!--' is legal on disk but never
        # legitimate here, and build_review interpolates f['file'] into
        # the nits and dropped-findings lines of the review body.
        with self.assertRaises(ValidationError):
            validate(_doc(findings=[{
                "file": "a<!-- ghost-review:0000 -->.go", "line": 1,
                "severity": "nit", "title": "t", "body": "b",
            }]))


DIFF = """diff --git a/a.go b/a.go
index 111..222 100644
--- a/a.go
+++ b/a.go
@@ -1,3 +1,5 @@
 package main
+
+func added() {}

 func kept() {}
diff --git a/b.go b/b.go
index 333..444 100644
--- a/b.go
+++ b/b.go
@@ -10,2 +10,3 @@ func x() {
 	a := 1
+	b := 2
 	_ = a
diff --git a/gone.go b/gone.go
deleted file mode 100644
index 555..000
--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package main
-func dead() {}
"""


class TestParseHunks(unittest.TestCase):
    def test_maps_added_and_context_lines_per_file(self):
        hunks = parse_hunks(DIFF)
        self.assertEqual(hunks["a.go"], {1, 2, 3, 4, 5})
        self.assertEqual(hunks["b.go"], {10, 11, 12})

    def test_ignores_deleted_files(self):
        self.assertNotIn("gone.go", parse_hunks(DIFF))

    def test_does_not_confuse_the_minus_header_with_a_removed_line(self):
        # '--- a/a.go' starts with '-' but is a header, not a deletion.
        self.assertIn(1, parse_hunks(DIFF)["a.go"])

    def test_empty_diff_yields_no_hunks(self):
        self.assertEqual(parse_hunks(""), {})

    def test_added_line_starting_with_plus_plus_plus_is_not_a_fake_header(self):
        # An added line whose own text starts with '++ ' becomes '+++ ...'
        # once the diff's leading '+' marker is prepended. The old code
        # tested raw.startswith("+++ ") unconditionally, so this line was
        # mistaken for a new file header and silently truncated the real
        # file's hunk (and fabricated a bogus 'something' entry).
        diff = (
            "--- a/docs/example.md\n"
            "+++ b/docs/example.md\n"
            "@@ -1,2 +1,5 @@\n"
            " # Example\n"
            "+Here is sample diff output:\n"
            "+++ something\n"
            "+more real content after the fake header\n"
        )
        hunks = parse_hunks(diff)
        self.assertEqual(hunks, {"docs/example.md": {1, 2, 3, 4}})
        self.assertNotIn("something", hunks)

    def test_handles_multiple_hunks_in_one_file(self):
        diff = (
            "diff --git a/a.go b/a.go\n"
            "index 111..222 100644\n"
            "--- a/a.go\n"
            "+++ b/a.go\n"
            "@@ -1,2 +1,3 @@\n"
            " package main\n"
            "+import \"fmt\"\n"
            " \n"
            "@@ -10,2 +11,3 @@ func x() {\n"
            " \ta := 1\n"
            "+\tb := 2\n"
            " \t_ = a\n"
        )
        self.assertEqual(parse_hunks(diff)["a.go"], {1, 2, 3, 11, 12, 13})

    def test_handles_a_rename_with_content_changes(self):
        diff = (
            "diff --git a/old_name.go b/new_name.go\n"
            "similarity index 87%\n"
            "rename from old_name.go\n"
            "rename to new_name.go\n"
            "index 111..222 100644\n"
            "--- a/old_name.go\n"
            "+++ b/new_name.go\n"
            "@@ -1,3 +1,3 @@\n"
            " package main\n"
            "-func old() {}\n"
            "+func renamed() {}\n"
        )
        hunks = parse_hunks(diff)
        self.assertEqual(hunks["new_name.go"], {1, 2})
        self.assertNotIn("old_name.go", hunks)

    def test_git_quoted_non_ascii_path_keys_the_plain_filename(self):
        # With git's default core.quotePath=true, a non-ASCII filename is
        # rendered as a double-quoted, C-escaped path, e.g.
        # +++ "b/caf\303\251.txt" for café.txt. Verified against a REAL
        # git repository/diff, not a hand-typed fixture.
        with tempfile.TemporaryDirectory() as d:
            env = dict(os.environ)
            env["GIT_CONFIG_GLOBAL"] = "/dev/null"
            env["GIT_CONFIG_SYSTEM"] = "/dev/null"
            env["GIT_AUTHOR_NAME"] = "Test"
            env["GIT_AUTHOR_EMAIL"] = "test@test.invalid"
            env["GIT_COMMITTER_NAME"] = "Test"
            env["GIT_COMMITTER_EMAIL"] = "test@test.invalid"

            def run(*args):
                return subprocess.run(
                    ["git", *args], cwd=d, env=env,
                    capture_output=True, text=True)

            fname = "café.txt"
            steps = [
                ["init", "-q", "-b", "main"],
                ["config", "core.quotePath", "true"],
                ["config", "commit.gpgsign", "false"],
            ]
            for step in steps:
                r = run(*step)
                self.assertEqual(r.returncode, 0, r.stderr)

            with open(os.path.join(d, fname), "w", encoding="utf-8") as fh:
                fh.write("hello\n")
            r = run("add", fname)
            self.assertEqual(r.returncode, 0, r.stderr)
            r = run("commit", "-q", "-m", "init")
            self.assertEqual(r.returncode, 0, r.stderr)

            with open(os.path.join(d, fname), "a", encoding="utf-8") as fh:
                fh.write("world\n")
            r = run("diff", "--", fname)
            self.assertEqual(r.returncode, 0, r.stderr)
            diff_text = r.stdout

            # Confirm git actually quoted the path (this would otherwise
            # pass vacuously if core.quotePath were ignored on this box).
            self.assertIn(r"caf\303\251.txt", diff_text)

            hunks = parse_hunks(diff_text)
            self.assertIn("café.txt", hunks)


class TestPartition(unittest.TestCase):
    def test_splits_blocking_from_nits(self):
        findings = [
            {"severity": "blocker"}, {"severity": "should-fix"},
            {"severity": "nit"},
        ]
        blocking, nits = partition(findings)
        self.assertEqual([f["severity"] for f in blocking],
                         ["blocker", "should-fix"])
        self.assertEqual([f["severity"] for f in nits], ["nit"])


class TestBuildReview(unittest.TestCase):
    def setUp(self):
        self.hunks = {"a.go": {1, 2, 3, 4, 5}}

    def _f(self, **over):
        f = {"file": "a.go", "line": 3, "severity": "should-fix",
             "title": "Unchecked error", "body": "Exec's error is dropped."}
        f.update(over)
        return f

    def test_anchors_a_single_line_finding(self):
        doc = {"verdict": "should-fix", "summary": "s",
               "findings": [self._f()]}
        payload, dropped = build_review(doc, "abc123", self.hunks)
        self.assertEqual(payload["event"], "COMMENT")
        self.assertEqual(payload["commit_id"], "abc123")
        self.assertEqual(len(payload["comments"]), 1)
        c = payload["comments"][0]
        self.assertEqual((c["path"], c["line"], c["side"]), ("a.go", 3, "RIGHT"))
        self.assertNotIn("start_line", c)
        self.assertEqual(dropped, [])

    def test_anchors_a_multi_line_finding_with_start_line(self):
        doc = {"verdict": "should-fix", "summary": "s",
               "findings": [self._f(line=2, end_line=4)]}
        payload, _ = build_review(doc, "abc123", self.hunks)
        c = payload["comments"][0]
        self.assertEqual((c["start_line"], c["line"]), (2, 4))
        self.assertEqual(c["start_side"], "RIGHT")

    def test_nits_never_become_comments(self):
        doc = {"verdict": "nit", "summary": "s",
               "findings": [self._f(severity="nit")]}
        payload, _ = build_review(doc, "abc123", self.hunks)
        self.assertEqual(payload["comments"], [])
        self.assertIn("Unchecked error", payload["body"])

    def test_out_of_diff_anchor_is_dropped_not_posted(self):
        doc = {"verdict": "should-fix", "summary": "s",
               "findings": [self._f(line=99)]}
        payload, dropped = build_review(doc, "abc123", self.hunks)
        self.assertEqual(payload["comments"], [])
        self.assertEqual(len(dropped), 1)
        self.assertIn("could not be anchored", payload["body"])

    def test_unknown_file_is_dropped(self):
        doc = {"verdict": "should-fix", "summary": "s",
               "findings": [self._f(file="nope.go")]}
        _, dropped = build_review(doc, "abc123", self.hunks)
        self.assertEqual(len(dropped), 1)

    def test_suggestion_renders_a_fenced_block(self):
        doc = {"verdict": "should-fix", "summary": "s",
               "findings": [self._f(suggestion="\tif err != nil {\n")]}
        payload, _ = build_review(doc, "abc123", self.hunks)
        self.assertIn("```suggestion", payload["comments"][0]["body"])

    def test_suggestion_containing_a_fence_gets_a_longer_fence(self):
        # A suggestion containing a literal ``` would otherwise close the
        # ```suggestion fence early and inject arbitrary Markdown into the
        # rendered review body.
        doc = {"verdict": "should-fix", "summary": "s",
               "findings": [self._f(
                   suggestion="before\n```\nfake fence\n```\nafter")]}
        payload, _ = build_review(doc, "abc123", self.hunks)
        body = payload["comments"][0]["body"]
        runs = re.findall(r"`+", body)
        # The suggestion's own runs are 3 backticks; the opening/closing
        # fence must be strictly longer (max(3, longest+1) == 4) and must
        # appear exactly twice — no other backtick run may reach that
        # length, or the "fence" could be confused with content.
        self.assertEqual(runs.count("`" * 4), 2)
        self.assertTrue(all(len(r) <= 4 for r in runs), runs)

    def test_body_carries_the_reviewed_sha_marker(self):
        doc = {"verdict": "clean", "summary": "All good.", "findings": []}
        payload, _ = build_review(doc, "deadbeef", self.hunks)
        self.assertIn("<!-- ghost-review:deadbeef -->", payload["body"])


class TestReviewBodyLayout(unittest.TestCase):
    """The rendered body is what a human actually reads on the PR page."""

    def setUp(self):
        self.hunks = {"a.go": {1, 2, 3, 4, 5}}

    def _f(self, **over):
        f = {"file": "a.go", "line": 3, "severity": "nit",
             "title": "Wording", "body": "Reads oddly."}
        f.update(over)
        return f

    def _body(self, verdict, findings, summary="Summary text."):
        doc = {"verdict": verdict, "summary": summary, "findings": findings}
        return build_review(doc, "abc123", self.hunks)[0]["body"]

    def test_verdict_precedes_the_summary(self):
        body = self._body("nit", [self._f()])
        self.assertLess(body.index("nit"), body.index("Summary text."))
        self.assertTrue(body.startswith("**"), body[:40])

    def test_clean_says_nothing_to_fix_rather_than_zero_counts(self):
        body = self._body("clean", [])
        self.assertIn("nothing to fix", body)
        self.assertNotIn("0 ", body)

    def test_counts_are_pluralised_not_parenthesised(self):
        one = self._body("nit", [self._f()])
        two = self._body("nit", [self._f(), self._f(line=4)])
        self.assertIn("1 nit.", one)
        self.assertIn("2 nits.", two)
        self.assertNotIn("(s)", one + two)

    def test_nits_omit_the_label_but_dropped_findings_keep_it(self):
        body = self._body("should-fix", [
            self._f(),
            self._f(severity="should-fix", line=99, title="Off-diff"),
        ])
        nits, dropped = body.split("</details>")
        self.assertNotIn("\U0001f535", nits)
        self.assertIn("\U0001f7e0 should-fix", dropped)

    def test_a_finding_body_sits_under_its_bullet_not_beside_it(self):
        body = self._body("nit", [self._f(body="Line one.")])
        self.assertIn("- **Wording** \u2014 `a.go:3`\n\n  Line one.", body)

    def test_a_blank_line_inside_a_body_carries_no_trailing_spaces(self):
        body = self._body("nit", [self._f(body="First.\n\nSecond.")])
        self.assertNotIn("  \n", body)
        self.assertIn("\n  First.\n\n  Second.", body)


if __name__ == "__main__":
    unittest.main()

import unittest

from review_findings import ValidationError, validate, parse_hunks


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


if __name__ == "__main__":
    unittest.main()

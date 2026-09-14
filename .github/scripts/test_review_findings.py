import unittest

from review_findings import ValidationError, validate


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


if __name__ == "__main__":
    unittest.main()

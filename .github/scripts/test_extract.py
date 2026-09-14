import unittest
from review_findings import ValidationError, extract_findings

DOC = '{"verdict":"clean","summary":"ok","findings":[]}'


class TestExtract(unittest.TestCase):
    def test_bare_json(self):
        self.assertEqual(extract_findings(DOC)["verdict"], "clean")

    def test_json_with_prose_before_and_after(self):
        t = f"Let me review this.\n\n{DOC}\n\nHope that helps!"
        self.assertEqual(extract_findings(t)["verdict"], "clean")

    def test_fenced_json_block(self):
        t = f"Here is the result:\n\n```json\n{DOC}\n```\n"
        self.assertEqual(extract_findings(t)["verdict"], "clean")

    def test_prefers_the_last_verdict_object(self):
        t = f'draft: {{"verdict":"nit","summary":"x","findings":[]}}\nfinal: {DOC}'
        self.assertEqual(extract_findings(t)["verdict"], "clean")

    def test_ignores_non_verdict_objects(self):
        t = f'{{"note":"thinking"}} {DOC} {{"unrelated":true}}'
        self.assertEqual(extract_findings(t)["verdict"], "clean")

    def test_handles_braces_inside_strings(self):
        d = '{"verdict":"nit","summary":"use {} not new Object()","findings":[]}'
        self.assertEqual(extract_findings(d)["summary"], "use {} not new Object()")

    def test_handles_escaped_quotes(self):
        d = '{"verdict":"nit","summary":"say \\"hi\\"","findings":[]}'
        self.assertEqual(extract_findings(d)["summary"], 'say "hi"')

    def test_nested_objects_in_findings(self):
        d = ('{"verdict":"should-fix","summary":"s","findings":'
             '[{"file":"a.go","line":1,"severity":"nit","title":"t","body":"b"}]}')
        self.assertEqual(len(extract_findings(d)["findings"]), 1)

    def test_raises_on_pure_prose(self):
        with self.assertRaises(ValidationError):
            extract_findings("I reviewed the PR and found three issues.")

    def test_raises_on_truncated_json(self):
        with self.assertRaises(ValidationError):
            extract_findings('{"verdict":"clean","summary":"ok"')

    def test_raises_on_empty(self):
        with self.assertRaises(ValidationError):
            extract_findings("")


if __name__ == "__main__":
    unittest.main()

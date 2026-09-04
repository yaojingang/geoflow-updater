import json
import pathlib
import subprocess
import tempfile
import unittest


class LogMatchDiagnosticTest(unittest.TestCase):
    def classify(self, content):
        with tempfile.TemporaryDirectory() as directory:
            log_file = pathlib.Path(directory) / "sample.log"
            log_file.write_bytes(content)
            result = subprocess.run([
                "python3", str(pathlib.Path(__file__).with_name("diagnose-log-match.py")),
                "application-log", str(log_file),
            ], input=b"\0".join([b"561902", b"fixture-secret-alpha", b"561902", b""]), capture_output=True, check=True)
            self.assertNotIn(b"fixture-secret-alpha", result.stdout)
            self.assertNotIn(b"561902", result.stdout)
            self.assertNotIn(b"sample.log", result.stdout)
            return json.loads(result.stdout)

    def test_actual_stack_argument_is_classified_without_disclosure(self):
        result = self.classify(b"#1 Client->startRollback('point', '561902')\n")
        self.assertEqual(len(result["matches"]), 1)
        match = result["matches"][0]
        self.assertTrue(match["matches_current_rollback_code"])
        self.assertTrue(match["php_stack_frame"])
        self.assertTrue(match["quoted_value"])
        self.assertFalse(match["embedded_in_digits"])
        self.assertEqual(match["known_frame_methods"], ["startRollback"])

    def test_numeric_substring_has_a_distinct_shape(self):
        match = self.classify(b"elapsed=15619029\n")["matches"][0]
        self.assertTrue(match["embedded_in_digits"])
        self.assertFalse(match["php_stack_frame"])

    def test_other_sensitive_value_and_clean_log(self):
        match = self.classify(b"value=fixture-secret-alpha\n")["matches"][0]
        self.assertEqual(match["value_shape"], "other-sensitive-value")
        self.assertEqual(self.classify(b"healthy\n")["matches"], [])


unittest.main()

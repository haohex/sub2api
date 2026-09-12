"""Exercise the actual jq predicate embedded in the KlN sync workflow."""
import json
from pathlib import Path
import re
import subprocess
import unittest


class SourceReleaseTests(unittest.TestCase):
    def test_only_explicit_stable_release_is_accepted(self):
        workflow = Path(__file__).resolve().parents[2] / '.github/workflows/sync-kln-release.yml'
        predicate = re.search(r"if ! jq -e '([^']+)'", workflow.read_text()).group(1)
        for payload, accepted in [
            ({'draft': False, 'prerelease': False}, True),
            ({'draft': True, 'prerelease': False}, False),
            ({'draft': False, 'prerelease': True}, False),
            ({'draft': None, 'prerelease': False}, False),
            ({'draft': False}, False),
            ({'draft': 'false', 'prerelease': False}, False),
        ]:
            with self.subTest(payload=payload):
                result = subprocess.run(['jq', '-e', predicate], input=json.dumps(payload),
                                        text=True, capture_output=True)
                self.assertEqual(result.returncode == 0, accepted)


if __name__ == '__main__':
    unittest.main()

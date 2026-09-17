"""Exercise the first-parent activation boundary with actual Git merge history."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class ActivationTests(unittest.TestCase):
    def test_feature_commit_time_does_not_republish_historical_merges(self):
        with tempfile.TemporaryDirectory() as temp:
            env = dict(os.environ, GIT_AUTHOR_NAME='Test', GIT_AUTHOR_EMAIL='test@example.invalid',
                       GIT_COMMITTER_NAME='Test', GIT_COMMITTER_EMAIL='test@example.invalid')

            def git(*args, date='2026-09-13T01:00:00+00:00'):
                return subprocess.check_output(['git', *args], cwd=temp, text=True, stderr=subprocess.DEVNULL,
                                               env=dict(env, GIT_AUTHOR_DATE=date, GIT_COMMITTER_DATE=date)).strip()

            git('init', '-b', 'main')
            git('commit', '--allow-empty', '-m', 'Initial')
            git('switch', '-c', 'feature')
            marker = Path(temp) / 'tools/release/merged-only.json'
            marker.parent.mkdir(parents=True)
            marker.write_text('{"schema":2}\n')
            git('add', '.')
            git('commit', '-m', 'Controller implementation', date='2026-09-13T02:00:00+00:00')
            git('switch', 'main')
            git('commit', '--allow-empty', '-m', 'Earlier main change', date='2026-09-13T03:00:00+00:00')
            git('merge', '--no-ff', 'feature', '-m', 'Activation PR', date='2026-09-13T04:00:00+00:00')
            git('commit', '--allow-empty', '-m', 'Later main change', date='2026-09-13T05:00:00+00:00')
            since = git('log', '--first-parent', '--diff-merges=first-parent', '--no-patch', '--diff-filter=A',
                        '--format=%cI', '--', 'tools/release/merged-only.json').splitlines()[-1]
            self.assertEqual(since.replace('Z', '+00:00'), '2026-09-13T04:00:00+00:00')


if __name__ == '__main__':
    unittest.main()

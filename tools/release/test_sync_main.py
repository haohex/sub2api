"""Real Git coverage for the trusted sync-branch merge helper."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

HELPER = Path(__file__).with_name('merge-sync-main.sh').resolve()


class SyncMainTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.cwd = Path(self.tmp.name)
        self.git('init', '-b', 'main')
        self.git('config', 'user.name', 'Test')
        self.git('config', 'user.email', 'test@example.com')
        self.commit('shared', 'base')
        self.base = self.git('rev-parse', 'HEAD')
        self.git('branch', 'sync')

    def git(self, *args):
        return subprocess.check_output(['git', *args], cwd=self.cwd,
                                       stderr=subprocess.STDOUT, text=True).strip()

    def commit(self, name, content):
        (self.cwd / name).write_text(content)
        self.git('add', name)
        self.git('commit', '-m', 'test')

    def run_helper(self):
        return subprocess.run(['bash', str(HELPER)], cwd=self.cwd,
                              env={**os.environ, 'BASE_BRANCH': 'main'},
                              capture_output=True, text=True)

    def prepare(self, conflict=False):
        self.commit('shared' if conflict else 'local-workflow', 'main change')
        self.main = self.git('rev-parse', 'HEAD')
        self.git('update-ref', 'refs/remotes/origin/main', self.main)
        self.git('switch', 'sync')
        self.commit('shared' if conflict else 'upstream', 'release change')
        self.release = self.git('rev-parse', 'HEAD')

    def test_merge_preserves_both_histories_and_is_idempotent(self):
        self.prepare()
        self.assertEqual(self.run_helper().returncode, 0)
        merged = self.git('rev-parse', 'HEAD')
        self.assertEqual(self.git('rev-list', '--parents', '-1', 'HEAD').split(),
                         [merged, self.release, self.main])
        self.assertTrue((self.cwd / 'local-workflow').exists())
        self.assertTrue((self.cwd / 'upstream').exists())
        self.assertEqual(self.run_helper().returncode, 0)
        self.assertEqual(self.git('rev-parse', 'HEAD'), merged)
        # A later main update is picked up by the next poll.
        self.git('switch', 'main')
        self.commit('later', 'new main change')
        self.git('update-ref', 'refs/remotes/origin/main', 'HEAD')
        self.git('switch', 'sync')
        self.assertEqual(self.run_helper().returncode, 0)
        self.assertTrue((self.cwd / 'later').exists())

    def test_conflict_leaves_original_branch_clean(self):
        self.prepare(conflict=True)
        result = self.run_helper()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('::warning::', result.stdout)
        self.assertEqual(self.git('rev-parse', 'HEAD'), self.release)
        self.assertEqual(self.git('status', '--porcelain'), '')
        self.assertFalse((self.cwd / '.git/MERGE_HEAD').exists())

    def test_missing_base_is_failure(self):
        self.assertNotEqual(self.run_helper().returncode, 0)


if __name__ == '__main__':
    unittest.main()

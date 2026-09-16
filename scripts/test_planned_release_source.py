"""Publication source identity across online metadata refresh commits."""
import datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[1]
HELPER = ROOT / 'scripts/planned-release-source.py'


class PublicationSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.runtime = tempfile.TemporaryDirectory()
        cls.verifier = Path(cls.runtime.name) / 'geoflow-tuf'
        subprocess.run(['go', 'build', '-o', str(cls.verifier), './cmd/geoflow-tuf'], cwd=ROOT, check=True, capture_output=True)
        workflow = yaml.safe_load((ROOT / '.github/workflows/release.yml').read_text())
        cls.download = next(step['run'] for step in workflow['jobs']['publish']['steps'] if step.get('name') == 'Download the approved candidate')

    @classmethod
    def tearDownClass(cls):
        cls.runtime.cleanup()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.base = Path(self.temp.name)
        self.repo = self.base / 'repo'
        self.repo.mkdir()
        self.environment = dict(os.environ, GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL='/dev/null')
        self.git('init', '-q')
        self.git('config', 'user.name', 'Release test')
        self.git('config', 'user.email', 'release-test@example.invalid')
        targets = self.base / 'targets'
        targets.mkdir()
        (targets / 'fixture.txt').write_text('approved existing target')
        self.keys = self.base / 'keys'
        self.repository = self.repo / 'tuf/repository'
        subprocess.run([str(self.verifier), 'init', '--keys-dir', str(self.keys), '--repository-dir', str(self.repository), '--targets-dir', str(targets)], check=True, capture_output=True)
        self.write('cmd/main.go', 'candidate runtime\n')
        if HELPER.exists():
            self.write('scripts/planned-release-source.py', HELPER.read_text())
        self.candidate = self.commit('candidate source')

    def tearDown(self):
        self.temp.cleanup()

    def git(self, *args):
        return subprocess.check_output(['git', *args], cwd=self.repo, env=self.environment, text=True).strip()

    def write(self, path, content):
        target = self.repo / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)

    def commit(self, message):
        self.git('add', '.')
        self.git('commit', '-q', '-m', message)
        return self.git('rev-parse', 'HEAD')

    def refresh(self):
        subprocess.run([str(self.verifier), 'refresh-online', '--repository-dir', str(self.repository), '--snapshot-key', str(self.keys / 'snapshot.pem'), '--timestamp-key', str(self.keys / 'timestamp.pem')], check=True, capture_output=True)
        return self.commit('daily online metadata refresh')

    def workflow(self, resume=False):
        commands = self.base / 'commands'
        commands.mkdir(exist_ok=True)
        gh = commands / 'gh'
        gh.write_text('''#!/bin/sh
if [ "$1 $2" = 'run download' ]; then echo downloaded >> "$TEST_TRACE"; exit 0; fi
case "$4" in
 .conclusion) echo success ;;
 .head_sha) echo "$TEST_CANDIDATE_SHA" ;;
 .name) echo 'Build Phase C release candidate' ;;
 .path) echo '.github/workflows/release-candidate.yml' ;;
 .event) echo workflow_dispatch ;;
 *) exit 3 ;;
esac
''')
        gh.chmod(0o700)
        go = commands / 'go'
        go.write_text('#!/bin/sh\ntest "$1 $2" = "run ./cmd/geoflow-tuf" || exit 3\nshift 2\nexec "$TEST_TUF_VERIFIER" "$@"\n')
        go.chmod(0o700)
        environment = dict(self.environment, PATH=str(commands) + os.pathsep + os.environ['PATH'], GITHUB_REPOSITORY='yaojingang/geoflow-updater', CANDIDATE_RUN_ID='42', GITHUB_SHA=self.git('rev-parse', 'HEAD'), TEST_CANDIDATE_SHA=self.candidate, PUBLICATION_RESUME='true' if resume else 'false', GITHUB_OUTPUT=str(self.base / 'output'), RUNNER_TEMP=str(self.base), TEST_TUF_VERIFIER=str(self.verifier), TEST_TRACE=str(self.base / 'trace'))
        return subprocess.run(['bash', '-euo', 'pipefail', '-c', self.download], cwd=self.repo, env=environment, text=True, capture_output=True)

    def test_first_publication_accepts_multiple_daily_refreshes_without_rebuilding(self):
        self.refresh()
        self.refresh()
        result = self.workflow()
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual('downloaded\n', (self.base / 'trace').read_text())
        self.assertIn('candidate_sha=' + self.candidate, (self.base / 'output').read_text())

    def test_first_publication_rejects_source_changed_then_reverted_during_refresh_window(self):
        self.write('cmd/main.go', 'unaccepted runtime\n')
        self.commit('runtime change')
        self.write('cmd/main.go', 'candidate runtime\n')
        self.commit('runtime reverted')
        self.refresh()
        result = self.workflow()
        self.assertNotEqual(0, result.returncode)
        self.assertFalse((self.base / 'trace').exists())

    def test_first_publication_rejects_targets_refresh_even_when_target_bytes_match(self):
        subprocess.run([str(self.verifier), 'refresh', '--repository-dir', str(self.repository), '--targets-key', str(self.keys / 'targets.pem'), '--snapshot-key', str(self.keys / 'snapshot.pem'), '--timestamp-key', str(self.keys / 'timestamp.pem')], check=True, capture_output=True)
        self.commit('reviewed targets refresh still needs a new candidate')
        result = self.workflow()
        self.assertNotEqual(0, result.returncode)
        self.assertFalse((self.base / 'trace').exists())

    def test_first_publication_rejects_changed_snapshot_target_reference(self):
        self.refresh()
        snapshot = self.repository / 'metadata/2.snapshot.json'
        document = json.loads(snapshot.read_text())
        document['signed']['meta']['targets.json']['version'] = 2
        snapshot.write_text(json.dumps(document))
        self.commit('changed existing snapshot')
        result = self.workflow()
        self.assertNotEqual(0, result.returncode)
        self.assertFalse((self.base / 'trace').exists())

    def test_first_publication_rejects_unsigned_daily_refresh(self):
        self.refresh()
        timestamp = self.repository / 'metadata/timestamp.json'
        document = json.loads(timestamp.read_text())
        document['signatures'][0]['sig'] = '0' * 128
        document['signed']['version'] += 1
        timestamp.write_text(json.dumps(document))
        self.commit('invalid timestamp signature')
        result = self.workflow()
        self.assertNotEqual(0, result.returncode)
        self.assertFalse((self.base / 'trace').exists())

    def test_first_publication_rejects_unapproved_file_classes(self):
        for path in ['.github/workflows/release.yml', 'go.mod', 'go.sum', 'assets/docker-compose.managed.yml', 'tuf/trust.go', 'tuf/repository/metadata/2.root.json', 'tuf/repository/metadata/2.targets.json', 'tuf/repository/targets/extra.txt']:
            with self.subTest(path=path):
                branch = self.git('rev-parse', 'HEAD')
                self.write(path, 'unapproved change')
                self.commit('unapproved publication change')
                result = self.workflow()
                self.assertNotEqual(0, result.returncode, path)
                self.assertFalse((self.base / 'trace').exists())
                self.git('checkout', '-q', '--detach', branch)


class CandidateRetentionTests(unittest.TestCase):
    def check(self, run_change=None, artifact_change=None, missing=False):
        now = datetime.datetime.now(datetime.timezone.utc)
        run = {'id': 42, 'run_attempt': 1, 'head_sha': 'a' * 40, 'head_branch': 'main', 'event': 'workflow_dispatch', 'path': '.github/workflows/release-candidate.yml', 'conclusion': 'success'}
        artifact = {'id': 123, 'name': 'phase-c-candidate-42', 'expired': False, 'created_at': (now - datetime.timedelta(days=1)).isoformat(), 'expires_at': (now + datetime.timedelta(days=29)).isoformat(), 'workflow_run': {'id': 42, 'head_sha': 'a' * 40}}
        run.update(run_change or {})
        artifact.update(artifact_change or {})
        with tempfile.TemporaryDirectory() as stage:
            directory = Path(stage)
            (directory / 'run.json').write_text(json.dumps(run))
            (directory / 'artifacts.json').write_text(json.dumps([{'artifacts': [] if missing else [artifact]}]))
            return subprocess.run([sys.executable, str(HELPER), 'artifact', '--run', str(directory / 'run.json'), '--artifacts', str(directory / 'artifacts.json'), '--run-id', '42', '--source', 'a' * 40], text=True, capture_output=True)

    def test_original_available_candidate_remains_eligible(self):
        result = self.check()
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(123, json.loads(result.stdout)['artifact_id'])

    def test_missing_expired_or_rebuilt_candidates_require_new_acceptance(self):
        for changes in [{'missing': True}, {'artifact_change': {'expired': True}}, {'run_change': {'run_attempt': 2}}, {'run_change': {'head_sha': 'b' * 40}}, {'artifact_change': {'created_at': (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=31)).isoformat()}}]:
            with self.subTest(changes=changes):
                result = self.check(**changes)
                self.assertNotEqual(0, result.returncode, result.stdout)


if __name__ == '__main__':
    unittest.main()

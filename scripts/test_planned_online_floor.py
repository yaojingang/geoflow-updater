"""Isolated review probe: no Docker process, network request, or real signing."""
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).with_name('build-online-rehearsal.py')
SPEC = importlib.util.spec_from_file_location('online_rehearsal', SCRIPT)
fixture = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(fixture)


class OnlineFixtureFloorTest(unittest.TestCase):
    def invoke(self, source_floor, reject=False):
        with tempfile.TemporaryDirectory(prefix='geoflow-fixture-review-') as tmp:
            root = Path(tmp)
            candidate = root / 'candidate'
            targets = candidate / 'targets-source'
            plan = targets / 'releases/3.1.0/upgrade-plan.json'
            plan.parent.mkdir(parents=True)
            (candidate / 'tuf/repository').mkdir(parents=True)
            identity = {'geoflow': {'release_sequence': 12, 'app_digest': 'sha256:' + 'a' * 64}}
            source = {'minimum_updater_protocol': source_floor, 'release_sequence': 12,
                      'app_image': 'ghcr.io/yaojingang/geoflow-app@sha256:' + 'a' * 64,
                      'upgrade_plan_target': 'releases/3.1.0/upgrade-plan.json'}
            (candidate / 'candidate.json').write_text(json.dumps(identity))
            (targets / 'releases/current.json').write_text(json.dumps(source))
            plan.write_text(json.dumps({'strategy': 'maintenance', 'steps': [{'kind': 'migrate'}]}))
            calls = []

            def fake_run(args, **kwargs):
                calls.append(args)
                if args[0] == 'docker':
                    Path(args[args.index('--metadata-file') + 1]).write_text(json.dumps({
                        'containerimage.digest': 'sha256:' + 'b' * 64}))

            cwd = Path.cwd()
            try:
                os.chdir(root)
                env = {'GITHUB_REPOSITORY': 'yaojingang/geoflow-updater',
                       'GITHUB_REF': 'refs/heads/main', 'GITHUB_RUN_ID': '123',
                       'GITHUB_RUN_ATTEMPT': '1', 'RUNNER_TEMP': tmp}
                with patch.dict(os.environ, env), patch.object(fixture.subprocess, 'run', side_effect=fake_run):
                    if reject:
                        with self.assertRaises((AssertionError, ValueError, TypeError)):
                            fixture.main()
                        self.assertEqual([], calls, 'invalid floor reached build or signing')
                        return None
                    fixture.main()
                result = json.loads((candidate / 'online-fixture/targets-source/releases/current.json').read_text())
                self.assertEqual(['docker', 'go'], [args[0] for args in calls])
                self.assertEqual('online', json.loads((candidate / 'online-fixture/targets-source/releases/3.1.0/upgrade-plan.json').read_text())['strategy'])
                return result['minimum_updater_protocol']
            finally:
                os.chdir(cwd)

    def test_legacy_floors_keep_online_protocol_four(self):
        for floor in (3, 4):
            with self.subTest(floor=floor):
                self.assertEqual(4, self.invoke(floor))

    def test_coordinated_core_keeps_protocol_five(self):
        self.assertEqual(5, self.invoke(5))

    def test_invalid_floor_is_rejected_before_build_or_sign(self):
        for floor in (True, False, 0, 2, 6, 5.0, '5', None):
            with self.subTest(floor=floor):
                self.invoke(floor, reject=True)


if __name__ == '__main__':
    unittest.main()

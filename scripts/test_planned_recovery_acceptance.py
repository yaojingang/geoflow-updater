"""Regressions for native maintenance acceptance under protocol 5."""
import importlib.util
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch


spec = importlib.util.spec_from_file_location('planned_host', Path(__file__).with_name('planned-host-rehearsal.py'))
host = importlib.util.module_from_spec(spec)
spec.loader.exec_module(host)


class ReachedSignedBaseline(Exception):
    pass


class PlannedRecoveryAcceptanceTests(unittest.TestCase):
    def rehearsal(self, scenario='upgrade'):
        rehearsal = host.Rehearsal.__new__(host.Rehearsal)
        rehearsal.args = SimpleNamespace(scenario=scenario)
        rehearsal.candidate = Path('/candidate')
        rehearsal.manifest = {'release_sequence': 4}
        return rehearsal

    def test_official_schema_three_310_baseline_reaches_runtime(self):
        rehearsal = self.rehearsal()
        rehearsal.repository = Mock(side_effect=ReachedSignedBaseline)
        baseline = {
            'schema_version': 3, 'minimum_updater_protocol': 3, 'release_sequence': 3, 'version': '3.1.0',
            'source_commit': '6c963783bbf49f0b5ec9d0121a924ee54005b60d',
            'app_image': 'ghcr.io/yaojingang/geoflow-app@sha256:94c01bfd52941cf48ad9693fce30fc571c29353189fb5e128d0e0752daa36448',
            'upgrade_plan_target': 'releases/3.1.0/upgrade-plan.json',
        }
        with tempfile.TemporaryDirectory() as directory:
            manifest = Path(directory) / 'current.json'
            manifest.write_text(json.dumps(baseline))
            with patch.object(host, 'target', return_value=manifest):
                with self.assertRaises(ReachedSignedBaseline):
                    rehearsal.legacy()
                manifest.write_text(json.dumps({**baseline, 'source_commit': 'f' * 40}))
                with self.assertRaisesRegex(RuntimeError, 'official Core 3.1.0'):
                    rehearsal.legacy()
        rehearsal.repository.assert_called_once()

    def test_a_crash_scenario_uses_one_clean_baseline_and_stops_after_restoration(self):
        for scenario, expected in [('crash-upgrade', ('upgrade', False)),
                                   ('crash-recovery-backoff', ('upgrade', True))]:
            with self.subTest(scenario=scenario):
                rehearsal = self.rehearsal(scenario)
                rehearsal.legacy = Mock()
                rehearsal.repository = Mock()
                rehearsal.crash = Mock()
                rehearsal.fixture = Mock(side_effect=AssertionError('Restored VM must not start another upgrade'))
                rehearsal.upgrade()
                rehearsal.legacy.assert_called_once_with()
                rehearsal.repository.assert_called_once_with(Path('/candidate/tuf/repository'))
                self.assertEqual(len(rehearsal.crash.call_args_list), 1)
                arguments, keywords = rehearsal.crash.call_args
                self.assertEqual((arguments[0], keywords.get('block_restore', False)), expected)
                rehearsal.fixture.assert_not_called()

    def test_restoration_checks_held_boundary_instead_of_full_service_health(self):
        rehearsal = self.rehearsal()
        rehearsal.query = Mock(side_effect=['baseline', 'original migrations'])
        rehearsal.redis = Mock(return_value='baseline')
        rehearsal.baseline_hashes = {}
        rehearsal.migrations = 'original migrations'
        rehearsal.healthy = Mock(side_effect=AssertionError('Doctor requires intentionally held workers'))
        rehearsal.verify_recovery_hold = Mock()
        rehearsal.record = Mock()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            marker = root / 'storage/app/rehearsal.txt'
            marker.parent.mkdir(parents=True)
            marker.write_text('baseline')
            with patch.object(host, 'ROOT', root):
                rehearsal.restored('manual-rollback')
        rehearsal.verify_recovery_hold.assert_called_once_with('manual-rollback')
        rehearsal.healthy.assert_not_called()

    def test_held_boundary_requires_http_only_and_rejects_restarted_background_roles(self):
        rehearsal = self.rehearsal()
        state = {'epoch': 'a' * 32}
        caps = {'updater_protocol': 5, 'protocol_version': 2, 'background_status': 'held', 'actions': ['restore'],
                'recovery': {'phase': 'http_ready', 'epoch': state['epoch']}}
        rehearsal.api = Mock(return_value=caps)
        rehearsal.run = Mock(return_value='app\nweb\npostgres\nredis\nedge')
        rehearsal.public_health = Mock()
        rehearsal.assert_background_held(state)
        rehearsal.public_health.assert_called_once_with()
        rehearsal.api.assert_called_once_with('GET', 'capabilities', protocol=2)
        for role in ['queue', 'ai-optimization-queue', 'scheduler', 'reverb']:
            with self.subTest(role=role):
                rehearsal.run.return_value = 'app\nweb\npostgres\nredis\n' + role
                with self.assertRaisesRegex(RuntimeError, 'reopened background services'):
                    rehearsal.assert_background_held(state)
        rehearsal.api.return_value = {**caps, 'actions': ['update', 'restore']}
        with self.assertRaisesRegex(RuntimeError, 'did not expose the held recovery boundary'):
            rehearsal.assert_background_held(state)


if __name__ == '__main__':
    unittest.main()

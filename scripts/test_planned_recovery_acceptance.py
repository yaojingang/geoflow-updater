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
docker_spec = importlib.util.spec_from_file_location(
    'planned_docker', Path(__file__).with_name('planned-rehearsal-docker.py'))
docker = importlib.util.module_from_spec(docker_spec)
docker_spec.loader.exec_module(docker)


class ReachedSignedBaseline(Exception):
    pass


class ReachedExplicitRestore(Exception):
    pass


class PlannedRecoveryAcceptanceTests(unittest.TestCase):
    def rehearsal(self, scenario='upgrade'):
        rehearsal = host.Rehearsal.__new__(host.Rehearsal)
        rehearsal.args = SimpleNamespace(scenario=scenario)
        rehearsal.candidate = Path('/candidate')
        rehearsal.manifest = {'release_sequence': 4}
        return rehearsal

    def test_pg_restore_audit_records_blocked_and_permitted_attempts(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory)
            authority = state / 'recovery-control/primary/authority.json'
            authority.parent.mkdir(parents=True)
            authority.write_text(json.dumps({'state': {'transaction_id': 'rollback-id', 'epoch': 'generation'},
                                             'point_id': 'update-checkpoint'}))
            fault = state / 'planned-rehearsal-block-restore'
            fault.touch()
            arguments = ['docker', 'compose', 'exec', '-T', 'postgres', 'sh', '-c', 'exec pg_restore --exit-on-error']
            with (patch.object(docker, 'STATE', state),
                  patch.object(docker, 'FAULT', state / 'unarmed'),
                  patch.object(docker.sys, 'argv', arguments),
                  patch.object(docker.os, 'execv') as execute):
                self.assertEqual(docker.main(), 98)
                execute.assert_not_called()
                fault.unlink()
                docker.main()
                execute.assert_called_once_with('/usr/bin/docker', ['/usr/bin/docker', *arguments[1:]])
            audit = [json.loads(line) for line in (state / 'planned-rehearsal-restore-attempts.jsonl').read_text().splitlines()]
            self.assertEqual(audit, [{'transaction_id': 'rollback-id', 'epoch': 'generation',
                                      'point_id': 'update-checkpoint', 'blocked': blocked}
                                     for blocked in (True, False)])

    def test_restart_compares_operation_content_and_rejects_a_restore_replay(self):
        original = {'id': 'explicit-restore', 'status': 'succeeded', 'stages': [{'name': 'verify', 'status': 'succeeded'}]}
        serialized = json.loads(json.dumps(original, separators=(',', ':'), sort_keys=True))
        for outcome, second, attempts in [
            ('same', serialized, [[], []]),
            ('changed', {**serialized, 'id': 'different-restore'}, [[], []]),
            ('replayed', serialized, [[], [{'transaction_id': original['id']}]]),
        ]:
            with self.subTest(outcome=outcome):
                rehearsal = self.rehearsal()
                rehearsal.api = Mock(side_effect=[original, second])
                rehearsal.recovery_state = Mock(return_value={'phase': 'http_ready', 'epoch': 'restored'})
                rehearsal.restore_attempts = Mock(side_effect=attempts)
                rehearsal.run = Mock()
                rehearsal.wait_for_agent = Mock()
                rehearsal.assert_background_held = Mock()
                rehearsal.record = Mock()
                if outcome == 'same':
                    rehearsal.verify_after_restart()
                    rehearsal.assert_background_held.assert_called_once()
                else:
                    with self.assertRaisesRegex(RuntimeError, 'repeated or changed generation'):
                        rehearsal.verify_after_restart()

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

    def test_interrupted_upgrade_verifies_the_original_hold_before_authorizing_the_exact_point(self):
        for stage, checkpoint in [('backup', None), ('upgrade', 'update-point')]:
            with self.subTest(stage=stage), tempfile.TemporaryDirectory() as directory:
                rehearsal = self.rehearsal('crash-' + stage)
                rehearsal.manifest['version'] = '3.2.1'
                state = Path(directory)
                marker = state / 'marker'
                (state / 'release-transaction.json').write_text(json.dumps({
                    'operation_id': 'update-id', 'stage': stage, 'traffic_opened': False,
                    'recovery_point_id': checkpoint, 'status': 'recovery_required',
                }))
                source_recovery = {'phase': 'ready', 'epoch': 'source-epoch'}
                (state / 'authority.json').write_text(json.dumps({'state': source_recovery}))
                held = {'id': 'update-id', 'kind': 'update', 'status': 'recovery_required',
                        'recovery_point_id': checkpoint, 'completed_at': 'now'}
                events = []
                rehearsal.source_recovery_state = source_recovery
                rehearsal.fixture = Mock(side_effect=lambda **_: events.append('fixture'))
                rehearsal.preview = Mock(return_value={'allow_maintenance': True})
                rehearsal.mutate = Mock(side_effect=lambda *args, **kwargs: (
                    events.append('manual-backup') or {'kind': 'backup', 'recovery_point_id': 'manual-point'}))
                rehearsal.verify_legacy_point = Mock(side_effect=lambda point, reason: events.append((point, reason)))
                rehearsal.corrupt = Mock(side_effect=lambda *_: events.append('change-fixture'))
                rehearsal.assert_fixture_values = Mock()
                rehearsal.recovery_state = Mock(return_value=source_recovery)
                rehearsal.run = Mock()
                rehearsal.save = Mock()
                rehearsal.record = Mock()
                rehearsal.restore_attempts = Mock(return_value=[])
                rehearsal.wait = Mock(side_effect=lambda predicate, *_: predicate())

                def request(method, endpoint, *_, expected=200, **kwargs):
                    if (method, endpoint, expected) == ('POST', 'updates', 202):
                        marker.touch()
                        events.append('start-upgrade')
                        return {'id': held['id']}
                    if method == 'POST' and expected == 409:
                        events.append('block-' + endpoint)
                        return {'error': 'operation_active'}
                    if (method, endpoint) == ('GET', 'operations/current'):
                        return held
                    self.fail(f'Unexpected request {method} {endpoint} {expected}')

                def operation(identity, statuses, _label):
                    self.assertEqual((identity, statuses), ('update-id', ['recovery_required']))
                    events.append('held')
                    return held

                rehearsal.api = Mock(side_effect=request)
                rehearsal.operation = Mock(side_effect=operation)
                rehearsal.explicit_rollback = Mock(side_effect=ReachedExplicitRestore)
                with (patch.multiple(host, INSTANCE=state, CONTROL=state, MARKER=marker, FAULT=state / 'fault'),
                      self.assertRaises(ReachedExplicitRestore)):
                    rehearsal.crash(stage)
                point = checkpoint or 'manual-point'
                if checkpoint:
                    self.assertNotIn('manual-backup', events)
                    self.assertIn((point, 'update-to-3.2.1'), events)
                    self.assertLess(events.index('start-upgrade'), events.index('change-fixture'))
                else:
                    self.assertEqual(events[:5], ['fixture', 'manual-backup', (point, 'manual-backup'),
                                                  'change-fixture', 'start-upgrade'])
                self.assertLess(events.index('held'), events.index('block-backups'))
                self.assertIn('block-updates', events)
                rehearsal.explicit_rollback.assert_called_once_with(point, 'recover-' + stage, manual=not checkpoint)

    def test_manual_restore_uses_the_installed_host_cli_and_update_checkpoint_uses_scoped_authorization(self):
        rehearsal = self.rehearsal()
        result = {'id': 'restore-id', 'kind': 'rollback', 'status': 'succeeded'}
        rehearsal.run = Mock(return_value=json.dumps(result))
        rehearsal.mutate = Mock(return_value=result)
        rehearsal.save = Mock()
        self.assertEqual(rehearsal.explicit_rollback('manual-point', 'manual', manual=True), result)
        rehearsal.run.assert_called_once_with('geoflow-updater', 'rollback', '--instance', 'primary',
                                              '--recovery-point', 'manual-point', '--json')
        rehearsal.mutate.assert_not_called()
        self.assertEqual(rehearsal.explicit_rollback('update-point', 'checkpoint', manual=False), result)
        rehearsal.mutate.assert_called_once_with('rollbacks', 'rollback', {'recovery_point_id': 'update-point'}, 'checkpoint')

    def test_manual_point_checks_every_component_before_use(self):
        rehearsal = self.rehearsal()
        rehearsal.save = Mock()
        point = '20261110T120000Z-1234abcd'
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'site'
            state = Path(directory) / 'state'
            backups = Path(directory) / 'backups'
            backup = backups / point
            (backup / 'managed').mkdir(parents=True)
            files = {}
            paths = {'site.env': root / '.env.prod', 'version.json': root / 'version.json',
                     'managed/instance.yml': state / 'instance.yml', 'managed/release.env': state / 'release.env',
                     'managed/docker-compose.yml': state / 'docker-compose.managed.yml'}
            for name in set(paths) | {'database.dump', 'redis.tar.gz', 'storage.tar.gz'}:
                path = backup / name
                path.write_text(name)
                files[name] = {'size': len(name), 'sha256': host.sha(path)}
            rehearsal.baseline_hashes = {str(path): files[name]['sha256'] for name, path in paths.items()}
            (backup / 'manifest.json').write_text(json.dumps({'schema_version': 1, 'id': point, 'instance_id': 'primary',
                'root': str(root), 'reason': 'manual-backup', 'release_sequence': 3, 'version': '3.1.0',
                'deployment': {}, 'files': files}))
            with patch.multiple(host, ROOT=root, INSTANCE=state, BACKUPS=backups):
                rehearsal.verify_legacy_point(point, 'manual-backup')
                (backup / 'database.dump').write_text('Database.dump')
                with self.assertRaisesRegex(RuntimeError, 'component checksum or length differs: database.dump'):
                    rehearsal.verify_legacy_point(point, 'manual-backup')

    def test_backoff_observes_reconciliation_after_restart_without_a_second_pg_restore(self):
        for replay in [False, True]:
            with self.subTest(replay=replay), tempfile.TemporaryDirectory() as directory:
                rehearsal = self.rehearsal('crash-recovery-backoff')
                fault = Path(directory) / 'restore-fault'
                failed = {'id': 'failed-rollback-id', 'kind': 'rollback', 'recovery_point_id': 'update-point',
                          'status': 'recovery_required', 'completed_at': 'time'}
                pending = {**failed, 'reconcile_attempts': 1, 'next_reconcile_at': '2026-11-10T12:00:30Z'}
                restarted = {**failed, 'reconcile_attempts': 2, 'next_reconcile_at': '2026-11-10T12:01:30Z'}
                repeated = {**failed, 'reconcile_attempts': 3, 'next_reconcile_at': '2026-11-10T12:03:30Z'}
                attempt = {'transaction_id': failed['id'], 'epoch': 'new-epoch',
                           'point_id': failed['recovery_point_id'], 'blocked': True}
                reconciliation = iter([pending, restarted, restarted, repeated])
                state = {'phase': 'restoring', 'transaction_id': failed['id'], 'epoch': 'new-epoch'}
                rehearsal.source_recovery_state = {'epoch': 'source-epoch'}
                rehearsal.recovery_state = Mock(return_value=state)
                rehearsal.restore_attempts = Mock(side_effect=[[attempt], [attempt] + ([{**attempt, 'blocked': False}] if replay else [])])
                rehearsal.operation = Mock(return_value=failed)
                rehearsal.wait = Mock(side_effect=lambda predicate, *_, **__: predicate() or predicate())
                rehearsal.wait_for_agent = Mock()
                rehearsal.save = Mock()
                rehearsal.record = Mock()
                rehearsal.assert_mutations_blocked = Mock()

                def request(method, endpoint, *args, expected=200):
                    if (method, endpoint, expected) == ('POST', 'rollbacks', 202):
                        self.assertTrue(fault.exists())
                        self.assertEqual(args, ({'recovery_point_id': 'update-point'}, 'rollback'))
                        return {'id': failed['id']}
                    self.assertEqual((method, endpoint), ('GET', 'operations/current'))
                    return next(reconciliation)

                def restart(*args):
                    self.assertEqual(args, ('systemctl', 'restart', 'geoflow-updater'))
                    self.assertFalse(fault.exists())

                rehearsal.api = Mock(side_effect=request)
                rehearsal.run = Mock(side_effect=restart)
                with patch.object(host, 'RESTORE_FAULT', fault):
                    if replay:
                        with self.assertRaisesRegex(RuntimeError, 'attempted PostgreSQL restoration'):
                            rehearsal.exercise_restore_backoff('update-point', 'update-id', {})
                        rehearsal.record.assert_not_called()
                    else:
                        self.assertEqual(rehearsal.exercise_restore_backoff('update-point', 'update-id', {}), failed)
                        self.assertEqual(rehearsal.record.call_args.args[0], 'rollback-backoff-held')
                rehearsal.operation.assert_called_once_with(failed['id'], ['recovery_required'], 'failed-explicit-rollback')
                self.assertFalse(fault.exists())

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

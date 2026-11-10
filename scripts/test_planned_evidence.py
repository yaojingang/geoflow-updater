"""Reject incomplete or mixed-candidate publication evidence."""
import base64
import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('planned_evidence', Path(__file__).with_name('planned-evidence.py'))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


def complete():
    candidate = {'candidate_run_id': '123', 'updater': {'commit': 'a' * 40}}
    evidence = {'schema_version': 2, 'candidate': candidate, 'harness_commit': candidate['updater']['commit'],
                'verification_scope': 'planned-container-contract-and-ingress',
                'installed_host_scope': gate.SCOPE, 'architectures': {},
                'approvals': {role: {'name': role, 'decision': 'approved'} for role in
                              ['release_operator', 'security_reviewer', 'product_owner']}}
    for platform, kernel in gate.PLATFORMS.items():
        common = {'schema_version': 1, 'candidate_run_id': '123', 'platform': platform,
                  'kernel_arch': kernel, 'result': 'pass', 'harness_commit': evidence['harness_commit']}
        evidence['architectures'][platform] = {
            'result': 'pass', 'evidence_link': 'https://github.com/yaojingang/geoflow-updater/actions/runs/456',
            'container': {**common, 'verification_scope': gate.CONTAINER_SCOPE, 'checks': sorted(gate.CONTAINER_CHECKS)},
            'hosts': {scenario: {**common, 'mode': 'upgrade' if scenario.startswith('crash-') else scenario,
                                'scenario': scenario, 'checks': [{'id': name, 'status': 'pass'} for name in sorted(required)]}
                      for scenario, required in gate.HOST_CHECKS.items()}}
    return evidence, copy.deepcopy(candidate)


class EvidenceTests(unittest.TestCase):
    def test_complete_same_candidate(self):
        gate.validate(*complete(), approved=True)

    def test_harness_only_rerun_can_report_but_cannot_authorize_publication(self):
        evidence, candidate = complete()
        evidence['harness_commit'] = 'b' * 40
        for architecture in evidence['architectures'].values():
            for result in [architecture['container'], *architecture['hosts'].values()]:
                result['harness_commit'] = evidence['harness_commit']
        gate.validate(evidence, candidate)
        with self.assertRaisesRegex(ValueError, 'exact candidate harness commit'):
            gate.validate(evidence, candidate, approved=True)

    def test_same_version_enrollment_cannot_be_omitted(self):
        for platform in ['linux-amd64', 'linux-arm64']:
            with self.subTest(platform=platform):
                evidence, candidate = complete()
                evidence['architectures'][platform]['hosts'].pop('enrollment', None)
                with self.assertRaises((ValueError, KeyError)):
                    gate.validate(evidence, candidate, approved=True)

    def test_enrollment_requires_the_complete_restored_identity_lifecycle(self):
        required = ['native-candidate-install', 'unmanaged-candidate-site', 'legacy-enrollment', 'session-login',
                    'same-sequence-enrollment', 'same-sequence-layout-conversion', 'restored-enrollment-legacy',
                    'same-sequence-enrollment-retry-blocked', 'enrollment-repeat-rejected',
                    'protocol-5-credentials-invalidated', 'protocol-5-read-only', 'protocol-5-hold-after-restart']
        for platform in ['linux-amd64', 'linux-arm64']:
            for missing in required:
                with self.subTest(platform=platform, missing=missing):
                    evidence, candidate = complete()
                    enrollment = evidence['architectures'][platform]['hosts']['enrollment']
                    enrollment['checks'] = [check for check in enrollment['checks'] if check['id'] != missing]
                    with self.assertRaisesRegex(ValueError, 'Required host checks missing'):
                        gate.validate(evidence, candidate, approved=True)

    def test_enrollment_identity_and_native_execution_are_bound(self):
        for platform, other in [('linux-amd64', 'linux-arm64'), ('linux-arm64', 'linux-amd64')]:
            for key, wrong in [('platform', other), ('kernel_arch', gate.PLATFORMS[other]),
                               ('candidate_run_id', '122'), ('mode', 'upgrade'), ('result', 'fail')]:
                with self.subTest(platform=platform, key=key):
                    evidence, candidate = complete()
                    evidence['architectures'][platform]['hosts']['enrollment'][key] = wrong
                    with self.assertRaisesRegex(ValueError, 'Host identity differs'):
                        gate.validate(evidence, candidate, approved=True)

    def test_cli_requires_production_pair_acceptance_for_online_plans(self):
        evidence, candidate = complete()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'candidate.json').write_text(json.dumps(candidate))
            (root / 'evidence.json').write_text(json.dumps(evidence))
            for strategy, expected in [('maintenance', 0), ('online', 1)]:
                (root / 'plan.json').write_text(json.dumps({'strategy': strategy}))
                result = subprocess.run([sys.executable, str(Path(__file__).with_name('planned-evidence.py')),
                    'validate', '--candidate', str(root / 'candidate.json'), '--evidence', str(root / 'evidence.json'),
                    '--plan', str(root / 'plan.json'), '--approved'], capture_output=True, text=True)
                self.assertEqual(result.returncode, expected, result.stderr)
                if strategy == 'online':
                    self.assertIn('source-to-target compatibility acceptance', result.stderr)

    def test_fail_closed(self):
        mutations = {
            'omitted architecture': lambda e: e['architectures'].pop('linux-arm64'),
            'omitted installed host': lambda e: e['architectures']['linux-amd64'].pop('hosts'),
            'missing restoration': lambda e: e['architectures']['linux-amd64']['hosts']['upgrade']['checks'].clear(),
            'mixed candidate': lambda e: e['architectures']['linux-arm64']['hosts']['upgrade'].update(candidate_run_id='122'),
            'emulated architecture': lambda e: e['architectures']['linux-arm64']['hosts']['install'].update(kernel_arch='x86_64'),
            'failed case': lambda e: e['architectures']['linux-amd64']['hosts']['upgrade']['checks'][0].update(status='fail'),
            'swapped mode': lambda e: e['architectures']['linux-amd64']['hosts']['install'].update(mode='upgrade'),
            'omitted isolated crash scenario': lambda e: e['architectures']['linux-arm64']['hosts'].pop('crash-layout'),
            'wrong crash scenario': lambda e: e['architectures']['linux-arm64']['hosts']['crash-layout'].update(scenario='crash-upgrade'),
            'mixed harness': lambda e: e['architectures']['linux-arm64']['hosts']['crash-upgrade'].update(harness_commit='c' * 40),
            'missing hold evidence': lambda e: e['architectures']['linux-amd64']['hosts']['crash-observe'].update(
                checks=[{'id': key, 'status': 'pass'} for key in sorted(gate.HOST_CHECKS['crash-observe'] - gate.HELD_CHECKS)]),
            'contradictory hold evidence': lambda e: e['architectures']['linux-amd64']['hosts']['crash-workers']['checks'].append(
                {'id': 'pre-checkpoint-resumed', 'status': 'pass'}),
            'different source': lambda e: e['candidate']['updater'].update(commit='b' * 40),
            'pending approval': lambda e: e['approvals']['product_owner'].update(decision='pending'),
            'blank reviewer': lambda e: e['approvals']['security_reviewer'].update(name='  '),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                evidence, candidate = complete()
                mutate(evidence)
                with self.assertRaises((ValueError, KeyError)):
                    gate.validate(evidence, candidate, approved=True)

    def test_crash_evidence_requires_an_explicit_held_restore_on_both_sides_of_the_checkpoint(self):
        required_by_scenario = {
            'crash-retain-assets': {'manual-recovery-point'},
            'crash-quiesce': {'manual-recovery-point'},
            'crash-scheduler-freeze': {'manual-recovery-point', 'legacy-scheduler-fixture', 'scheduler-frozen-at-interruption'},
            'crash-backup': {'manual-recovery-point'},
            'crash-upgrade': {'update-recovery-point'},
            'crash-layout': {'update-recovery-point'},
            'crash-candidate': {'update-recovery-point'},
            'crash-switch': {'update-recovery-point'},
            'crash-workers': {'update-recovery-point'},
            'crash-observe': {'update-recovery-point'},
            'crash-recovery-backoff': {'update-recovery-point', 'rollback-backoff-held'},
        }
        for scenario, specific in required_by_scenario.items():
            required = specific | {'maintenance-interruption-held', 'explicit-data-restore',
                                   'protocol-5-hold-after-restart', 'protocol-5-credentials-invalidated'}
            for missing in sorted(required):
                with self.subTest(scenario=scenario, missing=missing):
                    evidence, candidate = complete()
                    checks = evidence['architectures']['linux-arm64']['hosts'][scenario]['checks']
                    checks[:] = [check for check in checks if check['id'] != missing]
                    with self.assertRaisesRegex(ValueError, 'Required host checks missing'):
                        gate.validate(evidence, candidate, approved=True)

    def test_assembly_uses_all_native_reports_and_keeps_private_diagnostics_out_of_approval(self):
        evidence, candidate = complete()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)

            def write(path, value):
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(json.dumps(value))

            write(root / 'candidate/candidate.json', candidate)
            for platform, architecture in evidence['architectures'].items():
                write(root / f'architectures/planned-acceptance-{platform}/result.json', architecture['container'])
                for scenario, result in architecture['hosts'].items():
                    for check in result['checks']:
                        check['evidence'] = 'private diagnostics stay in the native artifact'
                    write(root / f'hosts/planned-host-{platform}-{scenario}/result.json', result)
            assembled = gate.assemble(root, 'https://github.com/yaojingang/geoflow-updater/actions/runs/456',
                                      evidence['harness_commit'])
            self.assertNotIn('private diagnostics', json.dumps(assembled))
            self.assertEqual(sum(len(architecture['hosts']) for architecture in assembled['architectures'].values()), 30)
            for approval in assembled['approvals'].values():
                approval.update(name='Named release reviewer', decision='approved')
            gate.validate(assembled, candidate, approved=True)
            encoded = base64.b64encode((json.dumps(assembled, separators=(',', ':')) + '\n').encode())
            self.assertLess(len(encoded), gate.GITHUB_SECRET_LIMIT)


if __name__ == '__main__':
    unittest.main()

"""Reject incomplete or mixed-candidate publication evidence."""
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
    evidence = {'schema_version': 1, 'candidate': candidate,
                'verification_scope': 'planned-container-contract-and-ingress',
                'installed_host_scope': gate.SCOPE, 'architectures': {},
                'approvals': {role: {'name': role, 'decision': 'approved'} for role in
                              ['release_operator', 'security_reviewer', 'product_owner']}}
    for platform, kernel in gate.PLATFORMS.items():
        common = {'schema_version': 1, 'candidate_run_id': '123', 'platform': platform,
                  'kernel_arch': kernel, 'result': 'pass'}
        evidence['architectures'][platform] = {
            'result': 'pass', 'evidence_link': 'https://github.com/yaojingang/geoflow-updater/actions/runs/456',
            'container': {**common, 'checks': sorted(gate.CONTAINER_CHECKS)},
            'hosts': {mode: {**common, 'mode': mode,
                            'checks': [{'id': name, 'status': 'pass'} for name in sorted(required)]}
                      for mode, required in gate.HOST_CHECKS.items()}}
    return evidence, copy.deepcopy(candidate)


class EvidenceTests(unittest.TestCase):
    def test_complete_same_candidate(self):
        gate.validate(*complete(), approved=True)

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


if __name__ == '__main__':
    unittest.main()

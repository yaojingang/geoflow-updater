#!/usr/bin/env python3
"""Assemble and validate candidate-bound native acceptance evidence."""
import argparse
import base64
import json
from pathlib import Path
import re

PLATFORMS = {'linux-amd64': 'x86_64', 'linux-arm64': 'aarch64'}
SCOPE = 'protocol-5-maintenance-upgrade-explicit-held-restore-isolated-crashes-install-enrollment-and-online-fixture'
CONTAINER_SCOPE = 'planned-container-contract-and-ingress'
GITHUB_SECRET_LIMIT = 48_000
CONTAINER_CHECKS = {'signed-image-identity', 'fresh-migrations', 'first-install', 'backfills',
                    'cache-compilation', 'standalone-readiness', 'install-idempotency',
                    'real-ingress-switch-and-stream'}
HELD_CHECKS = {'protocol-5-redis-quarantined', 'protocol-5-background-held',
               'protocol-5-credentials-invalidated', 'protocol-5-read-only', 'protocol-5-hold-after-restart'}
LEGACY_CHECKS = {'native-candidate-install', 'legacy-enrollment', 'official-310-baseline', 'session-login'}
PRE_CHECKPOINT = {'retain-assets', 'quiesce', 'scheduler-freeze', 'backup'}
POST_CHECKPOINT = {'upgrade', 'layout', 'candidate', 'switch', 'workers', 'observe', 'recovery-backoff'}
CRASH_CHECKS = LEGACY_CHECKS | HELD_CHECKS | {'maintenance-interruption-held', 'explicit-data-restore'}
HOST_CHECKS = {
    'enrollment': {'native-candidate-install', 'unmanaged-candidate-site', 'legacy-enrollment', 'session-login',
                   'same-sequence-enrollment', 'same-sequence-layout-conversion', 'restored-enrollment-legacy',
                   'same-sequence-enrollment-retry-blocked', 'enrollment-repeat-rejected'} | HELD_CHECKS,
    'online': {'native-candidate-install', 'session-login', 'fresh-install-retry',
               'online-upgrade', 'online-http-session', 'online-queue-handover',
               'online-reverb-cross-slot', 'online-reverb-reconnect', 'online-switch-back-preserves-data'},
    'install': {'native-candidate-install', 'session-login', 'fresh-install-retry'},
    'upgrade': LEGACY_CHECKS | HELD_CHECKS | {'session-after-upgrade', 'signed-upgrade', 'complete-backup-restore',
                                            'restored-manual-rollback'},
    **{'crash-' + stage: CRASH_CHECKS | {'crash-' + stage, 'restored-' + stage, 'manual-recovery-point'}
       for stage in sorted(PRE_CHECKPOINT)},
    **{'crash-' + stage: CRASH_CHECKS | {'crash-' + stage, 'restored-' + stage, 'update-recovery-point'}
       for stage in sorted(POST_CHECKPOINT)},
}
HOST_CHECKS['crash-scheduler-freeze'].add('scheduler-frozen-at-interruption')
HOST_CHECKS['crash-recovery-backoff'].add('rollback-backoff-held')


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read(path):
    return json.loads(Path(path).read_text())


def validate(evidence, candidate, approved=False):
    require(evidence['schema_version'] == 2 and evidence['candidate'] == candidate, 'Candidate identity differs')
    require(evidence['verification_scope'] == CONTAINER_SCOPE, 'Container scope missing')
    require(evidence['installed_host_scope'] == SCOPE, 'Installed-host scope missing')
    commit = evidence['harness_commit']
    require(isinstance(commit, str) and re.fullmatch(r'[a-f0-9]{40}|[a-f0-9]{64}', commit), 'Harness commit is invalid')
    if approved:
        require(commit == candidate['updater']['commit'], 'Approved evidence must use the exact candidate harness commit')
    run_id = str(candidate['candidate_run_id'])
    require(set(evidence['architectures']) == set(PLATFORMS), 'Both native architectures are required')
    for platform, kernel in PLATFORMS.items():
        architecture = evidence['architectures'][platform]
        require(architecture['result'] == 'pass' and architecture['evidence_link'].startswith(
            'https://github.com/yaojingang/geoflow-updater/actions/runs/'), 'Architecture evidence missing')
        result = architecture['container']
        require(result['schema_version'] == 1 and result['platform'] == platform and result['kernel_arch'] == kernel and
                str(result['candidate_run_id']) == run_id and result['result'] == 'pass' and
                result['verification_scope'] == CONTAINER_SCOPE and result['harness_commit'] == commit,
                'Container identity differs')
        require(CONTAINER_CHECKS <= set(result['checks']), 'Required container checks missing')
        require(set(architecture['hosts']) == set(HOST_CHECKS), 'A required isolated host scenario is missing')
        for scenario, required in HOST_CHECKS.items():
            host = architecture['hosts'][scenario]
            mode = 'upgrade' if scenario.startswith('crash-') else scenario
            require(host['schema_version'] == 1 and host['platform'] == platform and
                    host['kernel_arch'] == kernel and host['mode'] == mode and
                    host['scenario'] == scenario and host['harness_commit'] == commit and
                    str(host['candidate_run_id']) == run_id and host['result'] == 'pass', 'Host identity differs')
            checks = host['checks']
            require(all(check['status'] == 'pass' for check in checks), 'Host check failed')
            ids = {check['id'] for check in checks}
            require(len(ids) == len(checks), 'A host check was reported more than once')
            require(required <= ids, 'Required host checks missing')
            require('pre-checkpoint-resumed' not in ids and not {'manual-recovery-point', 'update-recovery-point'} <= ids,
                    'Recovery results contradict each other')
    if approved:
        for role in ['release_operator', 'security_reviewer', 'product_owner']:
            approval = evidence['approvals'][role]
            require(approval['decision'] == 'approved' and approval['name'].strip(), 'Approval missing: ' + role)


def compact_host(path):
    result = read(path)
    # Full diagnostics remain in the native job artifact. Keep the approved
    # evidence small enough for the protected GitHub environment secret.
    result['checks'] = [{key: check[key] for key in ('id', 'status')} for check in result['checks']]
    return result


def assemble(root, run_url, harness_commit):
    candidate = read(root / 'candidate/candidate.json')
    evidence = {
        'schema_version': 2, 'candidate': candidate, 'harness_commit': harness_commit,
        'verification_scope': CONTAINER_SCOPE, 'installed_host_scope': SCOPE,
        'limitations': [
            'Full restoration ends at HTTP readiness with background work held.',
            'The online fixture uses identical application code. Production online pairs need separate compatibility acceptance.',
        ],
        'architectures': {},
        'approvals': {role: {'name': '', 'decision': 'pending'} for role in
                      ['release_operator', 'security_reviewer', 'product_owner']},
    }
    for platform in PLATFORMS:
        evidence['architectures'][platform] = {
            'result': 'pass', 'evidence_link': run_url,
            'container': read(root / f'architectures/planned-acceptance-{platform}/result.json'),
            'hosts': {scenario: compact_host(root / f'hosts/planned-host-{platform}-{scenario}/result.json')
                      for scenario in HOST_CHECKS},
        }
    validate(evidence, candidate)
    return evidence


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    build = commands.add_parser('assemble')
    build.add_argument('--root', type=Path, required=True)
    build.add_argument('--run-url', required=True)
    build.add_argument('--harness-commit', required=True)
    build.add_argument('--output', type=Path, required=True)
    check = commands.add_parser('validate')
    check.add_argument('--candidate', type=Path, required=True)
    check.add_argument('--evidence', type=Path, required=True)
    check.add_argument('--approved', action='store_true')
    check.add_argument('--plan', type=Path, required=True)
    args = parser.parse_args()
    if args.command == 'assemble':
        evidence = assemble(args.root, args.run_url, args.harness_commit)
        payload = (json.dumps(evidence, separators=(',', ':')) + '\n').encode()
        require(len(base64.b64encode(payload)) < GITHUB_SECRET_LIMIT, 'Encoded evidence exceeds the GitHub secret size limit')
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_bytes(payload)
    else:
        require(read(args.plan)['strategy'] == 'maintenance',
                'Production online plans require source-to-target compatibility acceptance; the same-application fixture is insufficient.')
        validate(read(args.evidence), read(args.candidate), args.approved)


if __name__ == '__main__':
    main()

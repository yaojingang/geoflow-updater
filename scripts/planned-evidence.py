#!/usr/bin/env python3
"""Assemble and validate candidate-bound native acceptance evidence."""
import argparse
import json
from pathlib import Path

PLATFORMS = {'linux-amd64': 'x86_64', 'linux-arm64': 'aarch64'}
SCOPE = 'maintenance-upgrade-restore-stage-interruption-install-retry-same-version-enrollment-and-online-fixture'
CONTAINER_CHECKS = {'signed-image-identity', 'fresh-migrations', 'first-install', 'backfills',
                    'cache-compilation', 'standalone-readiness', 'install-idempotency',
                    'real-ingress-switch-and-stream'}
HOST_CHECKS = {
    'enrollment': {'native-candidate-install', 'unmanaged-candidate-site', 'legacy-enrollment', 'session-login',
                   'same-sequence-enrollment', 'same-sequence-layout-conversion', 'restored-enrollment-legacy',
                   'same-sequence-enrollment-retry', 'session-after-enrollment-retry', 'enrollment-repeat-rejected'},
    'online': {'native-candidate-install', 'session-login', 'fresh-install-retry',
               'online-upgrade', 'online-http-session', 'online-queue-handover',
               'online-reverb-cross-slot', 'online-reverb-reconnect', 'online-switch-back-preserves-data'},
    'install': {'native-candidate-install', 'session-login', 'fresh-install-retry'},
    'upgrade': {'native-candidate-install', 'legacy-enrollment', 'session-login',
                'session-after-upgrade', 'signed-upgrade', 'complete-backup-restore',
                'restored-manual-rollback'} | {
                    prefix + stage for prefix in ['crash-', 'restored-'] for stage in
                    ['retain-assets', 'quiesce', 'scheduler-freeze', 'backup', 'upgrade', 'layout', 'candidate',
                     'switch', 'workers', 'observe', 'recovery-backoff']},
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read(path):
    return json.loads(Path(path).read_text())


def validate(evidence, candidate, approved=False):
    require(evidence['schema_version'] == 1 and evidence['candidate'] == candidate, 'Candidate identity differs')
    require(evidence['verification_scope'] == 'planned-container-contract-and-ingress', 'Container scope missing')
    require(evidence['installed_host_scope'] == SCOPE, 'Installed-host scope missing')
    run_id = str(candidate['candidate_run_id'])
    for platform, kernel in PLATFORMS.items():
        architecture = evidence['architectures'][platform]
        require(architecture['result'] == 'pass' and architecture['evidence_link'].startswith(
            'https://github.com/yaojingang/geoflow-updater/actions/runs/'), 'Architecture evidence missing')
        result = architecture['container']
        require(result['platform'] == platform and result['kernel_arch'] == kernel and
                str(result['candidate_run_id']) == run_id and result['result'] == 'pass', 'Container identity differs')
        require(CONTAINER_CHECKS <= set(result['checks']), 'Required container checks missing')
        for mode, required in HOST_CHECKS.items():
            host = architecture['hosts'][mode]
            require(host['schema_version'] == 1 and host['platform'] == platform and
                    host['kernel_arch'] == kernel and host['mode'] == mode and
                    str(host['candidate_run_id']) == run_id and host['result'] == 'pass', 'Host identity differs')
            checks = host['checks']
            require(all(check['status'] == 'pass' for check in checks), 'Host check failed')
            require(required <= {check['id'] for check in checks}, 'Required host checks missing')
    if approved:
        for role in ['release_operator', 'security_reviewer', 'product_owner']:
            approval = evidence['approvals'][role]
            require(approval['decision'] == 'approved' and approval['name'].strip(), 'Approval missing: ' + role)


def assemble(root, run_url):
    candidate = read(root / 'candidate/candidate.json')
    evidence = {
        'schema_version': 1, 'candidate': candidate,
        'verification_scope': 'planned-container-contract-and-ingress', 'installed_host_scope': SCOPE,
        'limitations': ['Online fixture uses identical application code with no pending migrations. Each production online source/target pair requires its own compatibility approval.'],
        'architectures': {},
        'approvals': {role: {'name': '', 'decision': 'pending'} for role in
                      ['release_operator', 'security_reviewer', 'product_owner']},
    }
    for platform in PLATFORMS:
        evidence['architectures'][platform] = {
            'result': 'pass', 'evidence_link': run_url,
            'container': read(root / f'architectures/planned-acceptance-{platform}/result.json'),
            'hosts': {mode: read(root / f'hosts/planned-host-{platform}-{mode}/result.json') for mode in HOST_CHECKS},
        }
    validate(evidence, candidate)
    return evidence


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    build = commands.add_parser('assemble')
    build.add_argument('--root', type=Path, required=True)
    build.add_argument('--run-url', required=True)
    build.add_argument('--output', type=Path, required=True)
    check = commands.add_parser('validate')
    check.add_argument('--candidate', type=Path, required=True)
    check.add_argument('--evidence', type=Path, required=True)
    check.add_argument('--approved', action='store_true')
    check.add_argument('--plan', type=Path, required=True)
    args = parser.parse_args()
    if args.command == 'assemble':
        evidence = assemble(args.root, args.run_url)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(evidence, indent=2) + '\n')
    else:
        require(read(args.plan)['strategy'] == 'maintenance',
                'Production online plans require source-to-target compatibility acceptance; the same-application fixture is insufficient.')
        validate(read(args.evidence), read(args.candidate), args.approved)


if __name__ == '__main__':
    main()

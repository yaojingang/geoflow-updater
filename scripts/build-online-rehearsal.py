#!/usr/bin/env python3
"""Sign a same-application online fixture for isolated host acceptance only."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + '\n')


def main():
    assert os.environ['GITHUB_REPOSITORY'] == 'yaojingang/geoflow-updater'
    assert os.environ['GITHUB_REF'] == 'refs/heads/main'
    candidate = Path('candidate')
    identity = json.loads((candidate / 'candidate.json').read_text())
    source = json.loads((candidate / 'targets-source/releases/current.json').read_text())
    assert source['release_sequence'] == identity['geoflow']['release_sequence']
    assert re.fullmatch(r'ghcr.io/yaojingang/geoflow-app@sha256:[a-f0-9]{64}', source['app_image'])
    fixture = candidate / 'online-fixture'
    fixture.mkdir()
    targets = fixture / 'targets-source'
    shutil.copytree(candidate / 'targets-source', targets)
    plan_path = targets / source['upgrade_plan_target']
    plan = json.loads(plan_path.read_text())
    # The database has already completed the candidate's migrations. This fixture
    # changes only the plan and exercises an application-identical online update.
    # It provides no compatibility approval for another source release.
    plan['strategy'] = 'online'
    plan['allowed_sources'] = [source['release_sequence']]
    plan['compatibility'] = dict.fromkeys(['schema', 'queue', 'cache', 'storage'], True)
    plan['steps'] = [dict(step, online=True) for step in plan['steps'] if step['kind'] in ['migrate', 'cache_warmup']]
    write(plan_path, plan)
    context = fixture / 'image'
    context.mkdir()
    shutil.copyfile(plan_path, context / 'upgrade-plan.json')
    (context / 'Dockerfile').write_text('FROM ' + source['app_image'] + '\nCOPY --chown=33:33 upgrade-plan.json /var/www/html/deployment/upgrade-plan.json\n')
    tag = 'ghcr.io/yaojingang/geoflow-app:online-rehearsal-' + os.environ['GITHUB_RUN_ID'] + '-' + os.environ['GITHUB_RUN_ATTEMPT']
    metadata = fixture / 'build.json'
    subprocess.run(['docker', 'buildx', 'build', '--platform', 'linux/amd64,linux/arm64', '--push',
                    '--provenance=mode=max', '--sbom=true', '--metadata-file', str(metadata),
                    '--tag', tag, str(context)], check=True)
    image_digest = json.loads(metadata.read_text())['containerimage.digest']
    assert re.fullmatch(r'sha256:[a-f0-9]{64}', image_digest)
    manifest = dict(source, minimum_updater_protocol=4, release_sequence=source['release_sequence'] + 1,
                    app_image='ghcr.io/yaojingang/geoflow-app@' + image_digest)
    write(targets / 'releases/current.json', manifest)
    repository = fixture / 'tuf/repository'
    shutil.copytree(candidate / 'tuf/repository', repository)
    temporary = Path(os.environ['RUNNER_TEMP'])
    subprocess.run(['go', 'run', './cmd/geoflow-tuf', 'publish', '--repository-dir', str(repository),
                    '--targets-dir', str(targets), '--targets-key', str(temporary / 'targets.pem'),
                    '--snapshot-key', str(temporary / 'snapshot.pem'), '--timestamp-key', str(temporary / 'timestamp.pem')], check=True)
    identity['online_fixture'] = {
        'scope': 'same-application-online-switch-and-switch-back',
        'source_sequence': source['release_sequence'], 'target_sequence': manifest['release_sequence'],
        'base_app_digest': identity['geoflow']['app_digest'], 'app_digest': image_digest,
        'release_manifest_sha256': digest(targets / 'releases/current.json'),
        'upgrade_plan_sha256': digest(plan_path),
    }
    write(candidate / 'candidate.json', identity)


if __name__ == '__main__':
    main()

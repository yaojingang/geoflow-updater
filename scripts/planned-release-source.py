#!/usr/bin/env python3
"""Verify publication source drift and original candidate artifact retention."""
import argparse
import datetime
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import subprocess
import tempfile

COMMIT = re.compile(r'(?:[a-f0-9]{40}|[a-f0-9]{64})\Z')
SNAPSHOT = re.compile(r'tuf/repository/metadata/([1-9][0-9]*)\.snapshot\.json\Z')
TIMESTAMP = 'tuf/repository/metadata/timestamp.json'


def git(*args):
    return subprocess.check_output(['git', *args])


def read(commit, path):
    return git('show', commit + ':' + path)


def metadata(commit, path, role):
    document = json.loads(read(commit, path))['signed']
    if document.get('_type') != role or type(document.get('version')) is not int or document['version'] < 1:
        raise ValueError('Invalid ' + role + ' metadata identity')
    return document


def chain(commit):
    timestamp = metadata(commit, TIMESTAMP, 'timestamp')
    reference = timestamp['meta']['snapshot.json']
    if type(reference.get('version')) is not int or reference['version'] < 1:
        raise ValueError('Invalid timestamp snapshot version')
    path = f'tuf/repository/metadata/{reference["version"]}.snapshot.json'
    contents = read(commit, path)
    if reference.get('length') != len(contents) or reference.get('hashes', {}).get('sha256') != hashlib.sha256(contents).hexdigest():
        raise ValueError('Timestamp does not bind the exact snapshot bytes')
    snapshot = metadata(commit, path, 'snapshot')
    if snapshot['version'] != reference['version']:
        raise ValueError('Snapshot filename and signed version differ')
    return timestamp, snapshot


def changes(parent, commit):
    fields = git('diff', '--no-renames', '--raw', '-r', '-z', parent, commit).split(b'\0')
    for index in range(0, len(fields) - 1, 2):
        header, path = fields[index].decode().split(), fields[index + 1].decode()
        yield header[0][1:], header[1], header[4], path


def first_publication(candidate, publication):
    if not COMMIT.fullmatch(candidate) or not COMMIT.fullmatch(publication):
        raise ValueError('Exact candidate and publication commits are required')
    subprocess.run(['git', 'merge-base', '--is-ancestor', candidate, publication], check=True)
    if git('rev-parse', 'HEAD').decode().strip() != publication:
        raise ValueError('Publication checkout must match its exact source commit')
    baseline_timestamp, baseline_snapshot = chain(candidate)
    # Inspect every parent edge, so a source change followed by a revert never
    # becomes an approved metadata-only descendant through an endpoint diff.
    for row in git('rev-list', '--reverse', '--parents', candidate + '..' + publication).decode().splitlines():
        commit, *parents = row.split()
        if not parents:
            raise ValueError('Publication history must descend from the candidate')
        for parent in parents:
            before_timestamp, before_snapshot = chain(parent)
            after_timestamp, after_snapshot = chain(commit)
            changed = list(changes(parent, commit))
            for old_mode, new_mode, status, path in changed:
                snapshot = SNAPSHOT.fullmatch(path)
                if new_mode != '100644' or not (path == TIMESTAMP or snapshot):
                    raise ValueError('First publication requires unchanged candidate source and trust: ' + path)
                if path == TIMESTAMP:
                    if status != 'M' or old_mode != '100644':
                        raise ValueError('Timestamp refresh must modify its existing regular file')
                elif status != 'A' or old_mode != '000000':
                    raise ValueError('Refresh cannot replace an existing immutable snapshot: ' + path)
                else:
                    document = metadata(commit, path, 'snapshot')
                    if document['version'] != int(snapshot[1]) or document['version'] <= before_snapshot['version'] or document.get('meta') != baseline_snapshot['meta']:
                        raise ValueError('Daily refresh must preserve the exact candidate targets references')
            if changed:
                if after_timestamp['version'] <= before_timestamp['version'] or after_snapshot['version'] <= before_snapshot['version']:
                    raise ValueError('Daily refresh must advance timestamp and snapshot versions')
                if after_snapshot.get('meta') != baseline_snapshot['meta']:
                    raise ValueError('Daily refresh changed the candidate targets reference')
    # Reuse the production TUF verifier for signature thresholds, expiry and
    # target bytes. No signing key is read and no release asset is rebuilt.
    with tempfile.TemporaryDirectory(prefix='geoflow-source-gate-') as stage:
        repository, targets = Path(stage) / 'repository', Path(stage) / 'targets'
        for entry in git('ls-tree', '-r', '-z', publication, '--', 'tuf/repository').split(b'\0'):
            if not entry:
                continue
            header, name = entry.split(b'\t', 1)
            mode, kind, _ = header.decode().split()
            relative = PurePosixPath(name.decode()).relative_to('tuf/repository')
            if mode != '100644' or kind != 'blob' or '..' in relative.parts:
                raise ValueError('TUF repository must contain only regular publication files')
            output = repository / str(relative)
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_bytes(read(publication, name.decode()))
        target_version = baseline_snapshot['meta']['targets.json']['version']
        target_metadata = metadata(candidate, f'tuf/repository/metadata/{target_version}.targets.json', 'targets')
        for name, info in target_metadata['targets'].items():
            relative = PurePosixPath(name)
            if relative.is_absolute() or '..' in relative.parts or '\\' in name or str(relative) != name:
                raise ValueError('Unsafe candidate target path')
            digest = info['hashes']['sha256']
            if not re.fullmatch(r'[a-f0-9]{64}', digest):
                raise ValueError('Candidate target requires exact SHA-256')
            stored = 'tuf/repository/targets/' + str(relative.parent / (digest + '.' + relative.name))
            contents = read(candidate, stored)
            if len(contents) != info['length'] or hashlib.sha256(contents).hexdigest() != digest:
                raise ValueError('Candidate target bytes differ from signed metadata')
            output = targets / str(relative)
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_bytes(contents)
        subprocess.run(['go', 'run', './cmd/geoflow-tuf', 'verify-repository', '--repository-dir', str(repository), '--targets-dir', str(targets)], check=True)
    return {'mode': 'first-publication', 'candidate_commit': candidate, 'publication_commit': publication}


def artifact_retention(run, pages, run_id, source, now=None):
    if type(run_id) is not int or run_id < 1 or not COMMIT.fullmatch(source):
        raise ValueError('Exact original candidate identity is required')
    if run.get('id') != run_id or run.get('run_attempt') != 1 or run.get('head_sha') != source or run.get('head_branch') != 'main' or run.get('path') != '.github/workflows/release-candidate.yml' or run.get('event') != 'workflow_dispatch' or run.get('conclusion') != 'success':
        raise ValueError('Original candidate run identity differs')
    artifacts = [item for page in pages for item in page['artifacts'] if item.get('name') == f'phase-c-candidate-{run_id}']
    if len(artifacts) != 1 or artifacts[0].get('expired') is not False:
        raise ValueError('Original candidate is missing or expired; build and accept a new candidate')
    artifact = artifacts[0]
    if artifact.get('workflow_run', {}).get('id') != run_id or artifact.get('workflow_run', {}).get('head_sha') != source:
        raise ValueError('Candidate artifact belongs to a different source or run')
    created = datetime.datetime.fromisoformat(artifact['created_at'].replace('Z', '+00:00'))
    expires = datetime.datetime.fromisoformat(artifact['expires_at'].replace('Z', '+00:00'))
    now = now or datetime.datetime.now(datetime.timezone.utc)
    if created.tzinfo is None or expires.tzinfo is None or created > now or expires <= created or now >= min(expires, created + datetime.timedelta(days=30)):
        raise ValueError('Original candidate retention expired; build and accept a new candidate')
    return {'candidate_run_id': run_id, 'artifact_id': artifact['id'], 'created_at': artifact['created_at'], 'expires_at': artifact['expires_at']}


def main():
    parser = argparse.ArgumentParser(description=__doc__, allow_abbrev=False)
    commands = parser.add_subparsers(dest='action', required=True)
    source = commands.add_parser('first-publication', allow_abbrev=False)
    source.add_argument('--candidate', required=True)
    source.add_argument('--publication', required=True)
    artifact = commands.add_parser('artifact', allow_abbrev=False)
    artifact.add_argument('--run', type=Path, required=True)
    artifact.add_argument('--artifacts', type=Path, required=True)
    artifact.add_argument('--run-id', type=int, required=True)
    artifact.add_argument('--source', required=True)
    args = parser.parse_args()
    if args.action == 'first-publication':
        result = first_publication(args.candidate, args.publication)
    else:
        result = artifact_retention(json.loads(args.run.read_text()), json.loads(args.artifacts.read_text()), args.run_id, args.source)
    print(json.dumps(result, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        raise SystemExit(str(error))

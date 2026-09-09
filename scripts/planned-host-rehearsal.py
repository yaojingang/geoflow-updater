#!/usr/bin/env python3
"""Exercise an installed signed candidate on an empty GitHub-hosted Linux VM."""
import argparse
import base64
import hashlib
import hmac
import http.client
import http.cookiejar
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import struct
import subprocess
import sys
import time
import urllib.parse
import urllib.request

import yaml

ROOT = Path('/opt/geoflow-planned-rehearsal')
STATE = Path('/var/lib/geoflow-updater')
INSTANCE = STATE / 'instances/primary'
FAULT = STATE / 'planned-rehearsal-fault.json'
MARKER = STATE / 'planned-rehearsal-hit.json'
RESTORE_FAULT = STATE / 'planned-rehearsal-block-restore'
SOCKET = '/run/geoflow-updater/geoflow-updater.sock'
REPO = Path(__file__).resolve().parent.parent


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def read_json(path):
    return json.loads(Path(path).read_text())


def target(repository, name):
    timestamp = read_json(repository / 'metadata/timestamp.json')
    snapshot_version = timestamp['signed']['meta']['snapshot.json']['version']
    snapshot = read_json(repository / f'metadata/{snapshot_version}.snapshot.json')
    version = snapshot['signed']['meta']['targets.json']['version']
    metadata = read_json(repository / f'metadata/{version}.targets.json')
    digest = metadata['signed']['targets'][name]['hashes']['sha256']
    relative = Path(name)
    path = repository / 'targets' / relative.parent / (digest + '.' + relative.name)
    require(sha(path) == digest, 'Recorded signed target hash differs: ' + name)
    return path


class UnixConnection(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(SOCKET)


class Rehearsal:
    def __init__(self, args):
        self.args = args
        self.candidate = Path(args.candidate).resolve()
        self.evidence = Path(args.evidence).resolve()
        self.identity = read_json(self.candidate / 'candidate.json')
        self.manifest = read_json(self.candidate / 'targets-source/releases/current.json')
        self.checks = []
        self.secrets = []
        self.factors = {}
        self.counters = {}
        self.server = None
        self.owns_evidence = False
        self.installed = False
        self.current = 'bootstrap'
        self.environment = dict(os.environ)
        self.environment.update({
            'GEOFLOW_UPDATER_ALLOW_CANDIDATE_REPOSITORY': '1',
            'GEOFLOW_UPDATER_TUF_METADATA_URL': 'https://127.0.0.1:18443/metadata',
            'GEOFLOW_UPDATER_TUF_TARGETS_URL': 'https://127.0.0.1:18443/targets',
        })
        self.cookies = http.cookiejar.CookieJar()
        self.browser = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.cookies))

    def mask(self, value):
        if value and value not in self.secrets:
            self.secrets.append(value)
            print('::add-mask::' + value, flush=True)
        return value

    def redact(self, text):
        for value in sorted(self.secrets, key=len, reverse=True):
            text = text.replace(value, '[REDACTED]')
        return text

    def save(self, name, value):
        text = value if isinstance(value, str) else json.dumps(value, indent=2, default=str)
        (self.evidence / name).write_text(self.redact(text))

    def run(self, *args, input=None, check=True, timeout=1800, log=True):
        result = subprocess.run(list(map(str, args)), input=input, text=True, capture_output=True,
                                env=self.environment, timeout=timeout)
        if log:
            with (self.evidence / 'commands.log').open('a') as stream:
                stream.write(self.redact(result.stdout + result.stderr))
        if check and result.returncode:
            raise RuntimeError(f'{Path(str(args[0])).name} failed ({result.returncode}): '
                               + self.redact(result.stderr[-2500:] or result.stdout[-2500:]))
        return result.stdout.strip()

    def record(self, name, detail):
        self.checks.append({'id': name, 'status': 'pass', 'evidence': detail})
        print(f'[planned-host] PASS {name}', flush=True)

    def config(self):
        return yaml.safe_load((INSTANCE / 'instance.yml').read_text())

    def compose(self, *args, config=None):
        config = config or self.config()
        return self.run('/usr/bin/docker', 'compose', '--env-file', ROOT / '.env.prod',
                        '--env-file', config['environment_file'], '-f', config['compose_file'], *args)

    def container(self, service):
        ids = self.run('/usr/bin/docker', 'ps', '--filter', 'label=com.docker.compose.service=' + service,
                       '--format', '{{.ID}}', log=False).splitlines()
        require(len(ids) == 1, 'Expected exactly one running ' + service)
        return ids[0]

    def query(self, sql):
        return self.run('/usr/bin/docker', 'exec', '-i', self.container('postgres'), 'sh', '-ec',
                        'exec psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At -v ON_ERROR_STOP=1', input=sql, log=False)

    def redis(self, *args):
        return self.run('/usr/bin/docker', 'exec', self.container('redis'), 'sh', '-ec',
                        'exec redis-cli --no-auth-warning -a "${REDIS_PASSWORD:-}" "$@"', 'rehearsal', *args, log=False)

    def api(self, method, endpoint, payload=None, scope=None, expected=200):
        token = self.mask((INSTANCE / 'control.token').read_text().strip())
        headers = {'Authorization': 'Bearer ' + token}
        if scope:
            while int(time.time()) // 30 <= self.counters.get(scope, 0):
                time.sleep(1)
            counter = int(time.time()) // 30
            self.counters[scope] = counter
            key = base64.b32decode(self.factors[scope] + '=' * (-len(self.factors[scope]) % 8))
            digest = hmac.new(key, struct.pack('>Q', counter), hashlib.sha1).digest()
            offset = digest[-1] & 15
            code = f'{(struct.unpack(">I", digest[offset:offset+4])[0] & 0x7fffffff) % 1000000:06d}'
            headers['X-GEOFlow-Updater-Authorization'] = self.mask(code)
        body = None
        if payload is not None:
            headers['Content-Type'] = 'application/json'
            body = json.dumps(payload)
        connection = UnixConnection('localhost', timeout=1500)
        try:
            connection.request(method, '/v1/instances/primary/' + endpoint, body, headers)
            response = connection.getresponse()
            data = json.loads(response.read())
            require(response.status == expected, f'{method} {endpoint}: HTTP {response.status}: {data}')
            return data
        finally:
            connection.close()

    def wait(self, predicate, label, timeout=1800):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            result = predicate()
            if result:
                return result
            time.sleep(1)
        raise RuntimeError('Timed out waiting for ' + label)

    def operation(self, identity, statuses, label):
        def finished():
            try:
                operation = self.api('GET', 'operations/current')
            except (ConnectionError, FileNotFoundError, socket.timeout):
                return None
            require(operation['id'] == identity, 'Unexpected operation identity')
            if operation['status'] == 'recovery_required' or operation.get('completed_at'):
                self.save('operation-' + label + '.json', operation)
                require(operation['status'] in statuses, label + ': ' + json.dumps(operation))
                return operation
            return None
        return self.wait(finished, label)

    def mutate(self, endpoint, scope, payload=None, label=None):
        operation = self.api('POST', endpoint, payload, scope, expected=202)
        return self.operation(operation['id'], ['succeeded'], label or endpoint)

    def healthy(self, label):
        report = json.loads(self.run('geoflow-updater', 'doctor', '--instance', 'primary', '--json'))
        self.save('doctor-' + label + '.json', report)
        require(report['status'] == 'pass', 'Doctor failed: ' + label)
        with urllib.request.urlopen('http://localhost:18080/up', timeout=15) as response:
            require(response.status == 200, 'Public health failed')

    def login(self, password):
        url = 'http://localhost:18080/geo_admin/login'
        with self.browser.open(url, timeout=30) as response:
            html = response.read().decode()
        csrf = re.search(r'name="_token"[^>]*value="([^"]+)"', html)
        require(csrf is not None, 'Login CSRF token missing')
        self.mask(csrf[1])
        data = urllib.parse.urlencode({'_token': csrf[1], 'username': 'admin', 'password': password}).encode()
        with self.browser.open(urllib.request.Request(url, data=data), timeout=30) as response:
            require(response.status == 200 and '/login' not in response.url, 'Administrator login failed')
        for cookie in self.cookies:
            self.mask(cookie.value)
        self.session('login')

    def session(self, label):
        with self.browser.open('http://localhost:18080/geo_admin/system-updates', timeout=30) as response:
            require(response.status == 200 and '/system-updates' in response.url, 'Authenticated session unavailable: ' + label)
            require('name="current_admin_password"' in response.read().decode(), 'Update authorization controls missing')
        for cookie in self.cookies:
            self.mask(cookie.value)
        self.record('session-' + label, 'Real administrator HTTP session reaches the protected update center')

    def setup(self):
        require(os.geteuid() == 0, 'Run through sudo on a disposable GitHub runner')
        require(os.environ.get('GITHUB_ACTIONS') == 'true' and
                os.environ.get('RUNNER_ENVIRONMENT') == 'github-hosted' and
                os.environ.get('GITHUB_REPOSITORY') == 'yaojingang/geoflow-updater',
                'This rehearsal is restricted to disposable repository GitHub-hosted runners')
        occupied = [path for path in [ROOT, STATE, Path('/var/backups/geoflow-updater'),
            Path('/usr/local/sbin/geoflow-updater'), Path('/etc/systemd/system/geoflow-updater.service'),
            Path('/etc/systemd/system/geoflow-updater.service.d'), Path('/usr/local/lib/geoflow-planned-rehearsal'),
            Path('/run/geoflow-updater'), Path('/usr/local/share/ca-certificates/geoflow-planned-rehearsal.crt')]
            if path.exists() or path.is_symlink()]
        require(not occupied, 'An existing installation must not be touched: ' + ', '.join(map(str, occupied)))
        unit = subprocess.run(['systemctl', 'show', '-p', 'LoadState', '--value', 'geoflow-updater.service'], capture_output=True, text=True)
        require(unit.stdout.strip() == 'not-found', 'An existing updater unit must not be touched')
        expected_evidence = REPO / 'rehearsal/host-evidence' / (self.args.platform + '-' + self.args.mode)
        require(self.evidence == expected_evidence and expected_evidence.parent.resolve() == expected_evidence.parent,
                'Evidence must stay in the workflow-owned directory')
        parent_stat = expected_evidence.parent.stat()
        require((parent_stat.st_uid, parent_stat.st_gid) == (int(os.environ['SUDO_UID']), int(os.environ['SUDO_GID'])),
                'Evidence parent must be owned by the invoking runner')
        require(not self.evidence.exists(), 'Evidence directory already exists')
        expected = {'linux-amd64': 'x86_64', 'linux-arm64': 'aarch64'}
        require(self.args.platform in expected and os.uname().machine == expected[self.args.platform], 'Native architecture mismatch')
        require(self.manifest['schema_version'] == 3, 'A planned candidate is required')
        require(str(self.identity['candidate_run_id']) == os.environ['CANDIDATE_RUN_ID'], 'Candidate identity mismatch')
        self.evidence.mkdir(parents=True, mode=0o700)
        self.owns_evidence = True
        require(not self.run('/usr/bin/docker', 'ps', '-aq'), 'The disposable Docker daemon must start empty')
        self.save('candidate.json', self.identity)
        arch = self.args.platform.removeprefix('linux-')
        archive = self.candidate / f'dist/geoflow-updater_{self.identity["updater"]["version"]}_linux_{arch}.tar.gz'
        require(sha(archive) == self.identity['updater']['archives'][self.args.platform], 'Archive checksum mismatch')
        archive_root = self.evidence.parent / ('archive-' + self.args.mode)
        archive_root.mkdir(mode=0o700)
        self.run('tar', '-xzf', archive, '-C', archive_root)
        self.run('bash', archive_root / 'packaging/scripts/install.sh')
        self.installed = True
        require(self.run('geoflow-updater', 'version') == self.identity['updater']['version'], 'Installed binary version differs')
        self.tls = self.evidence.parent / ('tls-' + self.args.mode)
        self.tls.mkdir(mode=0o700)
        self.run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2', '-sha256',
                 '-subj', '/CN=GEOFlow planned rehearsal', '-addext', 'subjectAltName=IP:127.0.0.1',
                 '-keyout', self.tls / 'server.key', '-out', self.tls / 'server.crt')
        shutil.copyfile(self.tls / 'server.crt', '/usr/local/share/ca-certificates/geoflow-planned-rehearsal.crt')
        self.run('update-ca-certificates')
        wrapper_dir = Path('/usr/local/lib/geoflow-planned-rehearsal')
        wrapper_dir.mkdir(mode=0o755)
        shutil.copyfile(REPO / 'scripts/planned-rehearsal-docker.py', wrapper_dir / 'docker')
        (wrapper_dir / 'docker').chmod(0o755)
        self.environment['PATH'] = str(wrapper_dir) + ':' + self.environment['PATH']
        dropin = Path('/etc/systemd/system/geoflow-updater.service.d')
        dropin.mkdir(exist_ok=True)
        (dropin / 'planned-rehearsal.conf').write_text('[Service]\n' + ''.join(
            f'Environment={key}={value}\n' for key, value in self.environment.items()
            if key.startswith('GEOFLOW_UPDATER_') or key == 'PATH'))
        (dropin / 'planned-rehearsal.conf').chmod(0o600)
        self.run('systemctl', 'daemon-reload')
        self.record('native-candidate-install', 'Verified candidate archive installed through its systemd installer')

    def repository(self, path):
        if self.server:
            self.server.terminate()
            self.server.wait(timeout=10)
        output = (self.evidence / 'repository.log').open('a')
        self.server = subprocess.Popen([sys.executable, str(REPO / 'scripts/serve-rehearsal-repository.py'),
            '--directory', str(path), '--certificate', str(self.tls / 'server.crt'),
            '--private-key', str(self.tls / 'server.key'), '--port', '18443'], stdout=output, stderr=output)
        output.close()
        def ready():
            result = subprocess.run(['curl', '--fail', '--silent', 'https://127.0.0.1:18443/metadata/timestamp.json'], capture_output=True)
            return result.returncode == 0
        self.wait(ready, 'private signed repository', timeout=30)
        self.run('systemctl', 'restart', 'geoflow-updater')

    def authorize(self):
        text = self.run('geoflow-updater', 'authorization-uri', '--instance', 'primary', log=False)
        for scope, uri in re.findall(r'^(update|backup|rollback): (otpauth://\S+)$', text, re.M):
            self.mask(uri)
            self.factors[scope] = self.mask(urllib.parse.parse_qs(urllib.parse.urlparse(uri).query)['secret'][0])
        require(len(set(self.factors.values())) == 3, 'Expected three distinct authorization factors')
        require((INSTANCE / 'mutation.secret').stat().st_mode & 0o777 == 0o600, 'Master secret permissions changed')

    def legacy(self):
        repository = REPO / 'tuf/repository'
        baseline = read_json(target(repository, 'releases/current.json'))
        require(baseline['schema_version'] == 2 and baseline['release_sequence'] < self.manifest['release_sequence'], 'Expected an older stable baseline')
        self.repository(repository)
        ROOT.mkdir(mode=0o755)
        for relative in ['storage/app/public', 'storage/app/private', 'storage/framework/cache/data',
                         'storage/framework/sessions', 'storage/framework/views', 'storage/logs',
                         'docker-data/prod/postgres', 'docker-data/prod/redis']:
            (ROOT / relative).mkdir(parents=True, exist_ok=True)
        shutil.copyfile(target(repository, baseline['version_target']), ROOT / 'version.json')
        values = {
            'APP_ENV': 'production', 'APP_DEBUG': 'false', 'APP_KEY': 'base64:' + self.mask(base64.b64encode(secrets.token_bytes(32)).decode()),
            'APP_URL': 'http://localhost:18080', 'APP_LOCALE': 'en', 'SESSION_SECURE_COOKIE': 'false',
            'DB_CONNECTION': 'pgsql', 'DB_HOST': 'postgres', 'DB_PORT': '5432', 'DB_DATABASE': 'geo_flow', 'DB_USERNAME': 'geo_user',
            'DB_PASSWORD': self.mask(secrets.token_hex(24)), 'REDIS_HOST': 'redis', 'REDIS_PASSWORD': self.mask(secrets.token_hex(24)),
            'CACHE_STORE': 'redis', 'SESSION_DRIVER': 'database', 'QUEUE_CONNECTION': 'redis', 'BROADCAST_CONNECTION': 'reverb',
            'REVERB_APP_ID': 'rehearsal', 'REVERB_APP_KEY': 'rehearsal', 'REVERB_APP_SECRET': self.mask(secrets.token_hex(24)),
            'REVERB_HOST': 'localhost', 'REVERB_PORT': '18080', 'REVERB_SCHEME': 'http',
            'GEOFLOW_ADMIN_USERNAME': 'admin', 'GEOFLOW_ADMIN_EMAIL': 'rehearsal@example.invalid',
            'GEOFLOW_ADMIN_PASSWORD': self.mask(secrets.token_hex(24)), 'GEOFLOW_INITIAL_ADMIN_HINT_ENABLED': 'false',
            'GEOFLOW_TELEMETRY_ENABLED': 'false', 'GEOFLOW_UPDATE_CHECK_ENABLED': 'false',
            'GEOFLOW_PRIMARY_HOSTS': 'localhost', 'GEOFLOW_NGINX_PRIMARY_HOST': 'localhost',
            'GEOFLOW_NGINX_PUBLIC_SCHEME': 'http', 'GEOFLOW_NGINX_PUBLIC_PORT': '18080', 'WEB_PORT': '18080',
            'PGVECTOR_IMAGE': 'pgvector/pgvector:pg18', 'REDIS_IMAGE': 'redis:8-alpine',
            'POSTGRES_DATA_DIR': str(ROOT / 'docker-data/prod/postgres'), 'POSTGRES_CONTAINER_DATA_DIR': '/var/lib/postgresql',
            'GEOFLOW_REHEARSAL_MARKER': 'baseline', 'AUTO_OPTIMIZE': 'false',
        }
        (ROOT / '.env.prod').write_text(''.join(f'{key}={value}\n' for key, value in values.items()))
        (ROOT / '.env.prod').chmod(0o640)
        self.run('chown', '-R', '33:33', ROOT / 'storage')
        self.run('chown', '33:33', ROOT / '.env.prod')
        # Initialize the existing PostgreSQL layout before calling the public enrollment command.
        self.run('/usr/bin/docker', 'run', '-d', '--name', 'geoflow-rehearsal-bootstrap-postgres',
                 '-e', 'POSTGRES_DB=geo_flow', '-e', 'POSTGRES_USER=geo_user', '-e', 'POSTGRES_PASSWORD=' + values['DB_PASSWORD'],
                 '-v', str(ROOT / 'docker-data/prod/postgres') + ':/var/lib/postgresql', baseline['postgres_images']['18'])
        def initialized():
            result = subprocess.run(['/usr/bin/docker', 'exec', 'geoflow-rehearsal-bootstrap-postgres', 'sh', '-ec',
                'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "SELECT 1"'], capture_output=True)
            return result.returncode == 0 and result.stdout.strip() == b'1'
        self.wait(initialized, 'initialized existing PostgreSQL database over TCP', 120)
        self.run('/usr/bin/docker', 'stop', 'geoflow-rehearsal-bootstrap-postgres')
        self.run('/usr/bin/docker', 'rm', 'geoflow-rehearsal-bootstrap-postgres')
        self.run('geoflow-updater', 'enroll', '--instance-root', ROOT)
        self.compose('up', '-d', '--wait', 'postgres', 'redis')
        self.compose('run', '--rm', '--no-deps', '-e', 'GEOFLOW_SECURITY_FRESH_INSTALL_CONFIRMED=true', 'init', 'php', 'artisan', 'migrate', '--force')
        self.compose('run', '--rm', '--no-deps', 'init', 'php', 'artisan', 'geoflow:install', '--no-interaction')
        self.compose('up', '-d', '--wait', '--wait-timeout', '600')
        self.authorize()
        self.healthy('legacy')
        self.login(values['GEOFLOW_ADMIN_PASSWORD'])
        self.baseline = baseline
        self.record('legacy-enrollment', 'Existing signed stable deployment enrolled and started on PostgreSQL 18 / Redis 8')

    def fixture(self):
        self.query('CREATE TABLE IF NOT EXISTS geoflow_rehearsal_markers (id integer PRIMARY KEY, value text NOT NULL);'
                   "INSERT INTO geoflow_rehearsal_markers VALUES (1,'baseline') ON CONFLICT (id) DO UPDATE SET value='baseline';")
        self.redis('SET', 'geoflow:planned-rehearsal:marker', 'baseline')
        (ROOT / 'storage/app/rehearsal.txt').write_text('baseline')
        self.baseline_hashes = {str(path): sha(path) for path in [ROOT / '.env.prod', ROOT / 'version.json',
            INSTANCE / 'instance.yml', INSTANCE / 'release.env', INSTANCE / 'docker-compose.managed.yml']}
        self.migrations = self.query('SELECT migration || \':\' || batch FROM migrations ORDER BY migration;')

    def corrupt(self, label, config=False):
        self.query("UPDATE geoflow_rehearsal_markers SET value='changed';")
        self.redis('SET', 'geoflow:planned-rehearsal:marker', 'changed')
        (ROOT / 'storage/app/rehearsal.txt').write_text('changed')
        if config:
            with (ROOT / '.env.prod').open('a') as stream:
                stream.write('\nGEOFLOW_REHEARSAL_AFTER_BACKUP=changed\n')
            with (INSTANCE / 'release.env').open('a') as stream:
                stream.write('\n# rehearsal-after-backup\n')
        self.record('changed-' + label, 'Verified non-baseline database, Redis and storage markers before restoration')
        require(self.query('SELECT value FROM geoflow_rehearsal_markers WHERE id=1;') == 'changed', 'Corruption fixture did not write')

    def restored(self, label):
        require(self.query('SELECT value FROM geoflow_rehearsal_markers WHERE id=1;') == 'baseline', 'Database marker not restored')
        require(self.redis('GET', 'geoflow:planned-rehearsal:marker') == 'baseline', 'Redis marker not restored')
        require((ROOT / 'storage/app/rehearsal.txt').read_text() == 'baseline', 'Storage marker not restored')
        for path, digest in self.baseline_hashes.items():
            require(sha(path) == digest, 'Restored file differs: ' + path)
        require(self.query('SELECT migration || \':\' || batch FROM migrations ORDER BY migration;') == self.migrations, 'Migration history not restored')
        self.healthy(label)
        self.record('restored-' + label, 'Data markers, full configuration hashes, migration history and doctor match the baseline')

    def preview(self):
        plan = self.api('GET', 'plan')
        require(plan['strategy'] == 'maintenance' and plan['layout_change'], 'Legacy conversion must require maintenance')
        require(plan['target_sequence'] == self.manifest['release_sequence'], 'Candidate target sequence changed')
        return {'allow_maintenance': True, 'expected_plan_sha256': plan['plan_sha256']}

    def crash(self, stage, block_restore=False):
        label = 'recovery-backoff' if block_restore else stage
        self.current = 'crash-' + label
        self.fixture()
        payload = self.preview()
        MARKER.unlink(missing_ok=True)
        FAULT.write_text(json.dumps({'stage': stage, 'after': stage == 'upgrade'}))
        FAULT.chmod(0o600)
        operation = self.api('POST', 'updates', payload, 'update', expected=202)
        def boundary():
            if MARKER.exists():
                return True
            current = self.api('GET', 'operations/current')
            require(current['id'] == operation['id'] and not current.get('completed_at'),
                    'Update ended before fault boundary: ' + json.dumps(current))
            return False
        self.wait(boundary, 'durable stage ' + stage)
        transaction = read_json(INSTANCE / 'release-transaction.json')
        require(transaction['stage'] == stage and transaction['operation_id'] == operation['id'], 'Fault hit a different transaction')
        self.save('interrupted-' + stage + '.json', {'stage': stage, 'operation_id': operation['id'],
                  'traffic_opened': transaction['traffic_opened'], 'recovery_point_id': transaction.get('recovery_point_id')})
        if transaction.get('recovery_point_id'):
            self.corrupt(stage)
        if block_restore:
            RESTORE_FAULT.touch(mode=0o600)
        self.run('systemctl', 'kill', '--signal=SIGKILL', '--kill-whom=all', 'geoflow-updater')
        FAULT.unlink()
        self.run('systemctl', 'restart', 'geoflow-updater')
        expected = 'recovery_required' if transaction['traffic_opened'] or block_restore else ('rolled_back' if transaction.get('recovery_point_id') else 'failed')
        result = self.operation(operation['id'], [expected], label)
        if block_restore:
            require(result['reconcile_attempts'] >= 1 and result.get('next_reconcile_at'), 'Recovery backoff was not persisted')
            require('restore' in result.get('error', '').lower(), 'Recovery did not reach injected restore failure')
            self.api('POST', 'backups', scope='backup', expected=409)
            require(self.api('GET', 'operations/current')['id'] == operation['id'], 'Blocked write replaced interrupted operation')
            self.run('systemctl', 'restart', 'geoflow-updater')
            time.sleep(2)
            current = self.api('GET', 'operations/current')
            require(current['status'] == 'recovery_required' and current['recovery_point_id'] == result['recovery_point_id'], 'Recovery state lost after restart')
            RESTORE_FAULT.unlink()
        if expected == 'recovery_required':
            if transaction['traffic_opened']:
                require(self.query('SELECT value FROM geoflow_rehearsal_markers WHERE id=1;') == 'changed', 'Post-traffic data was rewound automatically')
            self.mutate('rollbacks', 'rollback', {'recovery_point_id': result['recovery_point_id']}, 'recover-' + stage)
        self.restored(label)
        current_hash = sha(INSTANCE / 'operations/current.json')
        self.run('systemctl', 'restart', 'geoflow-updater')
        time.sleep(3)
        require(sha(INSTANCE / 'operations/current.json') == current_hash, 'Completed recovery repeated after restart')
        self.record('crash-' + label, 'SIGKILL at the recorded durable stage; correct recovery policy and restart stability')

    def upgrade(self):
        self.legacy()
        self.repository(self.candidate / 'tuf/repository')
        for stage in ['retain-assets', 'quiesce', 'backup', 'upgrade', 'layout', 'candidate', 'switch', 'workers', 'observe']:
            self.crash(stage)
        self.crash('upgrade', block_restore=True)
        self.current = 'successful-upgrade'
        self.fixture()
        result = self.mutate('updates', 'update', self.preview(), 'successful-upgrade')
        checkpoint = result['recovery_point_id']
        config = self.config()
        require(config['layout'] == 'blue-green' and config['release_sequence'] == self.manifest['release_sequence'], 'Candidate was not activated')
        self.healthy('candidate')
        self.session('after-upgrade')
        self.record('signed-upgrade', 'Stable installed deployment converted to the signed candidate with automatic migrations')
        backup = Path('/var/backups/geoflow-updater/primary') / checkpoint
        for name in ['database.dump', 'storage.tar.gz', 'redis.tar.gz', 'site.env', 'version.json', 'managed/instance.yml', 'managed/release.env', 'managed/docker-compose.yml']:
            require((backup / name).is_file(), 'Missing complete backup component: ' + name)
        self.mutate('backups', 'backup', label='manual-backup')
        self.corrupt('manual-rollback', config=True)
        self.mutate('rollbacks', 'rollback', {'recovery_point_id': checkpoint}, 'manual-rollback')
        self.restored('manual-rollback')
        self.record('complete-backup-restore', 'A later manual backup retained the update checkpoint; explicit restore recovered all tested surfaces')

    def install(self):
        self.repository(self.candidate / 'tuf/repository')
        self.current = 'fresh-install-retry'
        FAULT.write_text(json.dumps({'stage': 'fresh-install', 'after': True}))
        FAULT.chmod(0o600)
        command = ['geoflow-updater', 'install', '--instance', 'primary', '--root', str(ROOT), '--url', 'http://localhost:18080']
        with (self.evidence / 'installation.log').open('w') as output:
            process = subprocess.Popen(command, env=self.environment, stdout=output, stderr=output, start_new_session=True)
            self.wait(lambda: MARKER.exists() or process.poll() is not None, 'fresh installation fault')
            require(MARKER.exists() and process.poll() is None, 'Installation exited before the fault boundary')
            before = {name: sha(ROOT / name) for name in ['.env.prod', 'install-credentials.txt']}
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=30)
        FAULT.unlink()
        self.run(*command)
        for name, digest in before.items():
            require(sha(ROOT / name) == digest, 'First-install retry replaced ' + name)
        self.run(*command)
        self.authorize()
        self.healthy('fresh-install')
        credentials = (ROOT / 'install-credentials.txt').read_text()
        password = re.search(r'^Password: (.+)$', credentials, re.M)
        require(password is not None, 'Initial administrator credentials missing')
        self.login(self.mask(password[1]))
        self.record('fresh-install-retry', 'Killed installer after real administrator initialization; repeated install preserved credentials and completed readiness')

    def finish(self, success):
        if not self.owns_evidence:
            return
        self.save('result.json', {'schema_version': 1, 'candidate_run_id': str(self.identity['candidate_run_id']),
            'platform': self.args.platform, 'kernel_arch': os.uname().machine, 'mode': self.args.mode,
            'harness_commit': os.environ.get('GITHUB_SHA'), 'result': 'pass' if success else 'fail',
            'failed_check': '' if success else self.current, 'checks': self.checks})
        if self.installed:
            self.run('journalctl', '-u', 'geoflow-updater', '--output=cat', '--lines=1000', check=False)
            self.run('/usr/bin/docker', 'ps', '-a', check=False)
            for container in self.run('/usr/bin/docker', 'ps', '-aq', check=False, log=False).splitlines():
                self.run('/usr/bin/docker', 'logs', '--tail', '80', container, check=False)
        # Load generated credentials before redacting captured diagnostics.
        for path in [ROOT / '.env.prod', ROOT / 'install-credentials.txt']:
            if path.exists():
                for line in path.read_text().splitlines():
                    if re.search(r'PASSWORD|SECRET|APP_KEY|Password:', line):
                        self.mask(line.split('=', 1)[-1].removeprefix('Password: ').strip())
        for path in self.evidence.rglob('*'):
            if path.is_file():
                path.write_text(self.redact(path.read_text()))
                path.chmod(0o600)
        uid, gid = int(os.environ['SUDO_UID']), int(os.environ['SUDO_GID'])
        for path in [self.evidence, *self.evidence.rglob('*')]:
            os.chown(path, uid, gid)
        if self.server:
            self.server.terminate()
            self.server.wait(timeout=10)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--candidate', required=True)
    parser.add_argument('--evidence', required=True)
    parser.add_argument('--platform', required=True, choices=['linux-amd64', 'linux-arm64'])
    parser.add_argument('--mode', required=True, choices=['upgrade', 'install'])
    rehearsal = Rehearsal(parser.parse_args())
    success = False
    try:
        rehearsal.setup()
        getattr(rehearsal, rehearsal.args.mode)()
        success = True
    except Exception as error:
        print('[planned-host] FAIL ' + rehearsal.redact(str(error)), file=sys.stderr, flush=True)
    finally:
        rehearsal.finish(success)
    return 0 if success else 1


if __name__ == '__main__':
    sys.exit(main())

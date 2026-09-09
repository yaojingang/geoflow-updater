"""Live HTTP, queued writes and Reverb checks for the isolated online fixture."""
import json
from contextlib import closing
import queue
import threading
import time
import urllib.request

import websocket


class ReverbConnection:
    def __init__(self, url):
        self.socket = websocket.create_connection(url, origin='http://localhost:18080', timeout=20)
        if json.loads(self.socket.recv())['event'] != 'pusher:connection_established':
            raise RuntimeError('Reverb connection did not initialize')
        self.socket.send(json.dumps({'event': 'pusher:subscribe', 'data': {'channel': 'rehearsal'}}))
        if json.loads(self.socket.recv())['event'] != 'pusher_internal:subscription_succeeded':
            raise RuntimeError('Reverb subscription failed')
        self.events = queue.Queue()
        self.stopping = threading.Event()
        self.reader = threading.Thread(target=self.receive, daemon=True)
        self.reader.start()

    def receive(self):
        while not self.stopping.is_set():
            try:
                event = json.loads(self.socket.recv())
                if event['event'] == 'pusher:ping':
                    self.socket.send(json.dumps({'event': 'pusher:pong', 'data': {}}))
                else:
                    self.events.put(event)
            except websocket.WebSocketTimeoutException:
                continue
            except Exception as error:
                self.events.put(error)
                return

    def close(self):
        self.stopping.set()
        self.socket.close()
        self.reader.join(timeout=25)


def run_online(rehearsal, root, instance, fault, marker, require, sha):
    rehearsal.install()
    identity = rehearsal.identity['online_fixture']
    fixture = rehearsal.candidate / 'online-fixture'
    manifest = fixture / 'targets-source/releases/current.json'
    require(sha(manifest) == identity['release_manifest_sha256'], 'Online fixture manifest differs')
    target = json.loads(manifest.read_text())
    require(identity['base_app_digest'] == rehearsal.identity['geoflow']['app_digest'] and
            identity['source_sequence'] == rehearsal.config()['release_sequence'] and
            target['release_sequence'] == identity['target_sequence'], 'Online fixture source differs')
    require(sha(fixture / 'targets-source' / target['upgrade_plan_target']) == identity['upgrade_plan_sha256'], 'Online fixture plan differs')
    rehearsal.fixture()
    environment = dict(line.split('=', 1) for line in (root / '.env.prod').read_text().splitlines() if '=' in line)

    def connect():
        return ReverbConnection('ws://localhost:18080/reverb/app/' + environment['REVERB_APP_KEY'] +
            '?protocol=7&client=js&version=8.4.0&flash=false')

    def broadcast(connection, label):
        code = '''require 'vendor/autoload.php'; $app = require 'bootstrap/app.php';
            $app->make(Illuminate\\Contracts\\Console\\Kernel::class)->bootstrap();
            config(['broadcasting.connections.reverb.options.host' => $argv[2]]);
            Illuminate\\Support\\Facades\\Broadcast::connection()->broadcast(['rehearsal'], 'rehearsal.probe', ['value' => $argv[1]]);'''
        container = rehearsal.compose('ps', '--quiet', 'reverb')
        networks = json.loads(rehearsal.run('/usr/bin/docker', 'inspect', '--format', '{{json .NetworkSettings.Networks}}', container))
        address = networks['geoflow-primary-' + rehearsal.config()['active_slot'] + '_app']['IPAddress']
        rehearsal.compose('exec', '-T', '--user', '33:33', 'app', 'php', '-r', code, label, address)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            event = connection.events.get(timeout=20)
            if isinstance(event, Exception):
                raise event
            if event['event'] == 'rehearsal.probe':
                require(json.loads(event['data'])['value'] == label, 'Reverb message differs')
                return
        raise RuntimeError('Reverb broadcast did not arrive')

    queue_probe = root / 'storage/app/rehearsal-queue.php'
    rehearsal.query('ALTER TABLE geoflow_rehearsal_markers ADD COLUMN executed_slot TEXT, ADD COLUMN executed_sequence BIGINT;')
    queue_probe.write_text('''<?php
require '/var/www/html/vendor/autoload.php';
$app = require '/var/www/html/bootstrap/app.php';
$app->make(Illuminate\\Contracts\\Console\\Kernel::class)->bootstrap();
for ($id = (int) $argv[1]; $id < (int) $argv[1] + 20; $id++) {
    Illuminate\\Support\\Facades\\DB::table('geoflow_rehearsal_markers')->insert(['id' => $id, 'value' => '0']);
    dispatch(function () use ($id) {
        usleep(250000);
        Illuminate\\Support\\Facades\\DB::statement("UPDATE geoflow_rehearsal_markers SET value = ((value::integer) + 1)::text, executed_slot = ?, executed_sequence = ? WHERE id = ?", [getenv('GEOFLOW_DEPLOYMENT_SLOT'), getenv('GEOFLOW_RELEASE_SEQUENCE'), $id]);
    })->onQueue('default');
}
''')
    queue_probe.chmod(0o644)

    def enqueue(first, source=None):
        rehearsal.compose('stop', '--timeout', '30', 'queue', config=source)
        rehearsal.compose('exec', '-T', '--user', '33:33', 'app', 'php', '/var/www/html/storage/app/rehearsal-queue.php', str(first))
        require(rehearsal.query(f"SELECT count(*) FROM geoflow_rehearsal_markers WHERE id >= {first} AND id < {first + 20} AND value='0';") == '20', 'Queued jobs must remain pending before handover')

    def consumed(first):
        rehearsal.wait(lambda: rehearsal.query(f"SELECT count(*) FROM geoflow_rehearsal_markers WHERE id >= {first} AND id < {first + 20} AND value='1';") == '20', 'queued writes after handover', 120)
        require(rehearsal.query(f"SELECT count(*) FROM geoflow_rehearsal_markers WHERE id >= {first} AND id < {first + 20} AND value <> '1';") == '0', 'Duplicate queue execution detected')
        config = rehearsal.config()
        require(rehearsal.query(f"SELECT count(*) FROM geoflow_rehearsal_markers WHERE id >= {first} AND id < {first + 20} AND executed_slot = '{config['active_slot']}' AND executed_sequence = {config['release_sequence']};") == '20', 'Pending jobs did not execute on the new worker slot')
        rehearsal.save(f'queue-{first}.txt', rehearsal.query(f"SELECT id, value, executed_slot, executed_sequence FROM geoflow_rehearsal_markers WHERE id >= {first} AND id < {first + 20} ORDER BY id;"))

    stopping = threading.Event()
    requests, errors = [], []

    def http_probe():
        while not stopping.is_set():
            started = time.monotonic()
            try:
                with urllib.request.urlopen('http://localhost:18080/up', timeout=10) as response:
                    require(response.status == 200, 'Online public request failed')
                with rehearsal.browser.open('http://localhost:18080/geo_admin/dashboard', timeout=10) as response:
                    require(response.status == 200 and '/dashboard' in response.url, 'Authenticated session was lost during transition')
                    response.read()
                requests.append(time.monotonic() - started)
            except Exception as error:
                errors.append(str(error))
            stopping.wait(0.5)

    old = connect()
    broadcast(old, 'before-online')
    failed_jobs = rehearsal.query('SELECT count(*) FROM failed_jobs;')
    worker = threading.Thread(target=http_probe, daemon=True)
    worker.start()
    try:
        rehearsal.repository(fixture / 'tuf/repository')
        plan = rehearsal.api('GET', 'plan')
        require(plan['strategy'] == 'online' and not plan['layout_change'] and not plan['pending_migrations'], 'Fixture is not eligible for an application-identical online upgrade')
        source = rehearsal.config()
        marker.unlink(missing_ok=True)
        fault.write_text(json.dumps({'stage': 'workers'}))
        fault.chmod(0o600)
        operation = rehearsal.api('POST', 'updates', {'expected_plan_sha256': plan['plan_sha256']}, 'update', expected=202)
        def activated():
            current = rehearsal.api('GET', 'operations/current')
            require(current['id'] == operation['id'] and not current.get('completed_at'), 'Online update ended before observation')
            return marker.exists()
        rehearsal.wait(activated, 'online worker handover boundary')
        require(rehearsal.config()['release_sequence'] == identity['target_sequence'], 'Online candidate did not become active')
        enqueue(100, source)
        broadcast(old, 'cross-slot')
        rehearsal.record('online-reverb-cross-slot', 'Old WebSocket received a broadcast from the newly active application through Redis scaling')
        rehearsal.corrupt('online-live-writes')
        fault.unlink()
        result = rehearsal.operation(operation['id'], ['succeeded'], 'online-upgrade')
        require(not result.get('recovery_point_id'), 'Online update incorrectly created a full restoration checkpoint')
        consumed(100)
        old.close()
        with closing(connect()) as connection:
            broadcast(connection, 'after-online')
        rehearsal.record('online-upgrade', 'Signed application-identical fixture switched slots and completed its database snapshot')
        enqueue(200)
        rehearsal.mutate('switch-backs', 'update', label='online-switch-back')
        require(rehearsal.config()['release_sequence'] == identity['source_sequence'], 'Application switch-back did not restore source slot')
        consumed(200)
        require(rehearsal.query('SELECT count(*) FROM failed_jobs;') == failed_jobs, 'Queue produced failed jobs')
        for value in [rehearsal.query('SELECT value FROM geoflow_rehearsal_markers WHERE id=1;'),
                      rehearsal.redis('GET', 'geoflow:planned-rehearsal:marker'),
                      (root / 'storage/app/rehearsal.txt').read_text()]:
            require(value == 'changed', 'Application switch-back rewound live data')
        with closing(connect()) as connection:
            broadcast(connection, 'after-switch-back')
        rehearsal.record('online-reverb-reconnect', 'Browser-equivalent WebSocket reconnected and received broadcasts after both transitions')
        rehearsal.record('online-queue-handover', 'Forty jobs queued with the old consumer stopped ran once on the destination workers; slot and sequence recorded')
        rehearsal.record('online-switch-back-preserves-data', 'Application switch-back retained database, Redis and storage writes')
        rehearsal.healthy('online-switch-back')
    finally:
        fault.unlink(missing_ok=True)
        old.close()
        stopping.set()
        worker.join(timeout=45)
    rehearsal.save('online-http.json', {'requests': len(requests), 'errors': errors, 'max_seconds': max(requests, default=0)})
    require(not worker.is_alive() and not errors and len(requests) >= 10, 'Continuous HTTP/session requests failed: ' + json.dumps(errors))
    rehearsal.record('online-http-session', 'Continuous public health and authenticated requests succeeded across upgrade and switch-back')

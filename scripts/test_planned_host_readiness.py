"""Exercise delayed control-socket startup without an installed host."""
import importlib.util
import json
from pathlib import Path
import socket
import tempfile
import threading
import time
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location('planned_host', Path(__file__).with_name('planned-host-rehearsal.py'))
host = importlib.util.module_from_spec(spec)
spec.loader.exec_module(host)


class ReadinessTests(unittest.TestCase):
    def setUp(self):
        self.rehearsal = host.Rehearsal.__new__(host.Rehearsal)
        self.rehearsal.identity = {'updater': {'version': '0.4.0-rc.3'}}

    def with_server(self, version, check):
        requests, failures = [], []
        with tempfile.TemporaryDirectory(prefix='gfr-', dir='/tmp') as directory:
            path = str(Path(directory) / 'api.sock')

            def serve():
                try:
                    time.sleep(0.1)
                    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
                        server.settimeout(5)
                        server.bind(path)
                        server.listen(1)
                        connection, _ = server.accept()
                        with connection:
                            connection.settimeout(5)
                            requests.append(connection.recv(4096).split(b'\r\n', 1)[0])
                            body = json.dumps({'schema_version': 1, 'status': 'ok', 'version': version}).encode()
                            connection.sendall(b'HTTP/1.1 200 OK\r\nContent-Length: ' + str(len(body)).encode() +
                                               b'\r\nConnection: close\r\n\r\n' + body)
                except Exception as error:
                    failures.append(error)

            worker = threading.Thread(target=serve)
            with patch.object(host, 'SOCKET', path):
                worker.start()
                try:
                    check()
                finally:
                    worker.join(timeout=7)
            self.assertFalse(worker.is_alive())
            self.assertEqual(failures, [])
            self.assertEqual(requests, [b'GET /v1/health HTTP/1.1'])

    def test_waits_for_real_delayed_socket(self):
        self.with_server('0.4.0-rc.3', lambda: self.rehearsal.wait_for_agent(timeout=4))

    def test_rejects_another_binary(self):
        def check():
            with self.assertRaisesRegex(RuntimeError, 'candidate version differs'):
                self.rehearsal.wait_for_agent(timeout=4)
        self.with_server('0.3.0', check)

    def test_missing_socket_has_bounded_wait(self):
        with tempfile.TemporaryDirectory(prefix='gfr-', dir='/tmp') as directory:
            with patch.object(host, 'SOCKET', str(Path(directory) / 'absent.sock')):
                with self.assertRaisesRegex(RuntimeError, 'Timed out waiting for restarted updater control API'):
                    self.rehearsal.wait_for_agent(timeout=0.1)


if __name__ == '__main__':
    unittest.main()

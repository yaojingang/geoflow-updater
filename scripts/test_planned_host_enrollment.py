"""Check the unmanaged fixture and both-architecture enrollment entry points."""
import importlib.util
from pathlib import Path
import subprocess
import sys
import unittest

import yaml

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('planned_host', ROOT / 'scripts/planned-host-rehearsal.py')
host = importlib.util.module_from_spec(spec)
spec.loader.exec_module(host)


class EnrollmentEntryTests(unittest.TestCase):
    def test_unmanaged_fixture_uses_real_pinned_services_without_control_state(self):
        release = {'app_image': 'example/app@sha256:' + 'a' * 64,
                   'web_image': 'example/web@sha256:' + 'b' * 64,
                   'postgres_images': {'18': 'example/postgres@sha256:' + 'c' * 64},
                   'redis_images': {'8': 'example/redis@sha256:' + 'd' * 64}}
        compose = host.unmanaged_compose(yaml.safe_load((ROOT / 'assets/docker-compose.managed.yml').read_text()), release)
        text = yaml.safe_dump(compose)
        for variable in ['GEOFLOW_UPDATER_', 'GEOFLOW_INSTANCE_ID', 'GEOFLOW_APP_IMAGE',
                         'GEOFLOW_WEB_IMAGE', 'GEOFLOW_POSTGRES_IMAGE', 'GEOFLOW_REDIS_IMAGE']:
            self.assertNotIn(variable, text)
        self.assertNotIn('/run/geoflow-updater', text)
        self.assertNotIn('/run/secrets', text)
        images = {'postgres': release['postgres_images']['18'], 'redis': release['redis_images']['8'],
                  'web': release['web_image']}
        for name, service in compose['services'].items():
            with self.subTest(service=name):
                self.assertEqual(service['image'], images.get(name, release['app_image']))
                self.assertNotIn('build', service)
        self.assertEqual(compose['services']['app']['command'], ['php-fpm', '-F'])
        self.assertEqual(compose['services']['redis']['volumes'], ['./docker-data/prod/redis:/data'])
        self.assertIn('php', compose['services']['queue']['command'])
        self.assertIn('queue:work', compose['services']['queue']['command'])
        self.assertIn('geoflow:schedule-work', compose['services']['scheduler']['command'])
        self.assertIn('reverb:start', compose['services']['reverb']['command'])

    def test_eight_native_host_jobs_and_enrollment_cli_are_available(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/planned-acceptance.yml').read_text())
        matrix = workflow['jobs']['host']['strategy']['matrix']['include']
        expected = {(platform, mode, runner)
                    for platform, runner in [('linux-amd64', 'ubuntu-24.04'), ('linux-arm64', 'ubuntu-24.04-arm')]
                    for mode in ['upgrade', 'install', 'online', 'enrollment']}
        self.assertEqual(len(matrix), 8)
        self.assertEqual({(item['platform'], item['mode'], item['runner']) for item in matrix}, expected)
        result = subprocess.run([sys.executable, str(ROOT / 'scripts/planned-host-rehearsal.py'), '--help'],
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertRegex(result.stdout, r'--mode\s+\{[^}]*\benrollment\b')


if __name__ == '__main__':
    unittest.main()

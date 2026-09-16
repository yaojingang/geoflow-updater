import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("core_protocol", Path(__file__).with_name("core-protocol.py"))
protocol = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(protocol)


class CoreProtocolTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "deployment").mkdir()
        self.path = self.root / "deployment/recovery-contract.json"

    def test_legacy_strategy_keeps_its_original_floor(self):
        self.assertEqual(3, protocol.minimum_protocol(self.root, "maintenance"))
        self.assertEqual(4, protocol.minimum_protocol(self.root, "online"))

    def test_coordinated_core_requires_five_independent_of_version(self):
        self.path.write_text(json.dumps(protocol.CONTRACT))
        (self.root / "version.json").write_text('{"version":"3.1.0"}')
        self.assertEqual(5, protocol.minimum_protocol(self.root, "maintenance"))
        with self.assertRaises(ValueError):
            protocol.minimum_protocol(self.root, "online")

    def test_invalid_declarations_cannot_fall_back_to_legacy(self):
        invalid = ["{}", "null", "[]", "{", json.dumps(dict(protocol.CONTRACT, minimum_updater_protocol=4)),
                   json.dumps(dict(protocol.CONTRACT, schema_version=True)), json.dumps(dict(protocol.CONTRACT, extra=1)),
                   json.dumps(protocol.CONTRACT)[:-1] + ',"minimum_updater_protocol":5}']
        for raw in invalid:
            with self.subTest(raw=raw):
                self.path.write_text(raw)
                with self.assertRaises(ValueError):
                    protocol.minimum_protocol(self.root, "maintenance")

    def test_symlink_directory_and_oversize_contract_are_rejected(self):
        self.path.symlink_to(self.root / 'missing')
        with self.assertRaises(ValueError):
            protocol.minimum_protocol(self.root, "maintenance")
        self.path.unlink()
        self.path.mkdir()
        with self.assertRaises(ValueError):
            protocol.minimum_protocol(self.root, "maintenance")
        self.path.rmdir()
        self.path.write_text(' ' * 4097)
        with self.assertRaises(ValueError):
            protocol.minimum_protocol(self.root, "maintenance")


if __name__ == '__main__':
    unittest.main()

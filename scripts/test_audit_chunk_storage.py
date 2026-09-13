import importlib.util
from pathlib import Path
import unittest


spec = importlib.util.spec_from_file_location("audit_chunk_storage", Path(__file__).with_name("audit-chunk-storage.py"))
audit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(audit)


class FakeStorage:
    def __init__(self):
        a, b = "a" * 64, "b" * 64
        self.manifest = {"chunks": [{"hash": a}, {"hash": a}, {"hash": b}, {"hash": "zero"}]}
        item = lambda name, size: {"name": name, "size": str(size), "generation": "1"}
        self.pages = {
            "": {"items": [item("chunks/" + a, 10)], "nextPageToken": "empty"},
            "empty": {"nextPageToken": "last"},
            "last": {"items": [item("chunks/" + b, 20), item("hib/id/manifest.json", 30),
                item("handoff/id/gen/record.json", 40), item("handoff/id/control.json", 50)]},
        }

    def page(self, token, count):
        return self.pages[token]

    def object_json(self, item):
        if item["name"].startswith("hib/"):
            return self.manifest
        descriptor = {"ref": {"generation": "gen"}, "manifest": self.manifest}
        return {"descriptor": descriptor} if item["name"].endswith("control.json") else descriptor


class StorageAuditTests(unittest.TestCase):
    def test_empty_page_and_duplicate_generation_references(self):
        report = audit.measure(FakeStorage(), 100, 100, 60)
        self.assertTrue(report["listing_complete"])
        self.assertTrue(report["manifest_scan_complete"])
        self.assertEqual(report["pages"], 3)
        self.assertEqual(report["live_object_bytes"], 150)
        self.assertEqual(report["prefixes"]["chunks"], {"objects": 2, "bytes": 30})
        self.assertEqual(report["observed_manifests"]["sets"], 2)
        self.assertEqual(report["observed_manifests"]["cross_set_multiplier"], 2)
        self.assertEqual(report["observed_manifests"]["hashes_missing_from_listing"], 0)

    def test_listing_and_manifest_limits_are_explicit(self):
        report = audit.measure(FakeStorage(), 1, 100, 60)
        self.assertFalse(report["listing_complete"])
        self.assertFalse(report["manifest_scan_complete"])
        report = audit.measure(FakeStorage(), 100, 1, 60)
        self.assertTrue(report["listing_complete"])
        self.assertFalse(report["manifest_scan_complete"])
        self.assertEqual(report["observed_manifests"]["sets"], 1)

    def test_repeated_page_token_fails(self):
        fake = FakeStorage()
        fake.pages["empty"]["nextPageToken"] = "empty"
        with self.assertRaisesRegex(ValueError, "repeated a page token"):
            audit.measure(fake, 100, 100, 60)

    def test_bad_manifest_is_not_reported_as_complete(self):
        fake = FakeStorage()
        fake.manifest = {"chunks": [{"hash": "bad"}]}
        report = audit.measure(fake, 100, 100, 60)
        self.assertFalse(report["manifest_scan_complete"])
        self.assertEqual(len(report["errors"]), 3)


if __name__ == "__main__":
    unittest.main()

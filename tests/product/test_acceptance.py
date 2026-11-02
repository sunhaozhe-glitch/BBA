"""Product acceptance rejects incomplete proof even after valid re-manifesting."""
import importlib.util
import json
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("product_acceptance", ROOT / "tools/product-acceptance.py")
ACCEPTANCE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ACCEPTANCE)


class ProductEvidenceContractTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="bba-product-proof-")
        self.addCleanup(self.tmp.cleanup)
        self.package = Path(self.tmp.name)

    def write_json(self, name, value):
        path = self.package / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value) + "\n", encoding="utf-8")

    def test_hash_valid_but_absent_business_evidence_is_rejected(self):
        self.write_json("acceptance.json", {"state": "ENGINEERING_ACCEPTED", "mode": "fixture", "requirements": []})
        ACCEPTANCE.write_package_manifest(self.package)
        result = ACCEPTANCE.verify_package(ROOT, self.package)
        self.assertFalse(result["ok"])
        self.assertIn("requirements-incomplete", [p["code"] for p in result["problems"]])

    def test_rehashed_extra_and_missing_artifacts_do_not_grant_coverage(self):
        rows = [{"id": item, "status": "PASS", "evidence": ["made-up.json"], "assertions": {"passed": True}}
                for item in ACCEPTANCE.REQUIREMENTS]
        self.write_json("acceptance.json", {"state": "ENGINEERING_ACCEPTED", "mode": "fixture", "requirements": rows})
        ACCEPTANCE.write_package_manifest(self.package)
        result = ACCEPTANCE.verify_package(ROOT, self.package)
        self.assertFalse(result["ok"])
        self.assertTrue(any(p["code"] == "requirement-evidence-missing" for p in result["problems"]))

    def test_source_manifest_escape_and_duplicate_are_rejected(self):
        (self.package / "source-files.sha256").write_text("0" * 64 + "  ../outside\n" + "0" * 64 + "  ../outside\n")
        ACCEPTANCE.write_package_manifest(self.package)
        result = ACCEPTANCE.verify_package(ROOT, self.package)
        self.assertFalse(result["ok"])
        self.assertTrue(any("source" in p["code"] for p in result["problems"]))

    def test_release_requires_real_scoped_evidence(self):
        self.write_json("release/evidence.json", {"authorized": True, "state": "RELEASE_ACCEPTED"})
        problems = ACCEPTANCE.release_problems(self.package)
        codes = [p["code"] for p in problems]
        self.assertIn("release-prerequisite-missing", codes)
        self.assertGreaterEqual(sum(code == "release-prerequisite-missing" for code in codes), 8)

    def test_cli_failed_verify_and_release_are_nonzero(self):
        for command in ("verify", "release-check"):
            with self.subTest(command=command):
                completed = subprocess.run(["bash", str(ROOT / "tools/product-acceptance.sh"), command,
                                            "--package", str(self.package)], cwd=ROOT, capture_output=True, text=True)
                self.assertNotEqual(completed.returncode, 0)
                if command == "release-check":
                    self.assertIn("BLOCKED", completed.stdout)


    def test_rehashed_business_assertion_forgery_is_rejected(self):
        self.write_json("resources.json", {"owned_resources_removed": True, "preexisting_preserved": True, "production_dsn_used": False})
        proof = {"state": "PASS", "assertions": ACCEPTANCE.FIXTURE.require_checks("resources", self.package), "evidence": ["resources.json"]}
        self.assertEqual([], ACCEPTANCE.scenario_problems("resources", self.package, proof))
        self.write_json("scenarios/resources.json", proof)
        ACCEPTANCE.write_package_manifest(self.package)
        forged = dict(proof, assertions={"forged": True})
        self.write_json("scenarios/resources.json", forged)
        (self.package / "manifest.sha256").unlink()
        ACCEPTANCE.write_package_manifest(self.package)
        problems = ACCEPTANCE.scenario_problems("resources", self.package, forged)
        self.assertIn("scenario-assertions-mismatch", [problem["code"] for problem in problems])

    def test_rehashed_missing_business_proof_is_rejected(self):
        self.write_json("resources.json", {"owned_resources_removed": True, "preexisting_preserved": True, "production_dsn_used": False})
        proof = {"state": "PASS", "assertions": ACCEPTANCE.FIXTURE.require_checks("resources", self.package), "evidence": ["resources.json"]}
        self.assertEqual([], ACCEPTANCE.scenario_problems("resources", self.package, proof))
        self.write_json("scenarios/resources.json", proof)
        ACCEPTANCE.write_package_manifest(self.package)
        (self.package / "scenarios/resources.json").unlink()
        (self.package / "manifest.sha256").unlink()
        ACCEPTANCE.write_package_manifest(self.package)
        read_problems = []
        missing = ACCEPTANCE.read_json(self.package / "scenarios/resources.json", read_problems, "scenario-proof-missing")
        self.assertIsNone(missing)
        self.assertIn("scenario-proof-missing", [problem["code"] for problem in read_problems])
        proof_problems = ACCEPTANCE.scenario_problems("resources", self.package, missing)
        self.assertIn("scenario-proof-invalid", [problem["code"] for problem in proof_problems])

    def test_rehashed_missing_browser_screenshot_is_rejected(self):
        browser = self.package / "browser"
        browser.mkdir()
        screenshots = []
        for index in range(8):
            name = f"screen-{index}.png"
            (browser / name).write_bytes(b"fixture screenshot")
            screenshots.append({"file": name})
        self.write_json("browser/report.json", {
            "ok": True, "state": "PASS", "checks": [{"ok": True}],
            "screenshots": screenshots,
            "report_context_observed": {"source": "collector.summary.v1"},
            "requests": [{"source": "server", "status": 200}]
        })
        proof = {"state": "PASS", "assertions": ACCEPTANCE.FIXTURE.require_checks("browser", self.package), "evidence": ["browser/report.json"]}
        self.write_json("scenarios/browser.json", proof)
        self.assertEqual([], ACCEPTANCE.scenario_problems("browser", self.package, proof))
        ACCEPTANCE.write_package_manifest(self.package)
        (browser / "screen-0.png").unlink()
        (self.package / "manifest.sha256").unlink()
        ACCEPTANCE.write_package_manifest(self.package)
        problems = ACCEPTANCE.scenario_problems("browser", self.package, proof)
        self.assertIn("scenario-business-evidence-invalid", [problem["code"] for problem in problems])

if __name__ == "__main__":
    unittest.main()

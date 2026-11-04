"""Guard tests for the B7 manual fixture launcher (fixture-only; no Docker).

Run: python3 -m unittest tests.product.test_b7_manual_fixture_launcher -v
"""
import ast
import re
import importlib.util
import json
import os
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("b7_manual_fixture", ROOT / "tools/b7-manual-fixture.py")
LAUNCHER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(LAUNCHER)


class OutputDirTest(unittest.TestCase):
    def test_direct_child_of_evals_results_required(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "evals" / "results").mkdir(parents=True)
            ok = root / "evals" / "results" / "new-run-1"
            self.assertEqual(LAUNCHER.guard_output_dir(ok, root), ok.resolve())
            ok.resolve().mkdir()
            with self.assertRaises(ValueError):
                LAUNCHER.guard_output_dir(ok, root)  # must not exist yet
            with self.assertRaises(ValueError):
                LAUNCHER.guard_output_dir(root / "build" / "elsewhere", root)
            with self.assertRaises(ValueError):
                LAUNCHER.guard_output_dir(root / "evals" / "results" / "nested" / "deeper", root)

    def test_launcher_refuses_inherited_dsn(self):
        for var in ("BBA_TASKENGINE_DSN", "DATABASE_URL"):
            with self.assertRaises(ValueError):
                LAUNCHER.guard_inherited_dsn({var: "postgres://prod/db"})
        LAUNCHER.guard_inherited_dsn({"PATH": "/usr/bin"})


class PortTest(unittest.TestCase):
    def test_live_and_default_ports_refused(self):
        for port in (5432, 18421, 18431, 18432, 18451, 18452):
            with self.assertRaises(ValueError):
                LAUNCHER.guard_port(port)
        self.assertEqual(LAUNCHER.guard_port(31234), 31234)
        with self.assertRaises(ValueError):
            LAUNCHER.guard_port(80)

    def test_loopback_only(self):
        LAUNCHER.guard_loopback("127.0.0.1")
        for host in ("0.0.0.0", "localhost", "10.0.0.5"):
            with self.assertRaises(ValueError):
                LAUNCHER.guard_loopback(host)


class SecretFilesTest(unittest.TestCase):
    def test_token_files_must_be_0600(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "operator-token"
            LAUNCHER.write_private(path, "tok")
            LAUNCHER.guard_token_file_mode(path)
            os.chmod(path, 0o644)
            with self.assertRaises(ValueError):
                LAUNCHER.guard_token_file_mode(path)

    def test_resolver_file_is_digest_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            good = Path(tmp) / "resolver.json"
            good.write_text(json.dumps({"credentials": [
                {"token_sha256": "a" * 64, "workspace_id": "w", "principal_id": "p",
                 "permissions": ["conversation.read"]}]}))
            LAUNCHER.guard_resolver_file(good)
            bad = Path(tmp) / "bad.json"
            bad.write_text(json.dumps({"credentials": [
                {"token_sha256": "not-a-digest", "workspace_id": "w", "principal_id": "p",
                 "permissions": ["conversation.read"]}]}))
            with self.assertRaises(ValueError):
                LAUNCHER.guard_resolver_file(bad)
            plaintext = Path(tmp) / "plain.json"
            plaintext.write_text(json.dumps({"credentials": [
                {"token_sha256": "b" * 64, "token": "hunter2", "workspace_id": "w",
                 "principal_id": "p", "permissions": ["conversation.read"]}]}))
            with self.assertRaises(ValueError):
                LAUNCHER.guard_resolver_file(plaintext)


class OwnershipTest(unittest.TestCase):
    def test_only_owned_containers_are_cleaned(self):
        LAUNCHER.guard_owned_container("bba-b7fix-abcd1234", {"bba.b7-manual-fixture": "abcd1234"})
        with self.assertRaises(ValueError):
            LAUNCHER.guard_owned_container("bba-dev-stack-default-db", {"bba.b7-manual-fixture": "abcd1234"})
        with self.assertRaises(ValueError):
            LAUNCHER.guard_owned_container("bba-b7fix-abcd1234", {})

    def test_pid_must_belong_to_this_fixture_binary(self):
        self.assertFalse(LAUNCHER.guard_owned_pid(os.getpid(), "/nonexistent/output/bin/taskengine-server"))


class RuntimeSeedConsistencyTest(unittest.TestCase):
    def test_resolver_runtime_has_current_seeded_owner_and_credential(self):
        # Parse only public identity literals; never evaluate token expressions.
        tree = ast.parse((ROOT / "tools/b7-manual-fixture.py").read_text())
        runtime = []
        for node in ast.walk(tree):
            if not isinstance(node, ast.Dict):
                continue
            fields = {k.value: v for k, v in zip(node.keys, node.values)
                      if isinstance(k, ast.Constant) and isinstance(k.value, str)}
            if "runtime_id" in fields and "account_ids" in fields:
                runtime.append({key: ast.literal_eval(fields[key])
                                for key in ("workspace_id", "runtime_id", "owner_epoch", "account_ids")})
        self.assertTrue(runtime, "resolver must declare a fixture runtime")
        seed = (ROOT / "tools/b7-manual-fixture-seed.sql").read_text()
        credentials = re.search(r"INSERT INTO core.runtime_credentials\([^;]+?VALUES(.*?);", seed, re.S)
        self.assertIsNotNone(credentials, "seed must persist runtime credentials")
        principals = re.search(r"INSERT INTO core.principal\([^;]+?VALUES(.*?);", seed, re.S)
        accounts = re.search(r"INSERT INTO core.account\([^;]+?VALUES(.*?);", seed, re.S)
        self.assertIsNotNone(principals)
        self.assertIsNotNone(accounts)
        for item in runtime:
            ws, rid, epoch = item["workspace_id"], item["runtime_id"], item["owner_epoch"]
            self.assertIn(f"('{ws}','{rid}')", principals[1], "runtime principal anchor missing")
            self.assertEqual(epoch, 1, "seed account owner_epoch uses DDL default 1")
            self.assertTrue(item["account_ids"])
            for account in item["account_ids"]:
                self.assertIn(f"('{ws}','{account}','{rid}',", accounts[1], "resolver is not current account owner")
                self.assertIn(f"('{ws}','{rid}','{account}',{epoch},true,false)", credentials[1],
                              "resolver needs an active, non-authoritative persisted credential")


class NoLiveConstraintTest(unittest.TestCase):
    def test_server_args_stay_fixture_only(self):
        args = LAUNCHER.server_args(31234, "/tmp/out/resolver.json")
        joined = " ".join(args)
        self.assertIn("127.0.0.1:31234", joined)
        self.assertNotIn("-worker", joined)
        self.assertNotIn("-development-operations", joined)
        self.assertNotIn("-login-fixture", joined)
        self.assertNotIn("-model-fixture", joined)
        LAUNCHER.guard_port(31234)
        with self.assertRaises(ValueError):
            LAUNCHER.server_args(18421, "/tmp/out/resolver.json")


if __name__ == "__main__":
    unittest.main()

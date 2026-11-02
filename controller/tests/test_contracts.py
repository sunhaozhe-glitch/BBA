import json
from pathlib import Path
import unittest

from jsonschema import Draft202012Validator
from bba_controller.contracts import TOOLS, output_for
from bba_controller.model_contracts import CONFIG


class ExportedContractTests(unittest.TestCase):
    def test_exported_contracts_match_runtime_and_validate(self):
        root = Path(__file__).resolve().parents[1]/'schemas'
        self.assertEqual(json.loads((root/'model-config.schema.json').read_text()),CONFIG)
        for name, schema in TOOLS.items():
            for direction, expected in [('input',schema),('output',output_for(name))]:
                path = root/(name+'.'+direction+'.json')
                self.assertTrue(path.is_file(),str(path.name))
                actual = json.loads(path.read_text())
                self.assertEqual(actual,expected)
                Draft202012Validator.check_schema(actual)

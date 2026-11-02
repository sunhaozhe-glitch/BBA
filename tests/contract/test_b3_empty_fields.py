import copy
import json
import unittest
from pathlib import Path

from tools.validate_contracts import ContractLoader

ROOT = Path(__file__).resolve().parents[2]


class B3EmptyFieldsTest(unittest.TestCase):
    def test_empty_does_not_mean_missing_null_or_unbounded(self):
        loader = ContractLoader(ROOT / 'contracts')
        cases = 0
        for component, filename, fields in [
            ('CreateTaskRequest', 'tasks.json', ['join_reason', 'verification_message']),
            ('JoinParams', 'bridge.json', ['reason', 'verification_message']),
        ]:
            schema, _ = loader.resolve_component(component)
            if component == 'JoinParams':
                base = {'reason': '', 'verification_message': ''}
            else:
                records = json.loads((ROOT / 'contracts/examples' / filename).read_text())
                base = copy.deepcopy(next(x['value'] for x in records
                    if x['valid'] and x['schema_ref'].endswith('/' + component)))
            for field in fields:
                self.assertIn(field, schema['required'])
                prop = schema['properties'][field]
                self.assertEqual(prop['type'], 'string')
                self.assertFalse(prop.get('nullable', False))
                self.assertNotIn('default', prop)
                self.assertEqual(prop['maxLength'], 500)
                for label, value, valid in [('empty', '', True), ('limit', 'x' * 500, True),
                                           ('over', 'x' * 501, False), ('null', None, False),
                                           ('missing', None, False)]:
                    with self.subTest(component=component, field=field, case=label):
                        candidate = copy.deepcopy(base)
                        if label == 'missing':
                            candidate.pop(field, None)
                        else:
                            candidate[field] = value
                        self.assertEqual(not loader.component_errors(component, candidate), valid)
                        cases += 1
            if component == 'CreateTaskRequest':
                self.assertIn('approval_ref', schema['required'])
                candidate = copy.deepcopy(base)
                candidate.pop('approval_ref')
                self.assertTrue(loader.component_errors(component, candidate))
        print('B3_EMPTY_FIELD_CONTRACT_CASES', cases, 'approval member remains required')

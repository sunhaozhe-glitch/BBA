#!/usr/bin/env python3
"""Run the default Koishi package/Go gateway chain; fail closed on evidence gaps."""
from pathlib import Path
import json
import re
import shlex
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent
TEST = 'TestB3CharacterRealGateway'


def replace_once(source, before, after):
    if source.count(before) != 1:
        raise ValueError(f'kernel runner replacement must match exactly once: {before!r}')
    return source.replace(before, after, 1)


def make_runner(source, node):
    source = replace_once(source, 'ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"', 'ROOT=' + shlex.quote(str(ROOT)))
    source = replace_once(source, 'GO_COMMAND=(go test -race -count=1 ./internal/taskengine)',
                          f'GO_COMMAND=(go test -race -count=1 ./cmd/task-bridge -run ^{TEST}$ -v)')
    return replace_once(source, ' -e CONTRACT_ROOT=/work/contracts',
                        ' -e BBA_CHARACTER_INTEGRATION=1 -e BBA_CHARACTER_NODE=/fixture-node -v '
                        + shlex.quote(str(node) + ':/fixture-node:ro') + ' \\\n -e CONTRACT_ROOT=/work/contracts')


def fixture_names(output):
    names = sorted(set(re.findall(r'^\+ FIXTURE_NAME=(bba-p004-011-[0-9]+-[0-9]+)$', output, re.M)))
    if not names:
        raise ValueError('no exact nonempty fixture set found in runner output')
    return names


def exact_absence(kind, target, code, stdout, stderr):
    expected = (f'Error response from daemon: No such container: {target}' if kind == 'container'
                else f'Error response from daemon: get {target}: no such volume')
    return code == 1 and stdout.strip() == '[]' and stderr.strip() == expected


def cleanup_readback(names, run=subprocess.run):
    if not names:
        raise ValueError('refusing vacuous cleanup success')
    checks = []
    for name in names:
        for kind, target in [('container', name), ('volume', name + '-socket')]:
            q = run(['docker', kind, 'inspect', target], capture_output=True, text=True)
            checks.append({'kind': kind, 'name': target, 'inspect_exit': q.returncode,
                           'absent': exact_absence(kind, target, q.returncode, q.stdout, q.stderr),
                           'stdout': q.stdout, 'stderr': q.stderr})
    return checks


def validate_run(output, code):
    if code != 0:
        return False
    if not re.search(r'^=== RUN\s+' + TEST + r'$', output, re.M):
        return False
    if not re.search(r'^--- PASS: ' + TEST + r' \(', output, re.M):
        return False
    if re.search(r'^--- (?:SKIP|FAIL): ' + TEST + r'\b', output, re.M):
        return False
    # Require the Node assertions to finish, not just a Go test with the same name.
    evidence = []
    for line in output.splitlines():
        try:
            item = json.loads(line.strip())
        except json.JSONDecodeError:
            continue
        if isinstance(item, dict):
            evidence.append(item)
    return any(x.get('result') == 'PASS' and x.get('real_go_gateway') is True
               and x.get('writer') == 6 and x.get('local_transport') == 0
               and x.get('restart_replay') == 'no additional writer'
               and x.get('lost_receipt') == 'unknown' for x in evidence) and any(
                   str(x.get('default_package_entry', '')).endswith('/dist/index.js')
                   and x.get('default_package_entry') == x.get('exports_entry') for x in evidence)


def main():
    node_path = shutil.which('node')
    if not node_path:
        raise RuntimeError('node is missing')
    node = Path(node_path).resolve()
    print('NODE', node, subprocess.check_output([str(node), '--version'], text=True).strip(), flush=True)
    runner = make_runner((ROOT / 'tools/kernel.sh').read_text(), node)
    # Always build the public package before the integration, using the ordinary user.
    build = subprocess.run(['npm', 'run', 'build'], cwd=ROOT / 'koishi/character-runtime')
    print('BUILD_EXIT', build.returncode, flush=True)
    if build.returncode:
        return build.returncode
    p = subprocess.run(['bash', '-x', '-c', runner, 'b3-character', 'test'], cwd=ROOT,
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    print(p.stdout, flush=True)
    names = fixture_names(p.stdout)
    checks = cleanup_readback(names)
    print('CLEANUP_READBACK', json.dumps(checks, ensure_ascii=False), flush=True)
    valid = validate_run(p.stdout, p.returncode)
    print('TEST_EVIDENCE_VALID', valid, 'CHILD_EXIT', p.returncode, flush=True)
    result = 0 if valid and all(x['absent'] for x in checks) else 1
    print('EXIT', result, flush=True)
    return result


if __name__ == '__main__':
    sys.exit(main())

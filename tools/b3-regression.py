#!/usr/bin/env python3
"""Offline UI/session regression; --integration all adds own disposable B1/B2 stacks."""
from pathlib import Path
import argparse, datetime, hashlib, json, os, subprocess, sys
ROOT = Path(__file__).resolve().parents[1]

def run_step(out, label, command):
    with (out / (label + '.log')).open('w') as log:
        try:
            process = subprocess.run(command, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
            code = process.returncode
        except OSError as error:
            log.write(str(error) + '\n')
            code = 127
    result = {'label': label, 'command': command, 'exit_code': code}
    print(json.dumps(result), flush=True)
    return result

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('out', nargs='?', type=Path)
    parser.add_argument('--integration', choices=['none', 'b1', 'b2', 'all'], default='none')
    args = parser.parse_args()
    out = (args.out or ROOT / 'evals/results' / ('b3-regression-' + datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ'))).resolve()
    if not out.is_relative_to(ROOT / 'evals/results') or out == ROOT / 'evals/results':
        parser.error('evidence directory must be below this project evals/results')
    out.mkdir(parents=True, exist_ok=False)
    if os.environ.get('BBA_TASKENGINE_DSN'):
        parser.error('refuse inherited DSN; never read production credentials')
    results = []
    def run(label, command):
        result = run_step(out, label, command)
        results.append(result)
        (out / 'exits.json').write_text(json.dumps(results, indent=2))
        return result['exit_code'] == 0
    for name in ['frontend/app.js', 'tools/b3-content-browser.cjs', 'tools/session-boundary-regression.cjs', 'tools/b2-config-browser.cjs', 'tools/phase-a-browser.cjs']:
        run('syntax-' + Path(name).stem, ['node', '--check', name])
    built = run('build-product', [sys.executable, 'tools/build-product.py'])
    asset_dir = ROOT / 'backend/cmd/taskengine-server/product'
    hashes = {}
    if built:
        for name in ['index.html', 'app.css', 'app.js']:
            a = hashlib.sha256((ROOT / 'frontend' / name).read_bytes()).hexdigest()
            b = hashlib.sha256((asset_dir / name).read_bytes()).hexdigest()
            hashes[name] = {'source': a, 'embedded': b, 'equal': a == b}
        (out / 'asset-hashes.json').write_text(json.dumps(hashes, indent=2))
    for label, assets in [('source', ROOT / 'frontend'), ('embedded', asset_dir)]:
        if label == 'embedded' and not built:
            continue
        run('session-' + label, ['node', 'tools/session-boundary-regression.cjs', str(out / ('session-' + label)), str(assets / 'app.js')])
        run('b3-ui-' + label, ['node', 'tools/b3-content-browser.cjs', str(out / ('b3-ui-' + label)), str(assets)])
    run('b2-ui-self', ['node', 'tools/b2-config-browser.cjs', '--self-test'])
    for stage in ['b1', 'b2']:
        if args.integration in [stage, 'all']:
            run(stage + '-integration', [sys.executable, 'tools/' + stage + '-integration.py', str(out / (stage + '-integration'))])
    failed = [r['label'] for r in results if r['exit_code']]
    matched = built and bool(hashes) and all(h['equal'] for h in hashes.values())
    summary = {'passed': not failed and matched, 'integration_requested': args.integration, 'failed_checks': failed, 'asset_hashes_equal': matched, 'fixture_only': True, 'backend_b3_acceptance': False, 'astra_acceptance': False, 'checks': len(results), 'evidence': str(out)}
    (out / 'summary.json').write_text(json.dumps(summary, indent=2))
    print(json.dumps(summary), flush=True)
    return 0 if summary['passed'] else 1

if __name__ == '__main__':
    sys.exit(main())

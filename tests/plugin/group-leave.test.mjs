// Behavioral tests for NapCat's SetGroupLeave action handler.
//
// The handler is the real implementation extracted from the frozen NapCat
// bundle, not a reimplementation. `fixtures/group-leave.original.mjs` is the
// exact installed extract and `fixtures/group-leave.fixed.mjs` is the same
// extract after `patches/napcat-set-group-leave.patch`.
//
// Default: run the fixed candidate.
//   node --test tests/plugin/group-leave.test.mjs
// Red demonstration against the installed handler:
//   BBA_GROUP_LEAVE_VARIANT=original node --test tests/plugin/group-leave.test.mjs
// Apply the patch in memory to the frozen bundle and test the result:
//   BBA_GROUP_LEAVE_VARIANT=patched node --test tests/plugin/group-leave.test.mjs
// Extract and run the still-installed handler directly:
//   BBA_GROUP_LEAVE_VARIANT=installed node --test tests/plugin/group-leave.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, '..', '..');
const FIXTURES = path.join(HERE, 'fixtures');
const PATCH_FILE = path.join(ROOT, 'patches', 'napcat-set-group-leave.patch');
const INSTALLED_FILE = process.env.BBA_NAPCAT_MJS
  ?? '/run/media/sun/6F53FE6DFAA8CECF/QQ群项目/QQ/Napcat/opt/QQ/resources/app/app_launcher/napcat/napcat.mjs';
const VARIANT = process.env.BBA_GROUP_LEAVE_VARIANT ?? 'candidate';
const INSTALLED_SHA256 = 'ee961eae58c88ec08952f81cb06fb9dc3b129704f50f21f5cfc7ea509f32bf34';

const GROUP_ID = 987654321;

const Handler = loadHandler(await handlerSource(VARIANT));

function fail(message) {
  throw new Error(`group-leave test setup: ${message}`);
}

async function handlerSource(variant) {
  switch (variant) {
    case 'candidate':
    case 'fixed':
      return readFile(path.join(FIXTURES, 'group-leave.fixed.mjs'), 'utf8');
    case 'original':
      return readFile(path.join(FIXTURES, 'group-leave.original.mjs'), 'utf8');
    case 'installed':
      return extractHandler(await readInstalled());
    case 'patched':
      return extractHandler(applyPatch(await readInstalled(), await readFile(PATCH_FILE, 'utf8')));
    default:
      fail(`unknown BBA_GROUP_LEAVE_VARIANT=${variant}`);
  }
}

async function readInstalled() {
  if (!existsSync(INSTALLED_FILE)) fail(`installed napcat.mjs not found at ${INSTALLED_FILE}`);
  const bytes = await readFile(INSTALLED_FILE);
  const actual = createHash('sha256').update(bytes).digest('hex');
  if (actual !== INSTALLED_SHA256) {
    fail(`installed napcat.mjs sha256 ${actual} does not match patch base ${INSTALLED_SHA256}`);
  }
  return bytes.toString('utf8');
}

// The handler fragment starts at the SetGroupLeave payload schema and ends
// before the next action schema. This is test setup, not an assertion about
// the implementation text.
function extractHandler(bundle) {
  const start = bundle.indexOf('const m1e = p.Object({');
  if (start < 0) fail('SetGroupLeave schema marker not found in NapCat bundle');
  const end = bundle.indexOf('\nconst v1e = p.Object({', start);
  if (end < 0) fail('SetGroupLeave class end marker not found in NapCat bundle');
  return `${bundle.slice(start, end)}\n`;
}

// Minimal single-hunk unified-diff application for repositories without git
// or GNU patch. It locates the hunk by its old-side content and replaces it.
function applyPatch(source, patch) {
  const src = source.split('\n');
  const lines = patch.split('\n');
  let hunks = 0;

  for (let i = 0; i < lines.length; i += 1) {
    const header = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/.exec(lines[i]);
    if (!header) continue;

    hunks += 1;
    const oldStart = Number(header[1]);
    const oldCount = header[2] === undefined ? 1 : Number(header[2]);
    const oldLines = [];
    const newLines = [];

    for (i += 1; i < lines.length && !lines[i].startsWith('@@ '); i += 1) {
      const line = lines[i];
      if (line === '') continue;
      const tag = line[0];
      const body = line.slice(1);
      if (tag === ' ') {
        oldLines.push(body);
        newLines.push(body);
      } else if (tag === '-') {
        oldLines.push(body);
      } else if (tag === '+') {
        newLines.push(body);
      } else if (tag === '\\') {
        // "\ No newline at end of file" carries no content.
      } else {
        fail(`unexpected patch line ${JSON.stringify(line)}`);
      }
    }
    // The outer loop increment would otherwise skip the next hunk header.
    i -= 1;

    if (oldLines.length !== oldCount) {
      fail(`hunk expects ${oldCount} old lines, parsed ${oldLines.length}`);
    }

    let at = oldStart - 1;
    if (!matchesAt(src, at, oldLines)) at = findLines(src, oldLines, Math.max(0, oldStart - 1));
    if (at < 0) at = findLines(src, oldLines, 0);
    if (at < 0) fail('patch hunk context not found in NapCat bundle');

    src.splice(at, oldLines.length, ...newLines);
  }

  if (hunks !== 1) fail(`expected one patch hunk, found ${hunks}`);
  return src.join('\n');
}

function matchesAt(lines, at, expected) {
  if (at < 0 || at + expected.length > lines.length) return false;
  return expected.every((line, offset) => lines[at + offset] === line);
}

function findLines(lines, expected, from) {
  for (let at = from; at + expected.length <= lines.length; at += 1) {
    if (matchesAt(lines, at, expected)) return at;
  }
  return -1;
}

function loadHandler(fragment) {
  const p = {
    Object: (shape) => ({ kind: 'object', shape }),
    String: (options) => ({ kind: 'string', options }),
    Boolean: () => ({ kind: 'boolean' }),
    Optional: (schema) => ({ kind: 'optional', schema }),
    Union: (schemas, options) => ({ kind: 'union', schemas, options }),
    Null: (options) => ({ kind: 'null', options }),
  };
  const re = { SetGroupLeave: 'set_group_leave' };
  const it = {
    SetGroupLeave: {
      payload: { group_id: '123456', is_dismiss: false },
      response: null,
    },
  };
  class ue {
    constructor(core, obContext) {
      this.core = core;
      this.obContext = obContext;
    }
  }

  const context = vm.createContext({ p, re, it, ue });
  vm.runInContext(
    `${fragment}\nglobalThis.__GroupLeaveAction = y1e;`,
    context,
    { filename: 'group-leave-handler.mjs' },
  );
  return context.__GroupLeaveAction;
}

function harness(HandlerClass = Handler) {
  const calls = [];
  const core = {
    apis: {
      GroupApi: {
        async quitGroup(groupId) {
          calls.push(groupId);
          return { ok: true };
        },
      },
    },
  };
  return { handler: new HandlerClass(core), calls };
}

async function rejectionOf(promise) {
  try {
    await promise;
    return undefined;
  } catch (error) {
    return error;
  }
}

test('no is_dismiss calls quitGroup once with the exact group id', async () => {
  const { handler, calls } = harness();
  assert.equal(await handler._handle({ group_id: GROUP_ID }), null);
  assert.deepEqual(calls, [String(GROUP_ID)]);
});

for (const value of [false, 'false']) {
  test(`is_dismiss=${JSON.stringify(value)} calls quitGroup once`, async () => {
    const { handler, calls } = harness();
    assert.equal(await handler._handle({ group_id: GROUP_ID, is_dismiss: value }), null);
    assert.deepEqual(calls, [String(GROUP_ID)]);
  });
}

for (const value of [true, 'true']) {
  test(`is_dismiss=${JSON.stringify(value)} is rejected before quitGroup`, async () => {
    const { handler, calls } = harness();
    const error = await rejectionOf(handler._handle({ group_id: GROUP_ID, is_dismiss: value }));
    assert.deepEqual(calls, [], 'is_dismiss=true must be rejected before quitGroup');
    // The handler runs in a VM realm, so the Error object does not inherit
    // from this realm's Error prototype; check its observable shape.
    assert.equal(Object.prototype.toString.call(error), '[object Error]');
    assert.match(error.message, /is_dismiss/);
    assert.match(error.message, /not supported/);
  });
}

for (const value of ['TRUE', 'False', '', '1', '0', 'yes', ' true', 'false ', 0, 1, null, {}, []]) {
  test(`malformed is_dismiss=${JSON.stringify(value)} is rejected before quitGroup`, async () => {
    const { handler, calls } = harness();
    const error = await rejectionOf(handler._handle({ group_id: GROUP_ID, is_dismiss: value }));
    assert.deepEqual(calls, [], 'malformed is_dismiss must be rejected before quitGroup');
    assert.equal(Object.prototype.toString.call(error), '[object Error]');
    assert.match(error.message, /is_dismiss/);
  });
}

test(
  'the patch applies to the installed bundle and honors is_dismiss',
  { skip: !existsSync(INSTALLED_FILE) && 'installed NapCat bundle not present' },
  async () => {
    const PatchedHandler = loadHandler(
      extractHandler(applyPatch(await readInstalled(), await readFile(PATCH_FILE, 'utf8'))),
    );
    const { handler, calls } = harness(PatchedHandler);

    const unsupported = await rejectionOf(
      handler._handle({ group_id: GROUP_ID, is_dismiss: true }),
    );
    assert.deepEqual(calls, [], 'patched bundle must reject is_dismiss=true before quitGroup');
    assert.equal(Object.prototype.toString.call(unsupported), '[object Error]');

    assert.equal(await handler._handle({ group_id: GROUP_ID, is_dismiss: false }), null);
    assert.deepEqual(calls, [String(GROUP_ID)], 'patched bundle must quit once for is_dismiss=false');
  },
);

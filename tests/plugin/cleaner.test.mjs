import test from 'node:test';
import assert from 'node:assert/strict';
import { resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const candidatePath = fileURLToPath(new URL('../../plugins/napcat-plugin-cleaner/index.mjs', import.meta.url));
const pluginPath = process.env.PLUGIN_UNDER_TEST ? resolve(process.env.PLUGIN_UNDER_TEST) : candidatePath;
const plugin = await import(pathToFileURL(pluginPath).href);

const REASON = '我是ai机器人';
const ANSWER = '我是小肥猪';
const GROUP_ID = 829117089;

function createHarness(options = {}) {
  const routes = new Map();
  const logs = { log: [], info: [], error: [] };
  const session = {};

  const groupService = {};
  if (options.joinGroup) groupService.joinGroup = options.joinGroup;
  if (options.reqToJoinGroup) groupService.reqToJoinGroup = options.reqToJoinGroup;
  if (options.joinGroup || options.reqToJoinGroup) session.getGroupService = () => groupService;
  if (options.searchService) session.getSearchService = () => options.searchService;

  const ctx = {
    router: {
      post(path, handler) {
        routes.set(path, handler);
      },
    },
    core: {
      eventWrapper: options.eventWrapper,
      context: { session },
    },
    logger: {
      log: (...args) => logs.log.push(args.map((v) => String(v)).join(' ')),
      info: (...args) => logs.info.push(args.map((v) => String(v)).join(' ')),
      error: (...args) => logs.error.push(args.map((v) => String(v)).join(' ')),
    },
  };

  return { ctx, routes, logs };
}

function recordingNative(method, result, expectedArgCount = 1) {
  const calls = [];
  const stub = (...args) => {
    assert.equal(args.length, expectedArgCount, `${method} native call must receive ${expectedArgCount} arguments`);
    const arg = expectedArgCount === 1 ? args[0] : args[1];
    const buffer = Buffer.from(JSON.stringify(arg), 'utf8');
    calls.push({ args, arg, buffer });
    return typeof result === 'function' ? result(arg, buffer) : result;
  };
  return { calls, stub };
}

async function post(harness, route, body) {
  const handler = harness.routes.get(route);
  assert.equal(typeof handler, 'function', `${route} handler is registered`);
  let responseBody;
  const res = {
    json(value) {
      responseBody = value;
      return this;
    },
  };
  const actualBody = body && Object.hasOwn(body, 'answer') ? body : { ...body, answer: ANSWER };
  await handler({ body: actualBody }, res);
  assert.notEqual(responseBody, undefined, `${route} returned a JSON body`);
  return responseBody;
}

async function init(options) {
  const harness = createHarness(options);
  await plugin.plugin_init(harness.ctx);
  return harness;
}

test('plugin_init registers only the existing search and join routes', async () => {
  const harness = await init();
  assert.deepEqual([...harness.routes.keys()].sort(), ['/join-group', '/search-group']);
});

test('search-group route keeps the existing eventWrapper search path', async () => {
  const searches = [];
  const harness = await init({
    eventWrapper: {
      async callNormalEventV2(method, listener, params, predicate) {
        searches.push({ method, listener, params, predicate });
        return [null, {
          groupInfos: [
            { groupCode: '123456', searchGroupInfo: { groupName: '群一', memberNum: 5 } },
            { groupCode: '654321', groupName: '群二', memberNum: 7 },
            { groupCode: '0', groupName: '非法群号', memberNum: 9 },
          ],
        }];
      },
    },
  });

  const body = await post(harness, '/search-group', { keyword: ' 教学 ', count: 2 });
  assert.deepEqual(body, {
    ok: true,
    groups: [
      { group_id: 123456, group_name: '群一', member_count: 5 },
      { group_id: 654321, group_name: '群二', member_count: 7 },
    ],
  });
  assert.equal(searches.length, 1);
  assert.equal(searches[0].method, 'NodeIKernelSearchService/searchGroup');
  assert.deepEqual(searches[0].params, [{ keyWords: '教学', groupNum: 2, exactSearch: false, penetrate: '' }]);
});

test('joinGroup receives separate reason and answer fields as exact UTF-8 bytes', async () => {
  const { calls, stub } = recordingNative('joinGroup', { result: 1, request_id: 'join-ref-1' });
  const harness = await init({ joinGroup: stub });

  const body = await post(harness, '/join-group', { group_id: GROUP_ID, confirm: '1', reason: REASON, answer: ANSWER });

  assert.equal(calls.length, 1);
  const call = calls[0];
  assert.equal(call.args.length, 1);
  assert.equal(call.arg.groupCode, String(GROUP_ID));
  assert.equal(call.arg.richMsg, REASON);
  assert.equal(call.arg.applyMsg, ANSWER);
  assert.equal(Object.hasOwn(call.arg, 'groupAnswer'), false);
  assert.equal(Object.hasOwn(call.arg, 'reason'), false);

  assert.ok(Buffer.isBuffer(call.buffer), 'native capture is a serialized argument Buffer');
  assert.equal(call.buffer.toString('utf8'), JSON.stringify(call.arg));
  for (const [field, value] of [['richMsg', REASON], ['applyMsg', ANSWER]]) {
    const expected = Buffer.from(`"${field}":"${value}"`, 'utf8');
    assert.ok(call.buffer.includes(expected), `${field} keeps the exact UTF-8 application bytes`);
  }
  assert.equal(harness.logs.info.some((line) => line.includes(REASON) || line.includes(ANSWER)), false);

  assert.deepEqual(body, {
    ok: true,
    result: 1,
    raw: { result: 1, request_id: 'join-ref-1' },
    request_ref: 'join-ref-1',
  });
});

test('reqToJoinGroup receives the same single ReqToGroup payload', async () => {
  const { calls, stub } = recordingNative('reqToJoinGroup', { result: 1, request_ref: 'join-ref-2' });
  const harness = await init({ reqToJoinGroup: stub });

  const body = await post(harness, '/join-group', { group_id: GROUP_ID, confirm: '1', reason: REASON, answer: ANSWER });

  assert.equal(calls.length, 1);
  const call = calls[0];
  assert.equal(call.args.length, 1);
  assert.equal(call.arg.groupCode, String(GROUP_ID));
  assert.equal(call.arg.richMsg, REASON);
  assert.equal(call.arg.applyMsg, ANSWER);
  assert.ok(call.buffer.includes(Buffer.from(`"richMsg":"${REASON}"`, 'utf8')));
  assert.ok(call.buffer.includes(Buffer.from(`"applyMsg":"${ANSWER}"`, 'utf8')));
  assert.deepEqual(body, {
    ok: true,
    result: 1,
    raw: { result: 1, request_ref: 'join-ref-2' },
    request_ref: 'join-ref-2',
  });
});

test('missing answer stops before the native call', async () => {
  const { calls, stub } = recordingNative('joinGroup', { result: 1 });
  const harness = await init({ joinGroup: stub });

  const body = await post(harness, '/join-group', { group_id: GROUP_ID, confirm: '1', reason: REASON, answer: '' });

  assert.deepEqual(body, { ok: false, error: '缺少 answer', code: 'ANSWER_REQUIRED' });
  assert.equal(calls.length, 0);
});

test('numeric result codes require the top-level response field', async (t) => {
  const cases = [
    {
      name: 'top-level numeric result',
      ret: { result: 1, request_id: 'ref-top' },
      expectedResult: 1,
      expectedRef: 'ref-top',
    },
    {
      name: 'nested data result is unknown',
      ret: { data: { result: 1, request_ref: 'ref-nested' } },
      expectedResult: null,
      expectedRef: 'ref-nested',
    },
    {
      name: 'nested result object is unknown',
      ret: { result: { result: 2, req_id: 'ref-deep' } },
      expectedResult: null,
      expectedRef: 'ref-deep',
    },
    {
      name: 'string serialized result',
      ret: '{"result":1}',
      expectedResult: 1,
      expectedRef: null,
    },
  ];

  for (const item of cases) {
    await t.test(item.name, async () => {
      const { stub } = recordingNative('joinGroup', item.ret);
      const harness = await init({ joinGroup: stub });
      const body = await post(harness, '/join-group', { group_id: GROUP_ID, confirm: '1', reason: REASON });

      assert.equal(body.ok, item.expectedResult !== null);
      if (item.expectedResult === null) assert.equal(body.code, 'UNKNOWN_RESULT');
      else assert.equal(body.result, item.expectedResult);
      assert.deepEqual(body.raw, item.ret);
      if (item.expectedRef) assert.equal(body.request_ref, item.expectedRef);
      else assert.equal(Object.hasOwn(body, 'request_ref'), false);
      assert.equal(Object.hasOwn(body, 'submitted'), false);
    });
  }
});

test('absent/null/NaN/unparseable native results fail closed as UNKNOWN_RESULT', async (t) => {
  const cases = [
    { name: 'void return', ret: undefined, expectedRaw: null, expectedRef: null },
    { name: 'null return', ret: null, expectedRaw: null, expectedRef: null },
    { name: 'NaN return', ret: Number.NaN, expectedRaw: null, expectedRef: null },
    { name: 'unparseable string', ret: 'native did not report a result', expectedRaw: 'native did not report a result', expectedRef: null },
    { name: 'null result field', ret: { result: null, request_id: 'ref-unknown' }, expectedRaw: { result: null, request_id: 'ref-unknown' }, expectedRef: 'ref-unknown' },
    { name: 'NaN nested result', ret: { data: { result: Number.NaN } }, expectedRaw: { data: { result: null } }, expectedRef: null },
    { name: 'non-numeric result field', ret: { result: '1' }, expectedRaw: { result: '1' }, expectedRef: null },
  ];

  for (const item of cases) {
    await t.test(item.name, async () => {
      const { stub } = recordingNative('joinGroup', item.ret);
      const harness = await init({ joinGroup: stub });
      const body = await post(harness, '/join-group', { group_id: GROUP_ID, confirm: '1', reason: REASON });

      assert.equal(body.ok, false);
      assert.equal(body.code, 'UNKNOWN_RESULT');
      assert.deepEqual(body.raw, item.expectedRaw);
      assert.equal(Object.hasOwn(body, 'result'), false, 'unknown result is never defaulted to 1');
      assert.equal(Object.hasOwn(body, 'submitted'), false, 'unknown result is never reported as submitted');
      if (item.expectedRef) assert.equal(body.request_ref, item.expectedRef);
    });
  }
});

test('native exceptions preserve original diagnostics and request_ref', async () => {
  const nativeError = Object.assign(new Error('native join rejected: authorization failed'), {
    code: 'E_NATIVE_JOIN',
    result: 2,
    request_id: 'err-ref-2',
  });
  let stackReads = 0;
  Object.defineProperty(nativeError, 'stack', {
    configurable: true,
    get() { stackReads += 1; return 'stack must not be read'; },
  });
  const { stub } = recordingNative('joinGroup', () => { throw nativeError; });
  const harness = await init({ joinGroup: stub });

  const body = await post(harness, '/join-group', { group_id: GROUP_ID, confirm: '1', reason: REASON });

  assert.equal(body.ok, false);
  assert.equal(body.code, 'QQ_RESULT_2');
  assert.equal(body.result, 2);
  assert.equal(body.request_ref, 'err-ref-2');
  assert.equal(body.raw.name, 'Error');
  assert.equal(body.raw.message, 'native join rejected: authorization failed');
  assert.equal(body.raw.code, 'E_NATIVE_JOIN');
  assert.equal(body.raw.result, 2);
  assert.equal(body.raw.request_id, 'err-ref-2');
  assert.equal(Object.hasOwn(body.raw, 'stack'), false, 'accessor stack is never executed for diagnostics');
  assert.equal(stackReads, 0, 'accessor stack getter must not run');
  assert.match(body.error, /native join rejected: authorization failed/);
});

test('join input validation stays before any native call', async () => {
  const { calls, stub } = recordingNative('joinGroup', { result: 1 });
  const harness = await init({ joinGroup: stub });

  const missingConfirm = await post(harness, '/join-group', { group_id: GROUP_ID, reason: REASON });
  assert.deepEqual(missingConfirm, { ok: false, error: '需显式 confirm=1 才提交加群申请', code: 'CONFIRM_REQUIRED' });

  const badGroup = await post(harness, '/join-group', { group_id: 0, confirm: '1', reason: REASON });
  assert.deepEqual(badGroup, { ok: false, error: '缺少或非法 group_id', code: 'BAD_REQUEST' });

  assert.equal(calls.length, 0);
});

// ── R12 行为回归：结果码解析、raw/日志/request_ref 边界、实际响应合约 ──────────
// 说明：原先只断言 plugin_cleanup 是函数的 export 测试没有行为价值，已删除（1 条）；
// 清理钩子的实际行为由各端点测试的 init/cleanup 生命周期覆盖。

const RAW_BYTE_LIMIT = 4096;
const RAW_PROP_LIMIT = 64;
const RESULT_CODE_PATTERN = /^[A-Z][A-Z0-9_]{2,63}$/;

function assertBoundedRaw(raw, label = 'raw') {
  if (raw === undefined || raw === null) return;
  if (typeof raw === 'string') {
    assert.ok(Buffer.byteLength(JSON.stringify(raw), 'utf8') <= RAW_BYTE_LIMIT, `${label}: string exceeds 4096 UTF-8 bytes`);
    return;
  }
  if (typeof raw === 'number') {
    assert.ok(Number.isSafeInteger(raw) && raw >= 0, `${label}: number must be a finite non-negative safe integer`);
    return;
  }
  assert.equal(typeof raw, 'object', `${label}: unsupported type ${typeof raw}`);
  assert.ok(!Array.isArray(raw), `${label}: arrays must degrade to a bounded string`);
  const proto = Object.getPrototypeOf(raw);
  assert.ok(proto === Object.prototype || proto === null, `${label}: must be a plain object`);
  assert.ok(Object.keys(raw).length <= RAW_PROP_LIMIT, `${label}: more than 64 properties`);
  assert.ok(Buffer.byteLength(JSON.stringify(raw), 'utf8') <= RAW_BYTE_LIMIT, `${label}: serialized form exceeds 4096 UTF-8 bytes`);
}

function assertJoinResponseShape(body) {
  assert.ok(body !== null && typeof body === 'object', 'join response is an object');
  assert.equal(typeof body.ok, 'boolean', 'ok is a boolean');
  assertBoundedRaw(body.raw);
  if (body.ok) {
    assert.ok(Number.isSafeInteger(body.result) && body.result >= 0, 'ok response carries a non-negative safe integer result');
  } else {
    assert.equal(typeof body.error, 'string', 'failure response carries error text');
    assert.ok([...body.error].length >= 1 && [...body.error].length <= 2000, 'error text is non-empty and bounded');
    assert.match(body.code, RESULT_CODE_PATTERN, 'failure code matches the wire code pattern');
    if (Object.hasOwn(body, 'result')) {
      assert.ok(Number.isSafeInteger(body.result) && body.result >= 0, 'failure result, when present, is a non-negative safe integer');
    }
  }
  if (Object.hasOwn(body, 'request_ref')) {
    assert.equal(typeof body.request_ref, 'string', 'request_ref is a string');
    const chars = [...body.request_ref].length;
    assert.ok(chars >= 1 && chars <= 256, `request_ref is 1..256 Unicode characters (got ${chars})`);
  }
  assert.equal(Object.hasOwn(body, 'submitted'), false, 'plugin never claims submitted');
}

async function joinBody(ret, body = { group_id: GROUP_ID, confirm: '1', reason: REASON }) {
  const { stub } = recordingNative('joinGroup', ret);
  const harness = await init({ joinGroup: stub });
  const out = await post(harness, '/join-group', body);
  return { harness, body: out };
}

test('serialized native results accept only complete JSON with a non-negative safe integer', async (t) => {
  const cases = [
    { name: 'truncated JSON', ret: '{"result":1oops}', expected: null },
    { name: 'infinite exponent', ret: '{"result":1e309}', expected: null },
    { name: 'decimal result', ret: '{"result":1.9}', expected: null },
    { name: 'negative result', ret: '{"result":-1}', expected: null },
    { name: 'unsafe integer', ret: '{"result":9007199254740993}', expected: null },
    { name: 'trailing text after JSON', ret: '{"result":1} trailing', expected: null },
    { name: 'plain-text result', ret: 'result=1', expected: null },
    { name: 'complete JSON with valid code', ret: '{"result":2}', expected: 2 },
    { name: 'complete JSON number', ret: '1', expected: 1 },
  ];

  for (const item of cases) {
    await t.test(item.name, async () => {
      const { body } = await joinBody(item.ret);
      assertJoinResponseShape(body);
      if (item.expected === null) {
        assert.equal(body.ok, false);
        assert.equal(body.code, 'UNKNOWN_RESULT');
        assert.equal(Object.hasOwn(body, 'result'), false, 'no result code is borrowed from an unparseable value');
      } else {
        assert.equal(body.ok, true);
        assert.equal(body.result, item.expected);
      }
    });
  }
});

test('object result fields reject decimal, negative, unsafe, non-finite and non-numeric values', async (t) => {
  const invalid = [
    ['decimal', { result: 1.9 }],
    ['negative', { result: -1 }],
    ['unsafe integer', { result: 2 ** 53 }],
    ['infinite', { result: Number.POSITIVE_INFINITY }],
    ['NaN', { result: Number.NaN }],
    ['numeric string', { result: '1' }],
    ['boolean', { result: true }],
  ];
  const valid = [
    ['zero', { result: 0 }, 0],
    ['max safe integer', { result: Number.MAX_SAFE_INTEGER }, Number.MAX_SAFE_INTEGER],
  ];

  for (const [name, ret] of invalid) {
    await t.test(`rejects ${name}`, async () => {
      const { body } = await joinBody(ret);
      assertJoinResponseShape(body);
      assert.equal(body.ok, false);
      assert.equal(body.code, 'UNKNOWN_RESULT');
      assert.equal(Object.hasOwn(body, 'result'), false);
    });
  }

  for (const [name, ret, expected] of valid) {
    await t.test(`accepts ${name}`, async () => {
      const { body } = await joinBody(ret);
      assertJoinResponseShape(body);
      assert.equal(body.ok, true);
      assert.equal(body.result, expected);
    });
  }
});

test('an explicit invalid result field is never replaced by nested values or message echoes', async (t) => {
  const cases = [
    ['null result with nested data', { result: null, data: { result: 2 } }],
    ['decimal result with nested data', { result: 1.9, data: { result: 2 } }],
    ['string result with nested data', { result: '1', data: { result: 2 } }],
    ['null result with message echo', { result: null, message: 'diagnostic example: "result":1' }],
    ['absent result with message echo', { message: 'diagnostic example: "result":1' }],
    ['plain text echo', 'join ok, result=1'],
  ];

  for (const [name, ret] of cases) {
    await t.test(name, async () => {
      const { body } = await joinBody(ret);
      assertJoinResponseShape(body);
      assert.equal(body.ok, false);
      assert.equal(body.code, 'UNKNOWN_RESULT');
      assert.equal(Object.hasOwn(body, 'result'), false);
    });
  }
});

test('exception diagnostics never lend a result code from message text', async () => {
  const nativeError = Object.assign(new Error('example: "result":1'), { code: 'E_NATIVE_JOIN' });
  const { stub } = recordingNative('joinGroup', () => { throw nativeError; });
  const harness = await init({ joinGroup: stub });
  const body = await post(harness, '/join-group', { group_id: GROUP_ID, confirm: '1', reason: REASON });

  assertJoinResponseShape(body);
  assert.equal(body.ok, false);
  assert.equal(body.code, 'E_NATIVE_JOIN');
  assert.equal(Object.hasOwn(body, 'result'), false, 'a quoted code in the message is not a result');
  assert.equal(body.raw.message, 'example: "result":1');
});

test('exception codes and error text stay inside contract bounds', async () => {
  const nativeError = Object.assign(new Error('boom '.repeat(20000)), { code: 'not a code!', result: -3 });
  const { stub } = recordingNative('joinGroup', () => { throw nativeError; });
  const harness = await init({ joinGroup: stub });
  const body = await post(harness, '/join-group', { group_id: GROUP_ID, confirm: '1', reason: REASON });

  assertJoinResponseShape(body);
  assert.equal(body.ok, false);
  assert.match(body.code, RESULT_CODE_PATTERN);
  assert.equal(Object.hasOwn(body, 'result'), false, 'negative native result is never reported');
  assert.ok(body.error.length > 0 && [...body.error].length <= 2000, 'error text is bounded');
  assertBoundedRaw(body.raw);
});

test('raw diagnostics stay within the 4096-byte UTF-8 budget', async () => {
  const wide = '汉'.repeat(100000);
  const first = await joinBody({ result: 1, payload: wide });
  assertJoinResponseShape(first.body);
  assert.equal(first.body.ok, true);
  assert.ok(Buffer.byteLength(JSON.stringify(first.body)) <= 8192, 'response stays bounded');
  assert.ok(!JSON.stringify(first.body.raw).includes(wide.slice(0, 64)), 'oversized payload is omitted, not truncated into the echo');

  const escapeHeavy = '"\\\n'.repeat(5000);
  const second = await joinBody({ result: 1, payload: escapeHeavy });
  assertJoinResponseShape(second.body);
  assert.ok(Buffer.byteLength(JSON.stringify(second.body)) <= 8192, 'escaping counts against the budget');
  assert.ok(!JSON.stringify(second.body.raw).includes(escapeHeavy.slice(0, 64)));
});

test('raw objects above 64 properties degrade instead of being echoed wholesale', async () => {
  const ret = { result: 1 };
  for (let i = 0; i < 65; i += 1) ret[`k${i}`] = i;
  assert.equal(Object.keys(ret).length, 66);

  const { body } = await joinBody(ret);
  assertJoinResponseShape(body);
  assert.equal(body.ok, true);
  assert.equal(body.result, 1);
});

test('deep and cyclic native returns stay bounded without running coercion hooks', async () => {
  let toStringCalls = 0;
  const withSpy = (value) => {
    Object.defineProperty(value, 'toString', {
      enumerable: true,
      configurable: true,
      value() { toStringCalls += 1; return '[coerced]'; },
    });
    return value;
  };

  let deep = withSpy({ leaf: true });
  for (let i = 0; i < 20000; i += 1) deep = withSpy({ next: deep });
  const deepRoot = withSpy({ result: 1 });
  deepRoot.payload = deep;

  const deepBody = (await joinBody(deepRoot)).body;
  assertJoinResponseShape(deepBody);
  assert.equal(deepBody.ok, true);
  assert.equal(deepBody.result, 1);
  assert.equal(toStringCalls, 0, 'serialization never coerces objects through toString');

  const cyclic = withSpy({ result: 1 });
  cyclic.self = cyclic;
  const cyclicBody = (await joinBody(cyclic)).body;
  assertJoinResponseShape(cyclicBody);
  assert.equal(cyclicBody.ok, true);
  assert.equal(cyclicBody.result, 1);
  assert.equal(toStringCalls, 0, 'cycle detection never coerces objects through toString');
});

test('raw serialization never invokes toJSON or property getters', async () => {
  let toJsonCalls = 0;
  let getterCalls = 0;

  const withToJson = {
    result: 1,
    toJSON() {
      toJsonCalls += 1;
      return { result: 1, leaked: 'x'.repeat(10000) };
    },
  };
  Object.defineProperty(withToJson, 'secret', {
    enumerable: true,
    get() { getterCalls += 1; throw new Error('getter must not run'); },
  });

  const first = (await joinBody(withToJson)).body;
  assertJoinResponseShape(first);
  assert.equal(first.ok, true);
  assert.equal(first.result, 1);
  assert.equal(toJsonCalls, 0, 'toJSON must not be called');
  assert.equal(getterCalls, 0, 'getters must not be called');
  assert.ok(!JSON.stringify(first.raw).includes('leaked'));

  const withGetter = { result: 1 };
  Object.defineProperty(withGetter, 'secret', {
    enumerable: true,
    get() { getterCalls += 1; throw new Error('getter must not run'); },
  });
  const second = (await joinBody(withGetter)).body;
  assertJoinResponseShape(second);
  assert.equal(second.ok, true);
  assert.equal(getterCalls, 0);

  let resultGetterCalls = 0;
  const accessorResult = {};
  Object.defineProperty(accessorResult, 'result', {
    enumerable: true,
    get() { resultGetterCalls += 1; return 1; },
  });
  const third = (await joinBody(accessorResult)).body;
  assertJoinResponseShape(third);
  assert.equal(third.ok, false);
  assert.equal(third.code, 'UNKNOWN_RESULT');
  assert.equal(resultGetterCalls, 0, 'an accessor result is never executed');
});

test('request_ref is capped at 256 Unicode code points and omitted when longer', async (t) => {
  const cases = [
    ['exactly 256 BMP characters', 'a'.repeat(256), true],
    ['exactly 256 astral characters', '😀'.repeat(256), true],
    ['257 BMP characters', 'a'.repeat(257), false],
    ['257 astral characters', '😀'.repeat(257), false],
    ['100000 characters', 'r'.repeat(100000), false],
  ];

  for (const [name, requestId, kept] of cases) {
    await t.test(name, async () => {
      const { body } = await joinBody({ result: 1, request_id: requestId });
      assertJoinResponseShape(body);
      if (kept) assert.equal(body.request_ref, requestId);
      else assert.equal(Object.hasOwn(body, 'request_ref'), false, 'over-long request_ref is omitted, not truncated');
      assert.ok(Buffer.byteLength(JSON.stringify(body)) <= 8192, 'response stays bounded');
    });
  }
});

test('logs stay bounded and never echo the application reason or native payloads', async () => {
  const secret = '我是ai机器人-SECRET-REASON';
  const payload = 'ANSWER-SECRET-'.repeat(4000);
  const success = await joinBody({ result: 1, payload }, { group_id: GROUP_ID, confirm: '1', reason: secret });
  const successLines = [...success.harness.logs.log, ...success.harness.logs.info, ...success.harness.logs.error];
  const successText = successLines.join('\n');
  assert.ok(successLines.length > 0);
  assert.ok(!successText.includes(secret), 'logs must not contain the application reason');
  assert.ok(!successText.includes('ANSWER-SECRET-'), 'logs must not contain the native payload');
  for (const line of successLines) assert.ok(line.length <= 512, `log line too long: ${line.length}`);

  const nativeError = Object.assign(new Error(`rejected ${secret} ${payload}`), { code: 'E_NATIVE_JOIN' });
  const { stub } = recordingNative('joinGroup', () => { throw nativeError; });
  const failing = await init({ joinGroup: stub });
  await post(failing, '/join-group', { group_id: GROUP_ID, confirm: '1', reason: secret });
  const failureLines = [...failing.logs.log, ...failing.logs.info, ...failing.logs.error];
  const failureText = failureLines.join('\n');
  assert.ok(!failureText.includes(secret), 'failure logs must not contain the application reason');
  assert.ok(!failureText.includes('ANSWER-SECRET-'), 'failure logs must not contain the native payload');
  for (const line of failureLines) assert.ok(line.length <= 512, `failure log line too long: ${line.length}`);
});

test('join responses satisfy the shared wire contract in every branch', async (t) => {
  const manyProps = { result: 1 };
  for (let i = 0; i < 65; i += 1) manyProps[`k${i}`] = i;

  const cases = [
    ['success object', { result: 1, request_id: 'ref' }],
    ['success serialized string', '{"result":2}'],
    ['numeric success', 3],
    ['unknown void', undefined],
    ['unknown null', null],
    ['oversized payload', { result: 1, payload: '汉'.repeat(100000) }],
    ['too many properties', manyProps],
    ['long request ref', { result: 1, request_id: 'r'.repeat(1000) }],
    ['negative result', { result: -1 }],
    ['message echo', { result: null, message: 'example: "result":1' }],
  ];

  for (const [name, ret] of cases) {
    await t.test(name, async () => {
      const { body } = await joinBody(ret);
      assertJoinResponseShape(body);
    });
  }
});

test('R13 result chains share the raw depth guard and never borrow a deep result code', async () => {
  let ret = { result: 1 };
  for (let i = 0; i < 100; i += 1) ret = { result: ret };

  const { body } = await joinBody(ret);
  assert.equal(body.ok, false);
  assert.equal(body.code, 'UNKNOWN_RESULT');
  assert.equal(Object.hasOwn(body, 'result'), false);
  assert.ok(Buffer.byteLength(JSON.stringify(body.raw), 'utf8') <= RAW_BYTE_LIMIT);
  assert.match(JSON.stringify(body.raw), /\[depth\]/);
});

test('R13 raw property budget rejects huge objects before descriptor reads', async () => {
  let descriptorReads = 0;
  const target = Object.fromEntries(Array.from({ length: 100000 }, (_, i) => [`p${i}`, i]));
  target.result = 1;
  const ret = new Proxy(target, {
    getOwnPropertyDescriptor(object, key) {
      descriptorReads += 1;
      return Reflect.getOwnPropertyDescriptor(object, key);
    },
  });
  const harness = await init({ joinGroup: () => ret });
  const body = await post(harness, '/join-group', { group_id: GROUP_ID, confirm: '1', reason: REASON });

  assert.ok(descriptorReads <= 1024, `descriptor reads must stay bounded (got ${descriptorReads})`);
  assert.equal(typeof body.raw, 'string');
  assert.match(body.raw, /too many properties/);
});

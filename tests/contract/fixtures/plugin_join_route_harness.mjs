/*
 * Real plugin route harness for the contract integration test.
 *
 * Imports the actual plugin package, runs its real plugin_init registration,
 * then invokes the registered /join-group handler for the six native return
 * shapes required by the R12 route-contract regression. No route, response,
 * or plugin code is stubbed here: only the native GroupService is controlled.
 *
 * Output: one JSON array on stdout, one entry per case.
 */

import { pathToFileURL } from 'node:url';

const pluginPath = process.env.PLUGIN_UNDER_TEST;
if (!pluginPath) {
  console.error('PLUGIN_UNDER_TEST must point at the real plugin index.mjs');
  process.exit(2);
}

const plugin = await import(pathToFileURL(pluginPath).href);

const routes = new Map();
let nativeReturn;
let nativeCalls = 0;
const logs = [];

const groupService = {
  joinGroup(arg) {
    nativeCalls += 1;
    if (arg?.richMsg !== 'operator-approved application text' || arg?.applyMsg !== 'group-answer') {
      throw new Error('native payload did not preserve reason/answer fields');
    }
    return nativeReturn;
  },
};

const recordLog = (level) => (...args) => {
  logs.push({ level, text: args.map((value) => String(value)).join(' ') });
};

const ctx = {
  router: {
    post(path, handler) {
      routes.set(path, handler);
    },
  },
  core: {
    context: {
      session: {
        getGroupService: () => groupService,
      },
    },
  },
  logger: {
    log: recordLog('log'),
    info: recordLog('info'),
    error: recordLog('error'),
  },
};

await plugin.plugin_init(ctx);

const joinHandler = routes.get('/join-group');
if (typeof joinHandler !== 'function') {
  console.error('plugin_init did not register /join-group');
  process.exit(3);
}

async function invoke(returnValue) {
  nativeReturn = returnValue;
  nativeCalls = 0;
  logs.length = 0;
  let body;
  const res = {
    json(value) {
      body = value;
      return this;
    },
  };
  await joinHandler(
    { body: { group_id: 829117089, confirm: '1', reason: 'operator-approved application text', answer: 'group-answer' } },
    res,
  );
  return { response: body, native_calls: nativeCalls, log_count: logs.length };
}

const objectWith66Properties = { result: 1 };
for (let index = 0; index < 65; index += 1) {
  objectWith66Properties[`p${String(index).padStart(2, '0')}`] = `v${index}`;
}

const cases = [
  { name: 'void_undefined', native: undefined },
  { name: 'integer_one', native: 1 },
  { name: 'object_66_props_with_result_one', native: objectWith66Properties },
  { name: 'request_id_257_chars', native: { result: 1, request_id: 'r'.repeat(257) } },
  { name: 'result_1_9', native: 1.9 },
  { name: 'result_negative_one', native: -1 },
];

const results = [];
for (const item of cases) {
  const outcome = await invoke(item.native);
  results.push({ name: item.name, ...outcome });
}

process.stdout.write(JSON.stringify(results));

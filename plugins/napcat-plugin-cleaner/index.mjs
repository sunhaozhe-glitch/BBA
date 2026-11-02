/* dxc-join-api：为 DXC 补回搜索群/主动申请加群的进程内 HTTP 端点。
 * 挂载（需 WebUI 鉴权）：POST /api/Plugin/ext/napcat-plugin-cleaner/search-group | join-group
 * 背景：NapCat v4.18.29 移除了 nc_search_group/nc_join_group action，core 内部能力仍在。
 * 证据：napcat.mjs ~10392 行保留 NodeIKernelSearchService/searchGroup +
 *       NodeIKernelSearchListener/onSearchGroupResult 的 callNormalEventV2 调用。
 * 红线：不做业务限流（DXC 侧已有）；join 需显式 confirm=1；
 *       日志不落 token、申请理由、答案或原生完整对象。
 * 结果语义：void/缺失/null/非有限/非安全整数/无法完整解析一律 UNKNOWN_RESULT，绝不默认成 result=1，
 *           也不从 message 等文案里借出「result」码；显式无效的 result 字段不得被相邻/嵌套字段替代。
 * raw 语义：只输出 null、有限非负安全整数、字符串或 ≤64 属性普通对象；序列化后的 UTF-8 字节
 *           （含转义）≤4096；遍历深度/节点/属性有界；数组/特殊对象/超限值退化为有界字符串；
 *           不执行 toJSON/getter/toString。request_ref 超过 256 Unicode 字符时整体省略。
 */

const SEARCH_TIMEOUT_MS = 10_000;
const JOIN_TIMEOUT_MS = 15_000;

const RAW_MAX_BYTES = 4096;
const RAW_MAX_DEPTH = 6;
const RAW_MAX_NODES = 256;
const RAW_MAX_PROPS = 64;
const RAW_MAX_KEY_BYTES = 256;
const RAW_OVERFLOW = '[raw omitted: exceeds bounded diagnostic limits]';
const RAW_OVERFLOW_TOKEN = Symbol('raw-overflow');

const REQUEST_REF_MAX_CHARS = 256;
const ERROR_MAX_CHARS = 500;
const WIRE_CODE = /^[A-Z][A-Z0-9_]{2,63}$/;

const DIAG_KEYS = ['name', 'message', 'code', 'result', 'request_ref', 'request_id', 'req_id', 'data'];
const REF_NEST_KEYS = ['data', 'result', 'raw', 'detail', 'details'];
const REQUEST_REF_KEYS = ['request_ref', 'request_id', 'req_id'];

const err = (code, message) => Object.assign(new Error(message), { code });

// 只读数据属性（自有或近端原型链，限深）；accessor 一律不执行，任何读取异常按缺失处理。
function dataProp(obj, key, maxDepth = 4) {
  let cur = obj;
  try {
    for (let depth = 0; depth <= maxDepth; depth += 1) {
      if (cur === null || (typeof cur !== 'object' && typeof cur !== 'function')) return undefined;
      const desc = Object.getOwnPropertyDescriptor(cur, key);
      if (desc) return 'value' in desc ? desc.value : undefined;
      cur = Object.getPrototypeOf(cur);
    }
  } catch {
    return undefined;
  }
  return undefined;
}

function messageText(e) {
  if (e === undefined || e === null) return '';
  const type = typeof e;
  if (type === 'string') return e;
  if (type !== 'object' && type !== 'function') return String(e);
  const message = dataProp(e, 'message', 4);
  return typeof message === 'string' ? message : '';
}

const sanitize = (e) => messageText(e)
  .replace(/(Bearer\s+)[A-Za-z0-9+/=._-]+/gi, '$1[REDACTED]')
  .slice(0, ERROR_MAX_CHARS);

function withTimeout(promise, ms, code) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(err(code, `操作超时(${ms}ms)`)), ms);
    Promise.resolve(promise).then(
      (v) => { clearTimeout(timer); resolve(v); },
      (e) => { clearTimeout(timer); reject(e); },
    );
  });
}

function readBody(req) {
  let b = req?.body;
  if (typeof b === 'string') { try { b = JSON.parse(b); } catch { b = {}; } }
  return b && typeof b === 'object' ? b : {};
}

// ── raw 诊断：有界遍历 + 精确字节预算；绝不执行 toJSON/getter/toString，也不做无界 stringify。
function rawValue(value, state, depth) {
  if (value === null || value === undefined) return null;
  const type = typeof value;
  if (type === 'string') {
    return Buffer.byteLength(value, 'utf8') > RAW_MAX_BYTES ? RAW_OVERFLOW_TOKEN : value;
  }
  // raw 只允许有限非负安全整数；其余数值（NaN/Infinity/小数/负数/超安全范围）退化为 null。
  if (type === 'number') return Number.isSafeInteger(value) && value >= 0 ? value : null;
  if (type === 'boolean') return value;
  if (type !== 'object') return null; // function/symbol/bigint 不属于 raw 允许的形态

  try {
    if (Array.isArray(value)) return '[array]';
    const proto = Object.getPrototypeOf(value);
    if (proto !== Object.prototype && proto !== null) return '[object]';
    if (state.seen.has(value)) return '[cycle]';
    if (depth >= RAW_MAX_DEPTH) return '[depth]';
    // Charge the node before enumerating keys or reading any descriptors.
    state.nodes += 1;
    if (state.nodes > RAW_MAX_NODES) return RAW_OVERFLOW_TOKEN;
    const keys = Reflect.ownKeys(value);
    if (keys.length > RAW_MAX_PROPS) return '[object: too many properties]';
    for (const key of keys) {
      if (typeof key === 'string' && Buffer.byteLength(key, 'utf8') > RAW_MAX_KEY_BYTES) return '[object: oversized key]';
    }
    state.seen.add(value);
    const out = {};
    for (const key of keys) {
      const desc = Object.getOwnPropertyDescriptor(value, key);
      if (!desc || !desc.enumerable) continue;
      let child;
      if (!('value' in desc)) {
        child = '[accessor]';
      } else {
        child = rawValue(desc.value, state, depth + 1);
        if (child === RAW_OVERFLOW_TOKEN) return RAW_OVERFLOW_TOKEN;
      }
      // defineProperty 而非赋值：键名可能是 "__proto__"，赋值会改写原型而不是生成自有属性。
      Object.defineProperty(out, key, { value: child, enumerable: true, writable: true, configurable: true });
    }
    state.seen.delete(value);
    return out;
  } catch {
    return RAW_OVERFLOW_TOKEN;
  }
}

function boundedRaw(value) {
  let built;
  try {
    built = rawValue(value, { nodes: 0, seen: new WeakSet() }, 0);
  } catch {
    return RAW_OVERFLOW;
  }
  if (built === RAW_OVERFLOW_TOKEN) return RAW_OVERFLOW;
  // 顶层 raw 只允许 null/非负安全整数/字符串/普通对象；布尔等其它标量退回 null。
  if (built === undefined || typeof built === 'boolean' || typeof built === 'symbol' || typeof built === 'bigint' || typeof built === 'function') built = null;
  let text;
  try {
    text = JSON.stringify(built);
  } catch {
    return RAW_OVERFLOW;
  }
  if (typeof text !== 'string' || Buffer.byteLength(text, 'utf8') > RAW_MAX_BYTES) return RAW_OVERFLOW;
  return built;
}

// 异常诊断同样只取数据属性；Node 的 stack 是 accessor，执行它就违反“不执行 getter”的约定。
function diagnosticFromException(e) {
  if (e === undefined) return null;
  if (e === null || typeof e !== 'object') return boundedRaw(e);
  const out = {};
  for (const key of DIAG_KEYS) {
    const value = dataProp(e, key, 4);
    if (value !== undefined) out[key] = value;
  }
  const stack = dataProp(e, 'stack', 1);
  if (typeof stack === 'string') out.stack = stack;
  return Object.keys(out).length > 0 ? boundedRaw(out) : boundedRaw(e);
}

// ── 搜索：优先 core.eventWrapper（v4.18.29 编译产物确认存在），回退 session.getSearchService()。
// 不用 GroupApi.searchGroup：它按 groupCode 再过滤，语义不对（astra 方案书 §2.1）。
async function searchGroups(ctx, keyword, count) {
  const param = { keyWords: keyword, groupNum: count, exactSearch: false, penetrate: '' };
  const ew = ctx?.core?.eventWrapper;
  if (ew && typeof ew.callNormalEventV2 === 'function') {
    const [, ret] = await ew.callNormalEventV2(
      'NodeIKernelSearchService/searchGroup',
      'NodeIKernelSearchListener/onSearchGroupResult',
      [param],
      (r) => r?.result === 0,
    );
    return ret;
  }
  const svc = ctx?.core?.context?.session?.getSearchService?.();
  if (svc && typeof svc.searchGroup === 'function') {
    const ret = await svc.searchGroup(param);
    return ret?.data ?? ret;
  }
  throw err('SEARCH_UNAVAILABLE', '本版本未找到搜索服务入口(eventWrapper/getSearchService)');
}

// -- 申请加群：当前安装 wrapper.node 的 ReqToGroup 是单对象参数。
// native 读取 groupCode/richMsg/applyMsg 等字段；理由与答案分别进入 richMsg/applyMsg。
// 这里不记录参数/返回日志：申请理由、答案与原生返回均不进日志。
async function requestJoin(ctx, groupCode, reason, answer) {
  const svc = ctx?.core?.context?.session?.getGroupService?.();
  if (!svc) throw err('JOIN_UNAVAILABLE', 'GroupService 不可用');
  const arg = {
    groupCode,
    sourceId: 0,
    sourceSubId: 0,
    richMsg: reason,
    applyMsg: answer,
    token: '',
    auth: '',
    noVerifyAuth: '',
    transInfo: {},
  };
  if (typeof svc.joinGroup === 'function') return svc.joinGroup(arg);
  if (typeof svc.reqToJoinGroup === 'function') return svc.reqToJoinGroup(arg);
  throw err('JOIN_UNAVAILABLE', '本版本未找到 joinGroup/reqToJoinGroup 入口');
}

// ── 结果码：只认非负安全整数；完整性由整个 JSON 字符串解析保证；不做数字前缀/正则/文案借用。
function isResultCode(value) {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

// 结果码只读取响应对象顶层的自有数据属性；RAW_MAX_DEPTH 是同一条对象边界守卫。
function resultFromObject(value, depth = 0) {
  if (!value || typeof value !== 'object' || depth >= RAW_MAX_DEPTH) return null;
  let desc;
  try {
    desc = Object.getOwnPropertyDescriptor(value, 'result');
  } catch {
    return null;
  }
  if (!desc || !('value' in desc)) return null;
  return isResultCode(desc.value) ? desc.value : null;
}

function extractResultCode(value) {
  if (typeof value === 'number') return isResultCode(value) ? value : null;
  if (typeof value === 'string') {
    let parsed;
    try {
      parsed = JSON.parse(value);
    } catch {
      return null;
    }
    if (typeof parsed === 'number') return isResultCode(parsed) ? parsed : null;
    return resultFromObject(parsed, 0);
  }
  return resultFromObject(value, 0);
}


// ── 请求关联标识：数据属性、限深、限 256 Unicode 字符；过长整体省略，不截断成另一个标识。
function ownRequestRef(value) {
  if (!value || typeof value !== 'object') return undefined;
  for (const key of REQUEST_REF_KEYS) {
    let desc;
    try {
      desc = Object.getOwnPropertyDescriptor(value, key);
    } catch {
      continue;
    }
    if (!desc || !('value' in desc)) continue;
    const raw = desc.value;
    if (typeof raw !== 'string') continue;
    const trimmed = raw.trim();
    if (trimmed !== '') return trimmed;
  }
  return undefined;
}

function extractRequestRef(value, depth = 0, seen = new Set()) {
  if (!value || typeof value !== 'object' || seen.has(value)) return null;
  seen.add(value);
  const direct = ownRequestRef(value);
  if (direct !== undefined) {
    return [...direct].length <= REQUEST_REF_MAX_CHARS ? direct : null;
  }
  if (depth >= 3) return null;
  for (const key of REF_NEST_KEYS) {
    let desc;
    try {
      desc = Object.getOwnPropertyDescriptor(value, key);
    } catch {
      continue;
    }
    if (!desc || !('value' in desc)) continue;
    const nested = desc.value;
    if (!nested || typeof nested !== 'object') continue;
    const found = extractRequestRef(nested, depth + 1, seen);
    if (found !== null) return found;
  }
  return null;
}

function wireCode(value) {
  return typeof value === 'string' && WIRE_CODE.test(value) ? value : 'JOIN_FAILED';
}

export const plugin_init = async (ctx) => {
  ctx.logger?.log?.('[dxc-join-api] 初始化：注册 search-group / join-group 端点');

  ctx.router.post('/search-group', async (req, res) => {
    const body = readBody(req);
    const keyword = String(body.keyword ?? '').trim();
    if (!keyword) return res.json({ ok: false, error: '缺少 keyword', code: 'BAD_REQUEST' });
    const count = Math.min(Math.max(Number(body.count) || 20, 1), 50);
    try {
      ctx.logger?.info?.('[dxc-join-api] search-group keyword 长度:', keyword.length, 'count:', count);
      const ret = await withTimeout(searchGroups(ctx, keyword, count), SEARCH_TIMEOUT_MS, 'SEARCH_TIMEOUT');
      const infos = Array.isArray(ret?.groupInfos) ? ret.groupInfos : [];
      // NTQQ 搜索结果字段是嵌套的：groupInfos[] = { groupCode, searchGroupInfo: { groupName, memberNum, ... }, privilege }
      // （官方类型定义：napcat-core/listeners/NodeIKernelSearchListener.ts, SearchGroupInfo）
      const groups = infos.map((g) => {
        const si = g?.searchGroupInfo ?? g ?? {};
        return {
          group_id: Number(g?.groupCode) || 0,
          group_name: String(si?.groupName ?? g?.groupName ?? ''),
          member_count: Number(si?.memberNum ?? si?.memberCount ?? g?.memberNum) || 0,
        };
      }).filter((g) => g.group_id > 0);
      ctx.logger?.info?.('[dxc-join-api] search-group 命中:', groups.length);
      res.json({ ok: true, groups });
    } catch (e) {
      const text = sanitize(e);
      const code = wireCode(dataProp(e, 'code', 1));
      ctx.logger?.error?.('[dxc-join-api] search-group 失败:', text);
      res.json({ ok: false, error: text || '搜索失败', code: code === 'JOIN_FAILED' ? 'SEARCH_FAILED' : code });
    }
  });

  ctx.router.post('/join-group', async (req, res) => {
    const body = readBody(req);
    const groupId = Number(body.group_id ?? body.groupId);
    if (!Number.isFinite(groupId) || groupId <= 0) return res.json({ ok: false, error: '缺少或非法 group_id', code: 'BAD_REQUEST' });
    if (String(body.confirm ?? '') !== '1') return res.json({ ok: false, error: '需显式 confirm=1 才提交加群申请', code: 'CONFIRM_REQUIRED' });
    const reason = String(body.reason ?? '');
    const answer = String(body.answer ?? '');
    if (!answer) return res.json({ ok: false, error: '缺少 answer', code: 'ANSWER_REQUIRED' });
    try {
      ctx.logger?.info?.('[dxc-join-api] join-group group:', groupId, 'reason 长度:', [...reason].length);
      const ret = await withTimeout(requestJoin(ctx, String(groupId), reason, answer), JOIN_TIMEOUT_MS, 'JOIN_TIMEOUT');
      const code = extractResultCode(ret);
      const raw = boundedRaw(ret);
      const requestRef = extractRequestRef(ret);
      if (code === null) {
        const out = { ok: false, error: '原生加群返回中没有可解析的 result 码', code: 'UNKNOWN_RESULT', raw };
        if (requestRef) out.request_ref = requestRef;
        ctx.logger?.error?.('[dxc-join-api] join-group 结果未知:', out.code, 'request_ref:', requestRef ? 'present' : 'none');
        return res.json(out);
      }
      const out = { ok: true, result: code, raw };
      if (requestRef) out.request_ref = requestRef;
      ctx.logger?.info?.('[dxc-join-api] join-group result:', code, 'request_ref:', requestRef ? 'present' : 'none');
      res.json(out);
    } catch (e) {
      const code = extractResultCode(e);
      const raw = diagnosticFromException(e);
      const requestRef = extractRequestRef(e);
      const out = {
        ok: false,
        error: sanitize(e) || '原生加群调用失败',
        code: code !== null ? `QQ_RESULT_${code}` : wireCode(dataProp(e, 'code', 1)),
        raw,
      };
      if (code !== null) out.result = code;
      if (requestRef) out.request_ref = requestRef;
      ctx.logger?.error?.('[dxc-join-api] join-group 失败:', out.code);
      res.json(out);
    }
  });

  ctx.logger?.log?.('[dxc-join-api] 就绪：/api/Plugin/ext/napcat-plugin-cleaner/{search-group,join-group}');
};

export const plugin_cleanup = (ctx) => {
  ctx.logger?.log?.('[dxc-join-api] 卸载：无持久资源需要清理');
};

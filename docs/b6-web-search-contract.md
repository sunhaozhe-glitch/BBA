# B6 网络搜索契约（实施边界）

## ARTEX 映射与来源
只读 ARTEX v0.3.24/e740334b：settings/page.tsx 网络搜索区；server/manager.go:260–268 明确独立搜索代理；agent/worker.go:28–51 和 442–451；db/config.go 的 Agent web_search 默认 false。实际协议在 go.mod 固定 norma v0.4.3 的 tool/websearch.go，本机只读缓存 /home/sun/go/pkg/mod/github.com/!autumn-27/norma@v0.4.3。

- brave-free：官方 GET /res/v1/web/search，q/count，X-Subscription-Token；web.results 标题/URL/description。BBA 保留该名称，不承诺免费额度。
- tavily：官方 POST /search，basic、max_results=5、不请求 answer/raw_content；results.title/url/content。norma 用 body api_key；BBA 按当前官方文档使用 Bearer header，不重复发送 key。
- ddgs：本批未由父实施者核实 norma 完整依赖/协议，不宣称已支持。本批未部署/审批该依赖；不临时拼 DuckDuckGo HTML 爬虫，禁用保留缺口。
- deepseek：官方 anthropic messages 内 web_search_20250305 服务端工具，额外一次模型调用，不经本地搜索代理。现有 OpenAI 兼容模型网关不支持；禁用，不模拟实现。

## API（先于配置/消费实现固定）
GET /v1/operator/system/web-search：system.read；认证 workspace 从 Actor 获取，不接收 workspace 参数。返回 enabled/source/has_key/version/can_configure/proxy(enabled,url,has_credentials)，不回显凭据。
PUT 同路径：system.configure；body enabled,source,expected_version,secret_action,api_key,proxy(enabled,url,secret_action,username,password)。CAS 冲突409；keep/replace/clear 必须明确，无多余秘密字段。来源仅 brave-free/tavily；未知/不支持拒绝；默认未启用。受保护既有 registry 原子持久化与审计，不新建配置存储或迁移。
POST /v1/operator/system/web-search/test：operator.read + model.request，认证 principal 必须存在于 workspace；只测当前已保存配置，body {query}，不接受任意 endpoint。不授予 QQ 或模型规划权限。与真实规划使用同一 consumer。
POST /v1/agent-model-requests：旧 agent_ref/request 不变，增加可选 search:{query}。无 search 时旧摘要/行为完全兼容。操作员必须明确提供独立查询；不从输入文本、QQ 历史/私聊/密钥自动生成。查询最多512 UTF-8字节、非空且无控制字符。含 search 的 request 必须 max_provider_attempts=1，防止含外部搜索请求歧义。搜索查询摘要加入既有幂等摘要，变更查询不能重放旧结果；旧无查询摘要不变。

## 消费与快照
现有任务授权/预算/版本/幂等检查先行；既有 Agent 历史版本 web_search 必须 true；workspace 总开关必须 true 且有 key。最多一次搜索，然后把结构化标题/链接/摘要作为独立、显式标记不可信的 user 数据交现有模型，绝不当 system 指令；不新增自治循环/工具/QQ权限。独立搜索代理空=直连，不继承 B5 或环境代理；固定官方 endpoint，DNS/TLS/重定向安全复用既有 transport。10秒硬超时，256KiB响应，最多5条，每 workspace 最多2个在途调用，无自动重试/正文抓取。

已开始搜索使用锁内取得的总配置/代理/Agent版本快照；关闭只拒绝之后的新调用，不撤销已开始请求。Agent 偏好不因总开关改变而清除。receipt 永久保存 workspace/request/config版本/来源/状态/耗时/结果及查询摘要；审计不存 key 或完整查询。失败与 no_results 区分，改变设置不重新解释旧结果。

## 错误分类
认证401；权限403；格式/查询/来源400；CAS409；关闭/Agent关闭/缺key/并发预算409；存储503；提供方429→502 SEARCH_PROVIDER_RATE_LIMIT、其他非200/网络→502 SEARCH_PROVIDER_FAILURE；超时504 SEARCH_PROVIDER_TIMEOUT；取消408 SEARCH_PROVIDER_CANCELLED；坏JSON/过大→502 SEARCH_RESPONSE_INVALID/SEARCH_RESPONSE_TOO_LARGE。认证/总开关/Agent关/缺key/不支持/预算拒绝均无 provider 请求，不返回伪造空结果成功。失败回执（无结果）持久记录后才返回；持久化失败阻断模型调用。

## 验收声明
本文件定义契约，不是执行/完成证据；最终以本次 REPORT、原始退出、同进程浏览器→HTTP fixture→模型请求/回执证据为准。真实付费 provider、公网、真实 QQ、生产迁移/服务替换均未授权。

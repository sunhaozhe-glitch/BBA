# P0-14 四号受控业务与故障注入（fixture 验收）

日期：2026-10-26

## 结论

P0-14 的**机制验收**在 isolated PostgreSQL fixture 上通过：

- 四个账号的 reservation、permit、execution、gateway operation、account usage 和 target claim 彼此隔离；
- 错误账号不能 accept/dispatch 另一账号的 operation，且在 adapter 调用前返回 `RESOURCE_NOT_FOUND`；
- 跨账号 observation 只进入 inbox / reconciliation，不写目标账号的 membership，也不创建 result；
- adapter 已进入但返回失联/错误时记录 `UNKNOWN`，再次 dispatch 不再调用 adapter；
- Store 关闭后重开并执行 `RecoverDispatching`，operation 落为 `UNKNOWN`，保留 goal slot 和 target claim，不自动重发；
- 四账号各自配额独立，额外 reservation 被拒绝，执行行数不增加。

**真实四号只读、BBA 四实例绑定已执行，真实四号业务写与故障注入未执行**：最终 R20 快照（2026-10-26 15:26:26 CST）中四个账号的 `get_login_info`、`get_status`、`get_group_list` 均成功，`self_id` 互不重复，共 12 次 OneBot 调用、0 次重试、0 次写入。BBA active 配置已替换为四实例，容器已重建，`qq_login_bindings` 已迁移为 `local-3210`–`local-3213`；BBA 实际登录客户端复验四个实例均 `is_login=true`、`core_ready=true`、`online=true`。15:24:48–15:26:18 CST 期间 3210、3212 曾短暂离线并自动重连；失败快照保留在 `/tmp/bba-r20-fail`，未用最终 PASS 覆盖。四号 engine 任务 smoke、真实业务写、真实网络分区和真实进程重启仍未执行。

## R23 当前四实例只读复验（2026-10-27 11:40 UTC）

本次 `tools/p0-14-engine.sh` 仍保持只读和 fail-closed，但未能进入 isolated engine smoke：3211 可连接并返回身份；3210、3212、3213 均 `transport failed`。此时 `ss` 仅观察到 `127.0.0.1:3211` 在监听。runner 在第一阶段退出，未发送消息、未创建 QQ 写入、未访问生产数据库；证据保留于 `/tmp/bba-p014-engine-rerun-20261027T114042Z/`，其中 `summary.json` 的 `outcome=FAIL`、`writes=0`，`engine_database_error=missing_engine_db_evidence` 是前置只读探测失败的结果，不是 engine assertion 通过。

因此 R23 不能替代 R22 的历史四实例只读 PASS，也不能证明当前四实例可用。真实四号业务写、网络分区、进程重启和 P0-14 受控故障注入仍未执行。
## R24 当前四实例只读与隔离 engine smoke（2026-10-27 12:21 UTC）

R24 的只读前置全部通过：3210→`443952741`、3211→`36374700`、3212→`3256395185`、3213→`454755768`，四个身份唯一且 `online=true`/`good=true`；12 次 OneBot 调用、0 重试、0 写入。证据目录：`/tmp/bba-p014-engine-schema12-20261027T122111Z/`。

随后隔离 PostgreSQL engine smoke、独立 SQL proof、敏感扫描与 manifest 校验均通过（run/db/scan/hash exit 均为 0）。四个 runtime heartbeat 与 paused `DRAFT` task 均一一对应；跨账号拒绝断言通过；execution、gateway operation、permit、result 均为 0，QQ writes=0。schema 为 12。

首轮相同 smoke 的 engine 与四账号断言已通过，但 SQL proof 因 runner 仍硬编码期待 schema 11 而失败；失败证据保留在 `/tmp/bba-p014-engine-recheck-20261027T121959Z/`。将 runner 断言更新为当前 schema 12 后，以独立新证据目录重跑通过。此项是 proof runner 的 schema expectation 更新，不改业务逻辑。

R24 仍只证明只读身份和隔离 fixture engine 行为，不包含真实 QQ 业务写、网络分区或运行中 NapCat 进程重启；P0-14 受控外部故障注入仍需明确测试群和动作范围。

## 真实四号只读核验（2026-10-26 15:14 CST）

| 实例 | WebUI | OneBot | UIN | 昵称 | 群数 | 登录状态 |
| --- | ---: | ---: | --- | --- | ---: | --- |
| `qq-2c7cdf77e452` | 6211 | 3211 | `36374700` | 逍遥游 | 52 | `isLogin=true` / `coreReady=true` |
| `qq-49b19b7c8937` | 6210 | 3210 | `3256395185` | 洽洽 | 0 | `isLogin=true` / `coreReady=true` |
| `qq-aeffae12147a` | 6212 | 3212 | `443952741` | 厄运旋律 | 27 | `isLogin=true` / `coreReady=true` |
| `qq-1e16626a4296` | 6213 | 3213 | `454755768` | ヾ斯密哒﹏ | 31 | `isLogin=true` / `coreReady=true` |

四个 `self_id` 互不重复。`3256395185` 的 `get_group_list` 为 HTTP 200、`status=ok`、`retcode=0`，群数为 0，不是读取失败；该账号当前没有群。

已知配置问题：supervisor 的 `state.json` label 与实际账号不一致——6210 标为 `443952741`，实际是 `3256395185`；6212 和 6213 的 label 为空。任何绑定必须以 OneBot `self_id` 为准，不能按 label 或残留的 `onebot11_443952741.json` 文件名判断。15:14 时 BBA 的 `qq-login-runtime/config.json` 仍只配置 `6099/3001`；BBA 四实例绑定已于 15:51 CST 完成，见下一节。

## R20 四号只读证据（2026-10-26 15:26 CST）

按 P0-13 的证据留存方式执行：

```bash
P014_EVIDENCE_DIR=/tmp/bba-r20 ./tools/p0-14-four.sh
```

runner 以 OneBot 端口绑定预期 `self_id`，不读取 supervisor label；每个账号只做 `get_login_info`、`get_status`、`get_group_list` 各一次。最终快照共 12 次 OneBot 调用、0 次重试、0 次写入。

| OneBot | UIN | 昵称 | `online` / `good` | 群数 | 群 ID 列表 SHA-256 |
| ---: | --- | --- | --- | ---: | --- |
| 3210 | `3256395185` | 洽洽 | `true` / `true` | 0 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |
| 3211 | `36374700` | 逍遥游 | `true` / `true` | 52 | `79942adbce0143412cc08d98fc6ccdd4367137331860aad795ee3e6dc549eb21` |
| 3212 | `443952741` | 厄运旋律 | `true` / `true` | 27 | `ceb6aa877d297df78ee861e100776179fe7c104038122a8e69ccfecce6cfcaed` |
| 3213 | `454755768` | ヾ斯密哒﹏ | `true` / `true` | 31 | `1e7a37549e3ebd34a7ed0546b96a8d62ee7f74af8fc3300bf2d29b489d804682` |

证据目录与校验：

| 快照 | 结果 | manifest SHA-256 | 说明 |
| --- | --- | --- | --- |
| `/tmp/bba-r20`；持久副本 `/home/sun/.local/state/bba-cdev/evidence/r20` | `PASS` | `450c31c5cbbcde0ab175fc46f665b0ad9965676e28376f99efb18276d6a563b7` | 最终四号快照；`sha256sum -c` 全部 OK；敏感扫描 `scan=pass` |
| `/tmp/bba-r20-fail`；持久副本 `/home/sun/.local/state/bba-cdev/evidence/r20-fail` | `FAIL` | `1138c928fd6fee71e670939f1430af8c891344b8b7917655a490c4c4ccb456f5` | 3210/3212 离线快照；同样完成哈希校验和敏感扫描，未删除 |

两次运行都包含 `environment.txt`、`requests.jsonl`、`steps.jsonl`、`summary.json`、`sensitive-scan.txt`、`stdout.log`、`evidence-sha256.txt` 和 `evidence-verify.json`。`requests.jsonl` 保留 OneBot 响应体，但不保存 Bearer、OneBot token、WebUI token 或认证 hash；扫描对四个实例的 token 均返回 `absent`。

短暂离线证据：`qq-49b19b7c8937/napcat.log` 在 15:24:48 CST 记录“账号状态变更为离线”；`qq-aeffae12147a/napcat.log` 在 15:24:59 CST 记录同一事件。15:25:33 CST 的 R20 重跑得到 `online=false` 的 `FAIL`，没有重试或写入。15:26:18 CST 只读观察两号恢复 `online=true`，15:26:26 CST 最终快照通过。该波动使“四号同时在线”只能作为快照结论，不能当作稳定性承诺。

## R20 BBA 绑定执行与冻结（2026-10-26 15:51 CST）

active 配置已从原单实例 `6099/3001` 替换为四实例，且去掉了当前源码不认识的 `onebot.api_style`。替换前备份：

- 旧配置：`/home/sun/.local/state/bba-cdev/qq-login-runtime/backup/r20-20261026T074556Z/config.json`，SHA-256 `ccf311b7c20262c4bbc7f1b812ef416ebfed9cfa7afc6165746bc0c324aa081f`；
- active 配置：`/home/sun/.local/state/bba-cdev/qq-login-runtime/config.json`，SHA-256 `76ebe278b17a441fe9ef0cf760b201847ddba75ce59ed2937fbdbf21ab3c846b`；
- 八个 per-port token 环境变量已写入 `bba-runtime.env`，容器内计数为 8；值未进入证据；
- 目标源码 `config.LoadValidated` 返回 `validated_instances=4`。

BBA 容器操作：

| 项 | 值 |
| --- | --- |
| 新容器 | `bba-cdev-login-f1`，id `deb4c34c5d77451ea979f7ecc4c63f13766ad0c91af0b3462da7c425a832da43` |
| 镜像 | `bba-cdev-f1:8bb315d` |
| 网络/端口 | host / `0.0.0.0:19328` |
| 健康检查 | `{"ok":true,"ready":true,"draining":false,"service":"artex","version":"2.2.0-f1"}` |
| 旧容器 | 停止并保留为 `bba-cdev-login-f1.pre-r20-20261026T074556Z`，用于回滚 |
| JWT key | 从旧容器复制，SHA-256 `2828339b7e082cac9c9a4639213d04e3fc6b09828cab7b10ff3a8012a802bc83` |
| 数据库 | `bba-cdev-pg-20261019` 已启动；`bba_ui_acceptance` 可连接 |
| 挂载 | 与旧容器一致；`/app/data` volume `9c1eae55…`，PG volume `012a0fb2…` |
| 环境键 | 旧键全部保留，另加 8 个 per-port token 键；值未进入证据 |

`qq_login_bindings` 迁移在一个 `ACCESS EXCLUSIVE` 事务内完成，只改 `instance_id`，bot id 与 QQ id 不变：

| 实例 | OneBot | bot_id | QQ |
| --- | ---: | ---: | --- |
| `local-3210` | 3210 | 12 | `3256395185` |
| `local-3211` | 3211 | 9 | `36374700` |
| `local-3212` | 3212 | 10 | `443952741` |
| `local-3213` | 3213 | 11 | `454755768` |

验证结果：

- `binding-verify.json`：用 BBA 自己的 `napcat.NewLoginClient` 对四个 WebUI 实例执行 `CheckStatus` + `QQInfo`，`count=4`、`all_ready=true`，四个均 `state=ready`、`is_login=true`、`core_ready=true`、`online=true`；
- `binding-db-verify.json`：`config.LoadValidated` 的四个实例与 `ListQQLoginBindings` 一一对应，`config_count=4`、`binding_count=4`、`all_ok=true`；
- 绑定证据目录 `/home/sun/.local/state/bba-cdev/evidence/r20-binding`，manifest SHA-256 `59e99674c70d87cfa48780067d737d69d00d522952e7bccfc6ae9186f846d70f`；`sha256sum -c` 全部 OK，敏感扫描 `scan=pass`；
- 全程没有 QQ 消息、入群、二维码刷新或 NapCat 重启；只重建了 BBA 容器。

## R21 管理面与任务准入 smoke（2026-10-26 16:18 CST）

在隔离数据库 `bba_r20_engine_smoke`（迁移 0002–0010，`migrate check` OK）上，用当前 active 四实例配置、真实 WebUI/OneBot token 和进程内测试服务器执行，不写生产库、不启动 LLM、不发送 QQ：

- `GET /api/qq-login/instances`：HTTP 200，4 个实例全部 `state=online`，`bound_qq` 分别为 `3256395185`、`36374700`、`443952741`、`454755768`，`bound_bot_id` 与隔离库绑定一致；
- 逐实例 `GET /api/qq-login/instances/{id}`：4/4 HTTP 200，`qq_id` 与绑定一致；
- 每个账号创建一条 `schedule.start_immediately=false` 的暂停任务：4/4 HTTP 201，`task_graph_roots.bot_id` 各自正确；正确账号读取 200，下一账号读取 404；任务数 4，engine 运行 0 次、LLM 调用 0 次、QQ 写 0 次。

### 发现并修复 OneBot wire 不兼容

旧 `onebot.Adapter` 对所有请求都发 `POST <endpoint>`，body 是 `{"action":"...","params":...}`。NapCat 4 的 `/` 返回 HTTP 200 且 `data={}`（`NapCat4 Is Running`），因此 `Health` 拿不到 `self_id`，管理面把在线实例误报为 `connecting` / “OneBot 身份尚未确认”；真实 `POST /get_login_info` 才返回账号身份。

修复（源码）：

- `onebot.Config` 新增 `PathRouted`；开启后请求改为 `POST <endpoint>/<action>`，body 直接是 params；
- BBA 的 `buildAccountOneBot` 与 `probeQQLoginOneBot` 显式设为 `PathRouted: true`；
- 默认仍保留原 envelope，避免影响其他 OneBot 实现；
- 新增 `TestPathRoutedWireUsesActionPathAndParamsBody`，验证 `/get_login_info` body `{}` 与 `/send_group_msg` params。

该修复尚未部署到运行中的 BBA 镜像；当前 R21 结论是“源码 smoke 通过，live 管理面仍待部署后复验”。

## 测试命令与结果

| 命令 | 退出码 | 结果 |
| --- | ---: | --- |
| `./tools/kernel.sh test -run '^TestP014' -v` | 0 | 5/5 PASS；包内 2.407s |
| `./tools/kernel.sh test -run '^TestP014' -count=5` | 0 | 5 轮重复通过；包内 7.953s |
| `./tools/kernel.sh test` | 0 | 完整 `internal/taskengine` 通过；包内 39.427s |
| `./tools/kernel.sh test-all -race=false` | 0 | 全仓 Go 包通过；`internal/taskengine` 33.066s |
| `go build ./... && go vet ./...`（golang:1.26，离线） | 0 | 无输出 |
| `gofmt -l internal/taskengine/p014_test.go` | 0 | 无输出 |
| `./tools/kernel.sh schema` | 0 | `PostgreSQL schema version=10` |
| `go test ./cmd/p0-14-four -count=1`（golang:1.26，离线） | 0 | 5/5 PASS；包内 0.005s |
| `P014_EVIDENCE_DIR=/tmp/bba-r20 ./tools/p0-14-four.sh` | 0 | 12 次 OneBot 读取、0 重试、0 写入；`scan_exit=0` |
| `go vet ./cmd/p0-14-four && go build ./cmd/p0-14-four`（golang:1.26，离线） | 0 | 无输出 |
| `gofmt -l cmd/p0-14-four/main.go cmd/p0-14-four/main_test.go` | 0 | 无输出 |
| BBA `config.LoadValidated`（golang:1.26，4 实例配置） | 0 | `validated_instances=4` |
| BBA `binding-verify`（实际 `napcat.NewLoginClient`） | 0 | `count=4`、`all_ready=true`；四个均登录/core/online |
| BBA `binding-db-verify`（config + `ListQQLoginBindings`） | 0 | `config_count=4`、`binding_count=4`、`all_ok=true` |
| `sha256sum -c .../r20-binding/evidence-sha256.txt` | 0 | 11/11 OK；敏感扫描 `pass` |
| `go test ./onebot`（golang:1.26） | 0 | 包含 path-routed 回归，PASS |
| R21 isolated smoke `go test ./server -run TestR20AdminBindingAndPausedTaskAdmissionSmoke` | 0 | 4 online、4 paused tasks、cross-account 404；engine/LLM/QQ 0 |

## R22 四号只读 engine smoke（2026-10-26）

最终运行 `tools/p0-14-engine.sh` 于 UTC 14:23:58–14:24:00 完成，退出码 0。首次探测时 3212 无 listener，runner 只读重试一次后 fail-closed；稍后确认四端口恢复，再使用独立证据目录重跑。没有重启服务。最终证据：`/home/sun/.local/state/bba-cdev/evidence/r22-engine-pass-20261026T142358Z/`。

| 检查 | 结果 |
| --- | --- |
| OneBot 只读身份/状态/群列表 | 12 次请求、0 重试；四个实时 `self_id` 与端口绑定匹配且唯一；0 QQ 写入 |
| Runtime heartbeat | 四个 runtime 各自 POST 成功（202），精确重放仍为 202；每账号恰好 1 条 heartbeat |
| Dry-run task | 每账号创建 1 条 task（201、`DRAFT`），精确重放返回原 task，读取为 200；每条 task 仅绑定自己的 account/runtime |
| 隔离拒绝 | 跨 runtime/account heartbeat 与跨账号 task read 为 404；跨账号 task create 为 403 |
| 独立 SQL proof | schema 10；4 accounts/credentials/tasks/task_accounts/heartbeats/heartbeat idempotency/task events；4 个 free goal slots 与 4 条 task outbox；execution、gateway operation、permit、result、subtask、usage、inbox、membership、held slot、claim、action lock、approval、target、baseline、retry、conflict、bridge outbox 均为 0 |
| 副作用与证据 | QQ/dispatch/engine execution/LLM 均为 0；敏感凭据扫描 PASS；12 个 manifest 文件校验通过；run/db/scan/hash exit 均为 0 |

engine smoke 使用 schema 10 的临时 PostgreSQL Unix socket `/fixture` 数据库、真实 heartbeat/task HTTP handlers 和独立 psql 复核；runtime token 仅在内存生成。此结果证明隔离 fixture 上暂停态 dry-run task API、heartbeat 与 account/runtime 边界，不代表生产数据库、planner/worker、reservation/dispatch 或真实业务写。

Evidence manifest SHA-256：`fdec45d99e0a7995cf2098a08d39753f0f0a886251b548ed23a6dd1856a1837c`。31 文件代码指纹 `8bd20bbdb9cc9ef0755ef8686725f020adf63ceaffbdcd63cb0aada92b99cc0c`。最终逐文件哈希、runner 摘要和独立数据库结果均保存在上述目录；首次失败记录保留于 `/home/sun/.local/state/bba-cdev/evidence/r22-engine-20261026T141326Z/`。

验证过程中的失败证据保持原样：`r22-engine-recheck-20261026T141831Z` 的 engine smoke 为 PASS，但首版 SQL 汇总 parser 未识别 `counts` 行；`r22-engine-final-20261026T142004Z` 修复分类后发现 parser 变量名错误；`r22-engine-verified-20261026T142138Z` 又确认 outbox 的预期值应为 4（每个 dry-run task 一条普通事件），并非执行写。修正独立核验器后，最终 `r22-engine-pass-20261026T142358Z` 全项通过；各次 evidence manifest 均独立保留。

| 命令 | 退出码 | 结果 |
| --- | ---: | --- |
| `go test ./cmd/p0-14-four -count=1`（golang:1.26，离线） | 0 | 定向 runner/engine tests 通过 |
| `../tools/kernel.sh test-all -race=false` | 0 | 全仓 Go fixture suite 通过 |
| `bash -n tools/p0-14-engine.sh` | 0 | 独立 SQL proof runner 语法通过 |
| `tools/p0-14-engine.sh`（最终 R22） | 0 | live reads、engine smoke、独立 DB proof、敏感扫描与 manifest 全通过 |


## 覆盖矩阵

| 用例 | 断言 |
| --- | --- |
| `TestP014FourAccountRoutingIsolation` | 4 个账号各 1 个 task/1 个 target；reservation 的 account/runtime 与任务唯一允许账号一致；错误账号 accept 和 dispatch 均被拒且 adapter 0 次；每账号恰好 1 次 dispatch、1 条 execution、1 条 gateway、1 个 consumed permit、usage=1、无残留 action lock；每账号自己的 joined observation 恰好产生 1 条 result，总计 4 条 result/4 个 fulfilled slot，不共享计数 |
| `TestP014CrossAccountObservationDoesNotCredit` | 账号 B 对账号 A 的 operation 提交 joined observation：inbox 可保留，result=0，membership B=0，A 的任务出现 1 条 `manual_reconciliation`；A 自己的 observation 只产生 1 条 result，重复 apply 不增加 result |
| `TestP014RestartNoResend` | 在 permit 已签发、adapter 未调用时关闭 Store 并重开；`RecoverDispatching` 将 execution 置为 `UNKNOWN`；goal slot 仍 reserved、target claim 仍 active；再次 dispatch 返回 unknown 且 adapter 0 次；再次 recover 为 0 |
| `TestP014OfflineAccountNoSend` | 账号离线时 dispatch 返回 `ACCOUNT_RESTRICTED` 且 adapter 0 次；恢复在线后同一 operation 恰好发送 1 次，未产生重复 |
| `TestP014FourAccountQuotaAndDisconnect` | 4 个账号并发 dispatch；账号 B adapter 返回失联错误后 execution=`UNKNOWN`、usage=1，再次 dispatch 返回 unknown 且不调用 adapter；其余账号各 `OBSERVED`、usage=1；额外 reservation 被 `NO_CAPACITY`/`LOCAL_BUDGET_EXHAUSTED` 拒绝，execution 总数仍为 4 |

## 代码指纹

```text
p014_test.go  e1d65f14e9c3d063166cc1d686ce7e3c6e3fd50f4b08cdda89a8cb23fb50dc36
bridge.go     f4e975c2c96d434b3e9736d0956f58eb0352d40bae8d60a5752664201251f5d8
observations.go d6778d13e8a9099a8d3113f88cdc9c7b8d1c6cdeed796c588f5d008e528e65a5
scheduler.go  b9a4390ef0a4de807ca4329122cd8829bcebb0fe43e0b29c2f8556162c6c2404
```

R20 runner 代码指纹：

```text
backend/cmd/p0-14-four/main.go       1d1ea289b71836a171531953dfb07e91418dff0f030d6137c0a4b4cac76056c3
backend/cmd/p0-14-four/main_test.go  2e5cc1c0c20d9366c8bee66695a148dd3cc833e82b46e2282ceff2c67f1ff5da
tools/p0-14-four.sh                  20f4c917c7ed246170fa13e5b718a83ed780ec79c4a556797ac364350edbcffd
```

三文件合并 `code_sha=9e3dcea9e8cb0e6d29d659e594ae4333fab227f178e2833f50867083e43d1778`。若修改 runner 或脚本，必须重跑并重新计算证据 manifest。

这些哈希只对应本轮 fixture 验收；若后续修改测试或引擎，必须重新计算，不能沿用本记录。

## 未验证与限制

- R22 已用四个真实账号完成只读 OneBot 核验，并在隔离 fixture 验证 heartbeat 与 paused dry-run task API；没有真实四号业务写、真实网络分区或真实 QQ 插件调用。`disconnect` 仍是 account 状态和 adapter 错误注入。
- R22 首次读取时 3212 无 listener，runner fail-closed；确认 listener 恢复后独立重跑通过。两次快照只证明各自探测时刻，不证明长时间稳定。
- `restart` 是 Store 关闭并重开到同一 fixture 数据库，不是 systemd/supervisor 进程重启；进程级重启仍需在部署环境中单独演练。
- 没有生产数据库、生产 writer、NapCat 插件或真实 QQ 写入。
- BBA active 配置与四个 durable binding 已验证；旧容器仍处于停止状态，回滚必须恢复备份配置/环境变量并切回旧容器。
- R21 只验证管理面读取与暂停任务准入；R22 未启动 engine planner/worker，也未验证 reservation/dispatch 的真实账号运行。
- OneBot path-routed 修复目前只通过源码测试，运行中的 BBA 镜像尚未重建部署。
- `3256395185` 当前 0 群；可用于登录/隔离核验，但不能当作已有群可操作账号。
- 没有验证 20 账号容量、Character/L1/L2 或灰度切换；这些属于 P0-15 之后。

## 下一步

1. 重建并部署含 OneBot path-routed 修复的 BBA 镜像；部署后用只读管理面复验四实例状态。尚未部署或操作运行中服务。
2. 真实四号写/故障注入必须单独授权，使用受控测试群，并保留 `UNKNOWN` 不重试语义。
3. 进入 P0-15 前，先确认 Character 隔离和发送门禁范围；P0-15 不应复用本轮的 adapter 测试替身。

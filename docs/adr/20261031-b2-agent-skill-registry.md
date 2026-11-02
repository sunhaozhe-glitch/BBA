# B2 ADR：复用B1保护文件注册表（实施草案，未验收）

选用既有 ModelRegistry 作为模型、Agent、Skill的唯一配置权威；不另增SQLite/YAML/浏览器存储。复用同一个受保护JSON文件、lifetime flock、进程mutex、临时文件fsync/rename和目录同步。数据库仍是任务、权限及模型请求幂等账本的权威，不增加第二任务执行器，无schema迁移。新增配置历史与消费receipt存同一注册表，快照不含模型APIKey，仅模型公开元数据。

Agent及Skill写采用独立agent.configure/skill.configure，读需operator.read；workspace内验证引用。工具当前没有核验可执行schema，tools只接受空数组；structured_output是唯一支持能力。运营文本在user层JSON中标记不可信，系统层仍固定澄清输出、账号边界与无工具权限。

跨存储限制：请求先在数据库占幂等位，再由原模型执行路径的bound provider解析与持久化配置receipt。两者不是一个跨存储事务。进程崩溃可遗留started请求，不能宣称自动恢复/分布式一致性完成。注册表4MiB上限未扩容及归档，历史满后明确拒绝写。

续修后权限历史缺失、重放比较和旧预算回归已修复；go-final与integration-fourth通过，尚待Astra独立复验，不能作为生产批准。模型配置元数据历史仅消费时快照，完整模型编辑历史未完成；跨存储事务与崩溃恢复未扩展。

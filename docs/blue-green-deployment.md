# 蓝绿部署与签名升级计划

updater 是宿主机部署执行器，统一管理安装、预检、迁移、切流和恢复。先按现有 README 中的信任引导步骤安装签名 updater 包；以下命令由主机管理员执行。

```sh
# 全新空目录安装
geoflow-updater install --instance primary --root /opt/geoflow --url https://geo.example

# 查看计划，记录 JSON 中的 plan_sha256
geoflow-updater update --instance primary --dry-run --json

# 在线计划执行；维护计划显式追加 --allow-maintenance
geoflow-updater update --instance primary --plan-sha256 <plan_sha256>

# 查看实例状态
geoflow-updater doctor --instance primary --json

# 应用回切保留当前数据库
geoflow-updater switch-back --instance primary

# 指定完整恢复点的数据恢复
geoflow-updater rollback --instance primary --recovery-point <point_id>
```

现有未受管站点先运行 `enroll --instance-id primary --instance-root /opt/geoflow`。初次从单套布局转换为蓝绿需要维护窗口。首次安装生成的管理员凭据仅写入站点 `install-credentials.txt`；相同安装参数可在中断后重试，保留原密钥与已有数据。

稳定的 PostgreSQL、Redis 与入口由 infra Compose 管理，blue/green 拥有各自的 web、PHP、Reverb、全部队列和调度器。槽位间共享业务存储与会话身份，编译视图和应用网络分别隔离。旧请求、队列和调度子进程通过优雅排空交接；排空未完成时保留旧槽位并报告恢复待处理。

应用升级、首次初始化、队列及调度进程以 UID 33 读写共享存储。部署器为新目录设置明确权限，运行进程关闭重复权限扫描和自动缓存优化；每个槽位的视图缓存由切流前的升级步骤预热。生产入口优先使用已注入的 APP_KEY，容器继续只读挂载受保护的 `.env.prod`。

在线更新要求签名计划声明允许来源与数据库、队列、缓存、存储兼容性。默认计划为 maintenance。在线失败执行应用切回并保留新写入；开放流量后需要恢复数据时，通过单独的数据恢复操作处理。在线数据库快照仅覆盖 PostgreSQL，完整恢复点在维护停写阶段创建。

宿主机预检最多 25 分钟；管理后台通过实例 token、分操作授权码和预检摘要调用受限 socket API。网站的应用回切与数据恢复使用不同授权范围。

发布采用 schema 3 并签名 `releases/<version>/upgrade-plan.json`。维护计划要求协议 3，在线计划要求协议 4。候选工作流校验 GEOFlow 的迁移文件清单；新 `planned-acceptance.yml` 验证两种原生架构的候选应用和真实入口契约。完整已安装主机的升级、数据库还原和崩溃恢复，以及登录、任务和 Reverb 行为，应随对应候选另行演练。上线前根据新旧应用并存峰值核实主机容量。

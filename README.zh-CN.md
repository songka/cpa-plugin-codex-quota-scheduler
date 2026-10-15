# Codex Quota Reserve 安装与回滚手册

独立插件 ID：`codex-quota-reserve`。目标宿主：**CLIProxyAPI v8.0.27**。发行版本：**0.2.2**。发布仓库：`songka/cpa-plugin-codex-quota-scheduler`。

基于 Jeffery 的 MIT Codex Quota Scheduler v0.3.2 改造。[原版中文说明](docs/upstream/README.zh-CN.md)保留成熟功能的详细说明。

## v0.2.2 主要更新

- 可归属到账号的上游临时失败增加 30 秒冷却；严格额度保护的嵌套重试切换其他安全账号，后续选号跳过失败账号，到期自动恢复。
- 宿主 YAML 的显式字段逐项覆盖历史配置，未传入的字段、私有 Webhook 和账号注释保留；持久化失败时返回错误，不先修改运行配置。
- 明确在执行前被拒绝的请求回滚最后一次准入预算。真实重试仍独立估算，503、断线、超时不能证明未消耗额度，不盲目退款。
- 额度保护开启且刷新间隔超过 5 分钟时显示警告。保留恢复围栏对陈旧额度重放的保护，补充相等时间戳的回归验证。
- 逐项核对与边界见[自检报告](docs/reserve-audit-2026-10-15.md)。after-auth 已终止请求仍不能在 CPA v8.0.27 内重新选路。

## v0.2.1 主要更新

额度周期重置和历史事件不再单独发送 Webhook。自动通知只在账号实际开始／停止参与调度，或定时使用报告到期时发送；主动重置后的恢复只有在新额度和 CPA 重新纳入调度都确认后发送一次“开始参与调度”。

插件页面保存设置后，会通过 CPA 管理 API 同步到 `plugins.configs.codex-quota-reserve` 配置面板。`enabled`、`store`、Webhook 地址、Chat ID 及其已配置标志不会被覆盖或泄露。

每次选账户时实时排除并发或预算受保护的账号，选择其他安全账号；请求被拦截后立即显示具体保护标记，并发占用在请求完成后立即解除。CPA v8.0.27 无法将后置拦截已终止的同一请求重新选路，极少数并发竞态仍返回 503（`Retry-After: 1`），后续请求会重新选择安全账号。

加入全局及单账户 5h、Weekly/Monthly 额度预留，严格拒绝、请求准入、历史曲线和可选通知。最低保留比例与实际调度停止线分别显示；默认最低保留 10%，安全余量 2%，剩余 **≤12%** 即保护。保护默认关闭，开启后默认严格模式和缓存过期保护。

预留保护独立于上游耗尽、429 冷却和人工停用。只有新的有效额度证据才允许恢复，不修改 OAuth 敏感字段。不能保证每个请求的真实消耗，10% 不是上游承诺的精确硬限制。

新增“达到预留线时在 CPA 中停用账号”开关，修复只标记保护但 CPA 仍显示启用的问题。默认不改变 CPA 停用状态，必须显式开启 `reserve_disable_accounts`；开启后记录停用所有权和凭据指纹，不接管人工停用账号，不自动启用轮换后的凭据。

支持主动重置后的恢复：插件会按额度刷新间隔重新查询自己停用的账号，不需要等旧重置时间；点击刷新可提前检查。5h 和长期窗口均可用且高于保护线后自动启用，已停用账号仍保留在管理页面和通知报告中。

支持自动估算每请求消耗，配置上下限防止估计过大或过小；支持插件内配置 Webhook、定时使用报告和参与调度状态变更通知，消息展示额度、主动重置剩余次数和到期时间。

## Docker 前提检查

1. 确认宿主版本为 v8.0.27，容器为 Linux AMD64 或 ARM64，使用 glibc。Alpine/musl 不在当前构建覆盖内。
2. 备份 CPA 配置、原插件文件及插件用户数据；禁止在验证前覆盖生产宿主。
3. 确认实际插件目录和管理 API 路径，配置 `plugins.enabled: true`。
4. 插件数据使用进程启动时的 `XDG_CONFIG_HOME`。推荐 `XDG_CONFIG_HOME=/data/config`，将宿主目录挂载到 `/data/config`，并保证 CPA 用户有写权限。
5. 将插件目录挂载到 `/data/plugins`。新插件用户数据位于 `/data/config/CLIProxyAPI/codex-quota-reserve/`，与原版分离。不要共享新旧插件的同一状态目录。

CPA 配置示例（合并进已有配置，不替换其他渠道和认证）：

```yaml
plugins:
  enabled: true
  dir: /data/plugins
  store-sources:
    - https://raw.githubusercontent.com/songka/cpa-plugin-codex-quota-scheduler/main/registry.json
  configs:
    codex-quota-scheduler:
      enabled: false
    codex-quota-reserve:
      enabled: true
      reserve_enabled: false
      quota_refresh_interval: 5m
```

使用现有公开仓库发布；商店安装需要 v0.2.2 Release 中存在对应架构 ZIP 和 `checksums.txt`。开发验收阶段也可将经过 SHA256 检查的对应架构 `.so` 放入挂载的插件目录。文件名必须为 `codex-quota-reserve.so`。插件库地址保持不变，版本由 GitHub 最新正式 Release 自动发现。CPA v8.0.27 缓存最新版本查询一小时，发布后仍显示旧版本时可等缓存过期，或备份后重启 CPA 再刷新插件库。

## 迁移与启用

1. 在原管理页面导出完整配置和账户注释，另存原状态目录备份。
2. 停用旧调度器并确认它不再出现在宿主活动插件列表；必要时重启 CPA。
3. 安装并启用新插件，先保持额度预留关闭。
4. 打开 **Codex Quota Reserve** 管理页面，用原 CPA 管理密钥加载状态。在新插件中导入原导出文件，检查账户优先级、月度模式、别名、分组、标签、备注、探活及模型重试设置。
5. 手动刷新所有账户，检查额度来源、更新时间和窗口重置时间。未知或查询失败的账户不得被认为有 100% 额度。
6. 设置 `quota_refresh_interval: 5m`；默认 30 分钟仅为兼容原版配置而保留。
7. 开启预留保护，设置最低保留比例、安全余量及严格模式。按需要在账户“编辑”中填写独立阈值，留空继承全局，0 为有效值。
8. 先使用模拟数据或非生产测试账户验证全部账户保护时拒绝请求、恢复后重新选账，以及 NewAPI 的错误透传。

默认每账户最多一个在途请求，自动估算开启；样本不足时回退为每请求 0.25 个百分点。至少三个有效样本后使用最近最多八个样本的中位数乘 1.25，默认限制在 0.01%-2%；可调整上下限，但必须满足 `0 < 下限 <= 上限 <= 10%`。主动重置、凭据变化、无请求、时间倒退和异常跳变不参与学习。关闭自动估算则使用固定预算。

新额度读数确认前，每次上游尝试都会累计扣减估算值；完成请求仅释放并发，不退还估算额度。根据实际请求大小选择预算和安全余量；并发竞争时可能返回本地 503，避免绕过保护。

管理 API 基址为 `/v0/management/plugins/codex-quota-reserve`。原有 `/settings`、`/refresh`、`/refresh/account`、`/annotations`、`/logs`、`/export`、`/import`、`/history` 保留，新增 `/notifications/test`。接口仍受宿主管理认证保护。

## 历史与通知

历史支持账户筛选、5h/24h/7d 范围、5h/长期窗口切换和当前保护线；重置、进入保护和恢复会显示事件标记。默认保留 30 天，每账户最多 10,000 个点。历史位于独立 `quota-history.json`，定期原子保存并在退出时刷新。

通知默认关闭。在插件“历史与通知”设置中直接填写 Webhook 地址；Telegram 另填 Chat ID。选择企业微信、飞书、钉钉、Telegram 或通用 JSON 格式，点击“发送测试通知”验证接收。已保存地址不回显，留空保留，勾选清除项才删除。地址和 Chat ID 保存在私有插件用户数据中，不包含在设置响应和导出配置里；导入脱敏配置不会清除已有秘密。环境变量 `RESERVE_WEBHOOK_URL`、`RESERVE_TELEGRAM_CHAT_ID` 仅作为没有已保存值时的回退。

开启通知后，只有账号开始/停止参与调度，以及到达定时使用报告时间才会发送消息。额度周期重置、进入保护、恢复历史等事件只写入历史和日志，不会因为刷新额度重复报警；主动重置后的恢复在确认重新参与调度后只发送一次开始通知。企业微信和钉钉使用 Markdown，飞书使用卡片，Telegram 使用 HTML；报告显示 5h/长期剩余额度及重置时间、主动重置剩余/累计次数、次数到期时间和额度观测时间。多账号在消息大小允许时合并到一条消息，超出平台限制时才按完整账号分页。

通知由独立工作线程尽力投递，不阻塞额度保护，不承诺必达；拒绝重定向，并检查平台成功回执。地址要求 HTTPS，仅本机回环测试接收器允许 HTTP。带签名的钉钉机器人需外部签名网关；当前不生成签名。保护插件数据目录和备份，不要把 bot token、带密钥的 URL 写入 Git 或聊天。

严格模式的嵌套重试会固定已通过保护检查的 Codex 账户，因为宿主会跳过发起插件自己的拦截器。重试目标使用 Antigravity 等其他提供方时请显式填写 provider；严格保护开启时空 provider 按 Codex 处理。常规非 Codex 选择继续交给宿主。

## 升级与回滚

升级前备份插件数据和旧动态库，核对发行 ZIP 的 SHA256。替换库后建议重启 CPA，再检查活动插件版本、管理页面、配置和历史。宿主有 quiesce/热替换机制，但本项目的生产热更新尚未全面验收，因此暂不承诺无重启升级。

回滚时先停用新插件并重启，确认其不再接管调度；恢复原动态库和原插件配置，启用旧调度器。保留新插件数据供排查，恢复旧插件自身备份而不是把新版状态直接覆盖到旧版。不得同时启用两套 Codex 调度插件。

## 开发与发布

需要 Go 1.26+、CGO、GCC 和 Make：

```sh
make test vet
go test -race -run '^TestReserve' -count=1 ./...
make package VERSION=0.2.2
make checksums VERSION=0.2.2
bash scripts/test-host-integration.sh
```

GitHub Actions 在 PR 时执行格式检查、回归、race 检查、真实宿主动态加载集成测试和双架构原生构建。配置带 required reviewers 的 `release-approval` GitHub Environment。测试完成并得到用户批准后才可以：

```sh
git tag -a v0.2.2 -m "v0.2.2"
git push origin v0.2.2
```

仓库沿用现有名称；插件 ID、动态库和发行 ZIP 基名仍为 `codex-quota-reserve`，保持独立运行身份。参阅[验证记录和限制](docs/reserve-validation.md)。原 [MIT 许可证](LICENSE)和版权保留；正式再分发前需核对依赖许可证。

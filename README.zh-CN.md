# Codex Quota Reserve 安装与回滚手册

独立插件 ID：`codex-quota-reserve`。目标宿主：**CLIProxyAPI v8.0.27**。开发版本：**0.1.0**。目前未获批准发布正式版本。

基于 Jeffery 的 MIT Codex Quota Scheduler v0.3.2 改造。[原版中文说明](docs/upstream/README.zh-CN.md)保留成熟功能的详细说明。

## v0.1.0 主要更新

加入全局及单账户 5h、Weekly/Monthly 额度预留，严格拒绝、请求准入、历史曲线和可选通知。最低保留比例与实际调度停止线分别显示；默认最低保留 10%，安全余量 2%，剩余 **≤12%** 即保护。保护默认关闭，开启后默认严格模式和缓存过期保护。

预留保护独立于上游耗尽、429 冷却和人工停用。只有新的有效额度证据才允许恢复，不修改 OAuth 敏感字段。不能保证每个请求的真实消耗，10% 不是上游承诺的精确硬限制。

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
    - https://raw.githubusercontent.com/songka/cpa-plugin-codex-quota-reserve/main/registry.json
  configs:
    codex-quota-scheduler:
      enabled: false
    codex-quota-reserve:
      enabled: true
      reserve_enabled: false
      quota_refresh_interval: 5m
```

上述新仓库和正式 Release 尚未创建，商店安装需要它们先存在。开发验收阶段可将经过 SHA256 检查的对应架构 `.so` 放入挂载的插件目录。文件名必须为 `codex-quota-reserve.so`。

## 迁移与启用

1. 在原管理页面导出完整配置和账户注释，另存原状态目录备份。
2. 停用旧调度器并确认它不再出现在宿主活动插件列表；必要时重启 CPA。
3. 安装并启用新插件，先保持额度预留关闭。
4. 打开 **Codex Quota Reserve** 管理页面，用原 CPA 管理密钥加载状态。在新插件中导入原导出文件，检查账户优先级、月度模式、别名、分组、标签、备注、探活及模型重试设置。
5. 手动刷新所有账户，检查额度来源、更新时间和窗口重置时间。未知或查询失败的账户不得被认为有 100% 额度。
6. 设置 `quota_refresh_interval: 5m`；默认 30 分钟仅为兼容原版配置而保留。
7. 开启预留保护，设置最低保留比例、安全余量及严格模式。按需要在账户“编辑”中填写独立阈值，留空继承全局，0 为有效值。
8. 先使用模拟数据或非生产测试账户验证全部账户保护时拒绝请求、恢复后重新选账，以及 NewAPI 的错误透传。

默认每账户最多一个在途请求；每请求估算扣减 0.25 个百分点，直到新额度读数确认。完成请求仅释放并发，不退还估算额度。根据实际请求大小选择预算和安全余量；并发竞争时可能返回本地 503，避免绕过保护。

管理 API 基址为 `/v0/management/plugins/codex-quota-reserve`。原有 `/settings`、`/refresh`、`/refresh/account`、`/annotations`、`/logs`、`/export`、`/import` 保留，新增 `/history`。接口仍受宿主管理认证保护。

## 历史与通知

历史支持账户筛选、5h/24h/7d 范围、5h/长期窗口切换和当前保护线；重置、进入保护和恢复会显示事件标记。默认保留 30 天，每账户最多 10,000 个点。历史位于独立 `quota-history.json`，定期原子保存并在退出时刷新。

通知默认关闭。启动容器前安全注入 `RESERVE_WEBHOOK_URL`；Telegram 另需 `RESERVE_TELEGRAM_CHAT_ID`。选择企业微信、飞书、钉钉、Telegram 或通用 JSON 格式。带签名的钉钉机器人需外部签名网关；当前不生成签名。不要把 bot token、带密钥的 URL 写入 Git、导出配置或聊天。

严格模式的嵌套重试会固定已通过保护检查的 Codex 账户，因为宿主会跳过发起插件自己的拦截器。重试目标使用 Antigravity 等其他提供方时请显式填写 provider；严格保护开启时空 provider 按 Codex 处理。常规非 Codex 选择继续交给宿主。

## 升级与回滚

升级前备份插件数据和旧动态库，核对发行 ZIP 的 SHA256。替换库后建议重启 CPA，再检查活动插件版本、管理页面、配置和历史。宿主有 quiesce/热替换机制，但本项目的生产热更新尚未全面验收，因此暂不承诺无重启升级。

回滚时先停用新插件并重启，确认其不再接管调度；恢复原动态库和原插件配置，启用旧调度器。保留新插件数据供排查，恢复旧插件自身备份而不是把新版状态直接覆盖到旧版。不得同时启用两套 Codex 调度插件。

## 开发与发布

需要 Go 1.26+、CGO、GCC 和 Make：

```sh
make test vet
go test -race -run '^TestReserve' -count=1 ./...
make package VERSION=0.1.0
make checksums VERSION=0.1.0
bash scripts/test-host-integration.sh
```

GitHub Actions 在 PR 时执行格式检查、回归、race 检查、真实宿主动态加载集成测试和双架构原生构建。配置带 required reviewers 的 `release-approval` GitHub Environment。测试完成并得到用户批准后才可以：

```sh
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

当前仍在原 fork 中开发；需先创建并迁移到独立公开发布仓库，才能使用 README 中的商店源。参阅[验证记录和限制](docs/reserve-validation.md)。原 [MIT 许可证](LICENSE)和版权保留；正式再分发前需核对依赖许可证。

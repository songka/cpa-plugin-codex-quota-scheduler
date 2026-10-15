# 额度保护修复自检

日期：2026-10-15。范围：从 `f804342` 到本次 v0.2.2 修改；宿主版本严格限定 CPA v8.0.27。

## 1. 503、重试与账号标记

### 上游路径：已修复

- `state.go::RecordUpstreamCooldown` 在已知 Codex 账号上写入 30 秒冷却及原因；未知账号不创建虚假状态。
- `retry_runtime.go::openNestedStream` 归属绑定账号的开流错误；`runRetryAttempt` 归属握手缓冲期间的流内错误；`recordNestedFailureCooldown` 写入状态并立即发布调度快照。
- `usage.go::HandleUsageFeedback` 处理宿主反馈的真实 AuthID/AuthIndex；本地 `quota_reserve_unavailable` 不误报成上游故障；明确额度耗尽的 429 继续走原有重置冷却。
- `selection.go::ClassifyAccount`、`scheduler.go::accountQueueState`、`scheduler_snapshot.go::accountExclusionReason` 排除冷却账号；管理页显示原因。冷却到期直接恢复资格，不等待下一次刷新。
- `state.go::preserveRequestCooldownLocked` 防止并发配额轮询用旧副本覆盖请求刚记录的冷却，凭据已更换时不继承旧凭据冷却。

与提示词不同：本仓库的 `RetryPlan` 是模型/提供商目标，不是账号数组，也没有 `plan.Attempts`。`RetryAlways` 才会按最大次数重复同模型，每次 nested admission 重新选择安全账号；配置的模型回退链继续按目标运行。不能把未绑定账号的失败猜测成某个账号：关闭严格保护时依赖宿主 usage 归属。

回归：`TestReserveAuditRetryFailoverMarksAndSkipsAccount` 分别覆盖 503/429/529 的 A -> B 成功、下一请求跳过 A、到期恢复、候选去重和并发刷新不擦除标记；`TestReserveAuditHeldStreamFailureMarksAccount` 覆盖流内故障。

### 本地 Admission / after-auth：部分受宿主限制

- `reserve.go::reserveNestedAdmit`、`scheduler_snapshot.go::schedulerPickPublished` 在选号/准入时实时检查预算及并发，池中仍有安全 B 时跳过饱和 A。
- `reserve.go::rejectReserveIntercept` 记录可见保护原因、立即发布快照，并返回 503 + `Retry-After: 1`；此前日志有拒绝而页面无标记的回归仍受现有测试覆盖。
- CPA v8.0.27 `RequestInterceptResponse.Terminate` 是终端响应，没有替换 AuthID 或重新选路字段。因此外部 pinned 账号及选号后发生并发竞争时，同一请求仍可能本地 503。这里没有宣称完全闭环，彻底消除需要宿主支持可重试的后置准入或原子选号预留。

回归：`TestReserveAuditNestedAdmissionSkipsSaturatedAccount`、现有 `TestReservePublishedPickSkipsRejectedAccountAndRecoversWithoutPoll`、`TestReserveDynamicRejectionMarksAccountAndRecovers`。

## 2. 配置持久化与优先级

- `management.go::saveSettingsPayload` 已经是先 `SaveUserData` 成功，再替换 `store/currentConfig`；保存失败返回 HTTP 500，内存不改。本次保留该顺序。
- `dispatch.go::configure` 改为读取历史作为基础，经 `config.go::decodeConfigOnBase` 逐字段应用显式 YAML，然后先保存合并结果、再激活内存。显式 false/0 优先，未传字段、私有通知地址和注释保留；只有 store 元数据的 YAML 不覆盖私有配置。
- 配置或历史读取/写入失败会返回错误，不覆盖损坏原文件；注册不再假装成功。相应更新 `integration_test.go::TestHandleMethodRegisterRejectsInvalidPersistedAnnotations`。
- 文件由 `semanticStatePaths(defaultStatePath()).UserData` 确定，默认是 `os.UserConfigDir()/CLIProxyAPI/codex-quota-reserve/.user-data.json`；Linux 设置 `XDG_CONFIG_HOME=/data/config` 后为 `/data/config/CLIProxyAPI/codex-quota-reserve/.user-data.json`。运行时状态另存 `.runtime-state.json`，旧 `state.json` 只作迁移。
- 插件没有任意宿主配置文件写入接口。页面保存的宿主同步仍通过 CPA `PATCH /v0/management/plugins/codex-quota-reserve/config`，更新 `plugins.configs.codex-quota-reserve`；保留 enabled/store，不同步 Webhook/ChatID 私密值。直接调用插件 PUT `/settings` 只写插件私有数据，API 客户端要自行调用宿主 PATCH。页面对第二步失败显示部分保存错误，可重试。

回归：`TestReserveAuditSettingsDiskAndExplicitYAMLPrecedence` 覆盖 Management 保存、落盘读取、显式 YAML、false/0、元数据重载、空 YAML 重启和私密字段/注释保留；`TestReserveAuditPersistenceFailureKeepsMemory` 覆盖保存失败。实际 CPA 宿主配置 PATCH 由 `tests/integration/reserve_settings_test.go.txt::TestReserveSettingsPatchPersists` 验证。

## 3. 预算、恢复与刷新边界

### 预算

- `reserve.go::admit` 为每次实际尝试收费。宿主复用入站 RequestID，不能把同 ID 视为重复调用直接放行。新尝试先释放旧并发租约，旧预算保留；如果新尝试被保护线拒绝，不留下可误退旧预算的租约。
- `reserve.go::rollbackRejected` 只回滚当前租约的预估和对应学习请求计数；只有观测时间及凭据代次相等才减去预算，防止扣减后来观测的账目。
- `request_complete.go::handleRequestComplete` 只有官方 `RequestCompletionRejected`（执行前拦截终止）退款。成功、失败、取消仅释放并发；上游 503、超时和断线可能已执行，不能假定免费。本插件自己的准入拒绝尚未扣款，本来无需退款。
- 正常完成仍靠新额度观测对账。自动估算的最小/最大阈值和异常样本过滤保持原有实现。

回归：`TestReserveAuditAttemptBudgetAndRefund`、`TestReserveAuditCompletionRefundPolicy`，以及现有并发、预算、自动学习与凭据隔离测试。

### 恢复围栏

提示词提到的 `LastRecoveredAt/obs.ObservedAt` 不是当前仓库实现。实际 `managed_lifecycle.go::recoveryGuardBlocksReserveDisable` 比较 `LastSuccessAt/RecoveredAt`，只防止陈旧缓存再次停用恢复账号，不负责过滤配额写入。

因此没有把 `!After` 机械改成 `Before`：相等时间戳的陈旧耗尽观测确实需要继续拦截。`ApplyQuotaRefreshSuccessIfAdmissionCurrent` 可接受合法相等时间戳配额。

回归：`TestReserveAuditRecoveryObservationAtEqualTimestamp` 验证合法数据接受；现有 `TestManagedQuotaRecoveryGuardIgnoresOldReserveObservation` 验证相等旧值不再停用；`TestManagedQuotaRecoveryGuardReleasesForNewObservationAndCanDisableAgain` 验证更新观测正常释放围栏。

### 刷新间隔

未强行覆盖操作者的 30m 或更长配置。`config.go::reserveRefreshWarning` 在 Reserve 开启且间隔 >5m 时警告，`configure` 记录日志，`buildStatusPayload` 提供 `configuration_warning`，页面警告栏显示。建议 `quota_refresh_interval: 5m` 或更短；主动重置仍可手动触发检查。

回归：`TestReserveAuditRefreshIntervalWarning`。

## 4. 验证与差异

以下均针对最终 v0.2.2 源码执行。Go/Make/GCC 在 Linux Go 1.26 Docker 环境运行，不是声称用户的 Windows 终端已安装这些 CLI。浏览器测试在本机 Edge Chromium 中执行，接口使用测试夹具。

| 验证命令 | 结果 |
| --- | --- |
| `make test vet` | PASS；根包 `ok ... 33.394s`，其余测试包通过，`go vet ./...` 无诊断 |
| `go test -race -run '^TestReserve' -count=1 ./...` | PASS；根包 `ok ... 1.362s`，无竞态报告 |
| `go test -run 'TestReserveAudit\|TestManagedQuotaRecoveryGuard' -count=1 -v ./...` | PASS；全部 10 个审计测试及 2 个既有恢复围栏测试通过 |
| `bash scripts/test-host-integration.sh` | PASS；CPA v8.0.27 实际动态加载 `.so`，识别版本 0.2.2；ABI、选号、固定账号保护、并发、主动重置恢复、设置/历史/导入导出、宿主 PATCH 落盘及 registry schema 均通过 |
| `node tests/integration/browser_settings_save_test.cjs dist/verification/reserve-ui-render.html` | PASS；账号编辑、历史、设置 PUT、宿主 config PATCH、私有字段过滤、部分失败提示和重试通过；无页面异常 |
| 桌面 1440x1000 / 手机 390x844 | 截图已查看；无横向溢出，警告与表单可见，未见文本重叠 |
| `git -c core.whitespace=cr-at-eol -c core.safecrlf=false diff --check` | PASS |

专项结果摘要：

```text
PASS TestReserveAuditRetryFailoverMarksAndSkipsAccount/{503,429,529}
PASS TestReserveAuditHeldStreamFailureMarksAccount
PASS TestReserveAuditNestedAdmissionSkipsSaturatedAccount
PASS TestReserveAuditUsageCooldownDoesNotInventLocalFailure
PASS TestReserveAuditSettingsDiskAndExplicitYAMLPrecedence
PASS TestReserveAuditPersistenceFailureKeepsMemory
PASS TestReserveAuditAttemptBudgetAndRefund
PASS TestReserveAuditRefreshIntervalWarning
PASS TestReserveAuditCompletionRefundPolicy/{rejected,failed,canceled,succeeded}
PASS TestReserveAuditRecoveryObservationAtEqualTimestamp
PASS TestManagedQuotaRecoveryGuardIgnoresOldReserveObservation
PASS TestManagedQuotaRecoveryGuardReleasesForNewObservationAndCanDisableAgain
```

以上 10 个审计测试函数包含多个子用例；测试包会另报告无匹配用例的子包，不表示验证失败。

核心 `git diff` 摘要（另有新增审计测试和两份文档）：

```text
dispatch.go        disk.Config 作为 base；显式 YAML 覆盖；SaveUserData 后激活
state.go           RecordUpstreamCooldown；刷新时保留同凭据请求冷却
retry_runtime.go   绑定账号的开流/握手失败写冷却并发布选号快照
usage.go           真实 usage 失败归属；排除本地 503 和明确额度耗尽
selection.go       ClassifyAccount 排除未到期冷却
scheduler*.go      队列原因及状态快照投影
reserve.go         重试独立收费；rollbackRejected 仅退款最后租约
request_complete.go 仅 RequestCompletionRejected 调用预算回滚
config.go          按基础配置解码；刷新间隔警告；版本 0.2.2
management.go      configuration_warning；上游冷却显示文案
```

发布计划：使用现有 GitHub Actions 的 `release-approval` 流程发布 v0.2.2，保留原插件库 URL，不绕过宿主或仓库的保护规则。公开 Release 和远程安装验证以发布后的实际结果为准。

核心差异：配置合并/保存；账号短期冷却及选号过滤；最后一次执行前拒绝的预算回滚；警告展示；专项回归及版本元数据。工作区原有大量 CRLF 差异和 `.serena/` 不属于本次功能修改，不会整批提交或回滚。

没有连接生产 CPA、实际 OAuth 账号或真实 Webhook；离线模拟和真实宿主测试不能替代部署后的真实业务验收。Serena MCP 连接关闭，本次未声称完成 Serena 分析。

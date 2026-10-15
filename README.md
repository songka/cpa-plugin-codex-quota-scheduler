# Codex Quota Reserve

Independent CPA dynamic plugin `codex-quota-reserve`, targeting **CLIProxyAPI v8.0.27**, based on Jeffery's MIT Codex Quota Scheduler v0.3.2. Release version **0.2.2**, distributed from the existing public repository `songka/cpa-plugin-codex-quota-scheduler`.

[中文安装与回滚手册](README.zh-CN.md) · [Upstream documentation](docs/upstream/README.md)

## v0.2.2 Highlights

- Attributed transient upstream failures receive a 30-second account cooldown. Nested strict-reserve retries switch to another safe account, and subsequent selection skips the failed account until expiry.
- Explicit host YAML fields override persisted settings individually; omitted fields, private Webhook settings and account annotations survive. Persistence failures are reported before activating settings.
- Only confirmed pre-execution rejections refund their last admitted attempt. Actual retries still consume separate estimates; ambiguous 503/transport failures are not assumed free.
- Reserve refresh intervals above five minutes show a management warning. The recovery fence retains stale-observation protection, including equal timestamps.
- See [the itemized audit](docs/reserve-audit-2026-10-15.md) for tests and the remaining CPA after-auth rerouting limitation.

## v0.2.1 Highlights

- Quota-cycle reset and history events no longer send Webhook notifications by themselves. Notifications are limited to actual scheduling participation transitions and scheduled usage reports; recovery after an active reset sends one start notification only after fresh quota and CPA admission both confirm participation.
- Settings saved in the plugin UI are synchronized back to `plugins.configs.codex-quota-reserve` through the CPA management API. `enabled`, `store`, Webhook URLs, Chat IDs and their configured flags are preserved or kept private.

- Optional global and per-account five-hour and weekly/monthly reserve minimums.
- Stop line = minimum + safety margin; defaults are 10% + 2%. Equality stops allocation.
- Strict terminal scheduler rejection using the actual v8 SDK, plus an after-auth guard for pinned execution. Nested Codex retry streams acquire a separate lease and pin a safe account because the host skips the originating plugin's interceptors.
- Per-account concurrency limit and cumulative estimated unobserved consumption. Completion releases concurrency without refunding estimated quota. A fresh quota observation reconciles the estimate.
- Embedded settings and account override controls; quota history for 5h/24h/7d, default 30-day retention with a 10,000-point cap per account, and reset/protection/recovery markers.
- Optional CPA-level disable at confirmed reserve thresholds. Ownership and credential fingerprints prevent recovery of manually disabled or rotated credentials. Fresh quota after an active reset restores owned accounts; manual refresh checks recovery immediately.
- Automatic per-request estimates learn from clean quota observations, using up to eight samples, at least three valid samples and a median with 25% headroom. Configurable bounds default to 0.01%-2%; resets, rotations and anomalous jumps are excluded.
- In-plugin Webhook URL and Telegram Chat ID settings, scheduled usage reports and scheduling start/stop notifications. WeCom/DingTalk Markdown, Feishu cards, Telegram HTML and generic JSON show both quota windows, reset times, active-reset credits and credit expiry. Secrets are omitted from settings responses and exports.
- Existing scheduling, credential lifecycle, reset probes, annotations, managed disable/recovery and retry functions retained. Reserve protection is off by default.

Reserve percentages describe upstream subscription windows, not NewAPI tokens or dollars. **They are not an exact hard quota guarantee.** In-flight work can consume more than its estimated budget; choose safety margin, concurrency and refresh interval for your workload. A saturated selected account can cause a local 503 if simultaneous selection races with admission; protected requests never reach upstream through the tested guard.

## Build and test

Go 1.26+, CGO, GCC and Make are required. Linux glibc is required; these shared libraries are not musl/Alpine builds.

```sh
make test vet
# Meaningful reserve concurrency checks:
go test -race -run '^TestReserve' -count=1 ./...
make package VERSION=0.2.2
make checksums VERSION=0.2.2
bash scripts/test-host-integration.sh
```

The integration runner checks out the exact v8.0.27 commit, loads the actual `.so` into the actual host loader and uses simulated OAuth quota responses. It does not send real model requests or modify production CPA. It verifies settings/history/import/export, quota parsing, selection, terminal rejection and after-auth admission. The host's own Manager terminal rejection tests run separately. See [validation and limits](docs/reserve-validation.md) for coverage boundaries.

Native AMD64 and ARM64 GitHub Actions runners produce `codex-quota-reserve_0.2.2_linux_<arch>.zip` and SHA256 files. For cross compilation, set `CC` to an ARM64 C compiler; packaging always runs its Go helper on the native platform.

## Configuration

```yaml
reserve_enabled: false
reserve_disable_accounts: false
reserve_5h_percent: 10
reserve_long_percent: 10
reserve_strict: true
reserve_fail_closed: true
reserve_safety_margin_percent: 2
reserve_max_in_flight: 1
reserve_request_budget_percent: 0.25
reserve_request_budget_auto: true
reserve_request_budget_min_percent: 0.01
reserve_request_budget_max_percent: 2
reserve_warning_percent: 20
quota_refresh_interval: 5m
history_enabled: true
history_retention_days: 30
notifications_enabled: false
notification_kind: generic
usage_report_enabled: false
usage_report_interval_minutes: 60
```

The original 30-minute refresh default is retained for compatibility; explicitly select 5 minutes when enabling reserve protection. While reserve is enabled, the monitor wakes the authoritative roster and refreshes periodically even without traffic. Existing concurrency controls, backoffs and credential checks remain authoritative. Unknown or stale quota never becomes 100%. Expired window deadlines do not prove recovery: a newly observed usable window is required.

Account annotations accept `reserve_override: {"five_hour": 15, "long_window": 20}`. Null/omitted window minimums inherit global policy; zero is a valid explicit override. They are written only to plugin user data, never OAuth tokens.

Enable `reserve_disable_accounts` explicitly to also write CPA's top-level `disabled` flag; without it, protection excludes the account from this scheduler but does not change CPA's enabled state. Only fresh observed threshold violations authorize a write, not stale data or estimated consumption. Owned disabled accounts remain visible and are checked each quota refresh interval, even with no traffic. An active reset can restore them before the previous reset deadline; use manual refresh to request an immediate check. Both quota windows must be usable and above the reserve stop line. OAuth credential fields are preserved.

Automatic estimates stay inside the configured bounds (`0 < min <= max <= 10%`) including the fallback estimate before enough samples exist. Disable `reserve_request_budget_auto` to use the fixed estimate. Each upstream attempt consumes another estimate; completion does not refund it. These estimates are not exact billing or hard quota guarantees.

Set Webhook URL directly in the plugin's settings and use **Send test notification**. Telegram also needs Chat ID. Saved secrets remain in private plugin user data, so protect its mounted directory and backups. Empty UI fields preserve existing values; use the explicit clear checkbox to remove one. Redacted export/import preserves existing secrets. `RESERVE_WEBHOOK_URL` and `RESERVE_TELEGRAM_CHAT_ID` remain fallbacks when no saved value exists. HTTPS is required except for loopback test receivers.

Enable notifications and scheduled usage reports to send a report every configured number of minutes (default 60, range 1-10080). Only transitions into or out of scheduling participation and due reports send automatically; quota-window reset/history events remain in history and logs without sending a separate message. Participation transitions are deduplicated in memory, including active-reset recovery. A bounded asynchronous worker handles best-effort delivery without delaying quota protection; provider error acknowledgements are treated as failures and redirects are refused. Signed DingTalk endpoints require an external signing gateway; native signature generation is not implemented. No delivery guarantee is claimed.

With strict reserve enabled, retry targets with an empty provider use a pinned safe Codex account. Set the provider explicitly for Antigravity or other non-Codex fallback targets. Hosts must supply `selected_auth_id` for accurate after-auth attribution. Disable the old scheduler before enabling this one. Do not run two Codex scheduler plugins together.

## Store and release

`registry.json` uses the official store's actual schema 1: GitHub Release discovery supplies versions, platform assets and checksums. It deliberately does not advertise nonexistent assets or guessed hashes. The existing public repository `songka/cpa-plugin-codex-quota-scheduler` is the selected publication destination. Installation requires its approved release and matching platform assets:

```
https://raw.githubusercontent.com/songka/cpa-plugin-codex-quota-scheduler/main/registry.json
```

Refresh the CPA plugin store after publication. CPA v8.0.27 caches successful latest-release lookups for one hour; an already-running instance may keep showing the previous version until that cache expires or CPA restarts.

Set up GitHub Environment `release-approval` with required reviewers. Tags run full validation and both native builds before the release job. No tag or release should be created before explicit user approval. After approval only:

```sh
# Approval and passing host/platform tests are prerequisites.
git tag -a v0.2.2 -m "v0.2.2"
git push origin v0.2.2
```

The repository name is retained; the plugin ID and archive basename remain `codex-quota-reserve`, so the plugin keeps an independent runtime identity.

## License

The original [MIT license](LICENSE) and Jeffery's copyright are retained. Source attribution and upstream feature documentation are retained under `docs/upstream/`. The original scheduler incorporates MIT work from doer-ee, dos1989 and Siriussee; their upstream notices remain in the archived documentation. Runtime dependencies include CLIProxyAPI (MIT) and yaml.v3 (MIT/Apache-2.0); review dependency license files when redistributing an approved binary.

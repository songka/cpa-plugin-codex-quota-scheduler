# Codex Quota Reserve

Independent CPA dynamic plugin `codex-quota-reserve`, targeting **CLIProxyAPI v8.0.27**, based on Jeffery's MIT Codex Quota Scheduler v0.3.2. Development version **0.1.0**; no stable release has been approved or published.

[中文安装与回滚手册](README.zh-CN.md) · [Upstream documentation](docs/upstream/README.md)

## v0.1.0 Highlights

- Optional global and per-account five-hour and weekly/monthly reserve minimums.
- Stop line = minimum + safety margin; defaults are 10% + 2%. Equality stops allocation.
- Strict terminal scheduler rejection using the actual v8 SDK, plus an after-auth guard for pinned execution. Nested Codex retry streams acquire a separate lease and pin a safe account because the host skips the originating plugin's interceptors.
- Per-account concurrency limit and cumulative estimated unobserved consumption. Completion releases concurrency without refunding estimated quota. A fresh quota observation reconciles the estimate.
- Embedded settings and account override controls; quota history for 5h/24h/7d, default 30-day retention with a 10,000-point cap per account, and reset/protection/recovery markers.
- Optional generic, WeCom, Feishu, DingTalk and Telegram webhook payloads. Notifications are off by default; URLs remain in process environment rather than exported plugin configuration.
- Existing scheduling, credential lifecycle, reset probes, annotations, managed disable/recovery and retry functions retained. Reserve protection is off by default.

Reserve percentages describe upstream subscription windows, not NewAPI tokens or dollars. **They are not an exact hard quota guarantee.** In-flight work can consume more than its estimated budget; choose safety margin, concurrency and refresh interval for your workload. A saturated selected account can cause a local 503 if simultaneous selection races with admission; protected requests never reach upstream through the tested guard.

## Build and test

Go 1.26+, CGO, GCC and Make are required. Linux glibc is required; these shared libraries are not musl/Alpine builds.

```sh
make test vet
# Meaningful reserve concurrency checks:
go test -race -run '^TestReserve' -count=1 ./...
make package VERSION=0.1.0
make checksums VERSION=0.1.0
bash scripts/test-host-integration.sh
```

The integration runner checks out the exact v8.0.27 commit, loads the actual `.so` into the actual host loader and uses simulated OAuth quota responses. It does not send real model requests or modify production CPA. It verifies settings/history/import/export, quota parsing, selection, terminal rejection and after-auth admission. The host's own Manager terminal rejection tests run separately. See [validation and limits](docs/reserve-validation.md) for coverage boundaries.

Native AMD64 and ARM64 GitHub Actions runners produce `codex-quota-reserve_0.1.0_linux_<arch>.zip` and SHA256 files. For cross compilation, set `CC` to an ARM64 C compiler; packaging always runs its Go helper on the native platform.

## Configuration

```yaml
reserve_enabled: false
reserve_5h_percent: 10
reserve_long_percent: 10
reserve_strict: true
reserve_fail_closed: true
reserve_safety_margin_percent: 2
reserve_max_in_flight: 1
reserve_request_budget_percent: 0.25
reserve_warning_percent: 20
quota_refresh_interval: 5m
history_enabled: true
history_retention_days: 30
notifications_enabled: false
notification_kind: generic
```

The original 30-minute refresh default is retained for compatibility; explicitly select 5 minutes when enabling reserve protection. While reserve is enabled, the monitor wakes the authoritative roster and refreshes periodically even without traffic. Existing concurrency controls, backoffs and credential checks remain authoritative. Unknown or stale quota never becomes 100%. Expired window deadlines do not prove recovery: a newly observed usable window is required.

Account annotations accept `reserve_override: {"five_hour": 15, "long_window": 20}`. Null/omitted window minimums inherit global policy; zero is a valid explicit override. They are written only to plugin user data, never OAuth tokens.

Configure `RESERVE_WEBHOOK_URL` before starting CPA; Telegram also needs `RESERVE_TELEGRAM_CHAT_ID`. Treat webhook URLs containing bot tokens as secrets. Signed DingTalk webhook endpoints require a compatible external signing gateway; native signature generation is not implemented. Failed delivery is logged without endpoint values; event transitions are deduplicated by retained history, and no delivery guarantee is claimed.

With strict reserve enabled, retry targets with an empty provider use a pinned safe Codex account. Set the provider explicitly for Antigravity or other non-Codex fallback targets. Hosts must supply `selected_auth_id` for accurate after-auth attribution. Disable the old scheduler before enabling this one. Do not run two Codex scheduler plugins together.

## Store and release

`registry.json` uses the official store's actual schema 1: GitHub Release discovery supplies versions, platform assets and checksums. It deliberately does not advertise nonexistent assets or guessed hashes. The destination `songka/cpa-plugin-codex-quota-reserve` must exist publicly and contain the approved release before this source is installable:

```
https://raw.githubusercontent.com/songka/cpa-plugin-codex-quota-reserve/main/registry.json
```

Set up GitHub Environment `release-approval` with required reviewers. Tags run full validation and both native builds before the release job. No tag or release should be created before explicit user approval. After approval only:

```sh
# Approval and passing host/platform tests are prerequisites.
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The current development repository remains the existing scheduler fork until the independent destination is created. Publishing a tag on that repository is not the final distribution workflow.

## License

The original [MIT license](LICENSE) and Jeffery's copyright are retained. Source attribution and upstream feature documentation are retained under `docs/upstream/`. The original scheduler incorporates MIT work from doer-ee, dos1989 and Siriussee; their upstream notices remain in the archived documentation. Runtime dependencies include CLIProxyAPI (MIT) and yaml.v3 (MIT/Apache-2.0); review dependency license files when redistributing an approved binary.

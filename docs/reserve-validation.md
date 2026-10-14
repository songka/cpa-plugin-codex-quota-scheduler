# Codex Quota Reserve validation and remaining limits

Development branch: `feat/codex-quota-reserve-v0.1.0`. No production CPA has been changed, no plugin has been enabled in production, and no stable tag or Release has been published.

## Compatibility conclusion

The starting fork and latest upstream scheduler both resolve to `f27aa969ec726a5cc01bdbfcd588cae49306f452` (v0.3.2). The supplied requirements mention an experimental ZIP, but that ZIP was not attached. No claim is made that its patches have been inspected.

CPA v8.0.27 is pinned to `bf9341629ef2300ac9d846a2e80fe2756f63a671`, module `/v8`, plugin ABI 1 and RPC schema 6. Its SDK supplies `Handled + Reject + RejectCode + RejectReason`. The actual host normalizes this as a terminal selection rejection; its actual auth Manager does not delegate that response to builtin schedulers. This implementation uses that response, not a fabricated `Fallback: false` setting.

The host skips the calling plugin's request interceptors and lifecycle notifications on nested model execution. Strict nested Codex retries therefore acquire their own safe-account lease, pin `AuthID` in the host execution request, and retain the lease until the nested stream is closed. The normal after-auth guard additionally prevents unsafe pinned execution and accounts for per-account concurrency and estimated consumption.

## Validation performed

- Go 1.26.9 installed with the official archive SHA256 intact; module checksums and `go mod verify` passed.
- Original baseline: 1,078 passing test results.
- Final existing and new suites: 1,093 passing test results; no failed or skipped results. Includes subtest results, not a count of top-level functions.
- `go vet ./...` and `git diff --check` passed.
- `go test -race -run '^TestReserve' -count=1 ./...` passed. Covers threshold equality, unknown/stale data, NaN rejection, account overrides, strict selection, recovery, 100-way concurrent admission, unobserved budgets, nested stream lease retention, legacy defaults, persistence, history retention and UI controls.
- Exact CPA v8.0.27 dynamically loaded the actual `.so`, parsed simulated OAuth quota, selected a safe lower-priority account and rejected protected requests. The actual Manager, configured with Session Affinity and the actual dynamically loaded scheduler, selected the safe account despite an affinity binding to the protected account, then returned `quota_reserve_unavailable` when all accounts were protected.
- Host management settings, history, configuration export and re-import worked. Pinned after-auth requests were blocked at HTTP 503; a second simultaneous request on the same safe account was rejected until request completion released concurrency.
- The host's own terminal Manager rejection suite passed, including explicit/default reject codes and rejection precedence.
- `registry.json` passed the exact host's `ParseRegistry` validator as schema 1 with GitHub Release discovery.
- Headless Chromium exercised settings initialization, account override editor, history curve, current stop line and event markers with simulated management responses; no JavaScript errors were observed.
- A network-disabled Docker container ran the actual v8.0.27 dynamic-host integration test binary and actual plugin. A second container process using the same mounted data checked that `.user-data.json` and `quota-history.json` existed and reran the integration checks.
- Linux AMD64 and ARM64 c-shared builds and ZIP packaging completed, including original license and dependency notices. ZIP SHA256 values and architecture identifiers were checked.

## Reproduce

```sh
make test vet
go test -race -run '^TestReserve' -count=1 ./...
bash scripts/test-host-integration.sh
# Optional Chromium / Playwright smoke check:
RESERVE_HTML_OUTPUT=/tmp/reserve-ui-render.html go test -run '^TestReserveManagementHTMLControls$' -count=1
python tests/integration/browser_smoke.py /tmp/reserve-ui-render.html
```

The integration runner uses a temporary host checkout or `CPA_HOST_CHECKOUT`, checks the exact tag SHA, removes only its own temporary fixtures, and injects `XDG_CONFIG_HOME` before starting the test process. Dynamic libraries have their own Go runtime environment view; setting a variable only inside the host's already-running Go test is not a reliable injection mechanism.

## Known limits and unfinished external steps

- `songka/cpa-plugin-codex-quota-reserve` is not yet created. GitHub GraphQL and REST both rejected creation with `Resource not accessible by integration` (REST 403). Existing repository read/push access works. Create the new public repository or grant the integration permission, then migrate the reviewed source before publishing store URLs.
- Stable Release v0.1.0 requires the user's explicit approval after acceptance. No tag has been created. The release job is behind GitHub Environment `release-approval`; configure required reviewers before using it.
- Quota percentages are cached upstream estimates. Neither safety margin nor per-request estimate guarantees an exact remaining percentage. Completion does not refund unobserved consumption, and retries consume another estimate. Simultaneous selection can still race at admission and return a local 503 rather than consume a protected account.
- An empty provider in a strict nested retry is interpreted as Codex. Explicitly name Antigravity or other non-Codex providers in mixed retry chains. Live NewAPI/Antigravity production traffic has not been exercised because production credentials were not supplied.
- The after-auth guard depends on host credential attribution. Tested host routes provide `selected_auth_id`; a future/new executor path must be integration-tested before relying on it.
- A disabled/unloaded/fused plugin cannot enforce reserve protection. CPA intentionally falls back when a plugin panics or is fused. Strict protection applies while the plugin is loaded and healthy; do not describe it as a host-wide invariant under plugin failure.
- ARM64 was cross-built and packaged, but no native ARM64 CPA loading run has been performed in this AMD64 environment. The GitHub Actions ARM runner performs native builds; full native ARM runtime acceptance remains a deployment check.
- Production hot replacement has not been comprehensively tested. Use restart-based upgrade/rollback and preserve the mounted data. Alpine/musl and other operating systems are outside the requested Linux artifacts.
- Webhook payload formats were exercised with fixtures; actual external deliveries were not sent. Signed DingTalk endpoints require a signing gateway. Notifications are best-effort, off by default, and have no delivery guarantee.
- History is saved about once per minute and at graceful shutdown; an abrupt crash can lose the last unsaved samples. Turning history off retains only a current state point for transition deduplication rather than collecting a full curve.
- Final GitHub Actions execution and release-asset installation/upgrade cannot be claimed before those remote operations occur. Schema validation is not an install of a nonexistent release.

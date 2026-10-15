package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func reserveAuditGlobals(t *testing.T) {
	t.Helper()
	cleanupIntegrationGlobals(t)
	oldLedger, oldPath, oldCallback := globalReserveLedger, defaultStatePath, callHostCallback
	oldSnapshot := publishedSchedulerSnapshot.Load()
	globalReserveLedger = newReserveLedger()
	path := filepath.Join(t.TempDir(), "state.json")
	defaultStatePath = func() string { return path }
	t.Cleanup(func() {
		globalReserveLedger, defaultStatePath, callHostCallback = oldLedger, oldPath, oldCallback
		publishedSchedulerSnapshot.Store(oldSnapshot)
	})
}

func reserveAuditAccount(t *testing.T, id string, now time.Time) AccountState {
	t.Helper()
	for _, a := range globalState.Snapshot(now).Accounts {
		if a.AuthID == id {
			return a
		}
	}
	t.Fatalf("missing account %s", id)
	return AccountState{}
}

func TestReserveAuditRetryFailoverMarksAndSkipsAccount(t *testing.T) {
	for _, status := range []int{503, 429, 529} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			reserveAuditGlobals(t)
			now := time.Now()
			a, cfg := reserveFixture(now, 50, 50)
			b := a
			b.AuthID = "b"
			cfg.RetryEnabled, cfg.RetryAlways = true, true
			currentConfig.Store(cfg)
			globalState = NewPluginState(cfg)
			globalState.UpsertQuota(a)
			globalState.UpsertQuota(b)
			globalState.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{"a": {}, "b": {}}})
			callHostCallback = func(string, any) (json.RawMessage, error) {
				return json.RawMessage(`{"files":[{"id":"a","provider":"codex","auth_index":"idx-a"},{"id":"b","provider":"codex","auth_index":"idx-b"}]}`), nil
			}
			host := installFakeRetryHost(t)
			host.push(fakeRetryAttempt{status: status})
			host.push(fakeRetryAttempt{chunks: retryFrames(retryDeltaFrame, retryDoneFrame)})
			runRetryChain(context.Background(), retryTestRequest("model"), "plugin-stream")
			requests, _, closeErr, closed := host.snapshot()
			if len(requests) != 2 || requests[0].AuthID != "a" || requests[1].AuthID != "b" || !closed || closeErr != "" {
				t.Fatalf("failover: requests=%+v closed=%v error=%q", requests, closed, closeErr)
			}
			failed := reserveAuditAccount(t, "a", now)
			if !failed.UpstreamCooldownUntil.After(now) {
				t.Fatal("failed auth not globally cooled down")
			}
			req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
				{ID: "a", Provider: "codex"}, {ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"},
			}}
			if decision := schedulerPickPublished(req, time.Now()); decision.AuthID != "b" {
				t.Fatalf("next request selected %+v", decision)
			}
			if ordered := BuildOrderedAccounts(req, globalState.Snapshot(now), now); len(ordered) != 2 || ordered[1].UnavailableReason != "upstream_cooldown" {
				t.Fatalf("status/candidate dedup: %+v", ordered)
			}
			// Recovery is deadline-driven, without waiting for another quota poll.
			after := failed.UpstreamCooldownUntil.Add(time.Second)
			if decision := schedulerPickPublished(req, after); decision.AuthID != "a" {
				t.Fatalf("expired cooldown did not recover: %+v", decision)
			}
			// An in-flight quota poll must not erase the newer request marker.
			if !globalState.ApplyQuotaRefreshSuccessIfAdmissionCurrent(a, globalState.Snapshot(now).CPAAdmissionVersion, now) {
				t.Fatal("refresh unexpectedly rejected")
			}
			if !reserveAuditAccount(t, "a", now).UpstreamCooldownUntil.Equal(failed.UpstreamCooldownUntil) {
				t.Fatal("quota refresh erased cooldown")
			}
		})
	}
}

func TestReserveAuditHeldStreamFailureMarksAccount(t *testing.T) {
	reserveAuditGlobals(t)
	now := time.Now()
	a, cfg := reserveFixture(now, 50, 50)
	globalState = NewPluginState(cfg)
	globalState.UpsertQuota(a)
	callHostCallback = func(string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"files":[{"id":"a","provider":"codex","auth_index":"idx-a"}]}`), nil
	}
	host := installFakeRetryHost(t)
	host.push(fakeRetryAttempt{chunks: retryFrames(retryCapacityFrame)})
	result := runRetryAttempt(context.Background(), cfg, retryTestRequest("model").ExecutorRequest, "", retryAttemptState{}, protocolCodex, "plugin-stream", now.Add(time.Minute))
	if !result.retryable || !reserveAuditAccount(t, "a", now).UpstreamCooldownUntil.After(now) {
		t.Fatalf("held stream failure was not attributed: %+v", result)
	}
}

func TestReserveAuditNestedAdmissionSkipsSaturatedAccount(t *testing.T) {
	reserveAuditGlobals(t)
	now := time.Now()
	a, cfg := reserveFixture(now, 50, 50)
	b := a
	b.AuthID = "b"
	globalState = NewPluginState(cfg)
	globalState.UpsertQuota(a)
	globalState.UpsertQuota(b)
	callHostCallback = func(string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"files":[{"id":"a","provider":"codex","auth_index":"idx-a"},{"id":"b","provider":"codex","auth_index":"idx-b"}]}`), nil
	}
	if _, ok := globalReserveLedger.admit("busy", a, cfg, now); !ok {
		t.Fatal("setup admission failed")
	}
	auth, lease, err := reserveNestedAdmit(cfg, "codex")
	if err != nil || auth != "b" {
		t.Fatalf("saturated A did not switch to B: %q, %v", auth, err)
	}
	globalReserveLedger.release(lease)
}

func TestReserveAuditUsageCooldownDoesNotInventLocalFailure(t *testing.T) {
	reserveAuditGlobals(t)
	now := time.Now()
	a, cfg := reserveFixture(now, 50, 50)
	a.AuthIndex = "idx-a"
	globalState = NewPluginState(cfg)
	globalState.UpsertQuota(a)
	local := pluginapi.UsageRecord{Provider: "codex", AuthID: "a", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 503, Body: `{"error":{"code":"quota_reserve_unavailable"}}`}}
	HandleUsageFeedback(globalState, local, now)
	if !reserveAuditAccount(t, "a", now).UpstreamCooldownUntil.IsZero() {
		t.Fatal("local admission rejection was treated as upstream overload")
	}
	local.AuthID, local.AuthIndex, local.Failure.Body = "", "idx-a", "service unavailable"
	HandleUsageFeedback(globalState, local, now)
	if !reserveAuditAccount(t, "a", now).UpstreamCooldownUntil.After(now) {
		t.Fatal("usage failure by auth index was not marked")
	}
	local.AuthIndex = "unknown"
	HandleUsageFeedback(globalState, local, now)
	if len(globalState.Snapshot(now).Accounts) != 1 {
		t.Fatal("unattributed usage created phantom account")
	}
}

func TestReserveAuditSettingsDiskAndExplicitYAMLPrecedence(t *testing.T) {
	reserveAuditGlobals(t)
	cfg := DefaultConfig()
	cfg.ReserveEnabled = true
	cfg.WebhookURL = "https://example.invalid/private-hook"
	globalState = NewPluginState(cfg)
	globalState.SetAnnotations(AnnotationState{Accounts: map[string]AccountAnnotation{"auth:a": {Alias: "keep"}}})
	settings := SettingsFromConfig(cfg)
	settings.QuotaRefreshInterval = "2m"
	if resp := saveSettingsPayload(globalState, settings); resp.StatusCode != 200 {
		t.Fatalf("management save: %+v", resp)
	}
	disk, loaded, err := loadUserData(semanticStatePaths(defaultStatePath()).UserData)
	if err != nil || !loaded || disk.Config.QuotaRefreshInterval != 2*time.Minute {
		t.Fatalf("not persisted: %+v, %v", disk, err)
	}
	if err := configure(lifecyclePayload(t, "reserve_enabled: false\nreserve_request_budget_percent: 0\nquota_refresh_interval: 45s\n")); err != nil {
		t.Fatal(err)
	}
	got := activeConfig()
	if got.ReserveEnabled || got.ReserveRequestBudgetPercent != 0 || got.QuotaRefreshInterval != 45*time.Second || got.WebhookURL != cfg.WebhookURL {
		t.Fatalf("explicit false/zero or omitted secret precedence: %+v", got)
	}
	if err := configure(lifecyclePayload(t, "store:\n  version: 0.2.2\n")); err != nil {
		t.Fatal(err)
	}
	if err := configure(lifecyclePayload(t, "")); err != nil {
		t.Fatal(err)
	}
	disk, _, err = loadUserData(semanticStatePaths(defaultStatePath()).UserData)
	if err != nil || disk.Config.QuotaRefreshInterval != 45*time.Second || disk.Config.ReserveEnabled ||
		disk.Accounts["auth:a"].Alias != "keep" || disk.Config.WebhookURL != cfg.WebhookURL {
		t.Fatalf("reload lost merged settings/annotations: %+v, %v", disk, err)
	}
}

func TestReserveAuditPersistenceFailureKeepsMemory(t *testing.T) {
	reserveAuditGlobals(t)
	cfg := DefaultConfig()
	currentConfig.Store(cfg)
	globalState = NewPluginState(cfg)
	// A file cannot be the parent directory of either persistent state file.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	defaultStatePath = func() string { return filepath.Join(blocker, "state.json") }
	settings := SettingsFromConfig(cfg)
	settings.QuotaRefreshInterval = "45s"
	if resp := saveSettingsPayload(globalState, settings); resp.StatusCode != 500 {
		t.Fatalf("save failure not reported: %+v", resp)
	}
	if err := configure(lifecyclePayload(t, "quota_refresh_interval: 45s\n")); err == nil {
		t.Fatal("configure ignored disk failure")
	}
	if activeConfig().QuotaRefreshInterval != cfg.QuotaRefreshInterval || globalState.Config().QuotaRefreshInterval != cfg.QuotaRefreshInterval {
		t.Fatal("disk failure changed runtime config")
	}
}

func TestReserveAuditAttemptBudgetAndRefund(t *testing.T) {
	now := time.Now()
	a, cfg := reserveFixture(now, 50, 50)
	l := newReserveLedger()
	first, ok := l.admit("one", a, cfg, now)
	if !ok {
		t.Fatal("first admission failed")
	}
	again, ok := l.admit("one", a, cfg, now)
	if !ok || again.EstimatedUnobserved != 2*first.RequestBudget || again.InFlight != 1 {
		t.Fatalf("retry must charge a new attempt but retain one concurrency slot: %+v", again)
	}
	l.rollbackRejected("one")
	l.rollbackRejected("one")
	if l.used[a.AuthID].charged != first.RequestBudget || l.counts[a.AuthID] != 0 || l.learning[a.AuthID].requests != 1 {
		t.Fatal("rejection did not refund only the latest attempt exactly once")
	}
	l.admit("two", a, cfg, now)
	l.release("two")
	if l.used[a.AuthID].charged != 2*first.RequestBudget {
		t.Fatal("ambiguous failure was incorrectly refunded")
	}
	l.admit("old", a, cfg, now)
	fresh := a
	fresh.LastSuccessAt = now.Add(time.Second)
	cfg.ReserveMaxInFlight = 4
	l.admit("new", fresh, cfg, fresh.LastSuccessAt)
	before := l.used[a.AuthID].charged
	l.rollbackRejected("old")
	if l.used[a.AuthID].charged != before {
		t.Fatal("old rejection refunded a newer observation's charges")
	}
	l = newReserveLedger()
	a, cfg = reserveFixture(now, 13, 50)
	cfg.ReserveRequestBudgetPercent = 0.5
	if _, ok := l.admit("retry", a, cfg, now); !ok {
		t.Fatal("initial attempt failed")
	}
	if _, ok := l.admit("retry", a, cfg, now); ok {
		t.Fatal("retry should hit the stop line")
	}
	l.rollbackRejected("retry")
	if l.used[a.AuthID].charged != 0.5 || l.counts[a.AuthID] != 0 {
		t.Fatal("rejected retry refunded its already executed predecessor")
	}
}

func TestReserveAuditRefreshIntervalWarning(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReserveEnabled = true
	if reserveRefreshWarning(cfg) == "" {
		t.Fatal("legacy 30m interval is silent")
	}
	if BuildStatusPayload(NewPluginState(cfg).Snapshot(time.Now()), nil).ConfigurationWarning == "" {
		t.Fatal("refresh warning missing from management status")
	}
	cfg.QuotaRefreshInterval = 5 * time.Minute
	if reserveRefreshWarning(cfg) != "" {
		t.Fatal("reasonable interval warned")
	}
	cfg.ReserveEnabled, cfg.QuotaRefreshInterval = false, time.Hour
	if reserveRefreshWarning(cfg) != "" {
		t.Fatal("disabled reserve warned")
	}
}

func TestReserveAuditCompletionRefundPolicy(t *testing.T) {
	for _, outcome := range []pluginapi.RequestCompletionOutcome{
		pluginapi.RequestCompletionRejected, pluginapi.RequestCompletionFailed,
		pluginapi.RequestCompletionCanceled, pluginapi.RequestCompletionSucceeded,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			reserveAuditGlobals(t)
			now := time.Now()
			a, cfg := reserveFixture(now, 50, 50)
			globalState = NewPluginState(cfg)
			charged, ok := globalReserveLedger.admit("complete", a, cfg, now)
			if !ok {
				t.Fatal("initial admission failed")
			}
			raw, _ := json.Marshal(pluginapi.RequestCompletion{RequestID: "complete", Outcome: outcome})
			if _, err := handleRequestComplete(raw); err != nil {
				t.Fatal(err)
			}
			expected := charged.RequestBudget
			if outcome == pluginapi.RequestCompletionRejected {
				expected = 0
			}
			if globalReserveLedger.used["a"].charged != expected || globalReserveLedger.counts["a"] != 0 {
				t.Fatalf("incorrect completion refund for %s", outcome)
			}
		})
	}
}

func TestReserveAuditRecoveryObservationAtEqualTimestamp(t *testing.T) {
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableManagedQuotaDisable, cfg.ReserveEnabled, cfg.ReserveDisableAccounts = true, true, true
	r, host, store := managedDisableTestRefresher(t, now, cfg)
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{"team": {}}})
	if err := r.ApplyManagedQuotaDisable(context.Background(), QuotaFailureEvent{
		AuthID: "team", AuthIndex: "idx-team", ResetAt: now, Reason: usageLimitReachedReason,
	}); err != nil {
		t.Fatal(err)
	}
	host.authJSON["idx-team"] = host.saved["idx-team.json"]
	host.httpBody = []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`)
	if err := r.requestManagedRecovery("team"); err != nil {
		t.Fatal(err)
	}
	if err := r.RecoverManagedQuotaAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	guard, ok := persisted.ManagedRecoveryGuards["idx-team"]
	if !ok || !guard.RecoveredAt.Equal(now) {
		t.Fatal("test did not exercise equal recovery timestamp")
	}
	a, _ := reserveFixture(now, 100, 95)
	a.AuthID, a.AuthIndex = "team", "idx-team"
	if !store.ApplyQuotaRefreshSuccessIfAdmissionCurrent(a, store.Snapshot(now).CPAAdmissionVersion, now) {
		t.Fatal("legitimate equal-time observation rejected")
	}
	for _, account := range store.Snapshot(now).Accounts {
		if account.AuthID == "team" && (account.LastSuccessAt != now || evaluateReserve(account, cfg, now).Blocked) {
			t.Fatalf("valid equal-time quota was discarded: %+v", account)
		}
	}
}

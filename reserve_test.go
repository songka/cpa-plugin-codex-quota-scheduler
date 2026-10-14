package main

import (
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func reserveFixture(now time.Time, five, long float64) (AccountState, Config) {
	cfg := DefaultConfig()
	cfg.ReserveEnabled = true
	return AccountState{AuthID: "a", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: now, Quota: ParsedQuota{FiveHour: &QuotaWindow{UsedPercent: reserveFloat(100 - five), ResetAt: now.Add(5 * time.Hour)}, LongWindow: &QuotaWindow{UsedPercent: reserveFloat(100 - long), ResetAt: now.Add(7 * 24 * time.Hour)}}}, cfg
}
func reserveFloat(v float64) *float64 { return &v }
func TestReserveThresholds(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name       string
		five, long float64
		reason     string
	}{{"normal", 50, 50, "normal"}, {"boundary", 12, 50, "reserve_five_hour"}, {"weekly", 50, 9, "reserve_long_window"}, {"multi", 12, 12, "reserve_multi_window"}, {"zero", 0, 0, "upstream_quota_exhausted"}} {
		t.Run(tc.name, func(t *testing.T) {
			a, cfg := reserveFixture(now, tc.five, tc.long)
			got := evaluateReserve(a, cfg, now)
			if got.Reason != tc.reason {
				t.Fatalf("got %s want %s", got.Reason, tc.reason)
			}
			if a.TemporaryExhausted || a.Quota.FiveHour.Exhausted {
				t.Fatal("reserve changed exhaustion state")
			}
		})
	}
}
func TestReserveUnknownStaleAndResetEvidence(t *testing.T) {
	now := time.Now()
	a, cfg := reserveFixture(now, 50, 50)
	a.LastSuccessAt = now.Add(-cfg.QuotaRefreshInterval - time.Second)
	if got := evaluateReserve(a, cfg, now); got.Reason != "reserve_stale" {
		t.Fatal(got)
	}
	cfg.ReserveFailClosed = false
	if evaluateReserve(a, cfg, now).Blocked {
		t.Fatal("fail-open stale")
	}
	cfg.ReserveFailClosed = true
	a.LastSuccessAt = now
	a.Quota.LongWindow.UsedPercent = reserveFloat(math.NaN())
	if evaluateReserve(a, cfg, now).Reason != "reserve_unknown" {
		t.Fatal("NaN treated as quota")
	}
	a, cfg = reserveFixture(now, 50, 50)
	a.Quota.FiveHour = nil
	if evaluateReserve(a, cfg, now).Blocked {
		t.Fatal("optional window")
	}
	a, cfg = reserveFixture(now, 50, 50)
	a.Quota.FiveHour.ResetAt = now.Add(-time.Second)
	if evaluateReserve(a, cfg, now).Reason != "reserve_waiting_reset" {
		t.Fatal("deadline is not evidence")
	}
	a.Quota.FiveHour.ResetAt = now.Add(5 * time.Hour)
	if evaluateReserve(a, cfg, now).Blocked {
		t.Fatal("new observed window did not recover")
	}
}
func TestReserveOverrideAndConfigValidation(t *testing.T) {
	now := time.Now()
	a, cfg := reserveFixture(now, 25, 50)
	a.Annotation.ReserveOverride = &ReserveOverride{FiveHour: reserveFloat(30)}
	if evaluateReserve(a, cfg, now).Reason != "reserve_five_hour" {
		t.Fatal("override ignored")
	}
	a.Annotation.ReserveOverride.FiveHour = reserveFloat(0)
	if evaluateReserve(a, cfg, now).Blocked {
		t.Fatal("zero override ignored")
	}
	for _, input := range []string{"reserve_5h_percent: -1", "reserve_long_percent: 101", "reserve_5h_percent: .nan", "reserve_safety_margin_percent: 99", "reserve_max_in_flight: 0", "history_retention_days: 0"} {
		if _, err := DecodeConfig([]byte(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	parsed, err := DecodeConfig([]byte("reserve_enabled: true\nreserve_safety_margin_percent: 0\nreserve_5h_percent: 0\n"))
	if err != nil || !parsed.ReserveStrict || !parsed.ReserveFailClosed || parsed.ReserveSafetyMarginPercent != 0 {
		t.Fatalf("%+v %v", parsed, err)
	}
	old, err := DecodeConfig([]byte("monthly_mode: priority\n"))
	if err != nil || old.ReserveEnabled || old.Reserve5HPercent != 10 {
		t.Fatalf("old config: %+v %v", old, err)
	}
	settings := SettingsFromConfig(parsed)
	back, err := ConfigFromSettings(DefaultConfig(), settings)
	if err != nil || back.ReservePolicy != parsed.ReservePolicy {
		t.Fatalf("settings roundtrip: %v", err)
	}
}
func TestReserveSelectionStrictRejectionAndRecovery(t *testing.T) {
	now := time.Now()
	a, cfg := reserveFixture(now, 12, 50)
	b := cloneAccountState(a)
	b.AuthID = "b"
	b.Quota.FiveHour.UsedPercent = reserveFloat(20)
	snap := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{a, b}, CPAAdmission: CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{"a": {}, "b": {}}}}
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex", Priority: 10}, {ID: "b", Provider: "codex", Priority: 0}}}
	if got := PickCodexAccount(req, snap, now); got.AuthID != "b" {
		t.Fatalf("did not switch: %+v", got)
	}
	snap.Accounts[1].Quota.FiveHour.UsedPercent = reserveFloat(90)
	got := PickCodexAccount(req, snap, now)
	if !got.Handled || !got.Reject || got.DelegateBuiltin != "" || got.RejectCode != "quota_reserve_unavailable" {
		t.Fatalf("fallback bypass: %+v", got)
	}
	old := publishedSchedulerSnapshot.Load()
	defer publishedSchedulerSnapshot.Store(old)
	PublishSchedulerSnapshot(schedulerSnapshotFromState(snap, nil))
	if got := schedulerPickPublished(req, now); !got.Reject {
		t.Fatalf("published bypass: %+v", got)
	}
	snap.Accounts[0].Quota.FiveHour.UsedPercent = reserveFloat(20)
	snap.Accounts[0].Quota.FiveHour.ResetAt = now.Add(6 * time.Hour)
	if PickCodexAccount(req, snap, now).AuthID != "a" {
		t.Fatal("did not recover")
	}
	snap.Config.ReserveEnabled = false
	if PickCodexAccount(req, snap, now).Reject {
		t.Fatal("disabled protection still rejects")
	}
}
func TestReserveConcurrentAdmissionAndUnobservedBudget(t *testing.T) {
	now := time.Now()
	a, cfg := reserveFixture(now, 50, 50)
	ledger := newReserveLedger()
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := time.Unix(int64(i), 0).String()
			if _, ok := ledger.admit(id, a, cfg, now); ok {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("admitted %d concurrent requests", accepted.Load())
	}
	ledger = newReserveLedger()
	a, cfg = reserveFixture(now, 13, 50)
	cfg.ReserveRequestBudgetPercent = 0.5
	if _, ok := ledger.admit("1", a, cfg, now); !ok {
		t.Fatal("first budget")
	}
	ledger.release("1")
	if _, ok := ledger.admit("2", a, cfg, now); ok {
		t.Fatal("completion incorrectly refunded unobserved consumption")
	}
	a.LastSuccessAt = now.Add(time.Second)
	if _, ok := ledger.admit("2", a, cfg, now.Add(time.Second)); !ok {
		t.Fatal("fresh read did not reconcile budget")
	}
	if _, ok := ledger.admit("2", a, cfg, now.Add(time.Second)); ok {
		t.Fatal("a repeated upstream attempt must consume another estimated budget")
	}
}
func TestReserveHistoryPersistenceRetentionAndDeduplication(t *testing.T) {
	now := time.Now()
	a, cfg := reserveFixture(now, 50, 50)
	h := newQuotaHistory()
	snap := StateSnapshot{Accounts: []AccountState{a}, Config: cfg, Now: now}
	h.collect(snap)
	h.collect(snap)
	if len(h.snapshot()["a"]) != 1 {
		t.Fatal("duplicate samples")
	}
	snap.Accounts[0].Quota.FiveHour.UsedPercent = reserveFloat(90)
	snap.Accounts[0].LastSuccessAt = now.Add(time.Second)
	snap.Now = now.Add(time.Second)
	events := h.collect(snap)
	if len(events) == 0 || events[0]["event"] != "protection" {
		t.Fatalf("events %+v", events)
	}
	path := filepath.Join(t.TempDir(), "quota-history.json")
	if err := h.save(path); err != nil {
		t.Fatal(err)
	}
	restored := newQuotaHistory()
	if err := restored.load(path); err != nil {
		t.Fatal(err)
	}
	if len(restored.snapshot()["a"]) != 2 {
		t.Fatal("restart lost history")
	}
	snap.Now = now.Add(31 * 24 * time.Hour)
	snap.Accounts = nil
	restored.collect(snap)
	if len(restored.snapshot()) != 0 {
		t.Fatal("retention not enforced")
	}
}
func TestReservePersistenceAndAnnotationIsolation(t *testing.T) {
	now := time.Now()
	a, cfg := reserveFixture(now, 50, 50)
	a.Annotation.ReserveOverride = &ReserveOverride{FiveHour: reserveFloat(15)}
	store := NewPluginState(cfg)
	store.UpsertQuota(a)
	store.SetAnnotations(AnnotationState{Accounts: map[string]AccountAnnotation{"auth:a": a.Annotation}})
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SavePluginDiskState(path, diskStateFromStore(store)); err != nil {
		t.Fatal(err)
	}
	disk, err := LoadPluginDiskState(path)
	if err != nil || disk.Config.ReservePolicy != cfg.ReservePolicy || *disk.Accounts["auth:a"].ReserveOverride.FiveHour != 15 {
		t.Fatalf("state roundtrip %+v %v", disk, err)
	}
	clone := cloneAccountAnnotation(a.Annotation)
	*clone.ReserveOverride.FiveHour = 25
	if *a.Annotation.ReserveOverride.FiveHour != 15 {
		t.Fatal("annotation alias")
	}
	for _, kind := range []string{"generic", "wecom", "feishu", "dingtalk", "telegram"} {
		if raw, err := json.Marshal(notificationBody(kind, "test")); err != nil || len(raw) == 0 {
			t.Fatalf("notification %s", kind)
		}
	}
}

func TestReserveNestedStreamPinsSafeAccountAndRetainsLease(t *testing.T) {
	cleanupIntegrationGlobals(t)
	previousLedger := globalReserveLedger
	globalReserveLedger = newReserveLedger()
	t.Cleanup(func() { globalReserveLedger = previousLedger })
	previousCallback := callHostCallback
	previousModel := callHostModel
	t.Cleanup(func() { callHostCallback = previousCallback; callHostModel = previousModel })
	now := time.Now()
	a, cfg := reserveFixture(now, 50, 50)
	globalState = NewPluginState(cfg)
	globalState.UpsertQuota(a)
	callHostCallback = func(method string, payload any) (json.RawMessage, error) {
		return json.RawMessage(`{"files":[{"id":"a","provider":"codex","auth_index":"idx-a"}]}`), nil
	}
	calls := 0
	callHostModel = func(method string, payload any) (json.RawMessage, error) {
		calls++
		if calls == 1 {
			req, ok := payload.(hostModelExecutionRequest)
			if !ok || req.AuthID != "a" {
				t.Fatalf("nested request not pinned: %+v", payload)
			}
			return json.RawMessage(`{"stream_id":"test-nested","status_code":200}`), nil
		}
		return json.RawMessage(`{}`), nil
	}
	stream, _, ok := openNestedStream(cfg, pluginapi.ExecutorRequest{}, "callback", retryAttemptState{}, "openai-response")
	if !ok || stream != "test-nested" {
		t.Fatal("nested open failed")
	}
	if _, ok := globalReserveLedger.admit("parallel", a, cfg, now); ok {
		t.Fatal("nested lease released before stream completion")
	}
	closeNestedStream(stream)
	if _, ok := globalReserveLedger.admit("parallel", a, cfg, now); !ok {
		t.Fatal("nested lease not released")
	}
}

func TestReserveLegacyJSONGetsNewDefaults(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"HandleEnabled":true,"MonthlyMode":"priority","QuotaRefreshInterval":300000000000}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ReserveEnabled || !cfg.ReserveStrict || !cfg.ReserveFailClosed || cfg.Reserve5HPercent != 10 || cfg.ReserveMaxInFlight != 1 || !cfg.HistoryEnabled {
		t.Fatalf("defaults lost: %+v", cfg)
	}
}

func TestReserveManagementHTMLControls(t *testing.T) {
	now := time.Now()
	a, cfg := reserveFixture(now, 50, 50)
	store := NewPluginState(cfg)
	store.UpsertQuota(a)
	payload := buildCurrentStatusPayload(store, now)
	html := RenderStatusHTML(payload)
	for _, id := range []string{"reserveEnabled", "reserve5H", "reserveLong", "reserveMargin", "editReserve5H", "historyChart", "historyLoad", "reserveNotify"} {
		if !strings.Contains(string(html), `id="`+id+`"`) {
			t.Fatalf("missing control %s", id)
		}
	}
	if path := os.Getenv("RESERVE_HTML_OUTPUT"); path != "" {
		if err := os.WriteFile(path, html, 0600); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(payload)
		if err := os.WriteFile(path+".json", raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

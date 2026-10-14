package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func managedDisableTestRefresher(t *testing.T, now time.Time, cfg Config) (*QuotaRefresher, *fakeHostClient, *PluginState) {
	t.Helper()
	if cfg.LifecycleEventLimit == 0 {
		cfg.LifecycleEventLimit = 50
	}
	token := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-team"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "team", AuthIndex: "idx-team", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-team": json.RawMessage(`{"access_token":"access-team","refresh_token":"refresh-team","id_token":"` + token + `"}`),
		},
	}
	store := NewPluginState(cfg)
	store.UpsertQuota(AccountState{AuthID: "team", AuthIndex: "idx-team", Provider: "codex", LastSuccessAt: now})
	r := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	r.runtimeStore = NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	return r, host, store
}

func TestManagedQuotaDisableRecordsOwnershipAndWritesHost(t *testing.T) {
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableManagedQuotaDisable = true
	r, host, _ := managedDisableTestRefresher(t, now, cfg)

	if err := r.ApplyManagedQuotaDisable(context.Background(), QuotaFailureEvent{
		AuthID: "team", AuthIndex: "idx-team", ResetAt: now.Add(3 * time.Hour), Reason: usageLimitReachedReason,
	}); err != nil {
		t.Fatal(err)
	}
	saved := string(host.saved["idx-team.json"])
	if !strings.Contains(saved, `"disabled":true`) {
		t.Fatalf("host auth not disabled: %s", saved)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	record, ok := persisted.ManagedLifecycle["idx-team"]
	if !ok || !record.DisableApplied {
		t.Fatalf("ownership record = %#v", record)
	}
	if !record.Fingerprint.ValidHashes() {
		t.Fatal("ownership record missing credential fingerprint")
	}
}

func TestManagedQuotaDisableNeverAdoptsManualDisable(t *testing.T) {
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableManagedQuotaDisable = true
	r, host, _ := managedDisableTestRefresher(t, now, cfg)
	// The operator disabled this account manually.
	host.authJSON["idx-team"] = json.RawMessage(`{"access_token":"access-team","refresh_token":"refresh-team","disabled":true}`)

	if err := r.ApplyManagedQuotaDisable(context.Background(), QuotaFailureEvent{
		AuthID: "team", AuthIndex: "idx-team", ResetAt: now.Add(3 * time.Hour), Reason: usageLimitReachedReason,
	}); err != nil {
		t.Fatal(err)
	}
	persisted, _ := r.runtimeStore.PersistentSnapshot()
	if _, adopted := persisted.ManagedLifecycle["idx-team"]; adopted {
		t.Fatal("manual disable was adopted by the plugin")
	}
	if _, wrote := host.saved["idx-team.json"]; wrote {
		t.Fatal("plugin wrote a manually disabled auth file")
	}
}

func TestManagedQuotaRecoveryEnablesOnlyOnUsableQuota(t *testing.T) {
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableManagedQuotaDisable = true
	r, host, _ := managedDisableTestRefresher(t, now, cfg)

	if err := r.ApplyManagedQuotaDisable(context.Background(), QuotaFailureEvent{
		AuthID: "team", AuthIndex: "idx-team", ResetAt: now, Reason: usageLimitReachedReason,
	}); err != nil {
		t.Fatal(err)
	}
	// The host now carries the disabled credential; simulate its shape.
	host.authJSON["idx-team"] = host.saved["idx-team.json"]
	disabledJSON := append(json.RawMessage(nil), host.saved["idx-team.json"]...)
	if err := r.requestManagedRecovery("team"); err != nil {
		t.Fatal(err)
	}

	// Still exhausted: recovery must keep the account disabled.
	host.httpBody = []byte(`{"rate_limit":{"primary_window":{"used_percent":100,"limit_reached":true,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`)
	if err := r.RecoverManagedQuotaAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _ := r.runtimeStore.PersistentSnapshot()
	if _, exists := persisted.ManagedLifecycle["idx-team"]; !exists {
		t.Fatal("recovery dropped ownership while quota was still exhausted")
	}
	if string(host.saved["idx-team.json"]) != string(disabledJSON) {
		t.Fatal("recovery re-enabled an exhausted account")
	}

	// Recovered quota in both windows: the account returns to CPA once the
	// next scheduled check is due.
	host.httpBody = []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`)
	if _, err := r.runtimeStore.Update(func(state *PersistentState) error {
		if record, ok := state.ManagedLifecycle["idx-team"]; ok {
			record.NextQuotaCheckAt = now
			state.ManagedLifecycle["idx-team"] = record
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.RecoverManagedQuotaAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _ = r.runtimeStore.PersistentSnapshot()
	if _, still := persisted.ManagedLifecycle["idx-team"]; still {
		t.Fatal("ownership record survived a verified recovery")
	}
	if !strings.Contains(string(host.saved["idx-team.json"]), `"disabled":false`) {
		t.Fatalf("account not re-enabled: %s", host.saved["idx-team.json"])
	}
}

func TestManagedQuotaRecoveryRefusesRotatedCredential(t *testing.T) {
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableManagedQuotaDisable = true
	r, host, _ := managedDisableTestRefresher(t, now, cfg)

	if err := r.ApplyManagedQuotaDisable(context.Background(), QuotaFailureEvent{
		AuthID: "team", AuthIndex: "idx-team", ResetAt: now, Reason: usageLimitReachedReason,
	}); err != nil {
		t.Fatal(err)
	}
	// A different credential now lives under the same auth file name.
	token := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-other"})
	host.authJSON["idx-team"] = json.RawMessage(`{"access_token":"access-other","refresh_token":"refresh-other","id_token":"` + token + `","disabled":true}`)
	host.httpBody = []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`)

	if err := r.requestManagedRecovery("team"); err != nil {
		t.Fatal(err)
	}
	if err := r.RecoverManagedQuotaAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _ := r.runtimeStore.PersistentSnapshot()
	if _, still := persisted.ManagedLifecycle["idx-team"]; still {
		t.Fatal("ownership record survived a credential rotation")
	}
	saved, written := host.saved["idx-team.json"]
	if written && strings.Contains(string(saved), `"disabled":false`) {
		t.Fatal("recovery enabled a rotated credential it never disabled")
	}
}

func TestManagedPlannedRecordReconcilesAfterCrash(t *testing.T) {
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableManagedQuotaDisable = true
	r, _, _ := managedDisableTestRefresher(t, now, cfg)

	// A planned record whose host write never happened (crash between the
	// ownership persist and SaveAuth).
	if _, err := r.runtimeStore.Update(func(state *PersistentState) error {
		state.ManagedLifecycle["idx-team"] = ManagedDisableRecord{DisabledAt: now.Add(-time.Hour), NextQuotaCheckAt: now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.RecoverManagedQuotaAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _ := r.runtimeStore.PersistentSnapshot()
	if _, still := persisted.ManagedLifecycle["idx-team"]; still {
		t.Fatal("stale planned record was not reconciled away")
	}
}

func TestManagedQuotaRecoveryKeepsAccountWhenFiveHourStillExhausted(t *testing.T) {
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableManagedQuotaDisable = true
	r, host, _ := managedDisableTestRefresher(t, now, cfg)

	if err := r.ApplyManagedQuotaDisable(context.Background(), QuotaFailureEvent{
		AuthID: "team", AuthIndex: "idx-team", ResetAt: now, Reason: usageLimitReachedReason,
	}); err != nil {
		t.Fatal(err)
	}
	host.authJSON["idx-team"] = host.saved["idx-team.json"]
	if err := r.requestManagedRecovery("team"); err != nil {
		t.Fatal(err)
	}
	// The long window recovered but the five-hour window is still exhausted:
	// the original fork would have re-enabled here; the hardened evidence
	// requires both windows usable.
	host.httpBody = []byte(`{"rate_limit":{"primary_window":{"used_percent":100,"limit_reached":true,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`)
	if err := r.RecoverManagedQuotaAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _ := r.runtimeStore.PersistentSnapshot()
	if _, exists := persisted.ManagedLifecycle["idx-team"]; !exists {
		t.Fatal("recovery enabled an account whose five-hour window is still exhausted")
	}
}

func TestManagedQuotaRecoveryGuardIgnoresOldReserveObservation(t *testing.T) {
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableManagedQuotaDisable = true
	cfg.ReserveEnabled = true
	cfg.ReserveDisableAccounts = true
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
	host.authJSON["idx-team"] = host.saved["idx-team.json"]
	if _, err := r.runtimeStore.PersistentSnapshot(); err != nil {
		t.Fatal(err)
	}

	// The old refresh result still says the account is exhausted. It must not
	// immediately disable the credential that recovery just enabled.
	exhausted := 100.0
	store.UpsertQuota(AccountState{
		AuthID:        "team",
		AuthIndex:     "idx-team",
		Provider:      "codex",
		LastSuccessAt: now,
		Quota: ParsedQuota{
			FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &exhausted, ResetAt: now.Add(time.Hour)},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: reserveFloat(5), ResetAt: now.Add(24 * time.Hour)},
		},
	})
	if err := r.ApplyReserveQuotaDisables(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _ := r.runtimeStore.PersistentSnapshot()
	if _, exists := persisted.ManagedLifecycle["idx-team"]; exists {
		t.Fatal("old quota observation re-disabled the recovered account")
	}
	if !strings.Contains(string(host.saved["idx-team.json"]), `"disabled":false`) {
		t.Fatalf("recovery write was overwritten: %s", host.saved["idx-team.json"])
	}
}

func TestManagedQuotaRecoveryGuardReleasesForNewObservationAndCanDisableAgain(t *testing.T) {
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableManagedQuotaDisable = true
	cfg.ReserveEnabled = true
	cfg.ReserveDisableAccounts = true
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
	host.authJSON["idx-team"] = host.saved["idx-team.json"]

	later := now.Add(time.Minute)
	r.now = func() time.Time { return later }
	exhausted := 100.0
	store.UpsertQuota(AccountState{
		AuthID:        "team",
		AuthIndex:     "idx-team",
		Provider:      "codex",
		LastSuccessAt: later,
		Quota: ParsedQuota{
			FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &exhausted, ResetAt: later.Add(time.Hour)},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: reserveFloat(5), ResetAt: later.Add(24 * time.Hour)},
		},
	})
	if err := r.ApplyReserveQuotaDisables(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _ := r.runtimeStore.PersistentSnapshot()
	if _, exists := persisted.ManagedRecoveryGuards["idx-team"]; exists {
		t.Fatal("new quota observation did not release recovery guard")
	}
	if record, exists := persisted.ManagedLifecycle["idx-team"]; !exists || !record.DisableApplied {
		t.Fatalf("new exhausted observation did not re-disable account: %#v", record)
	}
	if !strings.Contains(string(host.saved["idx-team.json"]), `"disabled":true`) {
		t.Fatalf("account was not disabled again: %s", host.saved["idx-team.json"])
	}
}

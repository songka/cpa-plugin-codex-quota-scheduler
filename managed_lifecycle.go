package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Managed quota disable-and-recovery, ported from dos1989's
// feature/managed-quota-recovery (MIT) and hardened:
//
//   - every ownership record carries the credential fingerprint computed from
//     the exact auth JSON that was disabled, and recovery refuses to enable a
//     different credential that happens to live under the same auth file name;
//   - recovery evidence must show BOTH windows usable (the original design
//     promised this; the fork only checked the long window);
//   - planned records that crashed before their host write are reconciled on
//     the next recovery pass instead of leaking forever;
//   - the feature is opt-in (enable_managed_quota_disable, default false).
//
// Safety invariants: a pre-existing disabled flag without plugin ownership is
// manual operator state and is never touched; recovery never enables from
// elapsed time alone — only from a fresh, usable quota read.

// LifecycleEvent is an auditable, bounded history entry for an account the
// plugin disabled after a confirmed upstream quota failure.
type LifecycleEvent struct {
	Kind   string    `json:"kind"`
	At     time.Time `json:"at"`
	Detail string    `json:"detail,omitempty"`
}

// ManagedDisableRecord is the durable ownership record. Its presence means the
// plugin, rather than an operator, owns the corresponding disabled flag.
type ManagedDisableRecord struct {
	AuthID           string                  `json:"auth_id,omitempty"`
	Reason           string                  `json:"reason,omitempty"`
	ReserveProtected bool                    `json:"reserve_protected,omitempty"`
	Account          *ManagedAccountSnapshot `json:"account,omitempty"`
	DisabledAt       time.Time               `json:"disabled_at"`
	DisableApplied   bool                    `json:"disable_applied"`
	Fingerprint      CredentialFingerprint   `json:"fingerprint,omitempty"`
	Last429At        time.Time               `json:"last_429_at,omitempty"`
	ResetAt          time.Time               `json:"reset_at,omitempty"`
	LastQuotaCheckAt time.Time               `json:"last_quota_check_at,omitempty"`
	NextQuotaCheckAt time.Time               `json:"next_quota_check_at,omitempty"`
	Events           []LifecycleEvent        `json:"events,omitempty"`
}

// Keep non-secret quota evidence after CPA removes a disabled auth from admission.
type ManagedAccountSnapshot struct {
	AuthID     string            `json:"auth_id"`
	Annotation AccountAnnotation `json:"annotation"`
	Quota      ParsedQuota       `json:"quota"`
	ObservedAt time.Time         `json:"observed_at"`
}

// managedAuthFingerprint fingerprints the credential that would be disabled or
// enabled, so ownership cannot leak across credential rotation under the same
// auth file name.
func managedAuthFingerprint(authIndex string, credentials CodexCredentials) CredentialFingerprint {
	return NewCredentialFingerprint(credentials.ChatGPTAccountID, credentials.RefreshToken, authIndex)
}

// ApplyManagedQuotaDisable records plugin ownership before changing CPA auth
// metadata, then sets the top-level disabled flag on the auth file. A
// pre-existing disabled flag without plugin ownership is manual operator state
// and is deliberately left untouched.
func (r *QuotaRefresher) ApplyManagedQuotaDisable(ctx context.Context, event QuotaFailureEvent) error {
	if r == nil || r.host == nil || r.runtimeStore == nil || !r.state.Config().EnableManagedQuotaDisable || event.AuthIndex == "" {
		return nil
	}
	r.managedMu.Lock()
	defer r.managedMu.Unlock()
	return r.applyManagedDisable(ctx, event, nil)
}

// ApplyReserveQuotaDisables runs outside the scheduler hot path. Unknown,
// stale, transient and estimated quota never authorize host credential writes.
func (r *QuotaRefresher) ApplyReserveQuotaDisables(ctx context.Context) error {
	if r == nil || r.host == nil || r.runtimeStore == nil || r.state == nil {
		return nil
	}
	r.managedMu.Lock()
	defer r.managedMu.Unlock()
	snap := r.state.Snapshot(r.now())
	if !snap.Config.ReserveEnabled || !snap.Config.ReserveDisableAccounts {
		return nil
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	var failures []error
	for _, a := range snap.Accounts {
		if a.Provider != "codex" || a.AuthIndex == "" || !snap.CPAAdmission.Observed {
			continue
		}
		if _, admitted := snap.CPAAdmission.AuthIDs[a.AuthID]; !admitted {
			continue
		}
		if _, owned := persisted.ManagedLifecycle[a.AuthIndex]; owned {
			continue
		}
		if a.LastSuccessAt.IsZero() || a.LastSuccessAt.After(snap.Now) || a.Stale || snap.Now.Sub(a.LastSuccessAt) > snap.Config.QuotaRefreshInterval {
			continue
		}
		status := evaluateReserve(a, snap.Config, snap.Now)
		switch status.Reason {
		case "reserve_five_hour", "reserve_long_window", "reserve_multi_window", "upstream_quota_exhausted":
			if err := r.applyManagedDisable(ctx, QuotaFailureEvent{AuthID: a.AuthID, AuthIndex: a.AuthIndex, Reason: status.Reason}, &a); err != nil {
				// One inaccessible credential must not prevent protecting other accounts.
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (r *QuotaRefresher) applyManagedDisable(ctx context.Context, event QuotaFailureEvent, reserveAccount *AccountState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	response, err := r.host.GetAuth(event.AuthIndex)
	if err != nil {
		return err
	}
	if !json.Valid(response.JSON) {
		return errors.New("managed quota disable requires valid auth JSON")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(response.JSON, &raw); err != nil {
		return err
	}
	var disabled bool
	if value, ok := raw["disabled"]; ok {
		_ = json.Unmarshal(value, &disabled)
	}
	credentials, credErr := ExtractCodexCredentials(response.JSON)
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	record, owned := persisted.ManagedLifecycle[event.AuthIndex]
	if disabled && !owned {
		return nil
	}
	if disabled && owned && record.DisableApplied {
		return nil
	}
	if owned && credErr == nil && record.Fingerprint.ValidHashes() && record.Fingerprint != managedAuthFingerprint(event.AuthIndex, credentials) {
		// The credential rotated after our disable: drop ownership instead of
		// writing a flag the operator may not want for the new credential.
		_, err := r.runtimeStore.Update(func(state *PersistentState) error {
			delete(state.ManagedLifecycle, event.AuthIndex)
			return nil
		})
		return err
	}
	now := r.now()
	if reserveAccount != nil {
		// Reject a stale observation from a credential that has since rotated.
		if credErr != nil || !managedAuthFingerprint(event.AuthIndex, credentials).ValidHashes() {
			return errors.New("reserve disable requires credential identity")
		}
		if reserveAccount.CredentialFingerprint.ValidHashes() && reserveAccount.CredentialFingerprint != managedAuthFingerprint(event.AuthIndex, credentials) {
			return errors.New("reserve disable credential changed")
		}
		latest := r.state.Snapshot(now)
		if !latest.Config.ReserveEnabled || !latest.Config.ReserveDisableAccounts {
			return nil
		}
		current := false
		for _, a := range latest.Accounts {
			if a.AuthID == reserveAccount.AuthID && a.AuthIndex == event.AuthIndex && a.LastSuccessAt.Equal(reserveAccount.LastSuccessAt) {
				_, current = latest.CPAAdmission.AuthIDs[a.AuthID]
				reason := evaluateReserve(a, latest.Config, now).Reason
				current = current && latest.CPAAdmission.Observed && !a.Stale && !a.LastSuccessAt.After(now) &&
					now.Sub(a.LastSuccessAt) <= latest.Config.QuotaRefreshInterval &&
					(reason == "reserve_five_hour" || reason == "reserve_long_window" || reason == "reserve_multi_window" || reason == "upstream_quota_exhausted")
				break
			}
		}
		if !current {
			return nil
		}
	}
	var fingerprint CredentialFingerprint
	if credErr == nil {
		fingerprint = managedAuthFingerprint(event.AuthIndex, credentials)
	} else if owned {
		fingerprint = record.Fingerprint
	}
	if _, err := r.runtimeStore.Update(func(state *PersistentState) error {
		record := state.ManagedLifecycle[event.AuthIndex]
		if record.DisabledAt.IsZero() {
			record.DisabledAt = now
		}
		record.AuthID = event.AuthID
		record.Reason = event.Reason
		if record.Account == nil {
			for _, a := range r.state.Snapshot(now).Accounts {
				if a.AuthID == event.AuthID {
					record.Account = &ManagedAccountSnapshot{AuthID: a.AuthID, Annotation: cloneAccountAnnotation(a.Annotation), Quota: cloneParsedQuota(a.Quota), ObservedAt: a.LastSuccessAt}
					break
				}
			}
		}
		if reserveAccount == nil {
			record.Last429At = now
		} else {
			record.ReserveProtected = true
			record.Account = &ManagedAccountSnapshot{AuthID: reserveAccount.AuthID, Annotation: cloneAccountAnnotation(reserveAccount.Annotation), Quota: cloneParsedQuota(reserveAccount.Quota), ObservedAt: reserveAccount.LastSuccessAt}
		}
		record.ResetAt = event.ResetAt
		record.NextQuotaCheckAt = now.Add(r.state.Config().QuotaRefreshInterval)
		if event.ResetAt.After(now) && event.ResetAt.Before(record.NextQuotaCheckAt) {
			record.NextQuotaCheckAt = event.ResetAt
		}
		if fingerprint.ValidHashes() {
			record.Fingerprint = fingerprint
		}
		kind := "quota_429"
		if reserveAccount != nil {
			kind = "reserve_threshold"
		}
		record = appendLifecycleEvent(record, LifecycleEvent{Kind: kind, At: now, Detail: event.Reason}, r.state.Config().LifecycleEventLimit)
		state.ManagedLifecycle[event.AuthIndex] = record
		return nil
	}); err != nil {
		return err
	}
	if disabled {
		return nil
	}
	current, err := r.host.GetAuth(event.AuthIndex)
	if err != nil {
		return err
	}
	if !bytes.Equal(current.JSON, response.JSON) {
		return errors.New("managed disable auth changed before write")
	}
	raw["disabled"] = json.RawMessage("true")
	next, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	name := response.Name
	if name == "" {
		name = event.AuthIndex
	}
	if err := r.host.SaveAuth(name, next); err != nil {
		return err
	}
	_, err = r.runtimeStore.Update(func(state *PersistentState) error {
		record := state.ManagedLifecycle[event.AuthIndex]
		record.DisableApplied = true
		record = appendLifecycleEvent(record, LifecycleEvent{Kind: "disabled", At: r.now()}, r.state.Config().LifecycleEventLimit)
		state.ManagedLifecycle[event.AuthIndex] = record
		return nil
	})
	if err == nil {
		r.state.RecordLog("info", "managed.disabled", "已在 CPA 层停用账号", map[string]any{"auth_id": event.AuthID, "auth_index": event.AuthIndex, "reason": event.Reason}, now)
		snapshot := snapshotWithManagedAccounts(r.state.Snapshot(now), r)
		r.state.queueQuotaNotification(notificationForEvent(snapshot, "cpa_disabled", event.AuthID, event.Reason))
		r.wakeRefreshLoop()
	}
	return err
}

// RecoverManagedQuotaAccounts reconciles planned records, then checks only
// accounts with a durable applied ownership record. Operators keep their
// manually disabled accounts; recovered quota accounts return to CPA
// automatically.
func (r *QuotaRefresher) RecoverManagedQuotaAccounts(ctx context.Context) error {
	if r == nil || r.host == nil || r.runtimeStore == nil {
		return nil
	}
	r.managedMu.Lock()
	defer r.managedMu.Unlock()
	if err := r.reconcilePlannedManagedRecords(ctx); err != nil {
		return err
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	indices := make([]string, 0, len(persisted.ManagedLifecycle))
	for index, record := range persisted.ManagedLifecycle {
		if record.DisableApplied && (record.NextQuotaCheckAt.IsZero() || !record.NextQuotaCheckAt.After(r.now())) {
			indices = append(indices, index)
		}
	}
	sort.Strings(indices)
	var failures []error
	for _, index := range indices {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.recoverManagedQuotaAccount(ctx, index); err != nil {
			failures = append(failures, err)
			if retryErr := r.recordManagedQuotaCheck(index, r.now()); retryErr != nil {
				failures = append(failures, retryErr)
			}
		}
	}
	return errors.Join(failures...)
}

// reconcilePlannedManagedRecords settles ownership records whose host write
// never confirmed: if the auth file is not disabled the plan is stale and the
// record is dropped; if it is disabled the write evidently happened and the
// record is completed. Without this, a crash between persisting the plan and
// the host write would leak a ghost record forever.
func (r *QuotaRefresher) reconcilePlannedManagedRecords(ctx context.Context) error {
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	for index, record := range persisted.ManagedLifecycle {
		if record.DisableApplied || record.NextQuotaCheckAt.After(r.now()) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		response, err := r.host.GetAuth(index)
		if err != nil {
			if err := r.recordManagedQuotaCheck(index, r.now()); err != nil {
				return err
			}
			continue
		}
		var raw map[string]json.RawMessage
		if !json.Valid(response.JSON) || json.Unmarshal(response.JSON, &raw) != nil {
			if err := r.recordManagedQuotaCheck(index, r.now()); err != nil {
				return err
			}
			continue
		}
		var disabled bool
		if value, ok := raw["disabled"]; ok {
			_ = json.Unmarshal(value, &disabled)
		}
		credentials, credentialErr := ExtractCodexCredentials(response.JSON)
		sameCredential := credentialErr == nil && record.Fingerprint.ValidHashes() && record.Fingerprint == managedAuthFingerprint(index, credentials)
		_, updateErr := r.runtimeStore.Update(func(state *PersistentState) error {
			record, ok := state.ManagedLifecycle[index]
			if !ok {
				return nil
			}
			if disabled && sameCredential {
				record.DisableApplied = true
				record = appendLifecycleEvent(record, LifecycleEvent{Kind: "disable_confirmed", At: r.now()}, r.state.Config().LifecycleEventLimit)
			} else {
				delete(state.ManagedLifecycle, index)
				return nil
			}
			state.ManagedLifecycle[index] = record
			return nil
		})
		if updateErr != nil {
			return updateErr
		}
	}
	return nil
}

// managedRecoveryDeadline is the earliest time the recovery lane must run.
func (r *QuotaRefresher) managedRecoveryDeadline(now time.Time) time.Time {
	if r == nil || r.runtimeStore == nil {
		return time.Time{}
	}
	var deadline time.Time
	for _, record := range r.runtimeStore.managedLifecycleSnapshot() {
		candidate := record.NextQuotaCheckAt
		if candidate.IsZero() || candidate.Before(now) {
			candidate = now
		}
		if deadline.IsZero() || candidate.Before(deadline) {
			deadline = candidate
		}
	}
	return deadline
}

func (r *QuotaRefresher) requestManagedRecovery(authID string) error {
	if r == nil || r.runtimeStore == nil {
		return nil
	}
	_, err := r.runtimeStore.Update(func(state *PersistentState) error {
		for index, record := range state.ManagedLifecycle {
			if record.DisableApplied && (authID == "" || record.AuthID == authID) {
				record.NextQuotaCheckAt = r.now()
				state.ManagedLifecycle[index] = record
			}
		}
		return nil
	})
	return err
}

func (r *QuotaRefresher) recoverManagedQuotaAccount(ctx context.Context, authIndex string) error {
	now := r.now()
	response, err := r.host.GetAuth(authIndex)
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if !json.Valid(response.JSON) || json.Unmarshal(response.JSON, &raw) != nil {
		return errors.New("managed quota recovery requires valid auth JSON")
	}
	var disabled bool
	if value, ok := raw["disabled"]; ok {
		_ = json.Unmarshal(value, &disabled)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	record, owned := persisted.ManagedLifecycle[authIndex]
	if !disabled {
		// An operator (or the host) enabled it: drop ownership, never write.
		_, err := r.runtimeStore.Update(func(state *PersistentState) error { delete(state.ManagedLifecycle, authIndex); return nil })
		return err
	}
	credentials, credErr := ExtractCodexCredentials(response.JSON)
	if !owned || !record.DisableApplied || !record.Fingerprint.ValidHashes() || credErr != nil || credentials.ChatGPTAccountID == "" {
		return r.recordManagedQuotaCheck(authIndex, now)
	}
	if owned && record.Fingerprint.ValidHashes() && record.Fingerprint != managedAuthFingerprint(authIndex, credentials) {
		// The credential under this file rotated after our disable. Do not
		// enable someone else's credential: drop ownership and leave the flag
		// for the operator.
		_, err := r.runtimeStore.Update(func(state *PersistentState) error {
			delete(state.ManagedLifecycle, authIndex)
			return nil
		})
		if err == nil {
			r.state.RecordLog("warn", "managed.fingerprint_rotation", "凭据轮换后放弃托管停用所有权，交由操作员处理", map[string]any{"auth_index": authIndex}, r.now())
		}
		return err
	}
	quotaResp, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: r.state.Config().QuotaEndpoint, Headers: http.Header{
		"Authorization": []string{"Bearer " + credentials.AccessToken}, "Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID}, "Content-Type": []string{"application/json"}, "User-Agent": []string{quotaUserAgent},
	}}, false)
	if err != nil || quotaResp.StatusCode < 200 || quotaResp.StatusCode >= 300 {
		return r.recordManagedQuotaCheck(authIndex, now)
	}
	quota, err := ParseCodexUsagePayload(quotaResp.Body, now)
	if err != nil {
		return r.recordManagedQuotaCheck(authIndex, now)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Disabled auths have left admission; the owned recovery lane retains
	// the lifecycle gate and reads credits without an admission bypass token.
	creditsResp, creditsErr := r.doBackgroundHTTPRequest(resetCreditsRequest(credentials), false)
	if creditsErr == nil && creditsResp.StatusCode >= 200 && creditsResp.StatusCode < 300 {
		if credits, parseErr := ParseResetCreditsPayload(creditsResp.Body, now); parseErr == nil {
			mergeResetCredits(&quota, credits)
		}
	}
	cfg := r.state.Config()
	if record.Account != nil {
		_, err = r.runtimeStore.Update(func(state *PersistentState) error {
			current, ok := state.ManagedLifecycle[authIndex]
			if !ok || current.Account == nil {
				return nil
			}
			current.Account.Quota = cloneParsedQuota(quota)
			current.Account.ObservedAt = now
			state.ManagedLifecycle[authIndex] = current
			return nil
		})
		if err != nil {
			return err
		}
	}
	usable := managedQuotaUsable(quota, now, now, cfg.QuotaRefreshInterval)
	if record.ReserveProtected && cfg.ReserveEnabled {
		a := AccountState{AuthID: record.AuthID, Provider: "codex", Quota: quota, LastSuccessAt: now}
		if record.Account != nil {
			a.Annotation = record.Account.Annotation
		}
		if annotation, ok := r.state.Annotations().Accounts["auth:"+record.AuthID]; ok {
			a.Annotation = annotation
		}
		// Fail-open scheduling is not sufficient evidence to re-enable a file.
		cfg.ReserveFailClosed = true
		usable = usable && !evaluateReserve(a, cfg, now).Blocked
	}
	if !usable {
		return r.recordManagedQuotaCheck(authIndex, now)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !r.normalBackgroundAllowed() {
		return r.recordManagedQuotaCheck(authIndex, now)
	}
	// Revalidate the exact file after the network read; a manual edit or login
	// during that read must never be overwritten by stale auth JSON.
	current, err := r.host.GetAuth(authIndex)
	if err != nil {
		return err
	}
	currentCredentials, err := ExtractCodexCredentials(current.JSON)
	if err != nil || managedAuthFingerprint(authIndex, currentCredentials) != record.Fingerprint || !bytes.Equal(current.JSON, response.JSON) {
		return r.recordManagedQuotaCheck(authIndex, now)
	}
	raw["disabled"] = json.RawMessage("false")
	next, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	name := response.Name
	if name == "" {
		name = authIndex
	}
	if err := r.host.SaveAuth(name, next); err != nil {
		return err
	}
	_, err = r.runtimeStore.Update(func(state *PersistentState) error {
		delete(state.ManagedLifecycle, authIndex)
		return nil
	})
	if err == nil {
		r.state.RecordLog("info", "managed.reenabled", "额度恢复，已自动重新启用账号", map[string]any{"auth_index": authIndex}, r.now())
		snapshot := r.state.Snapshot(now)
		if record.Account != nil {
			account := AccountState{AuthID: record.AuthID, AuthIndex: authIndex, Provider: "codex", Annotation: record.Account.Annotation, Quota: quota, LastSuccessAt: now}
			n := notificationForEvent(snapshot, "cpa_enabled", record.AuthID, "quota_recovered")
			n.Accounts = []NotificationAccount{notificationAccount(account, snapshot)}
			r.state.queueQuotaNotification(n)
		}
		r.wakeRefreshLoop()
	}
	return err
}

func (r *QuotaRefresher) recordManagedQuotaCheck(authIndex string, at time.Time) error {
	_, err := r.runtimeStore.Update(func(state *PersistentState) error {
		record, ok := state.ManagedLifecycle[authIndex]
		if !ok {
			return nil
		}
		record.LastQuotaCheckAt = at
		record.NextQuotaCheckAt = at.Add(r.state.Config().QuotaRefreshInterval)
		record = appendLifecycleEvent(record, LifecycleEvent{Kind: "quota_checked", At: at}, r.state.Config().LifecycleEventLimit)
		state.ManagedLifecycle[authIndex] = record
		return nil
	})
	return err
}

func appendLifecycleEvent(record ManagedDisableRecord, event LifecycleEvent, limit int) ManagedDisableRecord {
	if limit <= 0 {
		return record
	}
	record.Events = append(record.Events, event)
	if overflow := len(record.Events) - limit; overflow > 0 {
		record.Events = append([]LifecycleEvent(nil), record.Events[overflow:]...)
	}
	return record
}

// managedQuotaUsable is the enable evidence: the long window must be present
// and usable, and a present five-hour window must be usable too.
func managedQuotaUsable(quota ParsedQuota, observedAt, now time.Time, staleAfter time.Duration) bool {
	window := quota.LongWindow
	if window == nil || observedAt.IsZero() || observedAt.After(now) || staleAfter <= 0 || now.Sub(observedAt) > staleAfter {
		return false
	}
	if windowExhausted(window, now) {
		return false
	}
	for _, w := range []*QuotaWindow{quota.LongWindow, quota.FiveHour} {
		if w != nil && (w.UsedPercent == nil || !validPercent(*w.UsedPercent) || (!w.ResetAt.IsZero() && !w.ResetAt.After(now))) {
			return false
		}
	}
	if window.UsedPercent != nil && *window.UsedPercent >= 100 {
		return false
	}
	if quota.FiveHour != nil && windowExhausted(quota.FiveHour, now) {
		return false
	}
	return true
}

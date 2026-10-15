package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type TelemetryPolicy struct {
	HistoryEnabled       bool    `json:"history_enabled"`
	HistoryRetentionDays int     `json:"history_retention_days"`
	NotificationsEnabled bool    `json:"notifications_enabled"`
	NotificationKind     string  `json:"notification_kind"`
	WarningPercent       float64 `json:"reserve_warning_percent"`
	WebhookURL           string  `json:"webhook_url,omitempty"`
	TelegramChatID       string  `json:"telegram_chat_id,omitempty"`
	UsageReportEnabled   bool    `json:"usage_report_enabled"`
	UsageReportMinutes   int     `json:"usage_report_interval_minutes"`
}
type TelemetryInput struct {
	HistoryEnabled       *bool    `json:"history_enabled,omitempty" yaml:"history_enabled"`
	HistoryRetentionDays *int     `json:"history_retention_days,omitempty" yaml:"history_retention_days"`
	NotificationsEnabled *bool    `json:"notifications_enabled,omitempty" yaml:"notifications_enabled"`
	NotificationKind     *string  `json:"notification_kind,omitempty" yaml:"notification_kind"`
	WarningPercent       *float64 `json:"reserve_warning_percent,omitempty" yaml:"reserve_warning_percent"`
	WebhookURL           *string  `json:"webhook_url,omitempty" yaml:"webhook_url"`
	TelegramChatID       *string  `json:"telegram_chat_id,omitempty" yaml:"telegram_chat_id"`
	UsageReportEnabled   *bool    `json:"usage_report_enabled,omitempty" yaml:"usage_report_enabled"`
	UsageReportMinutes   *int     `json:"usage_report_interval_minutes,omitempty" yaml:"usage_report_interval_minutes"`
}

func defaultTelemetryPolicy() TelemetryPolicy {
	return TelemetryPolicy{HistoryEnabled: true, HistoryRetentionDays: 30, NotificationKind: "generic", WarningPercent: 20, UsageReportMinutes: 60}
}
func telemetryInputFromPolicy(p TelemetryPolicy) TelemetryInput {
	return TelemetryInput{
		HistoryEnabled: &p.HistoryEnabled, HistoryRetentionDays: &p.HistoryRetentionDays,
		NotificationsEnabled: &p.NotificationsEnabled, NotificationKind: &p.NotificationKind, WarningPercent: &p.WarningPercent,
		WebhookURL: &p.WebhookURL, TelegramChatID: &p.TelegramChatID,
		UsageReportEnabled: &p.UsageReportEnabled, UsageReportMinutes: &p.UsageReportMinutes,
	}
}
func applyTelemetryInput(p *TelemetryPolicy, in TelemetryInput) error {
	if in.WebhookURL != nil {
		if err := validateNotificationEndpoint(*in.WebhookURL); err != nil {
			return err
		}
		p.WebhookURL = *in.WebhookURL
	}
	if in.TelegramChatID != nil {
		if len(*in.TelegramChatID) > 200 {
			return fmt.Errorf("telegram_chat_id is too long")
		}
		p.TelegramChatID = *in.TelegramChatID
	}
	if in.UsageReportEnabled != nil {
		p.UsageReportEnabled = *in.UsageReportEnabled
	}
	if in.UsageReportMinutes != nil {
		if *in.UsageReportMinutes < 1 || *in.UsageReportMinutes > 10080 {
			return fmt.Errorf("usage_report_interval_minutes must be 1..10080")
		}
		p.UsageReportMinutes = *in.UsageReportMinutes
	}
	if in.HistoryEnabled != nil {
		p.HistoryEnabled = *in.HistoryEnabled
	}
	if in.NotificationsEnabled != nil {
		p.NotificationsEnabled = *in.NotificationsEnabled
	}
	if in.HistoryRetentionDays != nil {
		if *in.HistoryRetentionDays < 1 || *in.HistoryRetentionDays > 365 {
			return fmt.Errorf("history_retention_days must be 1..365")
		}
		p.HistoryRetentionDays = *in.HistoryRetentionDays
	}
	if in.WarningPercent != nil {
		if !validPercent(*in.WarningPercent) {
			return fmt.Errorf("reserve_warning_percent must be 0..100")
		}
		p.WarningPercent = *in.WarningPercent
	}
	if in.NotificationKind != nil {
		switch *in.NotificationKind {
		case "generic", "wecom", "feishu", "dingtalk", "telegram":
			p.NotificationKind = *in.NotificationKind
		default:
			return fmt.Errorf("unsupported notification_kind")
		}
	}
	return nil
}

type QuotaHistoryPoint struct {
	At            time.Time `json:"at"`
	ObservedAt    time.Time `json:"observed_at"`
	FiveHour      *float64  `json:"five_hour,omitempty"`
	LongWindow    *float64  `json:"long_window,omitempty"`
	FiveHourReset time.Time `json:"five_hour_reset,omitempty"`
	LongReset     time.Time `json:"long_reset,omitempty"`
	FiveHourLine  float64   `json:"five_hour_line"`
	LongLine      float64   `json:"long_line"`
	Reason        string    `json:"reason"`
	Event         string    `json:"event,omitempty"`
	Source        string    `json:"source"`
}
type quotaHistory struct {
	mu       sync.Mutex
	Accounts map[string][]QuotaHistoryPoint
	dirty    bool
}

func newQuotaHistory() *quotaHistory {
	return &quotaHistory{Accounts: make(map[string][]QuotaHistoryPoint)}
}
func (h *quotaHistory) collect(s StateSnapshot) []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var events []map[string]string

	for _, a := range s.Accounts {
		if a.AuthID == "" || a.Provider != "codex" || a.LastSuccessAt.IsZero() {
			continue
		}
		reserve := evaluateReserve(a, s.Config, s.Now)
		p := QuotaHistoryPoint{At: s.Now, ObservedAt: a.LastSuccessAt, Reason: reserve.Reason, FiveHourLine: reserve.FiveHourStopLine, LongLine: reserve.LongStopLine, Source: "poll"}
		if !a.LastObservedAt.IsZero() && a.LastObservedAt.Equal(a.LastSuccessAt) {
			p.Source = "response"
		}
		for _, w := range []struct {
			q     *QuotaWindow
			out   **float64
			reset *time.Time
		}{{a.Quota.FiveHour, &p.FiveHour, &p.FiveHourReset}, {a.Quota.LongWindow, &p.LongWindow, &p.LongReset}} {
			if w.q != nil && w.q.UsedPercent != nil && validPercent(*w.q.UsedPercent) {
				v := 100 - *w.q.UsedPercent
				*w.out = &v
				*w.reset = w.q.ResetAt
			}
		}
		rows := h.Accounts[a.AuthID]
		var previous QuotaHistoryPoint
		if len(rows) > 0 {
			previous = rows[len(rows)-1]
		}
		if previous.ObservedAt.Equal(p.ObservedAt) && previous.Reason == p.Reason && previous.FiveHourLine == p.FiveHourLine && previous.LongLine == p.LongLine {
			continue
		}
		if len(rows) > 0 && (!previous.FiveHourReset.Equal(p.FiveHourReset) || !previous.LongReset.Equal(p.LongReset)) {
			p.Event = "reset"
		}
		if previous.Reason != p.Reason {
			if reserve.Blocked {
				p.Event = "protection"
			} else if previous.Reason != "" && previous.Reason != "normal" {
				p.Event = "recovery"
			}
		}
		if p.Event != "" {
			events = append(events, map[string]string{"auth_id": a.AuthID, "event": p.Event, "reason": p.Reason})
		}
		low := func(v *float64) bool { return v != nil && *v <= s.Config.WarningPercent }
		if (low(p.FiveHour) && !low(previous.FiveHour)) || (low(p.LongWindow) && !low(previous.LongWindow)) {
			events = append(events, map[string]string{"auth_id": a.AuthID, "event": "low_quota", "reason": "warning_line"})
		}
		rows = append(rows, p)
		cut := s.Now.Add(-time.Duration(s.Config.HistoryRetentionDays) * 24 * time.Hour)
		i := sort.Search(len(rows), func(i int) bool { return !rows[i].At.Before(cut) })
		rows = rows[i:]
		if len(rows) > 10000 {
			rows = rows[len(rows)-10000:]
		}
		if !s.Config.HistoryEnabled && len(rows) > 1 {
			rows = rows[len(rows)-1:]
		}
		h.Accounts[a.AuthID] = rows
		h.dirty = true
	}
	// Expire accounts no longer present as well as active-account rows.
	cut := s.Now.Add(-time.Duration(s.Config.HistoryRetentionDays) * 24 * time.Hour)
	for id, rows := range h.Accounts {
		i := sort.Search(len(rows), func(i int) bool { return !rows[i].At.Before(cut) })
		if i > 0 {
			h.Accounts[id] = rows[i:]
			h.dirty = true
		}
		if len(h.Accounts[id]) == 0 {
			delete(h.Accounts, id)
		}
	}
	return events
}
func (h *quotaHistory) snapshot() map[string][]QuotaHistoryPoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string][]QuotaHistoryPoint, len(h.Accounts))
	for id, rows := range h.Accounts {
		copyRows := append([]QuotaHistoryPoint(nil), rows...)
		for i := range copyRows {
			copyRows[i].FiveHour = cloneFloat64Ptr(copyRows[i].FiveHour)
			copyRows[i].LongWindow = cloneFloat64Ptr(copyRows[i].LongWindow)
		}
		out[id] = copyRows
	}
	return out
}
func (h *quotaHistory) load(path string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var records map[string][]QuotaHistoryPoint
	if err = json.Unmarshal(raw, &records); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Accounts = records
	if h.Accounts == nil {
		h.Accounts = make(map[string][]QuotaHistoryPoint)
	}
	for id, rows := range h.Accounts {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.Before(rows[j].At) })
		if len(rows) > 10000 {
			h.Accounts[id] = rows[len(rows)-10000:]
		}
	}
	return nil
}
func (h *quotaHistory) save(path string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.dirty {
		return nil
	}
	raw, err := json.Marshal(h.Accounts)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".quota-history-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err == nil {
		h.dirty = false
	}
	return err
}

var reserveMonitorMu sync.Mutex
var reserveMonitorStop chan struct{}
var reserveMonitorDone chan struct{}

func startReserveMonitor() {
	reserveMonitorMu.Lock()
	defer reserveMonitorMu.Unlock()
	if reserveMonitorStop != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	reserveMonitorStop = stop
	reserveMonitorDone = done
	path := filepath.Join(filepath.Dir(defaultStatePath()), "quota-history.json")
	if err := globalState.history.load(path); err != nil {
		globalState.RecordLog("error", "history.load_failed", "历史读取失败", nil, time.Now())
	}
	go func() {
		defer close(done)
		deliveryContext, cancelDelivery := context.WithCancel(context.Background())
		deliveryDone := make(chan struct{})
		go func() {
			defer close(deliveryDone)
			globalState.runNotificationDelivery(deliveryContext)
		}()
		defer func() { cancelDelivery(); <-deliveryDone }()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var nextPoll, nextSave time.Time
		allUnavailable := false
		failureNotified := map[string]bool{}
		tracker := newParticipationTracker()
		report := usageReportClock{}
		var nextEnforce time.Time
		for {
			select {
			case <-stop:
				if err := globalState.history.save(path); err != nil {
					globalState.RecordLog("error", "history.save_failed", "历史保存失败", nil, time.Now())
				}
				return
			case now := <-ticker.C:
				snap := globalState.Snapshot(now)
				refresherMu.Lock()
				r := globalRefresher
				refresherMu.Unlock()
				if r != nil {
					r = r.lifecycleRefresher()
					if !now.Before(nextEnforce) {
						if err := r.ApplyReserveQuotaDisables(context.Background()); err != nil {
							globalState.RecordLog("warn", "reserve.disable_failed", "CPA 账号停用失败，将重试", nil, now)
						}
						nextEnforce = now.Add(5 * time.Second)
					}
					snap = snapshotWithManagedAccounts(globalState.Snapshot(now), r)
				}
				events := globalState.history.collect(snap)
				if snap.Config.ReserveEnabled && !now.Before(nextPoll) {
					refresherMu.Lock()
					r := globalRefresher
					controller := globalRosterController
					refresherMu.Unlock()
					if controller != nil {
						_, _ = controller.WakeForManagement(context.Background())
					}
					if r != nil {
						r.RefreshSoon()
					}
					nextPoll = now.Add(snap.Config.QuotaRefreshInterval)
				}
				usable := 0
				for _, a := range snap.Accounts {
					if a.Provider != "codex" {
						continue
					}
					if participating, _ := accountParticipation(a, snap); participating {
						usable++
					}
					failed := a.LastError != "" && !a.LastSuccessAt.IsZero() && now.Sub(a.LastSuccessAt) > time.Hour
					if failed && !failureNotified[a.AuthID] {
						events = append(events, map[string]string{"auth_id": a.AuthID, "event": "refresh_failed", "reason": "no_success_for_1h"})
					}
					failureNotified[a.AuthID] = failed
				}
				unavailable := snap.Config.ReserveEnabled && len(snap.Accounts) > 0 && usable == 0
				if unavailable && !allUnavailable {
					events = append(events, map[string]string{"event": "all_unavailable", "reason": "no_safe_account"})
				}
				allUnavailable = unavailable
				notifications := tracker.collect(snap)
				for _, e := range events {
					globalState.RecordLog("info", "reserve."+e["event"], "额度预留状态变化", map[string]any{"auth_id": e["auth_id"], "reason": e["reason"]}, now)
				}
				if report.due(snap.Config.TelemetryPolicy, now) {
					notifications = append(notifications, notificationForEvent(snap, "usage_report", "", ""))
				}
				for _, n := range notifications {
					globalState.queueQuotaNotification(n)
				}
				if !now.Before(nextSave) {
					if err := globalState.history.save(path); err != nil {
						globalState.RecordLog("error", "history.save_failed", "历史保存失败", nil, now)
					}
					nextSave = now.Add(time.Minute)
				}
			}
		}
	}()
}
func stopReserveMonitor() {
	reserveMonitorMu.Lock()
	stop, done := reserveMonitorStop, reserveMonitorDone
	if stop != nil {
		close(stop)
		reserveMonitorStop = nil
	}
	reserveMonitorMu.Unlock()
	if done != nil {
		<-done
	}
}

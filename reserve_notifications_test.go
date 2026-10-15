package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNotificationMarkdownUsesQuotaCardLayout(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	fiveHour := 0.0
	weekly := 53.0
	available := 2
	earned := 4
	n := QuotaNotification{
		Title: "账号开始参与调度",
		Event: "scheduling_started",
		At:    now,
		Accounts: []NotificationAccount{{
			AuthID:                     "auth-1",
			Alias:                      "测试账号",
			Participating:              true,
			FiveHourRemaining:          &fiveHour,
			LongWindowRemaining:        &weekly,
			FiveHourReset:              time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC),
			LongWindowReset:            time.Date(2026, 10, 20, 5, 4, 0, 0, time.UTC),
			LongWindowKind:             WindowWeekly,
			ResetCreditsAvailableCount: &available,
			ResetCreditsTotalEarned:    &earned,
			ResetCredits: []ResetCredit{
				{ExpiresAt: time.Date(2026, 11, 6, 23, 36, 0, 0, time.UTC)},
				{ExpiresAt: time.Date(2026, 10, 29, 19, 11, 0, 0, time.UTC)},
			},
		}},
	}

	text := notificationMarkdown(n)
	for _, want := range []string{
		"主动重置过期时间（GMT+8）",
		"第 1 次　10/30 03:11 · 14 天后",
		"第 2 次　11/07 07:36 · 22 天后",
		"剩余主动重置：**2 次**",
		"**5 小时限额**　**0%**",
		"`░░░░░░░░░░`",
		"**周限额**　**53%**",
		"`█████░░░░░`",
		"10/14 18:00 · 14 小时前",
		"10/20 13:04 · 5 天后",
		"累计主动重置：4 次",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("notification markdown missing %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "10/30 03:11") > strings.Index(text, "11/07 07:36") {
		t.Fatalf("reset credits were not sorted by expiry:\n%s", text)
	}
}

func TestNotificationResetCountUsesAllAvailableCreditsWhenRowsAreTruncated(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	credits := make([]ResetCredit, 0, notificationMaxResetRows+2)
	for i := 0; i < notificationMaxResetRows+2; i++ {
		credits = append(credits, ResetCredit{ExpiresAt: now.Add(time.Duration(i+1) * 24 * time.Hour)})
	}
	text := notificationMarkdown(QuotaNotification{
		Title: "定时额度使用报告",
		At:    now,
		Accounts: []NotificationAccount{{
			AuthID:       "auth-many-resets",
			ResetCredits: credits,
		}},
	})
	if !strings.Contains(text, "另有 2 次未展开") {
		t.Fatalf("truncated reset rows missing overflow count:\n%s", text)
	}
	if !strings.Contains(text, "剩余主动重置：**8 次**") {
		t.Fatalf("remaining reset count used display rows instead of all available credits:\n%s", text)
	}
}

func TestNotificationResetCountPrefersUpstreamAvailableCount(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	upstreamCount := 12
	text := notificationMarkdown(QuotaNotification{
		Title: "定时额度使用报告",
		At:    now,
		Accounts: []NotificationAccount{{
			AuthID:                     "auth-upstream-count",
			ResetCreditsAvailableCount: &upstreamCount,
			ResetCredits:               []ResetCredit{{ExpiresAt: now.Add(24 * time.Hour)}},
		}},
	})
	if !strings.Contains(text, "剩余主动重置：**12 次**") {
		t.Fatalf("upstream reset count was not preferred:\n%s", text)
	}
}

func TestNotificationMarkdownHandlesUnknownAndExpiredQuota(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	text := notificationMarkdown(QuotaNotification{
		Title: "通知测试",
		At:    now,
		Accounts: []NotificationAccount{{
			AuthID:          "auth-unknown",
			Participating:   false,
			FiveHourReset:   now.Add(-14 * time.Hour),
			LongWindowReset: time.Time{},
		}},
	})
	for _, want := range []string{
		"**5 小时限额**　**未知**",
		"**周限额**　**未知**",
		"重置时间未知",
		"14 小时前",
		"剩余主动重置：**未知**",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("unknown quota notification missing %q:\n%s", want, text)
		}
	}
}

func TestNotificationHTMLUsesEscapedQuotaCardLayout(t *testing.T) {
	remaining := 53.0
	text := notificationHTML(QuotaNotification{
		Title: "通知 <测试>",
		At:    time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC),
		Accounts: []NotificationAccount{{
			AuthID:              "auth-1",
			Alias:               "A&B",
			LongWindowRemaining: &remaining,
			LongWindowKind:      WindowWeekly,
		}},
	})
	for _, want := range []string{
		"<b>通知 &lt;测试&gt;</b>",
		"<b>A&amp;B</b>",
		"<b>周限额</b>　<b>53%</b>",
		"<code>█████░░░░░</code>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("notification HTML missing %q:\n%s", want, text)
		}
	}
}

func TestParticipationTrackerOnlyNotifiesStateTransitions(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	base := StateSnapshot{
		Config: cfg,
		CPAAdmission: CPAAdmissionState{
			Observed: true,
			AuthIDs:  map[string]struct{}{"auth-1": {}},
		},
		Now: now,
		Accounts: []AccountState{{
			AuthID:        "auth-1",
			AuthIndex:     "idx-1",
			Provider:      "codex",
			LastSuccessAt: now,
		}},
	}

	tracker := newParticipationTracker()
	if got := tracker.collect(base); len(got) != 0 {
		t.Fatalf("initial participating state generated notifications = %#v", got)
	}

	// Quota refreshes, including a new reset deadline, must not create another
	// notification while the scheduling state is unchanged.
	refreshed := base
	refreshed.Now = now.Add(time.Minute)
	refreshed.Accounts = append([]AccountState(nil), base.Accounts...)
	refreshed.Accounts[0].Quota.FiveHour = &QuotaWindow{
		Kind:        WindowFiveHour,
		ResetAt:     now.Add(5 * time.Hour),
		UsedPercent: reserveFloat(10),
	}
	if got := tracker.collect(refreshed); len(got) != 0 {
		t.Fatalf("quota refresh generated notifications = %#v", got)
	}

	stopped := refreshed
	stopped.Accounts = append([]AccountState(nil), refreshed.Accounts...)
	stopped.Accounts[0].ManagedDisabled = true
	if got := tracker.collect(stopped); len(got) != 1 || got[0].Event != "scheduling_stopped" {
		t.Fatalf("stopped state = %#v, want one stop event", got)
	}
	if got := tracker.collect(stopped); len(got) != 0 {
		t.Fatalf("unchanged stopped state generated notifications = %#v", got)
	}

	restarted := stopped
	restarted.Accounts = append([]AccountState(nil), stopped.Accounts...)
	restarted.Accounts[0].ManagedDisabled = false
	if got := tracker.collect(restarted); len(got) != 1 || got[0].Event != "scheduling_started" {
		t.Fatalf("restarted state = %#v, want one start event", got)
	}
}

func TestParticipationTrackerDoesNotNotifyInitialStoppedState(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	base := StateSnapshot{
		Config: cfg,
		CPAAdmission: CPAAdmissionState{
			Observed: true,
			AuthIDs:  map[string]struct{}{"auth-1": {}},
		},
		Now: now,
		Accounts: []AccountState{{
			AuthID:          "auth-1",
			AuthIndex:       "idx-1",
			Provider:        "codex",
			LastSuccessAt:   now,
			ManagedDisabled: true,
		}},
	}

	tracker := newParticipationTracker()
	if got := tracker.collect(base); len(got) != 0 {
		t.Fatalf("initial stopped state generated notifications = %#v", got)
	}
	if got := tracker.collect(base); len(got) != 0 {
		t.Fatalf("unchanged initial stopped state generated notifications = %#v", got)
	}
}

func TestParticipationTrackerQuotaCycleResetsStaySilent(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	account, cfg := reserveFixture(now, 50, 60)
	cfg.NotificationsEnabled = true
	store := NewPluginState(cfg)
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{"a": {}}})
	tracker := newParticipationTracker()
	history := newQuotaHistory()
	for i := 0; i < 20; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		account.LastSuccessAt = at
		account.Quota.FiveHour.ResetAt = at.Add(5 * time.Hour)
		account.Quota.LongWindow.ResetAt = at.Add(7 * 24 * time.Hour)
		store.UpsertQuota(account)
		snapshot := store.Snapshot(at)
		events := history.collect(snapshot)
		if i > 0 && (len(events) != 1 || events[0]["event"] != "reset") {
			t.Fatalf("refresh %d did not exercise a history reset: %#v", i, events)
		}
		if notifications := tracker.collect(snapshot); len(notifications) != 0 {
			t.Fatalf("quota-only refresh %d generated notifications: %#v", i, notifications)
		}
	}
}

func TestParticipationTrackerActiveResetWaitsForSchedulingRecovery(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	account, cfg := reserveFixture(now, 50, 60)
	account.ManagedDisabled = true
	snapshot := StateSnapshot{
		Config: cfg, Now: now, Accounts: []AccountState{account},
		CPAAdmission: CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{}},
	}
	tracker := newParticipationTracker()
	tracker.collect(snapshot)

	// New quota alone does not mean the CPA credential can participate again.
	snapshot.Now = now.Add(time.Minute)
	snapshot.Accounts[0].Quota.FiveHour.UsedPercent = reserveFloat(0)
	snapshot.Accounts[0].Quota.FiveHour.ResetAt = now.Add(6 * time.Hour)
	snapshot.Accounts[0].LastSuccessAt = snapshot.Now
	if got := tracker.collect(snapshot); len(got) != 0 {
		t.Fatalf("reset while disabled generated notifications: %#v", got)
	}
	snapshot.Accounts[0].ManagedDisabled = false
	if got := tracker.collect(snapshot); len(got) != 0 {
		t.Fatalf("recovery before CPA admission generated notifications: %#v", got)
	}
	snapshot.CPAAdmission.AuthIDs["a"] = struct{}{}
	if got := tracker.collect(snapshot); len(got) != 1 || got[0].Event != "scheduling_started" {
		t.Fatalf("confirmed recovery = %#v, want one start event", got)
	}
	if got := tracker.collect(snapshot); len(got) != 0 {
		t.Fatalf("unchanged recovered state generated notifications: %#v", got)
	}
}

func TestUsageReportClockSendsOnlyWhenDue(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	policy := defaultTelemetryPolicy()
	policy.NotificationsEnabled = true
	policy.UsageReportEnabled = true
	policy.UsageReportMinutes = 60

	clock := usageReportClock{}
	if clock.due(policy, now) {
		t.Fatal("first usage report sample was due")
	}
	if clock.due(policy, now.Add(59*time.Minute)) {
		t.Fatal("usage report was due before its interval")
	}
	if !clock.due(policy, now.Add(60*time.Minute)) {
		t.Fatal("usage report was not due at its interval")
	}
	if clock.due(policy, now.Add(61*time.Minute)) {
		t.Fatal("usage report repeated without another full interval")
	}
	if clock.due(policy, now.Add(119*time.Minute)) {
		t.Fatal("usage report was due before the next scheduled interval")
	}
	if !clock.due(policy, now.Add(120*time.Minute)) {
		t.Fatal("usage report did not become due after the next interval")
	}
	if clock.due(policy, now.Add(120*time.Minute)) {
		t.Fatal("usage report repeated at the same time")
	}

	policy.UsageReportEnabled = false
	if clock.due(policy, now.Add(2*time.Hour)) {
		t.Fatal("disabled usage report remained due")
	}
	policy.UsageReportEnabled = true
	if clock.due(policy, now.Add(3*time.Hour)) {
		t.Fatal("re-enabled usage report skipped its first interval")
	}
	policy.UsageReportMinutes = 30
	if clock.due(policy, now.Add(3*time.Hour+time.Minute)) {
		t.Fatal("changed usage report interval was immediately due")
	}
	if clock.due(policy, now.Add(3*time.Hour+30*time.Minute)) {
		t.Fatal("changed usage report interval was due too early")
	}
	if !clock.due(policy, now.Add(3*time.Hour+31*time.Minute)) {
		t.Fatal("changed usage report interval was not due")
	}
}

func TestParticipationTrackerWaitsForInitialAdmission(t *testing.T) {
	s := StateSnapshot{Config: DefaultConfig(), Now: time.Now(), Accounts: []AccountState{{
		AuthID: "auth-1", Provider: "codex",
	}}}
	tracker := newParticipationTracker()
	if got := tracker.collect(s); len(got) != 0 || tracker.initialized {
		t.Fatalf("unconfirmed roster initialized tracker: %#v", got)
	}
	s.CPAAdmission = CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{"auth-1": {}}}
	if got := tracker.collect(s); len(got) != 0 {
		t.Fatalf("initial confirmed roster generated notifications: %#v", got)
	}
}

func TestParticipationTrackerBatchesTransitionsAndRemoval(t *testing.T) {
	s := StateSnapshot{
		Config: DefaultConfig(), Now: time.Now(),
		CPAAdmission: CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{"auth-a": {}, "auth-b": {}}},
		Accounts: []AccountState{
			{AuthID: "auth-b", Provider: "codex"},
			{AuthID: "auth-a", Provider: "codex"},
		},
	}
	tracker := newParticipationTracker()
	tracker.collect(s)
	s.Accounts = s.Accounts[:1]
	s.Accounts[0].ManagedDisabled = true
	events := tracker.collect(s)
	if len(events) != 1 || events[0].Event != "scheduling_stopped" || len(events[0].Accounts) != 2 {
		t.Fatalf("batched stop events = %#v", events)
	}
	if events[0].Accounts[0].AuthID != "auth-a" || events[0].Accounts[0].Reason != "account_removed" ||
		events[0].Accounts[1].AuthID != "auth-b" || events[0].Accounts[1].Reason != "managed_disabled" {
		t.Fatalf("batched stop accounts = %#v", events[0].Accounts)
	}
	if got := tracker.collect(s); len(got) != 0 {
		t.Fatalf("unchanged state generated notifications = %#v", got)
	}
}

func TestNotificationQueueRejectsHistoryEvents(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NotificationsEnabled = true
	store := NewPluginState(cfg)
	for _, event := range []string{"reset", "protection", "recovery", "low_quota", "refresh_failed", "all_unavailable", "cpa_disabled", "cpa_enabled"} {
		store.queueQuotaNotification(QuotaNotification{Event: event})
	}
	if got := len(store.notifications); got != 0 {
		t.Fatalf("history events queued %d notifications", got)
	}
	for _, event := range []string{"scheduling_started", "scheduling_stopped", "usage_report"} {
		store.queueQuotaNotification(QuotaNotification{Event: event})
	}
	if got := len(store.notifications); got != 3 {
		t.Fatalf("allowed events queued %d notifications, want 3", got)
	}
}

func TestQuotaNotificationCombinesAccountsAndPaginatesByPayloadSize(t *testing.T) {
	for _, kind := range []string{"generic", "wecom", "dingtalk", "feishu", "telegram"} {
		t.Run(kind, func(t *testing.T) {
			policy := defaultTelemetryPolicy()
			policy.NotificationKind = kind
			policy.TelegramChatID = "test-chat"
			n := QuotaNotification{Title: "定时额度使用报告", Event: "usage_report", At: time.Now()}
			for i := 0; i < 2; i++ {
				n.Accounts = append(n.Accounts, NotificationAccount{AuthID: fmt.Sprintf("account-%03d", i)})
			}
			pages, err := notificationPages(policy, n)
			if err != nil || len(pages) != 1 || len(pages[0].Accounts) != 2 || pages[0].Title != n.Title {
				t.Fatalf("two accounts were not combined: %#v, %v", pages, err)
			}
			for i := 2; i < 40; i++ {
				n.Accounts = append(n.Accounts, NotificationAccount{AuthID: fmt.Sprintf("account-%03d", i)})
			}
			pages, err = notificationPages(policy, n)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "generic" && len(pages) != 1 || kind != "generic" && len(pages) <= 1 {
				t.Fatalf("page count = %d for %s", len(pages), kind)
			}
			var received [][]byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				received = append(received, raw)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errcode":0,"code":0,"ok":true}`))
			}))
			defer server.Close()
			policy.WebhookURL = server.URL
			if err := sendQuotaNotification(context.Background(), policy, n); err != nil {
				t.Fatal(err)
			}
			if len(received) != len(pages) {
				t.Fatalf("webhook received %d requests, want %d", len(received), len(pages))
			}
			index := 0
			for i, page := range pages {
				if kind != "generic" && len(received[i]) > notificationPayloadLimit {
					t.Fatalf("page %d is %d bytes", i, len(received[i]))
				}
				if len(pages) > 1 && page.Title != fmt.Sprintf("%s (%d/%d)", n.Title, i+1, len(pages)) {
					t.Fatalf("page title = %q", page.Title)
				}
				for _, account := range page.Accounts {
					if account.AuthID != n.Accounts[index].AuthID || !strings.Contains(string(received[i]), account.AuthID) {
						t.Fatalf("account %d missing or out of order", index)
					}
					index++
				}
				var body any
				if err := json.Unmarshal(received[i], &body); err != nil {
					t.Fatal(err)
				}
			}
			if index != len(n.Accounts) {
				t.Fatalf("delivered %d accounts, want %d", index, len(n.Accounts))
			}
		})
	}
}

func TestQuotaNotificationRejectsOversizedSingleAccount(t *testing.T) {
	policy := defaultTelemetryPolicy()
	policy.NotificationKind = "dingtalk"
	_, err := notificationPages(policy, QuotaNotification{
		Title:    strings.Repeat("x", notificationPayloadLimit),
		Accounts: []NotificationAccount{{AuthID: "account-1"}},
	})
	if err == nil {
		t.Fatal("oversized single-account payload was accepted")
	}
}

func TestQuotaNotificationBoundsMarkdownTitle(t *testing.T) {
	policy := defaultTelemetryPolicy()
	policy.NotificationKind = "wecom"
	pages, err := notificationPages(policy, QuotaNotification{
		Title:    strings.Repeat("x", notificationPayloadLimit),
		Accounts: []NotificationAccount{{AuthID: "account-1"}},
	})
	if err != nil || len(pages) != 1 {
		t.Fatalf("bounded Markdown title rejected: %#v, %v", pages, err)
	}
	raw, err := json.Marshal(richNotificationBody(policy, pages[0]))
	if err != nil || len(raw) > notificationPayloadLimit {
		t.Fatalf("bounded Markdown payload = %d bytes, %v", len(raw), err)
	}
}

func TestSendQuotaNotificationRejectsProviderErrorAndRedirect(t *testing.T) {
	providerError := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":400,"errmsg":"Invalid request parameters"}`))
	}))
	defer providerError.Close()

	policy := defaultTelemetryPolicy()
	policy.NotificationsEnabled = true
	policy.NotificationKind = "wecom"
	policy.WebhookURL = providerError.URL
	if err := sendQuotaNotification(context.Background(), policy, QuotaNotification{Title: "test"}); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("provider error was accepted: %v", err)
	}

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer redirect.Close()
	policy.NotificationKind = "generic"
	policy.WebhookURL = redirect.URL
	if err := sendQuotaNotification(context.Background(), policy, QuotaNotification{Title: "test"}); err == nil || !strings.Contains(err.Error(), "HTTP status 302") {
		t.Fatalf("redirect was followed or accepted: %v", err)
	}
}

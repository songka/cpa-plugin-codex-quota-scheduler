package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type NotificationAccount struct {
	AuthID                     string        `json:"auth_id"`
	Alias                      string        `json:"alias,omitempty"`
	Participating              bool          `json:"participating"`
	Reason                     string        `json:"reason"`
	FiveHourRemaining          *float64      `json:"five_hour_remaining_percent,omitempty"`
	LongWindowRemaining        *float64      `json:"long_window_remaining_percent,omitempty"`
	FiveHourReset              time.Time     `json:"five_hour_reset_at"`
	LongWindowReset            time.Time     `json:"long_window_reset_at"`
	ResetCreditsAvailableCount *int          `json:"reset_credits_available_count,omitempty"`
	ResetCreditsTotalEarned    *int          `json:"reset_credits_total_earned_count,omitempty"`
	ResetCredits               []ResetCredit `json:"reset_credits,omitempty"`
	ObservedAt                 time.Time     `json:"observed_at"`
}

type QuotaNotification struct {
	Plugin   string                `json:"plugin"`
	Event    string                `json:"event"`
	Title    string                `json:"title"`
	Reason   string                `json:"reason,omitempty"`
	At       time.Time             `json:"at"`
	Accounts []NotificationAccount `json:"accounts"`
}

func validateNotificationEndpoint(endpoint string) error {
	if endpoint == "" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || len(endpoint) > 4096 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.TrimSpace(endpoint) != endpoint {
		return fmt.Errorf("webhook_url must be an HTTPS URL (HTTP allowed only for loopback)")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return fmt.Errorf("webhook_url must be an HTTPS URL (HTTP allowed only for loopback)")
	}
	return nil
}

func notificationEndpoint(p TelemetryPolicy) string {
	if p.WebhookURL != "" {
		return p.WebhookURL
	}
	return os.Getenv("RESERVE_WEBHOOK_URL")
}

func notificationChatID(p TelemetryPolicy) string {
	if p.TelegramChatID != "" {
		return p.TelegramChatID
	}
	return os.Getenv("RESERVE_TELEGRAM_CHAT_ID")
}

func accountParticipation(a AccountState, s StateSnapshot) (bool, string) {
	if a.ManagedDisabled {
		return false, "managed_disabled"
	}
	if !s.Config.HandleEnabled {
		return false, "scheduler_disabled"
	}
	if !s.CPAAdmission.Observed {
		return false, "roster_unconfirmed"
	}
	if _, ok := s.CPAAdmission.AuthIDs[a.AuthID]; !ok {
		return false, "outside_admission"
	}
	v := accountViewFromState(a, s.Config, s.Now, nil)
	if ClassifyAccount(v, s.Now) == Excluded {
		if v.ReserveBlocked {
			return false, v.ReserveReason
		}
		return false, "quota_or_account_unavailable"
	}
	return true, "normal"
}

func notificationAccount(a AccountState, s StateSnapshot) NotificationAccount {
	active, reason := accountParticipation(a, s)
	n := NotificationAccount{
		AuthID: a.AuthID, Alias: a.Annotation.Alias, Participating: active, Reason: reason, ObservedAt: a.LastSuccessAt,
		ResetCreditsAvailableCount: cloneIntPtr(a.Quota.ResetCreditsAvailableCount),
		ResetCreditsTotalEarned:    cloneIntPtr(a.Quota.ResetCreditsTotalEarnedCount),
		ResetCredits:               append([]ResetCredit(nil), a.Quota.ResetCredits...),
	}
	for _, w := range []struct {
		q         *QuotaWindow
		remaining **float64
		reset     *time.Time
	}{{a.Quota.FiveHour, &n.FiveHourRemaining, &n.FiveHourReset}, {a.Quota.LongWindow, &n.LongWindowRemaining, &n.LongWindowReset}} {
		if w.q != nil {
			*w.reset = w.q.ResetAt
			if w.q.UsedPercent != nil && validPercent(*w.q.UsedPercent) {
				value := 100 - *w.q.UsedPercent
				*w.remaining = &value
			}
		}
	}
	return n
}

func notificationForEvent(s StateSnapshot, event, id, reason string) QuotaNotification {
	titles := map[string]string{
		"scheduling_started": "账号开始参与调度", "scheduling_stopped": "账号停止参与调度",
		"cpa_disabled": "CPA 账号已停用", "cpa_enabled": "CPA 账号已恢复启用",
		"usage_report": "定时额度使用报告", "test": "通知测试",
		"protection": "额度预留保护", "recovery": "额度保护解除", "reset": "额度周期重置",
		"low_quota": "账号低额度预警", "refresh_failed": "额度刷新失败", "all_unavailable": "没有可调度账号",
	}
	n := QuotaNotification{Plugin: PluginID, Event: event, Title: titles[event], Reason: reason, At: s.Now, Accounts: []NotificationAccount{}}
	if n.Title == "" {
		n.Title = event
	}
	for _, a := range s.Accounts {
		if a.Provider == "codex" && (id == "" || a.AuthID == id) {
			n.Accounts = append(n.Accounts, notificationAccount(a, s))
		}
	}
	sort.Slice(n.Accounts, func(i, j int) bool { return n.Accounts[i].AuthID < n.Accounts[j].AuthID })
	return n
}

func (s *PluginState) queueQuotaNotification(n QuotaNotification) {
	if !s.Config().NotificationsEnabled {
		return
	}
	select {
	case s.notifications <- n:
	default:
		s.RecordLog("warn", "notification.queue_full", "通知队列已满", nil, n.At)
	}
}

func (s *PluginState) runNotificationDelivery(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case n := <-s.notifications:
			policy := s.Config().TelemetryPolicy
			if !policy.NotificationsEnabled {
				continue
			}
			if err := sendQuotaNotification(ctx, policy, n); err != nil && ctx.Err() == nil {
				s.RecordLog("warn", "notification.failed", err.Error(), nil, time.Now())
			}
		}
	}
}

type participationTracker struct {
	previous map[string]NotificationAccount
}

func newParticipationTracker() *participationTracker {
	return &participationTracker{previous: map[string]NotificationAccount{}}
}

func (t *participationTracker) collect(s StateSnapshot) []QuotaNotification {
	next := map[string]NotificationAccount{}
	var events []QuotaNotification
	for _, a := range s.Accounts {
		if a.Provider != "codex" || a.AuthID == "" {
			continue
		}
		current := notificationAccount(a, s)
		next[a.AuthID] = current
		previous, known := t.previous[a.AuthID]
		if (!known && !current.Participating) || (known && previous.Participating == current.Participating) {
			continue
		}
		event := "scheduling_stopped"
		if current.Participating {
			event = "scheduling_started"
		}
		events = append(events, notificationForEvent(s, event, a.AuthID, current.Reason))
	}
	for id, previous := range t.previous {
		if _, exists := next[id]; !exists && previous.Participating {
			previous.Participating, previous.Reason = false, "account_removed"
			n := notificationForEvent(s, "scheduling_stopped", id, previous.Reason)
			n.Accounts = []NotificationAccount{previous}
			events = append(events, n)
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Accounts[0].AuthID < events[j].Accounts[0].AuthID })
	t.previous = next
	return events
}

type usageReportClock struct {
	next    time.Time
	minutes int
}

func (c *usageReportClock) due(p TelemetryPolicy, now time.Time) bool {
	if !p.NotificationsEnabled || !p.UsageReportEnabled {
		c.next = time.Time{}
		return false
	}
	minutes := p.UsageReportMinutes
	if minutes <= 0 {
		minutes = 60
	}
	if c.next.IsZero() || c.minutes != minutes {
		c.minutes, c.next = minutes, now.Add(time.Duration(minutes)*time.Minute)
		return false
	}
	if now.Before(c.next) {
		return false
	}
	c.next = now.Add(time.Duration(minutes) * time.Minute)
	return true
}

func snapshotWithManagedAccounts(s StateSnapshot, r *QuotaRefresher) StateSnapshot {
	if r == nil || r.runtimeStore == nil {
		return s
	}
	return mergeManagedAccounts(s, r.runtimeStore.managedLifecycleSnapshot())
}

func mergeManagedAccounts(s StateSnapshot, records map[string]ManagedDisableRecord) StateSnapshot {
	seen := map[string]int{}
	s.Accounts = append([]AccountState(nil), s.Accounts...)
	for i, a := range s.Accounts {
		seen[a.AuthIndex] = i
	}
	for index, record := range records {
		if !record.DisableApplied {
			continue
		}
		if i, ok := seen[index]; ok {
			s.Accounts[i].ManagedDisabled = true
			continue
		}
		if record.Account == nil {
			continue
		}
		a := AccountState{AuthID: record.AuthID, AuthIndex: index, Provider: "codex", ManagedDisabled: true,
			Quota: cloneParsedQuota(record.Account.Quota), Family: record.Account.Quota.Family,
			Annotation: cloneAccountAnnotation(record.Account.Annotation), LastSuccessAt: record.Account.ObservedAt}
		if annotation, ok := s.Annotations.Accounts["auth:"+a.AuthID]; ok {
			a.Annotation = cloneAccountAnnotation(annotation)
		}
		s.Accounts = append(s.Accounts, a)
	}
	return s
}

func notificationTime(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	return t.Format("2006-01-02 15:04:05 -07:00")
}

func notificationPercent(v *float64) string {
	if v == nil {
		return "未知"
	}
	return fmt.Sprintf("%.1f%%", *v)
}

func notificationCount(v *int) string {
	if v == nil {
		return "未知"
	}
	return fmt.Sprintf("%d 次", *v)
}

func notificationLabel(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	runes := []rune(s)
	if len(runes) > 80 {
		s = string(runes[:80])
	}
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "`", "'").Replace(s)
}

func notificationMarkdown(n QuotaNotification) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n\n%s\n", n.Title, notificationTime(n.At))
	for _, a := range n.Accounts {
		name := a.Alias
		if name == "" {
			name = a.AuthID
		}
		state := "停止参与"
		if a.Participating {
			state = "参与调度"
		}
		fmt.Fprintf(&b, "\n**%s** · %s\n\n", notificationLabel(name), state)
		fmt.Fprintf(&b, "- 5h 剩余：**%s** · 重置：%s\n", notificationPercent(a.FiveHourRemaining), notificationTime(a.FiveHourReset))
		fmt.Fprintf(&b, "- 长期剩余：**%s** · 重置/到期：%s\n", notificationPercent(a.LongWindowRemaining), notificationTime(a.LongWindowReset))
		fmt.Fprintf(&b, "- 主动重置剩余：**%s** · 累计：%s\n", notificationCount(a.ResetCreditsAvailableCount), notificationCount(a.ResetCreditsTotalEarned))
		for i, credit := range a.ResetCredits {
			if i >= 3 {
				fmt.Fprintf(&b, "- 另有 %d 条重置记录\n", len(a.ResetCredits)-i)
				break
			}
			fmt.Fprintf(&b, "- 重置次数到期：%s（%s）\n", notificationTime(credit.ExpiresAt), notificationLabel(credit.Status))
		}
		fmt.Fprintf(&b, "- 状态：%s · 额度观测：%s\n", notificationLabel(a.Reason), notificationTime(a.ObservedAt))
	}
	if len(n.Accounts) == 0 {
		b.WriteString("\n暂无账号额度数据\n")
	}
	return b.String()
}

func richNotificationBody(p TelemetryPolicy, n QuotaNotification) any {
	text := notificationMarkdown(n)
	switch p.NotificationKind {
	case "wecom":
		return map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": text}}
	case "dingtalk":
		return map[string]any{"msgtype": "markdown", "markdown": map[string]string{"title": n.Title, "text": text}}
	case "feishu":
		color := "green"
		if strings.Contains(n.Event, "stopped") || n.Event == "cpa_disabled" || n.Event == "protection" || n.Event == "low_quota" {
			color = "orange"
		}
		return map[string]any{"msg_type": "interactive", "card": map[string]any{
			"header":   map[string]any{"template": color, "title": map[string]string{"tag": "plain_text", "content": n.Title}},
			"elements": []any{map[string]any{"tag": "div", "text": map[string]string{"tag": "lark_md", "content": text}}},
		}}
	case "telegram":
		return map[string]string{"chat_id": notificationChatID(p), "text": notificationHTML(n), "parse_mode": "HTML"}
	default:
		return struct {
			QuotaNotification
			Message string `json:"message"`
		}{n, text}
	}
}

// Retain the small helper used by existing format tests.
func notificationBody(kind, text string) any {
	return richNotificationBody(TelemetryPolicy{NotificationKind: kind}, QuotaNotification{Plugin: PluginID, Title: text})
}

func sendQuotaNotification(ctx context.Context, p TelemetryPolicy, n QuotaNotification) error {
	endpoint := notificationEndpoint(p)
	if endpoint == "" {
		return fmt.Errorf("Webhook 地址未配置")
	}
	if err := validateNotificationEndpoint(endpoint); err != nil {
		return err
	}
	if p.NotificationKind == "telegram" && notificationChatID(p) == "" {
		return fmt.Errorf("Telegram Chat ID 未配置")
	}
	// Chat providers enforce small message limits; reports are sent in bounded pages.
	if len(n.Accounts) > 1 && p.NotificationKind != "generic" {
		for start := range n.Accounts {
			page := n
			page.Accounts = n.Accounts[start : start+1]
			page.Title = fmt.Sprintf("%s (%d/%d)", n.Title, start+1, len(n.Accounts))
			if err := sendQuotaNotification(ctx, p, page); err != nil {
				return err
			}
		}
		return nil
	}
	raw, err := json.Marshal(richNotificationBody(p, n))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("notification request invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("notification delivery failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notification HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16384))
	if err != nil {
		return fmt.Errorf("notification response unreadable")
	}
	if p.NotificationKind != "generic" {
		var ack struct {
			ErrCode *int  `json:"errcode"`
			Code    *int  `json:"code"`
			OK      *bool `json:"ok"`
		}
		if json.Unmarshal(body, &ack) != nil {
			return fmt.Errorf("notification provider response invalid")
		}
		accepted := false
		switch p.NotificationKind {
		case "wecom", "dingtalk":
			accepted = ack.ErrCode != nil && *ack.ErrCode == 0
		case "feishu":
			accepted = ack.Code != nil && *ack.Code == 0
		case "telegram":
			accepted = ack.OK != nil && *ack.OK
		}
		if !accepted {
			return fmt.Errorf("notification provider rejected message")
		}
	}
	return nil
}

func notificationHTML(n QuotaNotification) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n%s\n", html.EscapeString(n.Title), notificationTime(n.At))
	for _, a := range n.Accounts {
		name := a.Alias
		if name == "" {
			name = a.AuthID
		}
		runes := []rune(strings.ReplaceAll(strings.ReplaceAll(name, "\n", " "), "\r", " "))
		if len(runes) > 80 {
			runes = runes[:80]
		}
		state := "停止参与"
		if a.Participating {
			state = "参与调度"
		}
		fmt.Fprintf(&b, "\n<b>%s</b> · %s\n", html.EscapeString(string(runes)), state)
		fmt.Fprintf(&b, "5h 剩余：<b>%s</b>\n重置：%s\n", notificationPercent(a.FiveHourRemaining), notificationTime(a.FiveHourReset))
		fmt.Fprintf(&b, "长期剩余：<b>%s</b>\n重置/到期：%s\n", notificationPercent(a.LongWindowRemaining), notificationTime(a.LongWindowReset))
		fmt.Fprintf(&b, "主动重置剩余：<b>%s</b> · 累计：%s\n", notificationCount(a.ResetCreditsAvailableCount), notificationCount(a.ResetCreditsTotalEarned))
		for i, credit := range a.ResetCredits {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&b, "重置次数到期：%s\n", notificationTime(credit.ExpiresAt))
		}
		fmt.Fprintf(&b, "额度观测：%s\n", notificationTime(a.ObservedAt))
	}
	if len(n.Accounts) == 0 {
		b.WriteString("\n暂无账号额度数据\n")
	}
	return b.String()
}

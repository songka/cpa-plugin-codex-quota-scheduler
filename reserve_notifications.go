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
	LongWindowKind             WindowKind    `json:"long_window_kind,omitempty"`
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

var notificationZone = time.FixedZone("GMT+8", 8*60*60)

const (
	notificationProgressWidth   = 10
	notificationMaxResetRows    = 6
	notificationPayloadLimit    = 4096
	participationDebounceWindow = 30 * time.Second
)

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
	resetCredits := append([]ResetCredit(nil), a.Quota.ResetCredits...)
	sort.SliceStable(resetCredits, func(i, j int) bool {
		left, right := resetCredits[i].ExpiresAt, resetCredits[j].ExpiresAt
		if left.IsZero() {
			return false
		}
		if right.IsZero() {
			return true
		}
		return left.Before(right)
	})
	n := NotificationAccount{
		AuthID: a.AuthID, Alias: a.Annotation.Alias, Participating: active, Reason: reason, ObservedAt: a.LastSuccessAt,
		ResetCreditsAvailableCount: cloneIntPtr(a.Quota.ResetCreditsAvailableCount),
		ResetCreditsTotalEarned:    cloneIntPtr(a.Quota.ResetCreditsTotalEarnedCount),
		ResetCredits:               resetCredits,
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
	if a.Quota.LongWindow != nil {
		n.LongWindowKind = a.Quota.LongWindow.Kind
	}
	return n
}

func notificationForEvent(s StateSnapshot, event, id, reason string) QuotaNotification {
	titles := map[string]string{
		"scheduling_started": "账号开始参与调度", "scheduling_stopped": "账号停止参与调度",
		"usage_report": "定时额度使用报告", "test": "通知测试",
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
	switch n.Event {
	case "scheduling_started", "scheduling_stopped", "usage_report":
	default:
		return
	}
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
	previous    map[string]NotificationAccount
	pending     map[string]pendingParticipationChange
	initialized bool
}

type pendingParticipationChange struct {
	since time.Time
}

func newParticipationTracker() *participationTracker {
	return &participationTracker{
		previous: map[string]NotificationAccount{},
		pending:  map[string]pendingParticipationChange{},
	}
}

func temporaryParticipationReason(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch reason {
	case "upstream_cooldown", "upstream_429_cooldown", "temporary_exhausted":
		return true
	}
	return strings.Contains(reason, "cooldown") || strings.Contains(reason, "temporary")
}

func (t *participationTracker) collect(s StateSnapshot) []QuotaNotification {
	if !t.initialized && !s.CPAAdmission.Observed {
		return nil
	}
	now := s.Now
	if now.IsZero() {
		now = time.Now()
	}
	next := map[string]NotificationAccount{}
	changes := map[string][]NotificationAccount{}
	for _, a := range s.Accounts {
		if a.Provider != "codex" || a.AuthID == "" {
			continue
		}
		current := notificationAccount(a, s)
		if !t.initialized {
			next[a.AuthID] = current
			continue
		}
		previous, known := t.previous[a.AuthID]
		if !known {
			delete(t.pending, a.AuthID)
			next[a.AuthID] = current
			if !current.Participating {
				continue
			}
			changes["scheduling_started"] = append(changes["scheduling_started"], current)
			continue
		}

		if current.Participating {
			// A recovery inside the debounce window cancels the pending
			// temporary stop without publishing either side of the flicker.
			delete(t.pending, a.AuthID)
		}
		if previous.Participating == current.Participating {
			next[a.AuthID] = current
			continue
		}

		if !current.Participating && temporaryParticipationReason(current.Reason) {
			pending, ok := t.pending[a.AuthID]
			if !ok {
				t.pending[a.AuthID] = pendingParticipationChange{since: now}
				next[a.AuthID] = previous
				continue
			}
			if now.Before(pending.since.Add(participationDebounceWindow)) {
				next[a.AuthID] = previous
				continue
			}
			delete(t.pending, a.AuthID)
			next[a.AuthID] = current
			changes["scheduling_stopped"] = append(changes["scheduling_stopped"], current)
			continue
		}

		delete(t.pending, a.AuthID)
		next[a.AuthID] = current
		event := "scheduling_stopped"
		if current.Participating {
			event = "scheduling_started"
		}
		changes[event] = append(changes[event], current)
	}
	for id, previous := range t.previous {
		if _, hidden := s.HiddenAuthIDs[id]; hidden {
			delete(t.pending, id)
			continue
		}
		if _, exists := next[id]; !exists && previous.Participating {
			delete(t.pending, id)
			previous.Participating, previous.Reason = false, "account_removed"
			changes["scheduling_stopped"] = append(changes["scheduling_stopped"], previous)
		}
	}
	var events []QuotaNotification
	for _, event := range []string{"scheduling_stopped", "scheduling_started"} {
		accounts := changes[event]
		if len(accounts) == 0 {
			continue
		}
		sort.Slice(accounts, func(i, j int) bool { return accounts[i].AuthID < accounts[j].AuthID })
		n := notificationForEvent(s, event, "", "")
		n.Accounts = accounts
		if len(accounts) == 1 {
			n.Reason = accounts[0].Reason
		}
		events = append(events, n)
	}
	t.previous = next
	t.initialized = true
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

func notificationsForAccounts(s StateSnapshot, event string) []QuotaNotification {
	n := notificationForEvent(s, event, "", "")
	n.Accounts = nil
	for _, account := range s.Accounts {
		if account.Provider != "codex" || account.AuthID == "" {
			continue
		}
		n.Accounts = append(n.Accounts, notificationAccount(account, s))
	}
	sort.Slice(n.Accounts, func(i, j int) bool { return n.Accounts[i].AuthID < n.Accounts[j].AuthID })
	if len(n.Accounts) == 1 {
		n.Reason = n.Accounts[0].Reason
	}
	if len(n.Accounts) == 0 {
		return nil
	}
	return []QuotaNotification{n}
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
	return t.In(notificationZone).Format("2006-01-02 15:04:05 -07:00")
}

func notificationPercent(v *float64) string {
	if v == nil {
		return "未知"
	}
	return fmt.Sprintf("%.0f%%", *v)
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

func notificationReferenceTime(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now()
	}
	return at
}

func notificationShortTime(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	return t.In(notificationZone).Format("01/02 15:04")
}

func notificationRelativeTime(t, at time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	delta := t.Sub(notificationReferenceTime(at))
	if delta == 0 {
		return "刚刚"
	}
	future := delta > 0
	if !future {
		delta = -delta
	}
	var value int64
	var unit string
	switch {
	case delta >= 24*time.Hour:
		value, unit = int64(delta/(24*time.Hour)), "天"
	case delta >= time.Hour:
		value, unit = int64(delta/time.Hour), "小时"
	case delta >= time.Minute:
		value, unit = int64(delta/time.Minute), "分钟"
	default:
		value, unit = 1, "分钟"
	}
	if future {
		return fmt.Sprintf("%d %s后", value, unit)
	}
	return fmt.Sprintf("%d %s前", value, unit)
}

func notificationExpiryRelativeTime(t, at time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	if t.Before(notificationReferenceTime(at)) {
		return "已到期"
	}
	return notificationRelativeTime(t, at)
}

func notificationWindowResetText(resetAt, at time.Time) string {
	if resetAt.IsZero() {
		return "重置时间未知"
	}
	return fmt.Sprintf("%s · %s", notificationShortTime(resetAt), notificationRelativeTime(resetAt, at))
}

func notificationResetCreditText(expiresAt, at time.Time) string {
	if expiresAt.IsZero() {
		return "未知 · 未知"
	}
	return fmt.Sprintf("%s · %s", notificationShortTime(expiresAt), notificationExpiryRelativeTime(expiresAt, at))
}

func notificationResetCreditsForDisplay(credits []ResetCredit, at time.Time) []ResetCredit {
	reference := notificationReferenceTime(at)
	display := make([]ResetCredit, 0, len(credits))
	for _, credit := range credits {
		status := strings.ToLower(strings.TrimSpace(credit.Status))
		if !credit.UsedAt.IsZero() || status == "used" || status == "consumed" || status == "redeemed" {
			continue
		}
		if !credit.ExpiresAt.IsZero() && credit.ExpiresAt.Before(reference) {
			continue
		}
		display = append(display, credit)
	}
	sort.SliceStable(display, func(i, j int) bool {
		left, right := display[i].ExpiresAt, display[j].ExpiresAt
		if left.IsZero() {
			return false
		}
		if right.IsZero() {
			return true
		}
		return left.Before(right)
	})
	return display
}

func notificationResetCreditCount(a NotificationAccount, available []ResetCredit) string {
	if a.ResetCreditsAvailableCount != nil {
		return notificationCount(a.ResetCreditsAvailableCount)
	}
	if len(available) > 0 {
		count := len(available)
		return fmt.Sprintf("%d 次", count)
	}
	return "未知"
}

func notificationQuotaLabel(a NotificationAccount) string {
	if a.LongWindowKind == WindowMonthly {
		return "月限额"
	}
	return "周限额"
}

func notificationProgressBar(v *float64) string {
	if v == nil || !validPercent(*v) {
		return strings.Repeat("░", notificationProgressWidth)
	}
	percent := *v
	filled := int(percent/100*notificationProgressWidth + 0.5)
	if filled < 0 {
		filled = 0
	}
	if filled > notificationProgressWidth {
		filled = notificationProgressWidth
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", notificationProgressWidth-filled)
}

func notificationAccountName(a NotificationAccount) string {
	if a.Alias != "" {
		return a.Alias
	}
	return a.AuthID
}

func notificationReasonLabel(reason string) string {
	switch strings.TrimSpace(reason) {
	case "":
		return ""
	case "managed_disabled":
		return "插件托管停用"
	case "scheduler_disabled":
		return "插件调度功能已关闭"
	case "roster_unconfirmed":
		return "等待 CPA 调度清单确认"
	case "outside_admission":
		return "不在 CPA 当前调度清单"
	case "upstream_cooldown":
		return "上游临时冷却，模型暂时不可用"
	case "upstream_429_cooldown":
		return "上游返回 429，账号进入临时冷却"
	case "upstream_quota_exhausted":
		return "上游额度已耗尽"
	case "temporary_exhausted":
		return "临时额度耗尽，等待主动重置"
	case "reserve_five_hour":
		return "5 小时额度保护线"
	case "reserve_long_window":
		return "周/月额度保护线"
	case "reserve_multi_window":
		return "多个额度窗口同时触发保护"
	case "reserve_concurrency":
		return "在途请求达到并发限制"
	case "reserve_budget":
		return "预估消耗预算达到限制"
	case "auth_failure":
		return "认证异常，需要重新登录"
	case "local_failure":
		return "本地认证数据异常"
	case "account_removed":
		return "账号已从当前调度清单移除"
	case "quota_or_account_unavailable":
		return "额度或账号暂时不可用"
	case "normal":
		return ""
	default:
		return reason
	}
}

func notificationResetRows(a NotificationAccount, at time.Time) []ResetCredit {
	display := notificationResetCreditsForDisplay(a.ResetCredits, at)
	if len(display) > notificationMaxResetRows {
		return display[:notificationMaxResetRows]
	}
	return display
}

func notificationMarkdown(n QuotaNotification) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n", notificationLabel(n.Title))
	for _, a := range n.Accounts {
		state := "停止参与"
		if a.Participating {
			state = "参与调度"
		}
		fmt.Fprintf(&b, "\n---\n\n**%s** · %s\n\n", notificationLabel(notificationAccountName(a)), state)
		if !a.Participating {
			if reason := notificationReasonLabel(a.Reason); reason != "" {
				fmt.Fprintf(&b, "停止原因：**%s**\n\n", notificationLabel(reason))
			}
		}
		fmt.Fprintln(&b, "**主动重置过期时间（GMT+8）**")
		availableResetCredits := notificationResetCreditsForDisplay(a.ResetCredits, n.At)
		resetRows := notificationResetRows(a, n.At)
		for i, credit := range resetRows {
			fmt.Fprintf(&b, "第 %d 次　%s\n", i+1, notificationResetCreditText(credit.ExpiresAt, n.At))
		}
		if extra := len(availableResetCredits) - len(resetRows); extra > 0 {
			fmt.Fprintf(&b, "另有 %d 次未展开\n", extra)
		}
		fmt.Fprintf(&b, "剩余主动重置：**%s**\n", notificationResetCreditCount(a, availableResetCredits))
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "**5 小时限额**　**%s**\n", notificationPercent(a.FiveHourRemaining))
		fmt.Fprintf(&b, "`%s`\n", notificationProgressBar(a.FiveHourRemaining))
		fmt.Fprintf(&b, "%s\n\n", notificationWindowResetText(a.FiveHourReset, n.At))
		fmt.Fprintf(&b, "**%s**　**%s**\n", notificationQuotaLabel(a), notificationPercent(a.LongWindowRemaining))
		fmt.Fprintf(&b, "`%s`\n", notificationProgressBar(a.LongWindowRemaining))
		fmt.Fprintf(&b, "%s\n", notificationWindowResetText(a.LongWindowReset, n.At))
		if a.ResetCreditsTotalEarned != nil {
			fmt.Fprintf(&b, "累计主动重置：%s\n", notificationCount(a.ResetCreditsTotalEarned))
		}
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
		if n.Event == "scheduling_stopped" {
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

func notificationPages(p TelemetryPolicy, n QuotaNotification) ([]QuotaNotification, error) {
	if len(n.Accounts) == 0 {
		raw, err := json.Marshal(richNotificationBody(p, n))
		if err != nil {
			return nil, err
		}
		if p.NotificationKind != "generic" && len(raw) > notificationPayloadLimit {
			return nil, fmt.Errorf("notification exceeds message size limit")
		}
		return []QuotaNotification{n}, nil
	}
	if p.NotificationKind == "generic" {
		return []QuotaNotification{n}, nil
	}
	pages := make([]QuotaNotification, 0, 1)
	current := n
	current.Accounts = nil
	for _, account := range n.Accounts {
		candidate := current
		candidate.Accounts = append(append([]NotificationAccount(nil), current.Accounts...), account)
		measure := candidate
		measure.Title = fmt.Sprintf("%s (%d/%d)", n.Title, len(n.Accounts), len(n.Accounts))
		raw, err := json.Marshal(richNotificationBody(p, measure))
		if err != nil {
			return nil, err
		}
		if p.NotificationKind != "generic" && len(raw) > notificationPayloadLimit {
			if len(current.Accounts) == 0 {
				return nil, fmt.Errorf("notification account exceeds message size limit")
			}
			pages = append(pages, current)
			current = n
			current.Accounts = []NotificationAccount{account}
			continue
		}
		current = candidate
	}
	if len(current.Accounts) > 0 {
		pages = append(pages, current)
	}
	if len(pages) > 1 {
		for i := range pages {
			pages[i].Title = fmt.Sprintf("%s (%d/%d)", n.Title, i+1, len(pages))
		}
	}
	return pages, nil
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
	pages, err := notificationPages(p, n)
	if err != nil {
		return err
	}
	for _, page := range pages {
		if err := sendQuotaNotificationPage(ctx, p, endpoint, page); err != nil {
			return err
		}
	}
	return nil
}

func sendQuotaNotificationPage(ctx context.Context, p TelemetryPolicy, endpoint string, n QuotaNotification) error {
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
	fmt.Fprintf(&b, "<b>%s</b>\n", html.EscapeString(n.Title))
	for _, a := range n.Accounts {
		runes := []rune(strings.ReplaceAll(strings.ReplaceAll(notificationAccountName(a), "\n", " "), "\r", " "))
		if len(runes) > 80 {
			runes = runes[:80]
		}
		state := "停止参与"
		if a.Participating {
			state = "参与调度"
		}
		fmt.Fprintf(&b, "\n<hr>\n<b>%s</b> · %s\n", html.EscapeString(string(runes)), state)
		if !a.Participating {
			if reason := notificationReasonLabel(a.Reason); reason != "" {
				fmt.Fprintf(&b, "停止原因：<b>%s</b>\n", html.EscapeString(reason))
			}
		}
		fmt.Fprintln(&b, "<b>主动重置过期时间（GMT+8）</b>")
		availableResetCredits := notificationResetCreditsForDisplay(a.ResetCredits, n.At)
		resetRows := notificationResetRows(a, n.At)
		for i, credit := range resetRows {
			fmt.Fprintf(&b, "第 %d 次　%s\n", i+1, html.EscapeString(notificationResetCreditText(credit.ExpiresAt, n.At)))
		}
		if extra := len(availableResetCredits) - len(resetRows); extra > 0 {
			fmt.Fprintf(&b, "另有 %d 次未展开\n", extra)
		}
		fmt.Fprintf(&b, "剩余主动重置：<b>%s</b>\n\n", html.EscapeString(notificationResetCreditCount(a, availableResetCredits)))
		fmt.Fprintf(&b, "<b>5 小时限额</b>　<b>%s</b>\n", html.EscapeString(notificationPercent(a.FiveHourRemaining)))
		fmt.Fprintf(&b, "<code>%s</code>\n%s\n\n", notificationProgressBar(a.FiveHourRemaining), html.EscapeString(notificationWindowResetText(a.FiveHourReset, n.At)))
		fmt.Fprintf(&b, "<b>%s</b>　<b>%s</b>\n", notificationQuotaLabel(a), html.EscapeString(notificationPercent(a.LongWindowRemaining)))
		fmt.Fprintf(&b, "<code>%s</code>\n%s\n", notificationProgressBar(a.LongWindowRemaining), html.EscapeString(notificationWindowResetText(a.LongWindowReset, n.At)))
		if a.ResetCreditsTotalEarned != nil {
			fmt.Fprintf(&b, "累计主动重置：%s\n", html.EscapeString(notificationCount(a.ResetCreditsTotalEarned)))
		}
	}
	if len(n.Accounts) == 0 {
		b.WriteString("\n暂无账号额度数据\n")
	}
	return b.String()
}

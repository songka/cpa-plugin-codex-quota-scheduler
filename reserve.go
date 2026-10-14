package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ReservePolicy excludes unsafe accounts; CPA metadata writes require a separate opt-in.
type ReservePolicy struct {
	ReserveEnabled              bool    `json:"reserve_enabled"`
	ReserveDisableAccounts      bool    `json:"reserve_disable_accounts"`
	Reserve5HPercent            float64 `json:"reserve_5h_percent"`
	ReserveLongPercent          float64 `json:"reserve_long_percent"`
	ReserveStrict               bool    `json:"reserve_strict"`
	ReserveFailClosed           bool    `json:"reserve_fail_closed"`
	ReserveSafetyMarginPercent  float64 `json:"reserve_safety_margin_percent"`
	ReserveMaxInFlight          int     `json:"reserve_max_in_flight"`
	ReserveRequestBudgetPercent float64 `json:"reserve_request_budget_percent"`
	ReserveRequestBudgetAuto    bool    `json:"reserve_request_budget_auto"`
	ReserveRequestBudgetMin     float64 `json:"reserve_request_budget_min_percent"`
	ReserveRequestBudgetMax     float64 `json:"reserve_request_budget_max_percent"`
}
type ReserveInput struct {
	ReserveEnabled              *bool    `json:"reserve_enabled,omitempty" yaml:"reserve_enabled"`
	ReserveDisableAccounts      *bool    `json:"reserve_disable_accounts,omitempty" yaml:"reserve_disable_accounts"`
	Reserve5HPercent            *float64 `json:"reserve_5h_percent,omitempty" yaml:"reserve_5h_percent"`
	ReserveLongPercent          *float64 `json:"reserve_long_percent,omitempty" yaml:"reserve_long_percent"`
	ReserveStrict               *bool    `json:"reserve_strict,omitempty" yaml:"reserve_strict"`
	ReserveFailClosed           *bool    `json:"reserve_fail_closed,omitempty" yaml:"reserve_fail_closed"`
	ReserveSafetyMarginPercent  *float64 `json:"reserve_safety_margin_percent,omitempty" yaml:"reserve_safety_margin_percent"`
	ReserveMaxInFlight          *int     `json:"reserve_max_in_flight,omitempty" yaml:"reserve_max_in_flight"`
	ReserveRequestBudgetPercent *float64 `json:"reserve_request_budget_percent,omitempty" yaml:"reserve_request_budget_percent"`
	ReserveRequestBudgetAuto    *bool    `json:"reserve_request_budget_auto,omitempty" yaml:"reserve_request_budget_auto"`
	ReserveRequestBudgetMin     *float64 `json:"reserve_request_budget_min_percent,omitempty" yaml:"reserve_request_budget_min_percent"`
	ReserveRequestBudgetMax     *float64 `json:"reserve_request_budget_max_percent,omitempty" yaml:"reserve_request_budget_max_percent"`
}
type ReserveOverride struct {
	FiveHour   *float64 `json:"five_hour,omitempty" yaml:"five_hour,omitempty"`
	LongWindow *float64 `json:"long_window,omitempty" yaml:"long_window,omitempty"`
}
type ReserveStatus struct {
	Enabled             bool      `json:"enabled"`
	Blocked             bool      `json:"blocked"`
	Reason              string    `json:"reason"`
	FiveHourMinimum     float64   `json:"five_hour_minimum"`
	LongMinimum         float64   `json:"long_minimum"`
	FiveHourStopLine    float64   `json:"five_hour_stop_line"`
	LongStopLine        float64   `json:"long_stop_line"`
	InFlight            int       `json:"in_flight"`
	EstimatedUnobserved float64   `json:"estimated_unobserved"`
	RequestBudget       float64   `json:"request_budget_percent"`
	BudgetSamples       int       `json:"budget_samples"`
	ValidUntil          time.Time `json:"valid_until,omitempty"`
}

func DefaultReservePolicy() ReservePolicy {
	return ReservePolicy{Reserve5HPercent: 10, ReserveLongPercent: 10, ReserveStrict: true, ReserveFailClosed: true, ReserveSafetyMarginPercent: 2, ReserveMaxInFlight: 1, ReserveRequestBudgetPercent: 0.25, ReserveRequestBudgetAuto: true, ReserveRequestBudgetMin: 0.01, ReserveRequestBudgetMax: 2}
}
func reserveInputFromPolicy(p ReservePolicy) ReserveInput {
	return ReserveInput{
		ReserveEnabled: &p.ReserveEnabled, ReserveDisableAccounts: &p.ReserveDisableAccounts,
		Reserve5HPercent: &p.Reserve5HPercent, ReserveLongPercent: &p.ReserveLongPercent,
		ReserveStrict: &p.ReserveStrict, ReserveFailClosed: &p.ReserveFailClosed,
		ReserveSafetyMarginPercent: &p.ReserveSafetyMarginPercent,
		ReserveMaxInFlight:         &p.ReserveMaxInFlight, ReserveRequestBudgetPercent: &p.ReserveRequestBudgetPercent,
		ReserveRequestBudgetAuto: &p.ReserveRequestBudgetAuto, ReserveRequestBudgetMin: &p.ReserveRequestBudgetMin, ReserveRequestBudgetMax: &p.ReserveRequestBudgetMax,
	}
}
func validPercent(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 100 }
func applyReserveInput(p *ReservePolicy, in ReserveInput) error {
	if in.ReserveRequestBudgetAuto != nil {
		p.ReserveRequestBudgetAuto = *in.ReserveRequestBudgetAuto
	}
	if in.ReserveRequestBudgetMin != nil {
		p.ReserveRequestBudgetMin = *in.ReserveRequestBudgetMin
	}
	if in.ReserveRequestBudgetMax != nil {
		p.ReserveRequestBudgetMax = *in.ReserveRequestBudgetMax
	}
	if !validPercent(p.ReserveRequestBudgetMin) || !validPercent(p.ReserveRequestBudgetMax) ||
		p.ReserveRequestBudgetMin <= 0 || p.ReserveRequestBudgetMin > p.ReserveRequestBudgetMax || p.ReserveRequestBudgetMax > 10 {
		return fmt.Errorf("request budget bounds must satisfy 0 < min <= max <= 10 percent")
	}
	if in.ReserveEnabled != nil {
		p.ReserveEnabled = *in.ReserveEnabled
	}
	if in.ReserveDisableAccounts != nil {
		p.ReserveDisableAccounts = *in.ReserveDisableAccounts
	}
	if in.ReserveStrict != nil {
		p.ReserveStrict = *in.ReserveStrict
	}
	if in.ReserveFailClosed != nil {
		p.ReserveFailClosed = *in.ReserveFailClosed
	}
	for _, v := range []struct {
		dst *float64
		src *float64
	}{{&p.Reserve5HPercent, in.Reserve5HPercent}, {&p.ReserveLongPercent, in.ReserveLongPercent}, {&p.ReserveSafetyMarginPercent, in.ReserveSafetyMarginPercent}, {&p.ReserveRequestBudgetPercent, in.ReserveRequestBudgetPercent}} {
		if v.src != nil {
			if !validPercent(*v.src) {
				return fmt.Errorf("reserve percentages must be finite and between 0 and 100")
			}
			*v.dst = *v.src
		}
	}
	if in.ReserveMaxInFlight != nil {
		if *in.ReserveMaxInFlight < 1 || *in.ReserveMaxInFlight > 1000 {
			return fmt.Errorf("reserve_max_in_flight must be 1..1000")
		}
		p.ReserveMaxInFlight = *in.ReserveMaxInFlight
	}
	if p.Reserve5HPercent+p.ReserveSafetyMarginPercent > 100 || p.ReserveLongPercent+p.ReserveSafetyMarginPercent > 100 {
		return fmt.Errorf("reserve minimum plus safety margin must not exceed 100")
	}
	return nil
}
func validateReserveOverride(o *ReserveOverride, p ReservePolicy) error {
	if o == nil {
		return nil
	}
	for _, v := range []*float64{o.FiveHour, o.LongWindow} {
		if v != nil && (!validPercent(*v) || *v+p.ReserveSafetyMarginPercent > 100) {
			return fmt.Errorf("account reserve minimum plus safety margin must be within 0..100")
		}
	}
	return nil
}
func evaluateReserve(a AccountState, cfg Config, now time.Time) ReserveStatus {
	p := cfg.ReservePolicy
	s := ReserveStatus{Enabled: p.ReserveEnabled, FiveHourMinimum: p.Reserve5HPercent, LongMinimum: p.ReserveLongPercent, Reason: "normal"}
	if o := a.Annotation.ReserveOverride; o != nil {
		if o.FiveHour != nil {
			s.FiveHourMinimum = *o.FiveHour
		}
		if o.LongWindow != nil {
			s.LongMinimum = *o.LongWindow
		}
	}
	s.FiveHourStopLine = s.FiveHourMinimum + p.ReserveSafetyMarginPercent
	s.LongStopLine = s.LongMinimum + p.ReserveSafetyMarginPercent
	if !p.ReserveEnabled {
		return s
	}
	if validateReserveOverride(a.Annotation.ReserveOverride, p) != nil {
		s.Blocked = true
		s.Reason = "reserve_invalid_threshold"
		return s
	}
	block := func(reason string) ReserveStatus { s.Blocked = true; s.Reason = reason; return s }
	if a.Refresh.AuthFailure {
		return block("auth_failure")
	}
	if a.TemporaryExhausted && a.TemporaryResetAt.After(now) {
		return block("upstream_429_cooldown")
	}
	if effectiveCircuitState(a.Circuit, now).EffectiveState == CircuitStateOpen {
		return block("circuit_open")
	}
	for _, w := range []*QuotaWindow{a.Quota.FiveHour, a.Quota.LongWindow} {
		if w != nil && (w.Exhausted || (w.UsedPercent != nil && validPercent(*w.UsedPercent) && *w.UsedPercent >= 100)) {
			return block("upstream_quota_exhausted")
		}
	}
	// An absent optional five-hour window is permitted only alongside a valid long window.
	known := func(w *QuotaWindow) bool { return w != nil && w.UsedPercent != nil && validPercent(*w.UsedPercent) }
	if !known(a.Quota.LongWindow) || a.LastSuccessAt.IsZero() || (a.Quota.FiveHour != nil && !known(a.Quota.FiveHour)) {
		if p.ReserveFailClosed {
			return block("reserve_unknown")
		}
		return s
	}
	if a.Refresh.AuthFailure {
		return block("auth_failure")
	}
	if a.Stale || a.LastSuccessAt.After(now) || now.Sub(a.LastSuccessAt) > cfg.QuotaRefreshInterval {
		if p.ReserveFailClosed {
			return block("reserve_stale")
		}
	}
	s.ValidUntil = a.LastSuccessAt.Add(cfg.QuotaRefreshInterval)
	low5 := known(a.Quota.FiveHour) && 100-*a.Quota.FiveHour.UsedPercent <= s.FiveHourStopLine
	lowLong := 100-*a.Quota.LongWindow.UsedPercent <= s.LongStopLine
	if low5 && lowLong {
		return block("reserve_multi_window")
	}
	if low5 {
		return block("reserve_five_hour")
	}
	if lowLong {
		return block("reserve_long_window")
	}
	// A deadline alone cannot prove an upstream reset: require a newly observed window.
	for _, w := range []*QuotaWindow{a.Quota.FiveHour, a.Quota.LongWindow} {
		if w != nil && !w.ResetAt.IsZero() && !w.ResetAt.After(now) && p.ReserveFailClosed {
			return block("reserve_waiting_reset")
		}
	}
	return s
}
func reserveRejectDecision() PickDecision {
	return PickDecision{Handled: true, Reject: true, RejectCode: "quota_reserve_unavailable", RejectReason: "无符合额度保护条件的账户", Reason: "quota_reserve_unavailable"}
}

type reserveLease struct {
	authID      string
	observed    time.Time
	charged     float64
	fingerprint CredentialFingerprint
}
type reserveLedger struct {
	mu       sync.Mutex
	leases   map[string]reserveLease
	used     map[string]reserveLease
	counts   map[string]int
	learning map[string]*requestBudgetLearning
}

var globalReserveLedger = newReserveLedger()

func newReserveLedger() *reserveLedger {
	return &reserveLedger{leases: make(map[string]reserveLease), used: make(map[string]reserveLease), counts: make(map[string]int), learning: make(map[string]*requestBudgetLearning)}
}
func (l *reserveLedger) release(id string) { l.mu.Lock(); defer l.mu.Unlock(); l.releaseLocked(id) }
func (l *reserveLedger) releaseLocked(id string) {
	if lease, ok := l.leases[id]; ok {
		l.counts[lease.authID]--
		delete(l.leases, id)
	}
}

// The check and allocation share one mutex. Estimated charges remain until fresh
// quota is observed; completion frees concurrency, never invents quota refunds.
func (l *reserveLedger) admit(id string, a AccountState, cfg Config, now time.Time) (ReserveStatus, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := evaluateReserve(a, cfg, now)
	if !s.Enabled {
		return s, true
	}
	if id == "" {
		s.Blocked = true
		s.Reason = "reserve_missing_request_id"
		return s, false
	}
	old, exists := l.leases[id]
	sameLease := exists && old.authID == a.AuthID
	if exists && !sameLease {
		l.releaseLocked(id)
	}
	s.InFlight = l.counts[a.AuthID]
	budget, samples := l.budgetLocked(a, cfg)
	s.RequestBudget, s.BudgetSamples = budget, samples
	if sameLease {
		s.InFlight--
	}
	u := l.used[a.AuthID]
	if a.LastSuccessAt.Before(u.observed) {
		s.Blocked, s.Reason = true, "reserve_stale"
		return s, false
	}
	if a.LastSuccessAt.After(u.observed) || a.CredentialFingerprint != u.fingerprint {
		u = reserveLease{authID: a.AuthID, observed: a.LastSuccessAt, fingerprint: a.CredentialFingerprint}
	}
	s.EstimatedUnobserved = u.charged
	if s.Blocked {
		return s, false
	}
	if s.InFlight >= cfg.ReserveMaxInFlight {
		s.Blocked = true
		s.Reason = "reserve_concurrency"
		return s, false
	}
	for _, w := range []struct {
		quota *QuotaWindow
		line  float64
	}{{a.Quota.FiveHour, s.FiveHourStopLine}, {a.Quota.LongWindow, s.LongStopLine}} {
		if w.quota != nil && w.quota.UsedPercent != nil && 100-*w.quota.UsedPercent-u.charged-budget <= w.line {
			s.Blocked = true
			s.Reason = "reserve_budget"
			return s, false
		}
	}
	u.charged += budget
	l.used[a.AuthID] = u
	l.learning[a.AuthID].requests++
	if !sameLease {
		l.counts[a.AuthID]++
	}
	l.leases[id] = reserveLease{authID: a.AuthID}
	s.InFlight++
	s.EstimatedUnobserved = u.charged
	return s, true
}
func handleReserveIntercept(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	now := time.Now()
	snap := globalState.Snapshot(now)
	if !snap.Config.ReserveEnabled {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	authID := metadataString(req.Metadata, "selected_auth_id")
	for _, a := range snap.Accounts {
		if a.AuthID != authID {
			continue
		}
		if a.Provider != "codex" {
			return okEnvelope(pluginapi.RequestInterceptResponse{})
		}
		s, ok := globalReserveLedger.admit(req.RequestID, a, snap.Config, now)
		if ok {
			return okEnvelope(pluginapi.RequestInterceptResponse{})
		}
		globalState.RecordLog("warn", "reserve.rejected", "额度保护阻止请求", map[string]any{"auth_id": authID, "reason": s.Reason}, now)
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "quota_reserve_unavailable", "type": "quota_reserve", "message": s.Reason}})
		return okEnvelope(pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: http.StatusServiceUnavailable, ResponseBody: body})
	}
	if snap.Config.ReserveStrict && authID != "" {
		auths, err := (ABIHostClient{}).ListAuths()
		codex := req.ToFormat == "codex"
		if err == nil {
			for _, auth := range auths {
				if auth.ID == authID {
					codex = auth.Provider == "codex"
					break
				}
			}
		}
		if codex {
			body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "quota_reserve_unavailable", "message": "unknown Codex quota attribution"}})
			return okEnvelope(pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: http.StatusServiceUnavailable, ResponseBody: body})
		}
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

func cloneFloat64Ptr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

var nestedReserveLeases sync.Map
var nestedReserveSequence atomic.Uint64

// Host nested execution skips the calling plugin's interceptors. Explicitly
// reserve and pin a safe Codex auth before invoking it, retaining the lease for
// the entire nested stream. Non-Codex targets keep their original route.
func reserveNestedAdmit(cfg Config, provider string) (authID, leaseID string, err error) {
	if !cfg.ReserveEnabled || !cfg.ReserveStrict || (provider != "" && provider != "codex") {
		return "", "", nil
	}
	now := time.Now()
	snap := globalState.Snapshot(now)
	auths, e := (ABIHostClient{}).ListAuths()
	if e != nil {
		return "", "", fmt.Errorf("quota_reserve_unavailable: cannot verify nested candidates")
	}
	req := pluginapi.SchedulerPickRequest{Provider: "codex"}
	for _, a := range auths {
		if a.Provider == "codex" && isRefreshEligible(a) {
			req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: a.ID, Provider: a.Provider, Priority: a.Priority})
		}
	}
	byID := map[string]AccountState{}
	for _, a := range snap.Accounts {
		byID[a.AuthID] = a
	}
	leaseID = fmt.Sprintf("nested-reserve-%d", nestedReserveSequence.Add(1))
	for _, candidate := range BuildOrderedAccounts(req, snap, now) {
		if !candidate.Available {
			continue
		}
		if _, ok := globalReserveLedger.admit(leaseID, byID[candidate.AuthID], cfg, now); ok {
			return candidate.AuthID, leaseID, nil
		}
	}
	return "", "", fmt.Errorf("quota_reserve_unavailable: no safe nested Codex account")
}

// Seed new policy fields when decoding pre-reserve persisted configurations.
func (c *Config) UnmarshalJSON(raw []byte) error {
	type plain Config
	decoded := plain(DefaultConfig())
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
		return err
	}
	cfg := Config(decoded)
	if err := applyReserveInput(&cfg.ReservePolicy, reserveInputFromPolicy(cfg.ReservePolicy)); err != nil {
		return err
	}
	if err := applyTelemetryInput(&cfg.TelemetryPolicy, telemetryInputFromPolicy(cfg.TelemetryPolicy)); err != nil {
		return err
	}
	*c = cfg
	return nil
}

func (l *reserveLedger) selectable(a AccountView) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a.ReserveMaxInFlight > 0 && l.counts[a.ID] >= a.ReserveMaxInFlight {
		return false
	}
	u := l.used[a.ID]
	if u.observed.Equal(a.ReserveObservedAt) && u.charged+a.ReserveBudget >= a.ReserveCapacity {
		return false
	}
	return true
}

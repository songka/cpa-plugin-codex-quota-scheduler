package main

import (
	"math"
	"sort"
	"time"
)

type requestBudgetLearning struct {
	baseline AccountState
	requests int
	clean    bool
	samples  []float64
	min, max float64
}

func requestBudgetForView(a AccountState, cfg Config) float64 {
	if !cfg.ReserveEnabled {
		return cfg.ReserveRequestBudgetPercent
	}
	budget, _ := globalReserveLedger.requestBudget(a, cfg)
	return budget
}

func (l *reserveLedger) requestBudget(a AccountState, cfg Config) (float64, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.budgetLocked(a, cfg)
}

// Learn only between clean, increasing observations of the same credential
// and quota cycle. A read during a stream cannot attribute partial consumption.
func (l *reserveLedger) budgetLocked(a AccountState, cfg Config) (float64, int) {
	h := l.learning[a.AuthID]
	if h == nil {
		h = &requestBudgetLearning{}
		l.learning[a.AuthID] = h
	}
	if h.min != cfg.ReserveRequestBudgetMin || h.max != cfg.ReserveRequestBudgetMax {
		h.samples = nil
		h.min, h.max = cfg.ReserveRequestBudgetMin, cfg.ReserveRequestBudgetMax
	}
	if a.LastSuccessAt.After(h.baseline.LastSuccessAt) ||
		(a.LastSuccessAt.Equal(h.baseline.LastSuccessAt) && a.CredentialFingerprint != h.baseline.CredentialFingerprint) {
		delta, sameCycle := requestBudgetDelta(h.baseline, a)
		if !sameCycle {
			h.samples = nil
		}
		if cfg.ReserveRequestBudgetAuto && sameCycle && h.clean && l.counts[a.AuthID] == 0 && h.requests > 0 &&
			a.LastSuccessAt.Sub(h.baseline.LastSuccessAt) <= 2*cfg.QuotaRefreshInterval {
			sample := delta / float64(h.requests)
			// Reject jumps, rather than teaching the model a clamped anomaly.
			if delta <= 20 && sample >= h.min && sample <= h.max &&
				(len(h.samples) < 3 || (sample <= budgetMedian(h.samples)*4 && sample >= budgetMedian(h.samples)/4)) {
				h.samples = append(h.samples, sample)
				if len(h.samples) > 8 {
					h.samples = h.samples[len(h.samples)-8:]
				}
			}
		}
		h.baseline = AccountState{LastSuccessAt: a.LastSuccessAt, CredentialFingerprint: a.CredentialFingerprint, Quota: cloneParsedQuota(a.Quota)}
		h.requests, h.clean = 0, l.counts[a.AuthID] == 0
	}
	budget := cfg.ReserveRequestBudgetPercent
	if !cfg.ReserveRequestBudgetAuto {
		h.samples = nil
		return budget, 0
	}
	if len(h.samples) >= 3 {
		// Headroom for varying request sizes, still within operator bounds.
		budget = budgetMedian(h.samples) * 1.25
	}
	return math.Max(h.min, math.Min(h.max, budget)), len(h.samples)
}

func budgetMedian(samples []float64) float64 {
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[middle-1] + sorted[middle]) / 2
	}
	return sorted[middle]
}

func requestBudgetDelta(before, after AccountState) (float64, bool) {
	if before.LastSuccessAt.IsZero() || before.CredentialFingerprint != after.CredentialFingerprint {
		return 0, false
	}
	var delta float64
	for _, pair := range [][2]*QuotaWindow{{before.Quota.FiveHour, after.Quota.FiveHour}, {before.Quota.LongWindow, after.Quota.LongWindow}} {
		old, next := pair[0], pair[1]
		if old == nil && next == nil {
			continue
		}
		if old == nil || next == nil || old.UsedPercent == nil || next.UsedPercent == nil ||
			!validPercent(*old.UsedPercent) || !validPercent(*next.UsedPercent) ||
			(old.LimitWindowSeconds != nil && next.LimitWindowSeconds != nil && *old.LimitWindowSeconds != *next.LimitWindowSeconds) ||
			old.ResetAt.IsZero() || next.ResetAt.IsZero() || math.Abs(next.ResetAt.Sub(old.ResetAt).Seconds()) > (2*time.Minute).Seconds() ||
			!old.ResetAt.After(after.LastSuccessAt) || *next.UsedPercent < *old.UsedPercent {
			return 0, false
		}
		delta = math.Max(delta, *next.UsedPercent-*old.UsedPercent)
	}
	return delta, before.Quota.LongWindow != nil && after.Quota.LongWindow != nil
}

package main

import (
	"testing"
	"time"
)

func TestReserveRequestBudgetLearnsWithinConfiguredBounds(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	a, cfg := reserveFixture(now, 50, 50)
	cfg.ReserveRequestBudgetAuto = true
	cfg.ReserveRequestBudgetMin = 0.05
	cfg.ReserveRequestBudgetMax = 0.8
	ledger := newReserveLedger()

	if got, samples := ledger.requestBudget(a, cfg); got != cfg.ReserveRequestBudgetPercent || samples != 0 {
		t.Fatalf("initial budget = %v with %d samples, want fixed fallback and no samples", got, samples)
	}

	for i := 0; i < 3; i++ {
		for attempt := 0; attempt < 2; attempt++ {
			if _, ok := ledger.admit("sample", a, cfg, now); !ok {
				t.Fatal("sample request was not admitted")
			}
			ledger.release("sample")
		}
		a.LastSuccessAt = a.LastSuccessAt.Add(time.Minute)
		a.Quota.FiveHour.UsedPercent = reserveFloat(*a.Quota.FiveHour.UsedPercent + 0.8)
		a.Quota.LongWindow.UsedPercent = reserveFloat(*a.Quota.LongWindow.UsedPercent + 0.8)
		now = a.LastSuccessAt
		ledger.requestBudget(a, cfg)
	}

	got, samples := ledger.requestBudget(a, cfg)
	if samples != 3 {
		t.Fatalf("learned sample count = %d, want 3", samples)
	}
	if got < cfg.ReserveRequestBudgetMin || got > cfg.ReserveRequestBudgetMax {
		t.Fatalf("learned budget = %v, outside [%v, %v]", got, cfg.ReserveRequestBudgetMin, cfg.ReserveRequestBudgetMax)
	}
	if got < 0.49 || got > 0.51 {
		t.Fatalf("learned budget = %v, want median plus headroom near 0.5", got)
	}
}

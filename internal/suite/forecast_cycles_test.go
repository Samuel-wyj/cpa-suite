package suite

import (
	"math"
	"testing"
	"time"
)

func obsAt(min int, used float64, reset time.Time) QuotaObs {
	return QuotaObs{At: t0.Add(time.Duration(min) * time.Minute), Used: used, ResetAt: reset, Source: "header"}
}

func TestDetectCycles(t *testing.T) {
	r1 := t0.Add(5 * time.Hour)
	r2 := r1.Add(5 * time.Hour)
	bogus := t0.Add(3 * time.Hour)
	// Scheduled rollover is accepted with one reading; A→B→A blip rejected.
	cycles, rej, pend := DetectCycles([]QuotaObs{obsAt(0, 1, r1), obsAt(10, 3, bogus), obsAt(20, 4, r1), obsAt(301, 1, r2), obsAt(320, 2, r2)}, 5*3600)
	if len(cycles) != 2 || rej != 1 || pend != nil || len(cycles[0].Obs) != 2 || !cycles[1].Start.Equal(r1) {
		t.Fatalf("rollover/ABA: %d cycles rej=%d pend=%v %+v", len(cycles), rej, pend, cycles)
	}
	// An unscheduled change needs two consistent readings; a single last one is pending.
	cycles, rej, pend = DetectCycles([]QuotaObs{obsAt(0, 1, r1), obsAt(10, 2, r1), obsAt(20, 0, bogus)}, 5*3600)
	if len(cycles) != 1 || pend == nil || rej != 0 {
		t.Fatalf("pending: %+v rej=%d pend=%v", cycles, rej, pend)
	}
	cycles, _, pend = DetectCycles([]QuotaObs{obsAt(0, 1, r1), obsAt(10, 2, r1), obsAt(20, 0, bogus), obsAt(30, 1, bogus)}, 5*3600)
	if len(cycles) != 2 || pend != nil {
		t.Fatalf("confirmed change: %+v", cycles)
	}
	// Early reset under the same reset time: 3 non-decreasing readings ≥60 s from ≤5 %.
	cycles, rej, _ = DetectCycles([]QuotaObs{obsAt(0, 40, r1), obsAt(10, 60, r1), obsAt(20, 1, r1), obsAt(21, 1, r1), obsAt(22, 2, r1)}, 5*3600)
	if len(cycles) != 2 || !cycles[1].Early || rej != 0 {
		t.Fatalf("early reset: %+v rej=%d", cycles, rej)
	}
	// A lone dip is an anomaly.
	cycles, rej, _ = DetectCycles([]QuotaObs{obsAt(0, 40, r1), obsAt(10, 60, r1), obsAt(20, 1, r1), obsAt(30, 61, r1)}, 5*3600)
	if len(cycles) != 1 || rej != 1 || peak(cycles[0].Obs) != 61 {
		t.Fatalf("dip: %+v rej=%d", cycles, rej)
	}
}

func TestQuantileConfidence(t *testing.T) {
	v := []float64{10, 20, 30, 40}
	if Quantile(v, 0.5) != 25 || Quantile(v, 0.25) != 17.5 || Quantile(v, 0.75) != 32.5 {
		t.Fatal("quantile")
	}
	for _, c := range []struct {
		n    int
		span float64
		want string
	}{{5, 5, ConfHigh}, {5, 4, ConfMedium}, {2, 2, ConfMedium}, {1, 9, ConfLow}, {0, 0, ConfInsufficient}} {
		if got := Confidence(c.n, c.span); got != c.want {
			t.Fatalf("%+v → %s", c, got)
		}
	}
}

// steady builds a cycle where every whole percent costs $0.20 and 1000 tokens.
func steady(reset time.Time, from, to int, startMin int) ([]QuotaObs, []SpendEvent) {
	var obs []QuotaObs
	var spend []SpendEvent
	for p := from; p <= to; p++ {
		at := t0.Add(time.Duration(startMin+(p-from)*5) * time.Minute)
		obs = append(obs, QuotaObs{At: at, Used: float64(p), ResetAt: reset, Source: "header"})
		spend = append(spend, SpendEvent{At: at.Add(time.Second), USD: 0.2, Tokens: 1000})
	}
	return obs, spend
}

func TestForecast6Milestones(t *testing.T) {
	reset := t0.Add(5 * time.Hour)
	obs, spend := steady(reset, 0, 10, 0)
	now := t0.Add(60 * time.Minute)
	f := Forecast6(Forecast6Input{Provider: "codex", Window: "5h", Seconds: 5 * 3600, Obs: obs, Spend: spend, Now: now})
	if f.CapacitySource != "milestones" || math.Abs(*f.CapacityUSD-20) > 1e-6 || *f.CapacityTokens != 100000 || f.Samples != 10 {
		t.Fatalf("capacity %+v", f)
	}
	if f.Confidence != ConfHigh || f.Coverage != CoverageCPAOnly || math.Abs(*f.RemainingUSD-18) > 1e-6 || *f.RemainingTokens != 90000 {
		t.Fatalf("confidence/coverage/remaining %+v", f)
	}
	// 10 % in 50 min → 12 %/h recent; 1h of 5h elapsed (20 %) vs 10 % used → slow.
	if f.Burn == nil || f.Burn.RecentBasis != "milestones" || math.Abs(f.Burn.Recent-12) > 1e-6 || f.Burn.Pace != "slow" || math.Abs(f.Burn.Sustainable-22.5) > 1e-6 || math.Abs(f.Burn.Cumulative-10) > 1e-6 {
		t.Fatalf("burn %+v", f.Burn)
	}
	if f.ExhaustAt == nil || f.Risk != "medium" { // 90 % at 12 %/h = 7.5h > 4h left → low? no: 7.5h > reset → low
		if f.Risk != "low" {
			t.Fatalf("risk %s", f.Risk)
		}
	}
	// Grok: same data, confidence capped at medium (sampled endpoint).
	g := Forecast6(Forecast6Input{Provider: "xai", Window: "weekly", Seconds: 5 * 3600, Obs: obs, Spend: spend, Now: now})
	if g.Confidence != ConfMedium || g.ConfidenceCap != "sampled_endpoint" {
		t.Fatalf("grok cap %+v", g.Confidence)
	}
	// Usage outside CPA (repeated 3-point rises with no request) → mixed: no USD capacity.
	mixed := append(append([]QuotaObs{}, obs...), QuotaObs{At: t0.Add(52 * time.Minute), Used: 13, ResetAt: reset}, QuotaObs{At: t0.Add(54 * time.Minute), Used: 16, ResetAt: reset}, QuotaObs{At: t0.Add(56 * time.Minute), Used: 19, ResetAt: reset})
	m := Forecast6(Forecast6Input{Provider: "codex", Window: "5h", Seconds: 5 * 3600, Obs: mixed, Spend: spend, Now: now})
	if m.Coverage != CoverageMixed || m.CapacityUSD != nil || m.CapacityNote != "mixed_usage" {
		t.Fatalf("mixed %+v", m)
	}
	// …unless that interval is a suite candy test (its usage is not in request_events).
	ex := []TimeRange{{From: t0.Add(52 * time.Minute), To: t0.Add(55 * time.Minute)}}
	if c := Forecast6(Forecast6Input{Provider: "codex", Window: "5h", Seconds: 5 * 3600, Obs: mixed, Spend: spend, Exclude: ex, Now: now}); c.Coverage != CoverageCPAOnly || c.CapacityUSD == nil {
		t.Fatalf("excluded candy interval %+v", c)
	}
}

func TestForecast6LedgerAndFallback(t *testing.T) {
	r1 := t0.Add(5 * time.Hour)
	o1, s1 := steady(r1, 0, 6, 0)
	r2 := r1.Add(5 * time.Hour)
	o2 := []QuotaObs{{At: r1.Add(10 * time.Minute), Used: 0, ResetAt: r2}}
	now := r1.Add(20 * time.Minute)
	f := Forecast6(Forecast6Input{Provider: "codex", Window: "5h", Seconds: 5 * 3600, Obs: append(o1, o2...), Spend: s1, Now: now})
	if len(f.Ledger) != 1 || f.Ledger[0].PeakPercent != 6 || math.Abs(f.Ledger[0].USD-1.4) > 1e-6 || f.Ledger[0].Samples != 6 || math.Abs(*f.Ledger[0].CapacityUSD-20) > 1e-6 {
		t.Fatalf("ledger %+v", f.Ledger)
	}
	// Current cycle has no milestone interval: capacity from the previous cycle as a prior (0.7).
	if f.CapacitySource != "milestones" || f.CapacityBasis != "prior" || f.PriorCycles != 1 || f.Samples != 0 || math.Abs(*f.CapacityUSD-20) > 1e-6 || f.Confidence != ConfLow {
		t.Fatalf("pooled %+v", f)
	}
	// One reading only, 40 % and $8: 0.5 ratio fallback with low confidence.
	one := Forecast6(Forecast6Input{Provider: "codex", Window: "5h", Seconds: 5 * 3600, Obs: []QuotaObs{{At: t0.Add(30 * time.Minute), Used: 40, ResetAt: r1}},
		Spend: []SpendEvent{{At: t0.Add(10 * time.Minute), USD: 8}}, Now: t0.Add(31 * time.Minute)})
	if one.CapacitySource != "official_ratio" || one.Confidence != ConfLow || *one.CapacityUSD != 20 {
		t.Fatalf("fallback %+v", one)
	}
}

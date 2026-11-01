package suite

import (
	"math"
	"testing"
	"time"
)

// cycleAt: a 5h cycle ending at reset, readings every 10 min from 0 % to peak
// at $usdPerPct per point.
func cycleAt(reset time.Time, peakPct int, usdPerPct float64) ([]QuotaObs, []SpendEvent) {
	start := reset.Add(-5 * time.Hour)
	var obs []QuotaObs
	var spend []SpendEvent
	for p := 0; p <= peakPct; p++ {
		at := start.Add(time.Duration(5+p*10) * time.Minute)
		obs = append(obs, QuotaObs{At: at, Used: float64(p), ResetAt: reset, Source: "header"})
		if p > 0 {
			spend = append(spend, SpendEvent{At: at.Add(-time.Minute), USD: usdPerPct, Tokens: int64(usdPerPct * 5000)})
		}
	}
	return obs, spend
}

func TestForecast7PriorBlendAndBacktest(t *testing.T) {
	r1 := t0.Add(5 * time.Hour)
	r2, r3 := r1.Add(5*time.Hour), r1.Add(10*time.Hour)
	o1, s1 := cycleAt(r1, 20, 0.2) // $20 per 100 %
	o2, s2 := cycleAt(r2, 20, 0.2)
	// Current cycle: two intervals at $0.4/pt ($40 per 100 %).
	o3, s3 := cycleAt(r3, 2, 0.4)
	now := r3.Add(-5*time.Hour + 40*time.Minute)
	obs := append(append(append([]QuotaObs{}, o1...), o2...), o3...)
	spend := append(append(append([]SpendEvent{}, s1...), s2...), s3...)
	f := Forecast6(Forecast6Input{Provider: "codex", Window: "5h", Seconds: 5 * 3600, Obs: obs, Spend: spend, Now: now})
	if f.CapacityBasis != "blend" || f.PriorCycles != 2 || math.Abs(*f.PriorUSD-20) > 1e-6 || f.Confidence == ConfHigh {
		t.Fatalf("blend %+v", f)
	}
	// Log blend of prior $20 (1 unit precision) with 2 samples at $40: exp((ln20 + 2 ln40)/3) ≈ $31.7.
	if c := *f.CapacityUSD; math.Abs(c-math.Exp((math.Log(20)+2*math.Log(40))/3)) > 0.01 || *f.CapacityP25 >= c || *f.CapacityP75 <= c {
		t.Fatalf("blend value %v %v %v", c, *f.CapacityP25, *f.CapacityP75)
	}
	// Once the cycle has ≥ 5 samples at $40 the capacity is the cycle's own and the +100 % change is flagged.
	o3, s3 = cycleAt(r3, 6, 0.4)
	now = r3.Add(-5*time.Hour + 70*time.Minute)
	obs = append(append(append([]QuotaObs{}, o1...), o2...), o3...)
	spend = append(append(append([]SpendEvent{}, s1...), s2...), s3...)
	f = Forecast6(Forecast6Input{Provider: "codex", Window: "5h", Seconds: 5 * 3600, Obs: obs, Spend: spend, Now: now})
	if f.CapacityBasis != "cycle" || math.Abs(*f.CapacityUSD-40) > 1e-6 || f.QuotaChange == nil || math.Abs(*f.QuotaChange-1) > 1e-6 {
		t.Fatalf("cycle + quota change %+v", f)
	}
	// Backtest over the two finished cycles (steady pace → near-perfect prediction).
	if f.Backtest == nil || f.Backtest.Cycles != 2 || f.Backtest.Points != 6 || f.Backtest.MAPE > 0.6 {
		t.Fatalf("backtest %+v", f.Backtest)
	}
	// Projection and band are reported.
	if f.Burn == nil || f.Burn.ProjectedAtReset <= 6 || f.Burn.ProjectedLow > f.Burn.ProjectedAtReset || f.Burn.ProjectedHigh < f.Burn.ProjectedAtReset || f.Burn.AnchorHours != 1.5 {
		t.Fatalf("burn %+v", f.Burn)
	}
}

func TestAnchoredRate(t *testing.T) {
	ms := []Milestone{{At: t0, Percent: 0}, {At: t0.Add(20 * time.Hour), Percent: 10}, {At: t0.Add(23 * time.Hour), Percent: 12}, {At: t0.Add(44 * time.Hour), Percent: 30}}
	// Anchor = last milestone ≤ latest-24h = 20h (10 %): 20 points / 24 h.
	if r, ok := anchoredRate(ms, 24*time.Hour); !ok || math.Abs(r-20.0/24) > 1e-9 {
		t.Fatalf("rate %v %v", r, ok)
	}
	if _, ok := anchoredRate(ms[:1], time.Hour); ok {
		t.Fatal("one milestone")
	}
	if AnchorSpan(7*86400) != 24*time.Hour || AnchorSpan(5*3600) != 90*time.Minute {
		t.Fatal("anchor spans")
	}
}

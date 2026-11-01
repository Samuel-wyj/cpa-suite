package suite

import (
	"math/rand"
	"testing"
	"time"
)

// replay of the reported screenshot: Codex 5h window
// 14:13:09 → 19:13:09 (UTC+8), only CPA traffic from a coding agent (parallel,
// long streaming requests), $7.04 spent, 82 % used by 15:39. Quota headers are
// read when a response starts, so a long request's usage shows up between two
// readings although no request *started* there. 0.7 flagged this as
// "usage outside CPA".
var lastReplayObs []QuotaObs
var lastReplaySpend []SpendEvent

// agentReplayOld is 0.7's rule on the same data: a ≥2-point rise
// with no request *completed* between two readings means "outside CPA".
func agentReplayOld() string {
	agentReplay(false)
	idx := newSpendIndex(lastReplaySpend)
	for k := 1; k < len(lastReplayObs); k++ {
		a, b := lastReplayObs[k-1], lastReplayObs[k]
		if _, _, n := idx.between(a.At, b.At); n == 0 && b.Used-a.Used >= mixedMinJump {
			return CoverageMixed
		}
	}
	return CoverageCPAOnly
}

func agentReplay(keepStart bool) (Forecast6Result, Forecast6Result) {
	loc := time.FixedZone("UTC+8", 8*3600)
	start := time.Date(2026, 10, 15, 14, 13, 9, 0, loc).UTC()
	now := time.Date(2026, 10, 15, 15, 40, 0, 0, loc).UTC()
	const capUSD5h, capUSDWeek = 8.6, 54.0
	rng := rand.New(rand.NewSource(7))
	type req struct {
		at, end time.Time
		usd     float64
	}
	var reqs []req
	total := 0.0
	for t := start.Add(20 * time.Second); t.Before(now.Add(-8 * time.Minute)); t = t.Add(time.Duration(20+rng.Intn(70)) * time.Second) {
		usd := 0.01 + rng.Float64()*0.03
		if rng.Intn(6) == 0 {
			usd = 0.2 + rng.Float64()*0.3 // big context compaction / long tool turn
		}
		lat := time.Duration(60+rng.Intn(360)) * time.Second
		reqs = append(reqs, req{t, t.Add(lat), usd}) // at = start, end = completion
		total += usd
	}
	scale := 7.04 / total
	var spend []SpendEvent
	for i := range reqs {
		reqs[i].usd *= scale
		e := SpendEvent{At: reqs[i].end, USD: reqs[i].usd, Tokens: int64(reqs[i].usd * 5.6e6)} // request_events.at = completion
		if keepStart {
			e.Start = reqs[i].at
		}
		spend = append(spend, e)
	}
	used := func(t time.Time, cap float64) float64 {
		s := 0.0 // upstream books a streaming request while it runs
		for _, r := range reqs {
			switch {
			case !r.end.After(t):
				s += r.usd
			case r.at.Before(t):
				s += r.usd * float64(t.Sub(r.at)) / float64(r.end.Sub(r.at))
			}
		}
		return float64(int(s / cap * 100))
	}
	var o5, ow []QuotaObs
	for _, r := range reqs {
		at := r.at.Add(2 * time.Second) // header after ttft
		o5 = append(o5, QuotaObs{At: at, Used: used(at, capUSD5h), ResetAt: start.Add(5 * time.Hour), Source: "header"})
		ow = append(ow, QuotaObs{At: at, Used: used(at, capUSDWeek), ResetAt: start.Add(7 * 24 * time.Hour), Source: "header"})
	}
	lastReplayObs, lastReplaySpend = o5, spend
	f5 := Forecast6(Forecast6Input{Provider: "codex", Window: "5h", Seconds: 5 * 3600, Obs: o5, Spend: spend, Now: now})
	fw := Forecast6(Forecast6Input{Provider: "codex", Window: "weekly", Seconds: 7 * 86400, Obs: ow, Spend: spend, Now: now})
	return f5, fw
}

func TestReplayScreenshotNoExternalUsage(t *testing.T) {
	// Old attribution (request start only) reproduces the false "mixed".
	if old := agentReplayOld(); old != CoverageMixed {
		t.Fatalf("replay should reproduce the 0.7 false positive, got %s", old)
	}
	f5, fw := agentReplay(true)
	if f5.Coverage == CoverageMixed || f5.CapacityNote == "mixed_usage" {
		t.Fatalf("5h: CPA-only agent traffic flagged as external usage: %s %s", f5.Coverage, f5.CapacityNote)
	}
	if fw.Coverage == CoverageMixed {
		t.Fatalf("weekly flagged mixed")
	}
	if f5.UsedPercent == nil || *f5.UsedPercent < 75 || *f5.UsedPercent > 85 {
		t.Fatalf("replay not matching screenshot: used %v", f5.UsedPercent)
	}
	if f5.CapacityUSD == nil || *f5.CapacityUSD < 5 || *f5.CapacityUSD > 13 {
		t.Fatalf("5h capacity should be estimated near $8.6, got %v", f5.CapacityUSD)
	}
}

func TestReplayRealExternalUsageStillMixed(t *testing.T) {
	// Real external use: the agent pauses (no CPA requests at all) while
	// another client burns quota; a few CPA probes see the jumps.
	loc := time.FixedZone("UTC+8", 8*3600)
	start := time.Date(2026, 10, 15, 14, 13, 9, 0, loc).UTC()
	reset := start.Add(5 * time.Hour)
	obs := []QuotaObs{{At: start.Add(10 * time.Minute), Used: 5, ResetAt: reset}}
	spend := []SpendEvent{{Start: start.Add(9 * time.Minute), At: start.Add(10 * time.Minute), USD: 0.4}}
	for i := 1; i <= 4; i++ {
		at := start.Add(time.Duration(10+15*i) * time.Minute)
		obs = append(obs, QuotaObs{At: at, Used: float64(5 + 4*i), ResetAt: reset})
		// the tiny CPA request whose header gave the reading, nothing else in between
		spend = append(spend, SpendEvent{Start: at.Add(-time.Second), At: at.Add(2 * time.Second), USD: 0.001})
	}
	f := Forecast6(Forecast6Input{Provider: "codex", Window: "5h", Seconds: 5 * 3600, Obs: obs, Spend: spend, Now: start.Add(80 * time.Minute)})
	if f.Coverage != CoverageMixed {
		t.Fatalf("real external usage must stay mixed, got %s", f.Coverage)
	}
}

package plugin

import (
	"testing"
	"time"

	"cpa-key-billing/internal/suite"
)

// replay: the quota-risk false positives from the screenshots
// ("weekly risk high, 97 % left", "5h risk high, 80 % left") and true cases.

// replayWindow simulates one window: readings every 10 min from start to now
// following used(t), with $1 of spend per percent point.
func replayWindow(t *testing.T, key string, seconds int64, start, now time.Time, used func(h float64) float64) forecastWindow {
	t.Helper()
	reset := start.Add(time.Duration(seconds) * time.Second)
	var obs []suite.QuotaObs
	var spend []suite.SpendEvent
	prev := 0.0
	for at := start.Add(5 * time.Minute); !at.After(now); at = at.Add(10 * time.Minute) {
		u := used(at.Sub(start).Hours())
		obs = append(obs, suite.QuotaObs{At: at, Used: u, ResetAt: reset, Source: "header"})
		if d := u - prev; d > 0 {
			spend = append(spend, suite.SpendEvent{At: at.Add(-time.Minute), USD: d, Tokens: int64(d * 1000)})
		}
		prev = u
	}
	f := suite.Forecast6(suite.Forecast6Input{Provider: "codex", Window: key, Seconds: seconds, Obs: obs, Spend: spend, Now: now})
	return forecastWindow{Key: key, Seconds: seconds, Forecast6Result: f}
}

func TestReplayQuotaAlerts(t *testing.T) {
	st := suite.DefaultAlertSettings()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	week := int64(7 * 86400)
	cases := []struct {
		name    string
		key     string
		seconds int64
		elapsed time.Duration
		used    func(h float64) float64
		want    string // "" = no alert
	}{
		// Screenshot: weekly reset 2 h ago, a burst to 3 % (97 % left). 0.6: "high".
		{"weekly 97% left, burst after reset", "weekly", week, 2 * time.Hour, func(h float64) float64 { return 1.5 * h }, ""},
		// Weekly, day 3, 10 % used but a heavy last day (projection > 100 %), 90 % left.
		{"weekly 90% left, heavy day", "weekly", week, 60 * time.Hour, func(h float64) float64 {
			if h < 36 {
				return 0
			}
			return (h - 36) * 0.42
		}, ""},
		// Screenshot: 5h window, 20 % used in the first 40 min (80 % left). 0.6: "high".
		{"5h 80% left, early burst", "5h", 5 * 3600, 2 * time.Hour, func(h float64) float64 {
			if h < 2.0/3 {
				return h * 30
			}
			return 20
		}, ""},
		// 5h window right after the reset: quiet period.
		{"5h first 40 min", "5h", 5 * 3600, 40 * time.Minute, func(h float64) float64 { return h * 60 }, ""},
		// True: weekly day 5, 80 % used at 0.67 %/h → 112 % at reset, 20 % left → medium.
		{"weekly 20% left, runs out", "weekly", week, 120 * time.Hour, func(h float64) float64 { return h * 0.6667 }, "medium"},
		// True: weekly 92 % used → 8 % left → high.
		{"weekly 8% left", "weekly", week, 130 * time.Hour, func(h float64) float64 { return h * 0.7077 }, "high"},
		// True: 5h window 3.5 h in, 25 %/h for the last 90 min, 25 % left → medium.
		{"5h 25% left, runs out", "5h", 5 * 3600, 210 * time.Minute, func(h float64) float64 {
			if h < 2 {
				return h * 18.75
			}
			return 37.5 + (h-2)*25
		}, "medium"},
		// True: 5h window used up 1.5 h before the reset → high (≥ 25 % of the window early).
		{"5h runs out early", "5h", 5 * 3600, 150 * time.Minute, func(h float64) float64 { return h * 32 }, "high"},
		// Weekly 75 % used but slowing: projection < 100 % → no alert.
		{"weekly 25% left, slow pace", "weekly", week, 150 * time.Hour, func(h float64) float64 {
			if h < 120 {
				return h * 0.6
			}
			return 72 + (h-120)*0.1
		}, ""},
	}
	for _, c := range cases {
		start := now.Add(-c.elapsed)
		w := replayWindow(t, c.key, c.seconds, start, now, c.used)
		sev, ok := quotaAlertSeverity(w, st)
		if c.name == "weekly 97% left, burst after reset" && w.Risk != "high" {
			t.Errorf("replay must reproduce the pace-only 'high' risk that 0.6 alerted on, got %s", w.Risk)
		}
		if c.want == "" && ok || c.want != "" && sev != c.want {
			rem, proj := -1.0, -1.0
			if w.RemainingPercent != nil {
				rem = *w.RemainingPercent
			}
			if w.Burn != nil {
				proj = w.Burn.ProjectedAtReset
			}
			t.Errorf("%s: got %q (ok=%v), want %q; remaining %.1f projected %.1f risk %s", c.name, sev, ok, c.want, rem, proj, w.Risk)
		}
	}
}

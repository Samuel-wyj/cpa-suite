package suite

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func pf(v float64) *float64 { return &v }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestForecastOfficialRatio(t *testing.T) {
	// Weekly Codex window, 40% used, $20 spent → capacity $50, $30 left.
	// Burn $1/h over the last 24h → exhausts in 30h, reset in 3 days → high (< half the time left).
	f := Forecast(WindowInput{Seconds: 7 * 86400, ResetAt: t0.Add(72 * time.Hour), UsedPercent: pf(40), PercentSource: "header",
		UsedUSD: 20, RecentUSD: 24, RecentSlice: 24 * time.Hour, Now: t0})
	if f.CapacitySource != "official_ratio" || !near(*f.CapacityUSD, 50) || !near(*f.RemainingUSD, 30) || !near(*f.RemainingPercent, 60) {
		t.Fatalf("%+v", f)
	}
	if !near(f.BurnUSDPerHour, 1) || f.ExhaustAt == nil || !f.ExhaustAt.Equal(t0.Add(30*time.Hour)) || f.Risk != "high" || f.ExhaustBasis != "usd" {
		t.Fatalf("burn/exhaust %+v", f)
	}
	if !f.Start.Equal(t0.Add(72*time.Hour - 7*24*time.Hour)) {
		t.Fatalf("start %v", f.Start)
	}
}

func TestForecastRiskLevels(t *testing.T) {
	base := WindowInput{Seconds: 5 * 3600, ResetAt: t0.Add(4 * time.Hour), UsedPercent: pf(50), UsedUSD: 5, RecentSlice: time.Hour, Now: t0}
	cases := []struct {
		burn float64
		want string
	}{{10, "high"}, {2, "medium"}, {1, "low"}, {0, "low"}}
	for _, c := range cases {
		in := base
		in.RecentUSD = c.burn // capacity 10, remaining 5 → 0.5h / 2.5h / 5h / never
		if got := Forecast(in); got.Risk != c.want {
			t.Fatalf("burn %v: risk %s (%+v)", c.burn, got.Risk, got)
		}
	}
	in := base
	in.UsedPercent = pf(100)
	if f := Forecast(in); f.Risk != "exhausted" || !near(*f.RemainingUSD, 0) {
		t.Fatalf("exhausted %+v", f)
	}
}

func TestForecastSmallPercentIsNotEnoughData(t *testing.T) {
	// The real SuperGrok reading: creditUsagePercent 1.0 (1 %, 0–100 scale).
	f := Forecast(WindowInput{Seconds: 7 * 86400, ResetAt: t0.Add(6 * 24 * time.Hour), UsedPercent: pf(1), PercentSource: "api", UsedUSD: 0.42, Now: t0})
	if f.CapacityUSD != nil || f.CapacityNote != NoteNotEnoughData || !near(*f.RemainingPercent, 99) {
		t.Fatalf("1%% must not produce a capacity: %+v", f)
	}
	// With history the estimate is the median of earlier reliable cycles.
	f = Forecast(WindowInput{Seconds: 7 * 86400, ResetAt: t0.Add(6 * 24 * time.Hour), UsedPercent: pf(1), UsedUSD: 0.42, History: []float64{40, 100, 50}, Now: t0})
	if f.CapacitySource != "estimated" || !near(*f.CapacityUSD, 50) || !near(*f.RemainingUSD, 49.5) || f.CapacityNote != NoteHistory {
		t.Fatalf("history %+v", f)
	}
	// Below the threshold exactly at the boundary → ratio is used.
	f = Forecast(WindowInput{Seconds: 18000, ResetAt: t0.Add(time.Hour), UsedPercent: pf(MinRatioPercent), UsedUSD: 1, Now: t0})
	if f.CapacitySource != "official_ratio" || !near(*f.CapacityUSD, 20) {
		t.Fatalf("boundary %+v", f)
	}
}

func TestForecastEdgeCases(t *testing.T) {
	// No official percent at all.
	f := Forecast(WindowInput{Seconds: 18000, ResetAt: t0.Add(time.Hour), UsedUSD: 3, Now: t0})
	if f.CapacityUSD != nil || f.CapacityNote != NoteNoPercent || f.Risk != "unknown" {
		t.Fatalf("no percent %+v", f)
	}
	// Percent but nothing priced (missing prices).
	f = Forecast(WindowInput{Seconds: 18000, ResetAt: t0.Add(time.Hour), UsedPercent: pf(30), UsedUSD: 0, Unpriced: 7, Now: t0})
	if f.CapacityUSD != nil || f.CapacityNote != NoteNoUsage || f.Unpriced != 7 {
		t.Fatalf("unpriced %+v", f)
	}
	// Reading from a window that already reset: percent is dropped, the window rolls forward.
	f = Forecast(WindowInput{Seconds: 18000, ResetAt: t0.Add(-30 * time.Minute), UsedPercent: pf(80), UsedUSD: 2, Now: t0})
	if f.UsedPercent != nil || !f.Stale || f.CapacityNote != NoteReset || !f.ResetAt.Equal(t0.Add(270*time.Minute)) || !f.Start.Equal(t0.Add(-30*time.Minute)) {
		t.Fatalf("reset %+v", f)
	}
	// Percent-based exhaustion when no USD capacity is known.
	f = Forecast(WindowInput{Seconds: 18000, ResetAt: t0.Add(4 * time.Hour), UsedPercent: pf(2), UsedUSD: 0.001, PercentRate: pf(49), Now: t0})
	if f.ExhaustBasis != "percent" || f.ExhaustAt == nil || !f.ExhaustAt.Equal(t0.Add(2*time.Hour)) || f.Risk != "medium" {
		t.Fatalf("percent basis %+v", f)
	}
	// Out-of-range percents are clamped.
	f = Forecast(WindowInput{Seconds: 18000, ResetAt: t0.Add(time.Hour), UsedPercent: pf(130), UsedUSD: 1, Now: t0})
	if *f.UsedPercent != 100 || f.Risk != "exhausted" {
		t.Fatalf("clamp %+v", f)
	}
}

func TestRecentSliceAndCycleCapacity(t *testing.T) {
	if RecentSlice(18000) != time.Hour || RecentSlice(7*86400) != 24*time.Hour || RecentSlice(600) != time.Hour {
		t.Fatal("slice")
	}
	if c, ok := CycleCapacity(40, 20); !ok || c != 50 {
		t.Fatal("cycle")
	}
	for _, bad := range [][2]float64{{1, 20}, {40, 0}, {120, 5}} {
		if _, ok := CycleCapacity(bad[0], bad[1]); ok {
			t.Fatalf("%v must be unreliable", bad)
		}
	}
}

func TestExtDatabaseV1ToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.suite-ext.db")
	// A 0.4 (v1) database with client-guard data.
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(extMigrations[0] + `; INSERT INTO suite_meta VALUES('client_guard.rules','{"x":1}'); PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e, err := openExt(path)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	_ = e.db.QueryRow("PRAGMA user_version").Scan(&v)
	var raw map[string]int
	if ok, _ := e.get("client_guard.rules", &raw); v != extSchemaVersion || !ok || raw["x"] != 1 {
		t.Fatalf("v=%d data=%v", v, raw)
	}
	c := &Controller{ext: e, now: func() time.Time { return t0 }}
	s := QuotaSample{AuthIndex: "a", Provider: "xai", Window: "weekly", WindowSeconds: 604800, At: t0, UsedPercent: 1, ResetAt: t0.Add(time.Hour), Source: "api"}
	old := s
	old.At = t0.Add(-41 * 24 * time.Hour)
	if err := c.AddQuotaSamples([]QuotaSample{s, s, old}); err != nil {
		t.Fatal(err)
	}
	got, err := c.QuotaSamples("a", time.Time{})
	if err != nil || len(got) != 1 || got[0].UsedPercent != 1 || !got[0].ResetAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("samples %+v %v (duplicates ignored, >40 days pruned)", got, err)
	}
	if err := c.PutQuotaFetch(QuotaFetch{AuthIndex: "a", Provider: "xai", At: t0, OK: true, Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	if f, ok, err := c.LastQuotaFetch("a"); !ok || err != nil || !f.OK || !f.At.Equal(t0) {
		t.Fatalf("fetch %+v %v %v", f, ok, err)
	}
	// Documented downgrade back to v1 (rollback to 0.4) keeps the rest.
	if _, err := e.db.Exec(`DROP TABLE monitor_samples; DROP TABLE alerts; DROP TABLE alert_mutes; DROP TABLE alert_events; DROP TABLE forecast_ledger; DROP TABLE candy_runs; DROP TABLE quota_samples; DROP TABLE quota_fetch; PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	e.close()
	e, err = openExt(path) // and re-upgrading works
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.get("client_guard.rules", &raw); !ok {
		t.Fatal("data lost across downgrade")
	}
	e.close()
}

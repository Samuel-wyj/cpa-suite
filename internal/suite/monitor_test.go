package suite

import (
	"path/filepath"
	"testing"
	"time"
)

func monSamples(n int, from time.Time, step time.Duration, f func(i int, s *MonitorSample)) []MonitorSample {
	out := make([]MonitorSample, n)
	for i := range out {
		out[i] = MonitorSample{At: from.Add(time.Duration(i) * step), AuthIndex: "a", Provider: "xai", Model: "grok-4", ResponseModel: "grok-4",
			Effort: "high", Stream: true, Output: int64(800 + i%50), Reasoning: int64(2000 + i%100), LatencyMS: 9000, TTFTMS: 900}
		if f != nil {
			f(i, &out[i])
		}
	}
	return out
}

func TestMannWhitney(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	b := []float64{11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	if p := MannWhitneyLess(a, b); p > 0.001 {
		t.Fatalf("clearly less: p=%v", p)
	}
	if p := MannWhitneyLess(b, a); p < 0.99 {
		t.Fatalf("clearly greater: p=%v", p)
	}
	if p := MannWhitneyLess([]float64{5, 5, 5}, []float64{5, 5, 5}); p < 0.4 {
		t.Fatalf("all ties: p=%v", p)
	}
	if Median([]float64{3, 1, 2, 10}) != 2.5 {
		t.Fatal("median")
	}
}

func TestSameModel(t *testing.T) {
	for _, c := range [][2]string{{"grok-4", "Grok-4"}, {"xai/grok-4", "grok-4"}, {"gpt-5", "gpt-5-2025-08-07"}} {
		if !SameModel(c[0], c[1]) {
			t.Fatalf("%v should match", c)
		}
	}
	if SameModel("grok-4", "grok-3-mini") {
		t.Fatal("different models matched")
	}
}

func TestEvaluateMonitor(t *testing.T) {
	now := t0
	base := monSamples(300, now.Add(-7*24*time.Hour-23*time.Hour), 30*time.Minute, nil)
	// Stable: no signal.
	stable := append(append([]MonitorSample{}, base...), monSamples(80, now.Add(-23*time.Hour), 15*time.Minute, nil)...)
	if b := EvaluateMonitor(stable, now); len(b) != 1 || b[0].Verdict != "ok" {
		t.Fatalf("stable = %+v", b)
	}
	// Too few recent samples.
	few := append(append([]MonitorSample{}, base...), monSamples(10, now.Add(-2*time.Hour), time.Minute, nil)...)
	if b := EvaluateMonitor(few, now); b[0].Verdict != "insufficient" {
		t.Fatalf("few = %+v", b[0])
	}
	// Reasoning and output collapse alone (soft only, 0.7): observe, not suspect.
	bad := append(append([]MonitorSample{}, base...), monSamples(80, now.Add(-23*time.Hour), 15*time.Minute, func(i int, s *MonitorSample) {
		s.Reasoning, s.Output = int64(600+i%20), int64(300+i%20)
	})...)
	b := EvaluateMonitor(bad, now)
	if b[0].Verdict != "observe" || !b[0].Soft || b[0].Hard != 0 || b[0].Severity != "" {
		t.Fatalf("bad = %+v", b[0])
	}
	// Soft collapse plus a hard signal (error rate) → suspect medium.
	badErr := append(append([]MonitorSample{}, base...), monSamples(80, now.Add(-23*time.Hour), 15*time.Minute, func(i int, s *MonitorSample) {
		s.Reasoning, s.Output = int64(600+i%20), int64(300+i%20)
		if i%8 == 0 {
			s.Failed, s.Status, s.ErrKind = true, 500, "server"
		}
	})...)
	if b := EvaluateMonitor(badErr, now); b[0].Verdict != "suspect" || b[0].Severity != "medium" || b[0].Hard < 1 {
		t.Fatalf("bad+errors = %+v", b[0])
	}
	// One soft signal only (output) → observe.
	one := append(append([]MonitorSample{}, base...), monSamples(80, now.Add(-23*time.Hour), 15*time.Minute, func(i int, s *MonitorSample) { s.Output = int64(300 + i%20) })...)
	if b := EvaluateMonitor(one, now); b[0].Verdict != "observe" || b[0].Deviating != 1 {
		t.Fatalf("one = %+v", b[0])
	}
	// Reasoning never reported → unavailable, not a drop.
	zero := func(i int, s *MonitorSample) { s.Reasoning = 0 }
	nr := append(monSamples(300, now.Add(-7*24*time.Hour-23*time.Hour), 30*time.Minute, zero), monSamples(80, now.Add(-23*time.Hour), 15*time.Minute, zero)...)
	if b := EvaluateMonitor(nr, now); !b[0].ReasoningZero || b[0].Verdict == "suspect" {
		t.Fatalf("reasoning unreported = %+v", b[0])
	}
	// A different task mix in another bucket (effort low, short outputs) does not trigger the high bucket.
	mixed := append(append([]MonitorSample{}, stable...), monSamples(80, now.Add(-23*time.Hour), 15*time.Minute, func(i int, s *MonitorSample) {
		s.Effort, s.Output, s.Reasoning = "low", 50, 10
	})...)
	for _, b := range EvaluateMonitor(mixed, now) {
		if b.Verdict == "suspect" {
			t.Fatalf("task change triggered %+v", b)
		}
	}
	// Model mismatch ≥5% plus errors → suspect high.
	mm := append(append([]MonitorSample{}, base...), monSamples(80, now.Add(-23*time.Hour), 15*time.Minute, func(i int, s *MonitorSample) {
		if i%10 == 0 {
			s.ResponseModel = "grok-3-mini"
		}
		if i%8 == 0 {
			s.Failed, s.Status, s.ErrKind = true, 500, "server"
		}
	})...)
	if b := EvaluateMonitor(mm, now); b[0].Verdict != "suspect" || b[0].Severity != "high" {
		t.Fatalf("mismatch = %+v", b[0])
	}
}

func TestMonitorStoreAndAlerts(t *testing.T) {
	e, err := openExt(filepath.Join(t.TempDir(), "x.suite-ext.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	now := t0
	c := &Controller{ext: e, now: func() time.Time { return now }}
	c.ObserveSample(MonitorSample{At: now.Add(-15 * 24 * time.Hour), AuthIndex: "a", Provider: "xai", Model: "m"})
	c.ObserveSample(MonitorSample{At: now.Add(-time.Hour), AuthIndex: "a", Provider: "xai", Model: "m", Stream: true, Output: 5})
	got, err := c.MonitorSamples(time.Time{})
	if err != nil || len(got) != 1 || !got[0].Stream || got[0].Output != 5 {
		t.Fatalf("samples %+v %v (14-day prune, stream kept)", got, err)
	}

	if !c.AlertsDue(now, false) || c.AlertsDue(now.Add(30*time.Second), false) || !c.AlertsDue(now.Add(30*time.Second), true) {
		t.Fatal("60s throttle")
	}
	d := DetectedAlert{Source: AlertMonitor, Object: "a", Rule: "r", Severity: "medium", Payload: map[string]any{"x": 1}}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Legacy (0.6) semantics for this test: open at once, close at once, no cooldown.
	if _, err := c.SaveAlertSettings(map[string]any{"open_after_evaluations": 1, "close_after_minutes": 0, "reopen_cooldown_hours": 0}, false); err != nil {
		t.Fatal(err)
	}
	must(c.SyncAlerts([]DetectedAlert{d}, []string{AlertMonitor}, now))
	must(c.SyncAlerts([]DetectedAlert{d}, []string{AlertMonitor}, now.Add(time.Minute))) // merged, not duplicated
	s, _ := c.AlertState(now, 50)
	if len(s.Alerts) != 1 || s.Alerts[0].Count != 1 || !s.Alerts[0].LastAt.Equal(now.Add(time.Minute)) || s.Badge != 1 || s.Highest != "medium" {
		t.Fatalf("merge %+v", s)
	}
	if ok, _ := c.AckAlert(d.ID(), now); !ok {
		t.Fatal("ack")
	}
	if s, _ = c.AlertState(now, 50); s.Badge != 0 || s.Alerts[0].AckBy != AlertOperator {
		t.Fatalf("ack %+v", s.Alerts[0])
	}
	d.Severity = "high"
	must(c.SyncAlerts([]DetectedAlert{d}, []string{AlertMonitor}, now.Add(2*time.Minute)))
	if s, _ = c.AlertState(now, 50); s.Badge != 1 || s.Alerts[0].AckAt != nil {
		t.Fatalf("escalation must clear ack %+v", s.Alerts[0])
	}
	if _, err := c.MuteAlerts(AlertMonitor, "a", time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if s, _ = c.AlertState(now, 50); s.Badge != 0 {
		t.Fatal("muted alerts leave the badge")
	}
	if s, _ = c.AlertState(now.Add(2*time.Hour), 50); s.Badge != 1 {
		t.Fatal("expired mute")
	}
	// Not evaluated → untouched; evaluated and gone → resolved.
	must(c.SyncAlerts(nil, []string{AlertQuota}, now.Add(3*time.Minute)))
	if s, _ = c.AlertState(now.Add(2*time.Hour), 50); s.Alerts[0].Status != "active" {
		t.Fatal("unevaluated source resolved")
	}
	must(c.SyncAlerts(nil, []string{AlertMonitor}, now.Add(4*time.Minute)))
	if s, _ = c.AlertState(now.Add(2*time.Hour), 50); s.Alerts[0].Status != "resolved" || s.Badge != 0 {
		t.Fatalf("resolve %+v", s.Alerts[0])
	}
	must(c.SyncAlerts([]DetectedAlert{d}, []string{AlertMonitor}, now.Add(5*time.Minute)))
	if s, _ = c.AlertState(now.Add(2*time.Hour), 50); s.Alerts[0].Status != "active" {
		t.Fatal("reopen")
	}
	kinds := map[string]bool{}
	for _, h := range s.History {
		kinds[h.Kind] = true
	}
	for _, k := range []string{"opened", "acked", "escalated", "muted", "resolved", "reopened"} {
		if !kinds[k] {
			t.Fatalf("history lacks %s: %+v", k, s.History)
		}
	}
	// Retention: 90 days.
	must(c.SyncAlerts(nil, []string{AlertMonitor}, now.Add(6*time.Minute)))
	must(c.SyncAlerts(nil, []string{AlertMonitor}, now.Add(91*24*time.Hour)))
	if s, _ = c.AlertState(now.Add(91*24*time.Hour), 50); len(s.Alerts) != 0 || len(s.History) != 0 {
		t.Fatalf("retention %+v", s)
	}
	// Downgrade v3 → v2 (documented) and re-upgrade.
	if _, err := e.db.Exec(`DROP TABLE monitor_samples; DROP TABLE alerts; DROP TABLE alert_mutes; DROP TABLE alert_events; DROP TABLE forecast_ledger; DROP TABLE candy_runs; PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
}

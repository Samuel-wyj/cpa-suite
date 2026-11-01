package suite

import (
	"path/filepath"
	"testing"
	"time"
)

// replay tests: synthetic data reproducing the false positives seen
// on the user's server (screenshots of 2026-10-15) plus true cases that must
// still fire.

// codexBucket builds the screenshot bucket: Codex gpt-6.1-sol, xhigh, stream.
// short/long inputs have their own reasoning/output levels.
func codexSamples(n int, from, to time.Time, shortShare float64, scale float64, f func(i int, s *MonitorSample)) []MonitorSample {
	out := make([]MonitorSample, n)
	step := to.Sub(from) / time.Duration(n)
	for i := range out {
		s := MonitorSample{At: from.Add(time.Duration(i) * step), AuthIndex: "cx", Provider: "codex", Model: "gpt-6.1-sol", ResponseModel: "gpt-6.1-sol",
			Effort: "xhigh", Stream: true, LatencyMS: 20000, TTFTMS: 1500}
		j := int64(i % 40)
		if float64(i%100) < shortShare*100 {
			s.Input, s.Reasoning, s.Output = 1200+j*10, 100+j, 380+j*2 // short follow-ups
		} else {
			s.Input, s.Reasoning, s.Output = 30000+j*100, 420+j*2, 900+j*3 // long coding turns
		}
		s.Reasoning = int64(float64(s.Reasoning) * scale)
		s.Output = int64(float64(s.Output) * scale)
		if f != nil {
			f(i, &s)
		}
		out[i] = s
	}
	return out
}

func TestReplayDegradationScreenshot(t *testing.T) {
	now := t0
	recentFrom := now.Add(-MonitorRecent)
	base := codexSamples(201, now.Add(-MonitorRecent-MonitorBaseline), recentFrom, 0.4, 1, nil)

	// 1) Screenshot: 506 recent calls, mostly short follow-ups → medians fall
	//    (reasoning ~112 vs ~261, output ~398 vs ~662), hard metrics normal.
	recent := codexSamples(506, recentFrom, now, 0.9, 1, nil)
	b := EvaluateMonitor(append(append([]MonitorSample{}, base...), recent...), now)[0]
	if b.Verdict == "suspect" || b.Hard != 0 {
		t.Fatalf("task-mix shift must not be suspect: %+v", b)
	}
	if b.Verdict != "ok" {
		t.Fatalf("with input strata the shift is explained (ok), got %s %+v", b.Verdict, b.Signals)
	}
	var rs, os float64
	for _, s := range b.Signals {
		if s.ID == "reasoning" {
			rs = *s.Recent / *s.Baseline
		}
		if s.ID == "output" {
			os = *s.Recent / *s.Baseline
		}
	}
	if rs > 0.6 || os > 0.75 {
		t.Fatalf("replay must reproduce the screenshot's raw drops (reasoning %.2f, output %.2f)", rs, os)
	}

	// 2) Same data recorded by 0.6 (no input tokens): unstratified → observe at most.
	strip := func(list []MonitorSample) []MonitorSample {
		out := append([]MonitorSample{}, list...)
		for i := range out {
			out[i].Input = 0
		}
		return out
	}
	b = EvaluateMonitor(append(strip(base), strip(recent)...), now)[0]
	if b.Verdict != "observe" || b.Severity != "" {
		t.Fatalf("tokens-only drop without strata must be observe: %+v", b)
	}

	// 3) True degradation: same task mix, tokens halved in every stratum and a
	//    hard signal (stream disconnects +8 points) → suspect medium.
	bad := codexSamples(506, recentFrom, now, 0.4, 0.45, func(i int, s *MonitorSample) {
		if i%12 == 0 {
			s.Failed, s.Status, s.ErrKind = true, 502, "truncated"
		}
	})
	b = EvaluateMonitor(append(append([]MonitorSample{}, base...), bad...), now)[0]
	if b.Verdict != "suspect" || b.Severity != "medium" || b.Hard < 1 || !b.Soft {
		t.Fatalf("true degradation must be suspect medium: %+v", b)
	}

	// 4) Tokens halved but nothing hard → observe (candy test can confirm).
	soft := codexSamples(506, recentFrom, now, 0.4, 0.45, nil)
	b = EvaluateMonitor(append(append([]MonitorSample{}, base...), soft...), now)[0]
	if b.Verdict != "observe" || !b.Soft {
		t.Fatalf("soft-only must be observe: %+v", b)
	}

	// 5) Model swap (10 % other model) → suspect high, as before.
	mm := codexSamples(506, recentFrom, now, 0.4, 1, func(i int, s *MonitorSample) {
		if i%10 == 0 {
			s.ResponseModel = "gpt-5.6-mini"
		}
	})
	b = EvaluateMonitor(append(append([]MonitorSample{}, base...), mm...), now)[0]
	if b.Verdict != "suspect" || b.Severity != "high" {
		t.Fatalf("model swap must be suspect high: %+v", b)
	}

	// 6) A drop confined to one 6-hour segment is not persistent → observe.
	burst := codexSamples(506, recentFrom, now, 0.4, 1, func(i int, s *MonitorSample) {
		if s.At.After(now.Add(-6 * time.Hour)) {
			s.Reasoning, s.Output = s.Reasoning/4, s.Output/4
		}
		if i%12 == 0 {
			s.Failed, s.Status, s.ErrKind = true, 502, "truncated"
		}
	})
	b = EvaluateMonitor(append(append([]MonitorSample{}, base...), burst...), now)[0]
	if b.Verdict == "suspect" {
		t.Fatalf("one bad segment must not be suspect: %+v", b)
	}
}

func TestReplayAlertDebounce(t *testing.T) {
	newC := func(t *testing.T) *Controller {
		e, err := openExt(filepath.Join(t.TempDir(), "x.suite-ext.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { e.close() })
		return &Controller{ext: e, now: func() time.Time { return t0 }}
	}
	d := DetectedAlert{Source: AlertMonitor, Object: "cx", Rule: "degradation", Severity: "medium", Payload: map[string]any{}}
	// A condition hovering at its threshold: seen every other minute for 3 hours.
	run := func(c *Controller) (opened, reopened int, s AlertSnapshot) {
		for m := 0; m < 180; m++ {
			var det []DetectedAlert
			if m%2 == 0 {
				det = []DetectedAlert{d}
			}
			if err := c.SyncAlerts(det, []string{AlertMonitor}, t0.Add(time.Duration(m)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		s, _ = c.AlertState(t0.Add(3*time.Hour), 1000)
		for _, h := range s.History {
			switch h.Kind {
			case "opened":
				opened++
			case "reopened":
				reopened++
			}
		}
		return
	}
	legacy := newC(t)
	if _, err := legacy.SaveAlertSettings(map[string]any{"open_after_evaluations": 1, "close_after_minutes": 0, "reopen_cooldown_hours": 0}, false); err != nil {
		t.Fatal(err)
	}
	o, r, _ := run(legacy)
	if o != 1 || r < 80 {
		t.Fatalf("0.6 behaviour reproduces the flapping: opened %d reopened %d", o, r)
	}
	o, r, s := run(newC(t))
	if o != 1 || r != 0 || len(s.Alerts) != 1 || s.Alerts[0].Count != 1 || s.Alerts[0].Status != "active" {
		t.Fatalf("0.7 debounce: opened %d reopened %d %+v", o, r, s.Alerts)
	}

	// A single blip never opens (needs a second detection within 30 min).
	c := newC(t)
	_ = c.SyncAlerts([]DetectedAlert{d}, []string{AlertMonitor}, t0.Add(-2*time.Hour))
	_ = c.SyncAlerts(nil, []string{AlertMonitor}, t0.Add(-119*time.Minute))
	_ = c.SyncAlerts(nil, []string{AlertMonitor}, t0.Add(-80*time.Minute))
	if s, _ := c.AlertState(t0, 100); len(s.Alerts) != 0 || s.Badge != 0 {
		t.Fatalf("blip opened %+v", s.Alerts)
	}
	// Sustained: opens, closes 30 min after it stops, recurrence within 6 h stays quiet.
	_ = c.SyncAlerts([]DetectedAlert{d}, []string{AlertMonitor}, t0.Add(2*time.Minute))
	_ = c.SyncAlerts([]DetectedAlert{d}, []string{AlertMonitor}, t0.Add(3*time.Minute))
	if s, _ := c.AlertState(t0, 100); len(s.Alerts) != 1 || s.Badge != 1 {
		t.Fatalf("sustained must open %+v", s)
	}
	_ = c.SyncAlerts(nil, []string{AlertMonitor}, t0.Add(4*time.Minute))
	_ = c.SyncAlerts(nil, []string{AlertMonitor}, t0.Add(20*time.Minute))
	if s, _ := c.AlertState(t0, 100); s.Alerts[0].Status != "active" {
		t.Fatal("must stay active for 30 min")
	}
	_ = c.SyncAlerts(nil, []string{AlertMonitor}, t0.Add(35*time.Minute))
	if s, _ := c.AlertState(t0.Add(35*time.Minute), 100); s.Alerts[0].Status != "resolved" || s.Alerts[0].CoolingTil == nil {
		t.Fatalf("must resolve after 30 min %+v", s.Alerts[0])
	}
	_ = c.SyncAlerts([]DetectedAlert{d}, []string{AlertMonitor}, t0.Add(2*time.Hour))
	if s, _ := c.AlertState(t0.Add(2*time.Hour), 100); s.Alerts[0].Status != "resolved" || s.Badge != 0 || s.Alerts[0].Hits < 2 {
		t.Fatalf("recurrence in cooldown must stay quiet %+v", s.Alerts[0])
	}
	hi := d
	hi.Severity = "high"
	_ = c.SyncAlerts([]DetectedAlert{hi}, []string{AlertMonitor}, t0.Add(2*time.Hour+time.Minute))
	if s, _ := c.AlertState(t0.Add(2*time.Hour), 100); s.Alerts[0].Status != "active" || s.Alerts[0].Severity != "high" {
		t.Fatalf("escalation breaks the cooldown %+v", s.Alerts[0])
	}
	// Info alerts are listed but never counted in the badge.
	info := DetectedAlert{Source: AlertUsage, Object: "k", Rule: "key_new_usd", Severity: "info", Payload: map[string]any{}}
	_ = c.SyncAlerts([]DetectedAlert{info}, []string{AlertUsage}, t0.Add(3*time.Hour))
	_ = c.SyncAlerts([]DetectedAlert{info}, []string{AlertUsage}, t0.Add(3*time.Hour+time.Minute))
	s, _ = c.AlertState(t0.Add(3*time.Hour), 100)
	if s.Badge != 1 {
		t.Fatalf("info must not count: badge %d", s.Badge)
	}
}

func TestAlertSettingsPrecedence(t *testing.T) {
	e, err := openExt(filepath.Join(t.TempDir(), "x.suite-ext.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	c := &Controller{ext: e, now: func() time.Time { return t0 }, cfg: Config{Alerts: map[string]any{"quota_remain_weekly": 25}}}
	v := c.AlertSettings()
	if v.Settings.QuotaRemainWeekly != 25 || len(v.Locked) != 1 || v.Settings.QuotaRemain5h != 40 {
		t.Fatalf("config override %+v", v)
	}
	if _, err := c.SaveAlertSettings(map[string]any{"quota_remain_weekly": 50}, false); err == nil {
		t.Fatal("locked key must be rejected")
	}
	v, err = c.SaveAlertSettings(map[string]any{"quota_remain_5h": 35, "min_severity": map[string]any{"usage": "medium"}}, false)
	if err != nil || v.Settings.QuotaRemain5h != 35 || v.Settings.MinSeverity["usage"] != "medium" || v.Settings.MinSeverity["quota"] != "info" {
		t.Fatalf("save %+v %v", v, err)
	}
	if _, err := c.SaveAlertSettings(map[string]any{"quota_remain_5h": 140}, false); err == nil {
		t.Fatal(">100 % must be rejected")
	}
	if _, err := c.SaveAlertSettings(map[string]any{"nope": 1}, false); err == nil {
		t.Fatal("unknown key must be rejected")
	}
	v, _ = c.SaveAlertSettings(nil, true)
	if v.Settings.QuotaRemain5h != 40 || v.Settings.QuotaRemainWeekly != 25 {
		t.Fatalf("reset keeps config %+v", v.Settings)
	}
	if err := ValidateAlertConfig(map[string]any{"open_after_evaluations": "x"}); err == nil {
		t.Fatal("bad config must fail")
	}
}

package suite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedAlertsQ(t *testing.T) (*Controller, time.Time) {
	t.Helper()
	e, err := openExt(filepath.Join(t.TempDir(), "q.suite-ext.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.close() })
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	c := &Controller{ext: e, now: func() time.Time { return now }}
	sevs := []string{"high", "medium", "info"}
	srcs := []string{AlertQuota, AlertMonitor, AlertAuthErrors}
	for i := 0; i < 300; i++ {
		status, resolved := "active", int64(0)
		if i%3 == 0 {
			status, resolved = "resolved", now.Add(-time.Duration(i)*time.Hour).UnixNano()
		}
		ack := int64(0)
		if i%7 == 0 && status == "active" {
			ack = now.UnixNano()
		}
		obj := fmt.Sprintf("acc%02d", i%20)
		payload := fmt.Sprintf(`{"name":"=evil%d@example.com","provider":"codex"}`, i%20)
		if _, err := e.db.Exec(`INSERT INTO alerts(id, source, object, rule, severity, status, payload, first_at, last_at, resolved_at, count, ack_at, hits) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,1)`,
			fmt.Sprintf("a%03d", i), srcs[i%3], obj, fmt.Sprintf("r%d", i%4), sevs[i%3], status, payload,
			now.Add(-time.Duration(i)*time.Hour).UnixNano(), now.Add(-time.Duration(i)*time.Minute).UnixNano(), resolved, i+1, ack); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Exec(`INSERT INTO alert_events(alert_id, at, kind, severity) VALUES(?,?,?,?)`, fmt.Sprintf("a%03d", i), now.Add(-time.Duration(i)*time.Minute).UnixNano(), "opened", sevs[i%3]); err != nil {
			t.Fatal(err)
		}
	}
	return c, now
}

func TestAlertQueryPagingFiltersBadge(t *testing.T) {
	c, now := seedAlertsQ(t)
	if _, err := c.MuteAlerts(AlertMonitor, "acc01", time.Hour, now); err != nil {
		t.Fatal(err)
	}
	snap, err := c.AlertState(now, 10)
	if err != nil {
		t.Fatal(err)
	}
	n, h, err := c.AlertBadge(now)
	if err != nil || n != snap.Badge || h != snap.Highest {
		t.Fatalf("badge %d/%s vs snapshot %d/%s (%v)", n, h, snap.Badge, snap.Highest, err)
	}
	all, _ := c.QueryAlerts(AlertQuery{Status: "all", Limit: 200}, now)
	if all.Total != 300 || len(all.Items) != 200 {
		t.Fatalf("all: %d %d", all.Total, len(all.Items))
	}
	p2, _ := c.QueryAlerts(AlertQuery{Status: "all", Limit: 200, Offset: 200}, now)
	if len(p2.Items) != 100 || p2.Items[0].ID == all.Items[0].ID {
		t.Fatalf("page 2: %d", len(p2.Items))
	}
	act, _ := c.QueryAlerts(AlertQuery{}, now)
	if act.Total != 200 || act.Limit != 50 {
		t.Fatalf("active %d", act.Total)
	}
	for _, a := range act.Items {
		if a.Status != "active" {
			t.Fatal("non-active in active filter")
		}
	}
	hi, _ := c.QueryAlerts(AlertQuery{Status: "all", Severity: "high", Source: AlertQuota}, now)
	if hi.Total != 100 {
		t.Fatalf("high quota %d", hi.Total)
	}
	q, _ := c.QueryAlerts(AlertQuery{Status: "all", Q: "evil5@"}, now)
	if q.Total != 15 {
		t.Fatalf("q %d", q.Total)
	}
	if pct, _ := c.QueryAlerts(AlertQuery{Status: "all", Q: "%"}, now); pct.Total != 0 {
		t.Fatalf("LIKE wildcard not escaped: %d", pct.Total)
	}
	muted, _ := c.QueryAlerts(AlertQuery{Status: "muted"}, now)
	if muted.Total != 5 || muted.Items[0].MutedUntil == nil {
		t.Fatalf("muted %d", muted.Total)
	}
	since, _ := c.QueryAlerts(AlertQuery{Status: "all", Since: now.Add(-60 * time.Minute)}, now)
	if since.Total != 61 {
		t.Fatalf("since %d", since.Total)
	}
	g, _ := c.QueryAlerts(AlertQuery{Status: "all", Group: true, Limit: 10}, now)
	if g.Total != 60 || len(g.Groups) != 10 || g.Groups[0].Alerts != 5 || len(g.Payload0(t)) == 0 {
		t.Fatalf("groups %+v", g.Total)
	}
	sum, _ := c.AlertSummaryAt(now)
	if sum.Active != 200 || sum.Resolved != 100 || sum.Badge != snap.Badge || len(sum.Top) != 3 || sum.Muted != 5 {
		t.Fatalf("summary %+v", sum)
	}
	hist, total, _ := c.AlertHistory("", AlertQuota, "", 0, 20)
	if total < 100 || len(hist) != 20 || hist[0].Source != AlertQuota || !strings.Contains(string(hist[0].Payload), "name") {
		t.Fatalf("history %d %+v", total, hist[0])
	}
}

func (p AlertPage) Payload0(t *testing.T) string { return string(p.Groups[0].Payload) }

func TestAlertBulkClearCSV(t *testing.T) {
	c, now := seedAlertsQ(t)
	n, err := c.AckAlerts([]string{"a001", "a002", "a003", "missing"}, now)
	if err != nil || n != 2 { // a003 is resolved
		t.Fatalf("ack %d %v", n, err)
	}
	csvb, err := c.AlertsCSV(AlertQuery{Status: "all"}, now)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(csvb)), "\n")
	if len(lines) != 301 || !strings.HasPrefix(lines[0], "\ufeffid,") {
		t.Fatalf("csv lines %d", len(lines))
	}
	if !strings.Contains(string(csvb), ",'=evil") {
		t.Fatal("formula injection not defused")
	}
	// cooldown 6 h by default: resolved more than 6 h ago go.
	removed, err := c.ClearResolved(now)
	if err != nil || removed == 0 || removed >= 100 {
		t.Fatalf("clear %d %v", removed, err)
	}
	left, _ := c.QueryAlerts(AlertQuery{Status: "resolved"}, now)
	if left.Total != 100-int(removed) {
		t.Fatalf("left %d", left.Total)
	}
}

func TestRetentionSavePrune(t *testing.T) {
	c, now := seedAlertsQ(t)
	if r := c.CurrentRetention(); r != RetentionDefaults {
		t.Fatalf("defaults %+v", r)
	}
	if _, err := c.SaveRetention(Retention{AlertsDays: 3, MonitorDays: 14, CandyDays: 90, QuotaDays: 40}, false, now); err == nil {
		t.Fatal("out of bounds accepted")
	}
	_, _ = c.ext.db.Exec(`INSERT INTO candy_runs(auth_index, provider, model, started_at, status) VALUES('x','codex','m',?, 'done'), ('x','codex','m',?, 'running')`,
		now.Add(-20*24*time.Hour).UnixNano(), now.Add(-20*24*time.Hour).UnixNano())
	_, _ = c.ext.db.Exec(`UPDATE alerts SET resolved_at = ? WHERE id = 'a000'`, now.Add(-10*24*time.Hour).UnixNano())
	v, err := c.SaveRetention(Retention{AlertsDays: 7, MonitorDays: 14, CandyDays: 7, QuotaDays: 40}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if v.Retention.CandyDays != 7 || v.Counts["candy_runs"] != 1 || v.Counts["alerts"] >= 300 {
		t.Fatalf("after prune %+v", v)
	}
	c.retention.Store(nil) // reload from suite_meta
	if c.CurrentRetention().CandyDays != 7 {
		t.Fatal("not persisted")
	}
	if v, _ := c.SaveRetention(Retention{}, true, now); v.Retention != RetentionDefaults {
		t.Fatal("reset")
	}
}

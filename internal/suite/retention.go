package suite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// retention days for the extension tables, set on the page and
// stored in suite_meta (no schema change). Bounds keep the heuristics working:
// the monitor needs its 7-day baseline + 24 h recent window, the forecast reads
// 35 days of quota history.

const retentionKey = "retention"

// Retention is the effective retention, in days.
type Retention struct {
	AlertsDays  int `json:"alerts_days"`
	MonitorDays int `json:"monitor_days"`
	CandyDays   int `json:"candy_days"`
	QuotaDays   int `json:"quota_days"`
}

// RetentionBound is the allowed range of one field.
type RetentionBound struct {
	Min     int `json:"min"`
	Max     int `json:"max"`
	Default int `json:"default"`
}

// RetentionDefaults match the fixed values used up to test6.
var RetentionDefaults = Retention{AlertsDays: 90, MonitorDays: 14, CandyDays: 90, QuotaDays: 40}

// RetentionBounds per JSON field name.
var RetentionBounds = map[string]RetentionBound{
	"alerts_days":  {7, 365, 90},
	"monitor_days": {9, 60, 14},
	"candy_days":   {7, 365, 90},
	"quota_days":   {36, 180, 40},
}

// RetentionView is what the page shows.
type RetentionView struct {
	Retention Retention                 `json:"retention"`
	Defaults  Retention                 `json:"defaults"`
	Bounds    map[string]RetentionBound `json:"bounds"`
	Counts    map[string]int64          `json:"counts"`
}

func (r Retention) validate() error {
	for name, v := range map[string]int{"alerts_days": r.AlertsDays, "monitor_days": r.MonitorDays, "candy_days": r.CandyDays, "quota_days": r.QuotaDays} {
		b := RetentionBounds[name]
		if v < b.Min || v > b.Max {
			return fmt.Errorf("%s 必须在 %d–%d 天之间", name, b.Min, b.Max)
		}
	}
	return nil
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// CurrentRetention returns the effective retention, loading it once. It takes
// extMu on first use, so never call it inside withExt (use retentionCached).
func (c *Controller) CurrentRetention() Retention {
	if r := c.retention.Load(); r != nil {
		return *r
	}
	r := RetentionDefaults
	_ = c.withExt(func(db *sql.DB) error {
		var raw string
		if db.QueryRow(`SELECT value FROM suite_meta WHERE key = ?`, retentionKey).Scan(&raw) == nil {
			var saved Retention
			if json.Unmarshal([]byte(raw), &saved) == nil && saved.validate() == nil {
				r = saved
			}
		}
		return nil
	})
	c.retention.Store(&r)
	return r
}

// retentionCached never touches the DB (for use while holding extMu).
func (c *Controller) retentionCached() Retention {
	if r := c.retention.Load(); r != nil {
		return *r
	}
	return RetentionDefaults
}

// RetentionState returns the effective values and row counts.
func (c *Controller) RetentionState() (RetentionView, error) {
	v := RetentionView{Retention: c.CurrentRetention(), Defaults: RetentionDefaults, Bounds: RetentionBounds, Counts: map[string]int64{}}
	err := c.withExt(func(db *sql.DB) error {
		for _, t := range []string{"alerts", "alert_events", "monitor_samples", "candy_runs", "quota_samples"} {
			var n int64
			if err := db.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n); err != nil {
				return err
			}
			v.Counts[t] = n
		}
		return nil
	})
	return v, err
}

// SaveRetention validates, stores and immediately prunes; reset restores defaults.
func (c *Controller) SaveRetention(r Retention, reset bool, now time.Time) (RetentionView, error) {
	if reset {
		r = RetentionDefaults
	}
	if err := r.validate(); err != nil {
		return RetentionView{}, err
	}
	raw, _ := json.Marshal(r)
	err := c.withExt(func(db *sql.DB) error {
		var err error
		if reset {
			_, err = db.Exec(`DELETE FROM suite_meta WHERE key = ?`, retentionKey)
		} else {
			_, err = db.Exec(`INSERT INTO suite_meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, retentionKey, string(raw))
		}
		if err != nil {
			return err
		}
		c.retention.Store(&r)
		return pruneAll(db, r, now)
	})
	if err != nil {
		return RetentionView{}, err
	}
	return c.RetentionState()
}

func pruneAll(db *sql.DB, r Retention, now time.Time) error {
	cut := func(d int) int64 { return now.Add(-days(d)).UnixNano() }
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{`DELETE FROM alert_events WHERE at < ?`, cut(r.AlertsDays)},
		{`DELETE FROM alerts WHERE status = 'resolved' AND resolved_at < ?`, cut(r.AlertsDays)},
		{`DELETE FROM monitor_samples WHERE at < ?`, cut(r.MonitorDays)},
		{`DELETE FROM candy_runs WHERE started_at < ? AND status != 'running'`, cut(r.CandyDays)},
		{`DELETE FROM quota_samples WHERE at < ?`, cut(r.QuotaDays)},
	} {
		if _, err := db.Exec(q.sql, q.arg); err != nil {
			return err
		}
	}
	return nil
}

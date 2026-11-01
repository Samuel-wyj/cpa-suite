package suite

import (
	"database/sql"
	"encoding/json"
	"math"
	"strings"
	"time"
)

// Page-only alert center (0.6). Alerts are derived on demand from the
// other modules' read-only results; nothing is pushed anywhere.

// Alert sources.
const (
	AlertQuota      = "quota"       // forecast risk high/exhausted
	AlertUsage      = "usage"       // key usage far above its own baseline
	AlertBlocks     = "blocks"      // client-guard block spike
	AlertMonitor    = "monitor"     // passive degradation heuristic
	AlertAuthErrors = "auth_errors" // upstream 401/403 bursts
)

const (
	AlertRetention   = 90 * 24 * time.Hour
	AlertEventCap    = 50000
	AlertEvalEvery   = 60 * time.Second
	AlertOperator    = "管理员" // admin login has no user name (approved)
	AlertMuteForever = int64(1<<62 - 1)
)

// DetectedAlert is one condition currently true.
type DetectedAlert struct {
	Source   string         `json:"source"`
	Object   string         `json:"object"`
	Rule     string         `json:"rule"`
	Severity string         `json:"severity"` // info | medium | high
	Payload  map[string]any `json:"payload"`
}

// ID is stable per source + object + rule so repeats merge into one alert.
func (d DetectedAlert) ID() string { return d.Source + "|" + d.Object + "|" + d.Rule }

// Alert is the stored state.
type Alert struct {
	ID         string          `json:"id"`
	Source     string          `json:"source"`
	Object     string          `json:"object"`
	Rule       string          `json:"rule"`
	Severity   string          `json:"severity"`
	Status     string          `json:"status"` // active | resolved
	Payload    json.RawMessage `json:"payload"`
	FirstAt    time.Time       `json:"first_at"`
	LastAt     time.Time       `json:"last_at"`
	ResolvedAt *time.Time      `json:"resolved_at,omitempty"`
	Count      int64           `json:"count"`
	Hits       int64           `json:"hits"`                     // detections (incl. quiet ones while cooling down)
	CoolingTil *time.Time      `json:"cooldown_until,omitempty"` // resolved, recurrences stay quiet until then
	AckAt      *time.Time      `json:"ack_at,omitempty"`
	AckBy      string          `json:"ack_by,omitempty"`
	MutedUntil *time.Time      `json:"muted_until,omitempty"`
}

// AlertEvent is one history entry.
type AlertEvent struct {
	AlertID  string    `json:"alert_id"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"` // opened | reopened | escalated | acked | muted | unmuted | resolved
	Severity string    `json:"severity,omitempty"`
	By       string    `json:"by,omitempty"`
}

func severityRank(s string) int {
	switch s {
	case "high":
		return 3
	case "medium":
		return 2
	case "info":
		return 1
	}
	return 0
}

// AlertsDue reports whether a new evaluation may run (at most once a minute)
// and claims the slot when it does.
func (c *Controller) AlertsDue(now time.Time, force bool) bool {
	c.alertMu.Lock()
	defer c.alertMu.Unlock()
	if !force && !c.alertEval.IsZero() && now.Sub(c.alertEval) < AlertEvalEvery {
		return false
	}
	c.alertEval = now
	return true
}

// SyncAlerts merges the conditions found by the evaluated sources (0.7
// debounce): a new condition opens after OpenAfter consecutive evaluations
// (quota at once — its own rules already require sustained pace; a pending
// condition is forgotten when not seen again within CloseAfterMin), an active
// alert resolves only after CloseAfterMin minutes without a detection, and a
// condition returning within ReopenCooldownH hours of resolving only updates
// the resolved alert (count, last seen) unless its severity rises. Alerts of
// sources not evaluated this time (module off, read failed) keep their state.
func (c *Controller) SyncAlerts(detected []DetectedAlert, evaluated []string, now time.Time) error {
	st := c.AlertSettings().Settings
	ret := c.CurrentRetention()
	seen := map[string]bool{}
	ns := now.UnixNano()
	closeAfter := int64(st.CloseAfterMin * float64(time.Minute))
	cooldown := int64(st.ReopenCooldownH * float64(time.Hour))
	return c.withExt(func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		event := func(id, kind, sev, by string) error {
			_, err := tx.Exec(`INSERT INTO alert_events(alert_id, at, kind, severity, by) VALUES(?,?,?,?,?)`, id, ns, kind, sev, by)
			return err
		}
		for _, d := range detected {
			if min := st.MinSeverity[d.Source]; min != "" && severityRank(d.Severity) < severityRank(min) {
				continue
			}
			id := d.ID()
			if seen[id] {
				continue
			}
			seen[id] = true
			openAfter := int64(math.Max(1, math.Round(st.OpenAfter)))
			if d.Source == AlertQuota {
				openAfter = 1
			}
			payload, _ := json.Marshal(d.Payload)
			var status, sev, ackSev string
			var ackAt, resolvedAt, hits int64
			err := tx.QueryRow(`SELECT status, severity, ack_at, ack_severity, resolved_at, hits FROM alerts WHERE id = ?`, id).Scan(&status, &sev, &ackAt, &ackSev, &resolvedAt, &hits)
			switch {
			case err == sql.ErrNoRows:
				st0 := "active"
				if openAfter > 1 {
					st0 = "pending"
				}
				if _, err := tx.Exec(`INSERT INTO alerts(id, source, object, rule, severity, status, payload, first_at, last_at, hits) VALUES(?,?,?,?,?,?,?,?,?,1)`,
					id, d.Source, d.Object, d.Rule, d.Severity, st0, string(payload), ns, ns); err != nil {
					return err
				}
				if st0 == "active" {
					if err := event(id, "opened", d.Severity, ""); err != nil {
						return err
					}
				}
			case err != nil:
				return err
			case status == "pending":
				hits++
				if hits >= openAfter {
					if _, err := tx.Exec(`UPDATE alerts SET status='active', severity=?, payload=?, last_at=?, hits=?, missing_since=0 WHERE id=?`, d.Severity, string(payload), ns, hits, id); err != nil {
						return err
					}
					if err := event(id, "opened", d.Severity, ""); err != nil {
						return err
					}
				} else if _, err := tx.Exec(`UPDATE alerts SET severity=?, payload=?, last_at=?, hits=?, missing_since=0 WHERE id=?`, d.Severity, string(payload), ns, hits, id); err != nil {
					return err
				}
			case status == "resolved" && ns-resolvedAt < cooldown && severityRank(d.Severity) <= severityRank(sev):
				// Cooling down: remember it was seen again, stay quiet.
				if _, err := tx.Exec(`UPDATE alerts SET payload=?, last_at=?, hits=hits+1 WHERE id=?`, string(payload), ns, id); err != nil {
					return err
				}
			case status == "resolved":
				kind := "reopened"
				if ns-resolvedAt < cooldown {
					kind = "escalated"
				}
				if _, err := tx.Exec(`UPDATE alerts SET status='active', severity=?, payload=?, last_at=?, resolved_at=0, count=count+1, hits=1, missing_since=0, ack_at=0, ack_by='', ack_severity='' WHERE id=?`,
					d.Severity, string(payload), ns, id); err != nil {
					return err
				}
				if err := event(id, kind, d.Severity, ""); err != nil {
					return err
				}
			default:
				escalated := severityRank(d.Severity) > severityRank(sev)
				clearAck := ackAt != 0 && severityRank(d.Severity) > severityRank(ackSev)
				q := `UPDATE alerts SET severity=?, payload=?, last_at=?, missing_since=0 WHERE id=?`
				if clearAck {
					q = `UPDATE alerts SET severity=?, payload=?, last_at=?, missing_since=0, ack_at=0, ack_by='', ack_severity='' WHERE id=?`
				}
				if _, err := tx.Exec(q, d.Severity, string(payload), ns, id); err != nil {
					return err
				}
				if escalated {
					if err := event(id, "escalated", d.Severity, ""); err != nil {
						return err
					}
				}
			}
		}
		for _, source := range evaluated {
			// Pending conditions not seen again within CloseAfterMin are dropped.
			if _, err := tx.Exec(`DELETE FROM alerts WHERE source = ? AND status = 'pending' AND missing_since > 0 AND ? - missing_since >= ? AND id NOT IN (SELECT value FROM json_each(?))`,
				source, ns, closeAfter, jsonIDs(seen)); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE alerts SET missing_since = ? WHERE source = ? AND status = 'pending' AND missing_since = 0 AND id NOT IN (SELECT value FROM json_each(?))`,
				ns, source, jsonIDs(seen)); err != nil {
				return err
			}
			rows, err := tx.Query(`SELECT id, severity, missing_since FROM alerts WHERE source = ? AND status = 'active'`, source)
			if err != nil {
				return err
			}
			type row struct {
				id, sev string
				missing int64
			}
			var gone []row
			for rows.Next() {
				var r row
				if err := rows.Scan(&r.id, &r.sev, &r.missing); err != nil {
					rows.Close()
					return err
				}
				if !seen[r.id] {
					gone = append(gone, r)
				}
			}
			rows.Close()
			for _, g := range gone {
				if g.missing == 0 {
					if _, err := tx.Exec(`UPDATE alerts SET missing_since=? WHERE id=?`, ns, g.id); err != nil {
						return err
					}
					if closeAfter > 0 {
						continue
					}
				} else if ns-g.missing < closeAfter {
					continue
				}
				if _, err := tx.Exec(`UPDATE alerts SET status='resolved', resolved_at=?, missing_since=0 WHERE id=?`, ns, g.id); err != nil {
					return err
				}
				if err := event(g.id, "resolved", g.sev, ""); err != nil {
					return err
				}
			}
		}
		cut := now.Add(-days(ret.AlertsDays)).UnixNano()
		if _, err := tx.Exec(`DELETE FROM alert_events WHERE at < ? OR id <= (SELECT COALESCE(MAX(id),0) - ? FROM alert_events)`, cut, AlertEventCap); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM alerts WHERE status = 'resolved' AND resolved_at < ?`, cut); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM alert_mutes WHERE until < ?`, ns); err != nil {
			return err
		}
		// page-configurable retention for candy runs (newest CandyKeep per account still apply).
		if _, err := tx.Exec(`DELETE FROM candy_runs WHERE started_at < ? AND status != 'running'`, now.Add(-days(ret.CandyDays)).UnixNano()); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func jsonIDs(seen map[string]bool) string {
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	raw, _ := json.Marshal(ids)
	return string(raw)
}

// AckAlert acknowledges an active alert at its current severity.
func (c *Controller) AckAlert(id string, now time.Time) (bool, error) {
	ok := false
	err := c.withExt(func(db *sql.DB) error {
		res, err := db.Exec(`UPDATE alerts SET ack_at=?, ack_by=?, ack_severity=severity WHERE id=? AND status='active'`, now.UnixNano(), AlertOperator, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if ok = n > 0; ok {
			var sev string
			_ = db.QueryRow(`SELECT severity FROM alerts WHERE id=?`, id).Scan(&sev)
			_, err = db.Exec(`INSERT INTO alert_events(alert_id, at, kind, severity, by) VALUES(?,?,?,?,?)`, id, now.UnixNano(), "acked", sev, AlertOperator)
		}
		return err
	})
	return ok, err
}

// MuteAlerts mutes source+object (all rules) until the given time; zero
// duration means until unmuted.
func (c *Controller) MuteAlerts(source, object string, d time.Duration, now time.Time) (time.Time, error) {
	until := AlertMuteForever
	if d > 0 {
		until = now.Add(d).UnixNano()
	}
	err := c.withExt(func(db *sql.DB) error {
		if _, err := db.Exec(`INSERT INTO alert_mutes(source, object, until, by, at) VALUES(?,?,?,?,?)
			ON CONFLICT(source, object) DO UPDATE SET until=excluded.until, by=excluded.by, at=excluded.at`, source, object, until, AlertOperator, now.UnixNano()); err != nil {
			return err
		}
		_, err := db.Exec(`INSERT INTO alert_events(alert_id, at, kind, by) SELECT id, ?, 'muted', ? FROM alerts WHERE source=? AND object=?`, now.UnixNano(), AlertOperator, source, object)
		return err
	})
	return time.Unix(0, until).UTC(), err
}

// UnmuteAlerts removes a mute.
func (c *Controller) UnmuteAlerts(source, object string, now time.Time) error {
	return c.withExt(func(db *sql.DB) error {
		if _, err := db.Exec(`DELETE FROM alert_mutes WHERE source=? AND object=?`, source, object); err != nil {
			return err
		}
		_, err := db.Exec(`INSERT INTO alert_events(alert_id, at, kind, by) SELECT id, ?, 'unmuted', ? FROM alerts WHERE source=? AND object=?`, now.UnixNano(), AlertOperator, source, object)
		return err
	})
}

// AlertMute is one active mute.
type AlertMute struct {
	Source string    `json:"source"`
	Object string    `json:"object"`
	Until  time.Time `json:"until"`
	By     string    `json:"by"`
}

// AlertSnapshot is what the page shows.
type AlertSnapshot struct {
	Alerts  []Alert      `json:"alerts"`
	Mutes   []AlertMute  `json:"mutes"`
	History []AlertEvent `json:"history"`
	Badge   int          `json:"badge"`   // active, unacknowledged, unmuted, not info
	Highest string       `json:"highest"` // highest severity among badge alerts
}

// AlertState lists alerts (active first), mutes and the latest history.
func (c *Controller) AlertState(now time.Time, historyLimit int) (AlertSnapshot, error) {
	snap := AlertSnapshot{Alerts: []Alert{}, Mutes: []AlertMute{}, History: []AlertEvent{}}
	cooldownH := c.AlertSettings().Settings.ReopenCooldownH
	err := c.withExt(func(db *sql.DB) error {
		mutes := map[string]int64{}
		rows, err := db.Query(`SELECT source, object, until, by FROM alert_mutes WHERE until >= ?`, now.UnixNano())
		if err != nil {
			return err
		}
		for rows.Next() {
			var m AlertMute
			var until int64
			if err := rows.Scan(&m.Source, &m.Object, &until, &m.By); err != nil {
				rows.Close()
				return err
			}
			m.Until = time.Unix(0, until).UTC()
			mutes[m.Source+"|"+m.Object] = until
			snap.Mutes = append(snap.Mutes, m)
		}
		rows.Close()
		rows, err = db.Query(`SELECT id, source, object, rule, severity, status, payload, first_at, last_at, resolved_at, count, ack_at, ack_by, hits
			FROM alerts WHERE status != 'pending' ORDER BY status = 'active' DESC, CASE severity WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END, last_at DESC LIMIT 500`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var a Alert
			var payload string
			var first, last, resolved, ack int64
			if err := rows.Scan(&a.ID, &a.Source, &a.Object, &a.Rule, &a.Severity, &a.Status, &payload, &first, &last, &resolved, &a.Count, &ack, &a.AckBy, &a.Hits); err != nil {
				rows.Close()
				return err
			}
			a.Payload = json.RawMessage(payload)
			a.FirstAt, a.LastAt = time.Unix(0, first).UTC(), time.Unix(0, last).UTC()
			if resolved > 0 {
				t := time.Unix(0, resolved).UTC()
				a.ResolvedAt = &t
				if cool := t.Add(time.Duration(cooldownH * float64(time.Hour))); a.Status == "resolved" && cool.After(now) {
					a.CoolingTil = &cool
				}
			}
			if ack > 0 {
				t := time.Unix(0, ack).UTC()
				a.AckAt = &t
			}
			if until, ok := mutes[a.Source+"|"+a.Object]; ok {
				t := time.Unix(0, until).UTC()
				a.MutedUntil = &t
			}
			if a.Status == "active" && a.AckAt == nil && a.MutedUntil == nil && a.Severity != "info" {
				snap.Badge++
				if severityRank(a.Severity) > severityRank(snap.Highest) {
					snap.Highest = a.Severity
				}
			}
			snap.Alerts = append(snap.Alerts, a)
		}
		rows.Close()
		rows, err = db.Query(`SELECT alert_id, at, kind, severity, by FROM alert_events ORDER BY id DESC LIMIT ?`, historyLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e AlertEvent
			var at int64
			if err := rows.Scan(&e.AlertID, &at, &e.Kind, &e.Severity, &e.By); err != nil {
				return err
			}
			e.At = time.Unix(0, at).UTC()
			snap.History = append(snap.History, e)
		}
		return rows.Err()
	})
	return snap, err
}

// ValidAlertSource reports whether s names a known source.
func ValidAlertSource(s string) bool {
	switch strings.TrimSpace(s) {
	case AlertQuota, AlertUsage, AlertBlocks, AlertMonitor, AlertAuthErrors:
		return true
	}
	return false
}

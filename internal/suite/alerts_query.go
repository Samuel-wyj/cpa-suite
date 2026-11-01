package suite

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// server-side paging, filtering, grouping and bulk actions for
// the alert center. AlertState (the old full snapshot) stays for rollback /
// older pages.

// AlertQuery filters the alert list. Empty fields match everything.
type AlertQuery struct {
	Status   string    // open (active, default) | active | acked | muted | resolved | all
	Severity string    // high | medium | info
	Source   string    // quota | usage | blocks | monitor | auth_errors
	Object   string    // exact object (account auth_index / key scope / "all")
	Q        string    // substring of object, rule or payload
	Since    time.Time // last_at >= Since
	Group    bool      // aggregate by source + object
	Offset   int
	Limit    int // 1…200, default 50
}

// AlertGroup is one source+object aggregate.
type AlertGroup struct {
	Source   string          `json:"source"`
	Object   string          `json:"object"`
	Alerts   int             `json:"alerts"`
	Active   int             `json:"active"`
	Count    int64           `json:"count"`
	Severity string          `json:"severity"`
	FirstAt  time.Time       `json:"first_at"`
	LastAt   time.Time       `json:"last_at"`
	Rules    []string        `json:"rules"`
	Muted    *time.Time      `json:"muted_until,omitempty"`
	Payload  json.RawMessage `json:"payload"` // latest alert's payload (name, provider …)
	LatestID string          `json:"latest_id"`
}

// AlertSummary feeds the cards on top of the page.
type AlertSummary struct {
	Active   int              `json:"active"`
	High     int              `json:"high"`
	Medium   int              `json:"medium"`
	Info     int              `json:"info"`
	Acked    int              `json:"acked"`
	Muted    int              `json:"muted"`
	Resolved int              `json:"resolved"`
	New24h   int              `json:"new_24h"`
	Badge    int              `json:"badge"`
	Highest  string           `json:"highest"`
	Top      []AlertTopObject `json:"top"`
	BySource map[string]int   `json:"by_source"`
}

// AlertTopObject is one of the most frequent active objects.
type AlertTopObject struct {
	Source string `json:"source"`
	Object string `json:"object"`
	Alerts int    `json:"alerts"`
	Count  int64  `json:"count"`
}

// AlertPage is one page of the list.
type AlertPage struct {
	Items  []Alert      `json:"items,omitempty"`
	Groups []AlertGroup `json:"groups,omitempty"`
	Total  int          `json:"total"`
	Offset int          `json:"offset"`
	Limit  int          `json:"limit"`
}

// AlertHistoryItem is one event with its alert's identity.
type AlertHistoryItem struct {
	AlertEvent
	Source  string          `json:"source"`
	Object  string          `json:"object"`
	Rule    string          `json:"rule"`
	Payload json.RawMessage `json:"payload"`
}

const alertMuteJoin = ` FROM alerts a LEFT JOIN alert_mutes m ON m.source = a.source AND m.object = a.object AND m.until >= ? `

func (q AlertQuery) where(now time.Time) (string, []any) {
	conds := []string{"a.status != 'pending'"}
	args := []any{now.UnixNano()}
	switch q.Status {
	case "", "open", "active":
		conds = append(conds, "a.status = 'active'")
		if q.Status == "open" {
			conds = append(conds, "a.ack_at = 0", "m.until IS NULL")
		}
	case "acked":
		conds = append(conds, "a.status = 'active'", "a.ack_at > 0")
	case "muted":
		conds = append(conds, "m.until IS NOT NULL")
	case "resolved":
		conds = append(conds, "a.status = 'resolved'")
	}
	if q.Severity != "" {
		conds, args = append(conds, "a.severity = ?"), append(args, q.Severity)
	}
	if q.Source != "" {
		conds, args = append(conds, "a.source = ?"), append(args, q.Source)
	}
	if q.Object != "" {
		conds, args = append(conds, "a.object = ?"), append(args, q.Object)
	}
	if s := strings.TrimSpace(q.Q); s != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s) + "%"
		conds = append(conds, `(a.object LIKE ? ESCAPE '\' OR a.rule LIKE ? ESCAPE '\' OR a.payload LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if !q.Since.IsZero() {
		conds, args = append(conds, "a.last_at >= ?"), append(args, q.Since.UnixNano())
	}
	return alertMuteJoin + " WHERE " + strings.Join(conds, " AND "), args
}

func (q *AlertQuery) norm() {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
}

const alertSevOrder = `CASE a.severity WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END`

// QueryAlerts returns one page of alerts or groups.
func (c *Controller) QueryAlerts(q AlertQuery, now time.Time) (AlertPage, error) {
	q.norm()
	page := AlertPage{Offset: q.Offset, Limit: q.Limit}
	cooldownH := c.AlertSettings().Settings.ReopenCooldownH
	where, args := q.where(now)
	err := c.withExt(func(db *sql.DB) error {
		if q.Group {
			if err := db.QueryRow(`SELECT COUNT(*) FROM (SELECT 1`+where+` GROUP BY a.source, a.object)`, args...).Scan(&page.Total); err != nil {
				return err
			}
			rows, err := db.Query(`SELECT a.source, a.object, COUNT(*), SUM(a.status = 'active'), SUM(a.count), MIN(`+alertSevOrder+`), MIN(a.first_at), MAX(a.last_at),
				GROUP_CONCAT(DISTINCT a.rule), MAX(m.until),
				(SELECT b.id FROM alerts b WHERE b.source = a.source AND b.object = a.object AND b.status != 'pending' ORDER BY b.last_at DESC LIMIT 1)`+where+`
				GROUP BY a.source, a.object ORDER BY SUM(a.status = 'active') > 0 DESC, MIN(`+alertSevOrder+`), MAX(a.last_at) DESC LIMIT ? OFFSET ?`, append(args, q.Limit, q.Offset)...)
			if err != nil {
				return err
			}
			defer rows.Close()
			page.Groups = []AlertGroup{}
			for rows.Next() {
				var g AlertGroup
				var sev int
				var first, last int64
				var rules sql.NullString
				var until sql.NullInt64
				if err := rows.Scan(&g.Source, &g.Object, &g.Alerts, &g.Active, &g.Count, &sev, &first, &last, &rules, &until, &g.LatestID); err != nil {
					return err
				}
				g.Severity = []string{"high", "medium", "info"}[min(sev, 2)]
				g.FirstAt, g.LastAt = time.Unix(0, first).UTC(), time.Unix(0, last).UTC()
				g.Rules = strings.Split(rules.String, ",")
				if until.Valid {
					t := time.Unix(0, until.Int64).UTC()
					g.Muted = &t
				}
				page.Groups = append(page.Groups, g)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			for i := range page.Groups {
				var p string
				if db.QueryRow(`SELECT payload FROM alerts WHERE id = ?`, page.Groups[i].LatestID).Scan(&p) == nil {
					page.Groups[i].Payload = json.RawMessage(p)
				} else {
					page.Groups[i].Payload = json.RawMessage(`{}`)
				}
			}
			return nil
		}
		if err := db.QueryRow(`SELECT COUNT(*)`+where, args...).Scan(&page.Total); err != nil {
			return err
		}
		rows, err := db.Query(`SELECT a.id, a.source, a.object, a.rule, a.severity, a.status, a.payload, a.first_at, a.last_at, a.resolved_at, a.count, a.ack_at, a.ack_by, a.hits, m.until`+where+`
			ORDER BY a.status = 'active' DESC, `+alertSevOrder+`, a.last_at DESC LIMIT ? OFFSET ?`, append(args, q.Limit, q.Offset)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		page.Items = []Alert{}
		for rows.Next() {
			a, err := scanAlert(rows, now, cooldownH)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, a)
		}
		return rows.Err()
	})
	return page, err
}

func scanAlert(rows *sql.Rows, now time.Time, cooldownH float64) (Alert, error) {
	var a Alert
	var payload string
	var first, last, resolved, ack int64
	var until sql.NullInt64
	if err := rows.Scan(&a.ID, &a.Source, &a.Object, &a.Rule, &a.Severity, &a.Status, &payload, &first, &last, &resolved, &a.Count, &ack, &a.AckBy, &a.Hits, &until); err != nil {
		return a, err
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
	if until.Valid {
		t := time.Unix(0, until.Int64).UTC()
		a.MutedUntil = &t
	}
	return a, nil
}

// AlertSummaryAt computes the summary cards (cheap aggregate queries).
func (c *Controller) AlertSummaryAt(now time.Time) (AlertSummary, error) {
	s := AlertSummary{Top: []AlertTopObject{}, BySource: map[string]int{}}
	err := c.withExt(func(db *sql.DB) error {
		ns := now.UnixNano()
		if err := db.QueryRow(`SELECT
			COALESCE(SUM(a.status='active'),0),
			COALESCE(SUM(a.status='active' AND a.severity='high'),0),
			COALESCE(SUM(a.status='active' AND a.severity='medium'),0),
			COALESCE(SUM(a.status='active' AND a.severity='info'),0),
			COALESCE(SUM(a.status='active' AND a.ack_at>0),0),
			COALESCE(SUM(m.until IS NOT NULL),0),
			COALESCE(SUM(a.status='resolved'),0),
			COALESCE(SUM(a.first_at >= ?),0)`+alertMuteJoin+` WHERE a.status != 'pending'`, now.Add(-24*time.Hour).UnixNano(), ns).
			Scan(&s.Active, &s.High, &s.Medium, &s.Info, &s.Acked, &s.Muted, &s.Resolved, &s.New24h); err != nil {
			return err
		}
		if err := badgeQuery(db, ns, &s.Badge, &s.Highest); err != nil {
			return err
		}
		rows, err := db.Query(`SELECT a.source, COUNT(*)`+alertMuteJoin+` WHERE a.status = 'active' GROUP BY a.source`, ns)
		if err != nil {
			return err
		}
		for rows.Next() {
			var src string
			var n int
			if err := rows.Scan(&src, &n); err != nil {
				rows.Close()
				return err
			}
			s.BySource[src] = n
		}
		rows.Close()
		rows, err = db.Query(`SELECT a.source, a.object, COUNT(*), SUM(a.count)`+alertMuteJoin+` WHERE a.status = 'active' GROUP BY a.source, a.object ORDER BY SUM(a.count) DESC, COUNT(*) DESC LIMIT 3`, ns)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t AlertTopObject
			if err := rows.Scan(&t.Source, &t.Object, &t.Alerts, &t.Count); err != nil {
				return err
			}
			s.Top = append(s.Top, t)
		}
		return rows.Err()
	})
	return s, err
}

func badgeQuery(db *sql.DB, ns int64, badge *int, highest *string) error {
	var top sql.NullInt64
	if err := db.QueryRow(`SELECT COUNT(*), MIN(`+alertSevOrder+`)`+alertMuteJoin+`
		WHERE a.status = 'active' AND a.ack_at = 0 AND m.until IS NULL AND a.severity != 'info'`, ns).Scan(badge, &top); err != nil {
		return err
	}
	*highest = ""
	if *badge > 0 && top.Valid {
		*highest = []string{"high", "medium", "info"}[min(int(top.Int64), 2)]
	}
	return nil
}

// AlertBadge is the nav badge only (same rule as AlertSnapshot.Badge).
func (c *Controller) AlertBadge(now time.Time) (int, string, error) {
	var n int
	var h string
	err := c.withExt(func(db *sql.DB) error { return badgeQuery(db, now.UnixNano(), &n, &h) })
	return n, h, err
}

// AlertHistory returns one page of events (newest first) with alert identity.
func (c *Controller) AlertHistory(kind, source, alertID string, offset, limit int) ([]AlertHistoryItem, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	items := []AlertHistoryItem{}
	total := 0
	conds, args := []string{"1=1"}, []any{}
	if kind != "" {
		conds, args = append(conds, "e.kind = ?"), append(args, kind)
	}
	if source != "" {
		conds, args = append(conds, "a.source = ?"), append(args, source)
	}
	if alertID != "" {
		conds, args = append(conds, "e.alert_id = ?"), append(args, alertID)
	}
	where := ` FROM alert_events e LEFT JOIN alerts a ON a.id = e.alert_id WHERE ` + strings.Join(conds, " AND ")
	err := c.withExt(func(db *sql.DB) error {
		if err := db.QueryRow(`SELECT COUNT(*)`+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, err := db.Query(`SELECT e.alert_id, e.at, e.kind, e.severity, e.by, COALESCE(a.source,''), COALESCE(a.object,''), COALESCE(a.rule,''), COALESCE(a.payload,'{}')`+where+` ORDER BY e.id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it AlertHistoryItem
			var at int64
			var p string
			if err := rows.Scan(&it.AlertID, &at, &it.Kind, &it.Severity, &it.By, &it.Source, &it.Object, &it.Rule, &p); err != nil {
				return err
			}
			it.At, it.Payload = time.Unix(0, at).UTC(), json.RawMessage(p)
			items = append(items, it)
		}
		return rows.Err()
	})
	return items, total, err
}

// AckAlerts acknowledges several active alerts; returns how many changed.
func (c *Controller) AckAlerts(ids []string, now time.Time) (int, error) {
	n := 0
	for _, id := range ids {
		ok, err := c.AckAlert(strings.TrimSpace(id), now)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// ClearResolved deletes resolved alerts (and their events) whose cooldown is
// over; returns how many were removed.
func (c *Controller) ClearResolved(now time.Time) (int64, error) {
	cooldownH := c.AlertSettings().Settings.ReopenCooldownH
	cut := now.Add(-time.Duration(cooldownH * float64(time.Hour))).UnixNano()
	var n int64
	err := c.withExt(func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`DELETE FROM alert_events WHERE alert_id IN (SELECT id FROM alerts WHERE status = 'resolved' AND resolved_at <= ?)`, cut); err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM alerts WHERE status = 'resolved' AND resolved_at <= ?`, cut)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return tx.Commit()
	})
	return n, err
}

// AlertsCSV renders every alert matching q (no paging, at most 20000 rows).
func (c *Controller) AlertsCSV(q AlertQuery, now time.Time) ([]byte, error) {
	q.Group = false
	var b strings.Builder
	b.WriteString("\ufeff") // Excel: UTF-8
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"id", "source", "object", "name", "rule", "severity", "status", "count", "first_at", "last_at", "resolved_at", "acked_at", "muted_until", "payload"})
	where, args := q.where(now)
	err := c.withExt(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT a.id, a.source, a.object, a.rule, a.severity, a.status, a.payload, a.first_at, a.last_at, a.resolved_at, a.count, a.ack_at, a.ack_by, a.hits, m.until`+where+`
			ORDER BY a.last_at DESC LIMIT 20000`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		ts := func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.Format(time.RFC3339)
		}
		for rows.Next() {
			a, err := scanAlert(rows, now, 0)
			if err != nil {
				return err
			}
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(a.Payload, &p)
			if err := w.Write([]string{a.ID, a.Source, csvSafe(a.Object), csvSafe(p.Name), csvSafe(a.Rule), a.Severity, a.Status, fmt.Sprint(a.Count),
				a.FirstAt.Format(time.RFC3339), a.LastAt.Format(time.RFC3339), ts(a.ResolvedAt), ts(a.AckAt), ts(a.MutedUntil), csvSafe(string(a.Payload))}); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	w.Flush()
	return []byte(b.String()), err
}

// csvSafe defuses spreadsheet formula injection.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		return "'" + s
	}
	return s
}

// ActiveMutes lists the mutes still in force.
func (c *Controller) ActiveMutes(now time.Time) ([]AlertMute, error) {
	out := []AlertMute{}
	err := c.withExt(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT source, object, until, by FROM alert_mutes WHERE until >= ? ORDER BY at DESC`, now.UnixNano())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m AlertMute
			var until int64
			if err := rows.Scan(&m.Source, &m.Object, &until, &m.By); err != nil {
				return err
			}
			m.Until = time.Unix(0, until).UTC()
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

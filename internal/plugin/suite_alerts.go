package plugin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"cpa-key-billing/internal/suite"
)

// alert center: page-only. Conditions are derived from the other
// read-only modules when the page asks (at most once a minute); nothing is
// pushed and no upstream request is made (the forecast is read without fetch).

const (
	routeSuiteAlerts        = "/suite/alerts"
	routeSuiteAlertsAck     = "/suite/alerts/ack"
	routeSuiteAlertsMute    = "/suite/alerts/mute"
	routeSuiteAlertsUnmute  = "/suite/alerts/unmute"
	routeSuiteAlertSettings = "/suite/alerts/settings"

	authAlertWindow = 15 * time.Minute
	authAlertMin    = 5
)

func (a *App) alertsEvaluate(now time.Time, force bool) {
	if !a.suite.AlertsDue(now, force) {
		return
	}
	var detected []suite.DetectedAlert
	var evaluated []string
	st := a.suite.AlertSettings().Settings
	if a.suite.ModuleMode("forecast") != suite.ModeOff {
		if f := a.forecastData("", "", false); f["error"] == nil {
			evaluated = append(evaluated, suite.AlertQuota)
			accounts, _ := f["accounts"].([]forecastAccount)
			for _, acc := range accounts {
				for _, w := range acc.Windows {
					sev, ok := quotaAlertSeverity(w, st)
					if !ok {
						continue
					}
					payload := map[string]any{"provider": acc.Provider, "name": acc.Name, "window": w.Key, "risk": w.Risk, "remaining_percent": w.RemainingPercent,
						"remaining_usd": w.RemainingUSD, "burn_usd_per_hour": w.BurnUSDPerHour, "exhaust_at": w.ExhaustAt, "reset_at": w.ResetAt}
					if w.Burn != nil {
						payload["projected_at_reset"], payload["recent_per_hour"], payload["anchor_hours"] = w.Burn.ProjectedAtReset, w.Burn.Recent, w.Burn.AnchorHours
					}
					detected = append(detected, suite.DetectedAlert{Source: suite.AlertQuota, Object: acc.AuthIndex, Rule: "risk_" + w.Key, Severity: sev, Payload: payload})
				}
			}
		}
	}
	if a.suite.ModuleMode("ranking") == suite.ModeOn {
		if err := a.suite.ReadMain(func(db *sql.DB) error {
			list, err := keyAnomalies(db, now, st)
			if err != nil {
				return err
			}
			for _, an := range list {
				detected = append(detected, suite.DetectedAlert{Source: suite.AlertUsage, Object: an.Scope, Rule: an.Kind + "_" + an.Metric, Severity: capSeverity(an.Severity, st.UsageSeverity),
					Payload: map[string]any{"scope": an.Scope, "kind": an.Kind, "metric": an.Metric, "recent": an.Recent, "baseline": an.Baseline, "ratio": an.Ratio,
						"active_days": an.ActiveDays, "computed_severity": an.Severity}})
			}
			return nil
		}); err == nil {
			evaluated = append(evaluated, suite.AlertUsage)
		}
	}
	if a.suite.ModuleMode("client_guard") != suite.ModeOff {
		evaluated = append(evaluated, suite.AlertBlocks)
		for _, an := range blockAnomalies(a.suite.ClientDaily(), now) {
			detected = append(detected, suite.DetectedAlert{Source: suite.AlertBlocks, Object: "all", Rule: "spike", Severity: an.Severity,
				Payload: map[string]any{"recent": an.Recent, "baseline": an.Baseline, "ratio": an.Ratio}})
		}
	}
	if a.suite.ModuleMode("grok_monitor") == suite.ModeOn {
		m := a.monitorData(now)
		if m["error"] == nil {
			evaluated = append(evaluated, suite.AlertMonitor)
			accounts, _ := m["accounts"].([]monitorAccount)
			for _, acc := range accounts {
				// One alert per account (0.7); the page expands the buckets.
				var buckets []map[string]any
				sev := ""
				for _, b := range acc.Buckets {
					if b.Verdict != "suspect" {
						continue
					}
					signals := []string{}
					for _, s := range b.Signals {
						if s.State == "deviates" {
							signals = append(signals, s.ID)
						}
					}
					buckets = append(buckets, map[string]any{"model": b.Model, "effort": b.Effort, "stream": b.Stream, "severity": b.Severity,
						"signals": signals, "recent_count": b.RecentCount, "candy_failed": b.CandyFailed})
					if suiteSeverityRank(b.Severity) > suiteSeverityRank(sev) {
						sev = b.Severity
					}
				}
				if len(buckets) == 0 {
					continue
				}
				first := buckets[0]
				detected = append(detected, suite.DetectedAlert{Source: suite.AlertMonitor, Object: acc.AuthIndex, Rule: "degradation", Severity: sev,
					Payload: map[string]any{"provider": acc.Provider, "name": acc.Name, "model": first["model"], "effort": first["effort"], "stream": first["stream"],
						"signals": first["signals"], "recent_count": first["recent_count"], "buckets": buckets, "heuristic": true}})
			}
		}
	}
	if a.suite.ModuleMode("auth_alerts") == suite.ModeOn {
		names := map[string]string{}
		if files, err := a.listHostAuthFiles(); err == nil {
			for _, f := range files {
				names[f.AuthIndex] = f.Name
			}
		}
		if err := a.suite.ReadMain(func(db *sql.DB) error {
			rows, err := db.Query(`SELECT e.auth_index, e.provider, COUNT(*), MAX(r.status_code) FROM request_events e JOIN request_errors r ON r.request_event_id = e.id
				WHERE e.at >= ? AND e.auth_index != '' AND r.status_code IN (401, 403) GROUP BY e.auth_index, e.provider HAVING COUNT(*) >= ?`,
				now.Add(-authAlertWindow).UnixNano(), authAlertMin)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var idx, provider string
				var n, status int64
				if err := rows.Scan(&idx, &provider, &n, &status); err != nil {
					return err
				}
				sev := "medium"
				if n >= 4*authAlertMin {
					sev = "high"
				}
				detected = append(detected, suite.DetectedAlert{Source: suite.AlertAuthErrors, Object: idx, Rule: "auth_burst", Severity: sev,
					Payload: map[string]any{"provider": provider, "name": names[idx], "count": n, "status": status, "window_minutes": int(authAlertWindow.Minutes())}})
			}
			return rows.Err()
		}); err == nil {
			evaluated = append(evaluated, suite.AlertAuthErrors)
		}
	}
	_ = a.suite.SyncAlerts(detected, evaluated, now)
}

func (a *App) alertsData(now time.Time, force bool) map[string]any {
	mode := a.suite.ModuleMode("alerts")
	out := map[string]any{"mode": mode, "alerts": []suite.Alert{}, "mutes": []suite.AlertMute{}, "history": []suite.AlertEvent{}, "badge": 0}
	if mode != suite.ModeOn {
		return out
	}
	a.alertsEvaluate(now, force)
	snap, err := a.suite.AlertState(now, 200)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	out["alerts"], out["mutes"], out["history"], out["badge"], out["highest"] = snap.Alerts, snap.Mutes, snap.History, snap.Badge, snap.Highest
	out["retention_days"] = a.suite.CurrentRetention().AlertsDays
	out["sources"] = map[string]bool{
		suite.AlertQuota: a.suite.ModuleMode("forecast") != suite.ModeOff, suite.AlertUsage: a.suite.ModuleMode("ranking") == suite.ModeOn,
		suite.AlertBlocks: a.suite.ModuleMode("client_guard") != suite.ModeOff, suite.AlertMonitor: a.suite.ModuleMode("grok_monitor") == suite.ModeOn,
		suite.AlertAuthErrors: a.suite.ModuleMode("auth_alerts") == suite.ModeOn}
	return out
}

func (a *App) suiteAlerts(req ManagementRequest) ManagementResponse {
	return JSONResponse(http.StatusOK, a.alertsData(time.Now().UTC(), req.Query.Get("refresh") == "1"))
}

func (a *App) suiteAlertAction(req ManagementRequest, suffix string) ManagementResponse {
	if a.suite.ModuleMode("alerts") != suite.ModeOn {
		return JSONError(http.StatusConflict, "alerts_off", "The alerts module is off")
	}
	var body struct {
		ID      string `json:"id"`
		Source  string `json:"source"`
		Object  string `json:"object"`
		Minutes int    `json:"minutes"` // 0 = until unmuted
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return JSONError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	now := time.Now().UTC()
	switch suffix {
	case routeSuiteAlertsAck:
		ok, err := a.suite.AckAlert(strings.TrimSpace(body.ID), now)
		if err != nil {
			return JSONError(http.StatusInternalServerError, "ack_failed", err.Error())
		}
		if !ok {
			return JSONError(http.StatusNotFound, "not_found", "No active alert with that id")
		}
	case routeSuiteAlertsMute, routeSuiteAlertsUnmute:
		if !suite.ValidAlertSource(body.Source) || strings.TrimSpace(body.Object) == "" || body.Minutes < 0 || body.Minutes > 60*24*90 {
			return JSONError(http.StatusBadRequest, "invalid_mute", "source, object and minutes (0…129600) are required")
		}
		var err error
		if suffix == routeSuiteAlertsUnmute {
			err = a.suite.UnmuteAlerts(body.Source, body.Object, now)
		} else {
			_, err = a.suite.MuteAlerts(body.Source, body.Object, time.Duration(body.Minutes)*time.Minute, now)
		}
		if err != nil {
			return JSONError(http.StatusInternalServerError, "mute_failed", err.Error())
		}
	}
	return JSONResponse(http.StatusOK, a.alertsData(now, false))
}

// quotaAlertSeverity (0.7): alert only when the remaining quota is at or
// below the window's threshold AND the anchored recent pace reaches 100 %
// before the reset; never in the first hours of a window. High when little
// is left or the quota would run out well before the reset.
func quotaAlertSeverity(w forecastWindow, st suite.AlertSettings) (string, bool) {
	if w.Risk == "exhausted" {
		return "high", true
	}
	if w.RemainingPercent == nil || w.Burn == nil || w.Stale {
		return "", false
	}
	remain, high, quiet := st.QuotaRemainWeekly, st.QuotaHighWeekly, st.QuotaQuietWeeklyH
	if w.Key == "5h" {
		remain, high, quiet = st.QuotaRemain5h, st.QuotaHigh5h, st.QuotaQuiet5hMin/60
	}
	rem := *w.RemainingPercent
	if w.Burn.ElapsedHours < quiet || w.Burn.ProjectedAtReset < 100 || rem > remain {
		return "", false
	}
	if rem <= high {
		return "high", true
	}
	if w.ExhaustAt != nil && w.Seconds > 0 && w.ResetAt.Sub(*w.ExhaustAt) >= time.Duration(float64(w.Seconds)*st.QuotaEarlyFrac)*time.Second {
		return "high", true
	}
	return "medium", true
}

// quotaQuiet reports whether the window is still in its alert quiet period.
func quotaQuiet(w forecastWindow, st suite.AlertSettings) bool {
	if w.Burn == nil {
		return false
	}
	quiet := st.QuotaQuietWeeklyH
	if w.Key == "5h" {
		quiet = st.QuotaQuiet5hMin / 60
	}
	return w.Burn.ElapsedHours < quiet
}

func suiteSeverityRank(s string) int {
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

// capSeverity lowers s to at most limit ("" = no cap).
func capSeverity(s, limit string) string {
	if limit != "" && suiteSeverityRank(s) > suiteSeverityRank(limit) {
		return limit
	}
	return s
}

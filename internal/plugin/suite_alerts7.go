package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cpa-key-billing/internal/suite"
)

// paged / filtered / grouped alert list, a cheap badge count,
// paged history, bulk actions, clear-resolved, CSV export and the
// page-configurable retention. /suite/alerts (full snapshot) is unchanged.

const (
	routeSuiteAlertsList     = "/suite/alerts/list"
	routeSuiteAlertsCount    = "/suite/alerts/count"
	routeSuiteAlertsHistory  = "/suite/alerts/history"
	routeSuiteAlertsExport   = "/suite/alerts/export.csv"
	routeSuiteAlertsBulk     = "/suite/alerts/bulk"
	routeSuiteAlertsClearRes = "/suite/alerts/clear-resolved"
	routeSuiteRetention      = "/suite/retention"
)

func suiteAlerts7Routes() []ManagementRoute {
	return []ManagementRoute{
		{Method: http.MethodGet, Path: managementBase + routeSuiteAlertsList, Description: "Alert list with paging, filters (status, severity, source, object, q, since) and grouping by object"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteAlertsCount, Description: "Alert badge count and highest severity only"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteAlertsHistory, Description: "Alert history with paging, filtered by kind, source or alert"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteAlertsExport, Description: "Export the filtered alerts as CSV"},
		{Method: http.MethodPost, Path: managementBase + routeSuiteAlertsBulk, Description: "Acknowledge several alerts or mute several objects"},
		{Method: http.MethodPost, Path: managementBase + routeSuiteAlertsClearRes, Description: "Delete resolved alerts whose cooldown is over"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteRetention, Description: "Data retention days and row counts"},
		{Method: http.MethodPut, Path: managementBase + routeSuiteRetention, Description: "Save data retention days (reset=true restores the defaults); prunes at once"},
	}
}

func alertQueryFrom(v url.Values, now time.Time) suite.AlertQuery {
	q := suite.AlertQuery{Status: v.Get("status"), Severity: v.Get("severity"), Source: v.Get("source"), Object: v.Get("object"), Q: v.Get("q"), Group: v.Get("group") == "1"}
	q.Offset, _ = strconv.Atoi(v.Get("offset"))
	q.Limit, _ = strconv.Atoi(v.Get("limit"))
	if s := v.Get("since"); s != "" {
		if h, err := strconv.ParseFloat(strings.TrimSuffix(s, "h"), 64); err == nil && strings.HasSuffix(s, "h") && h > 0 {
			q.Since = now.Add(-time.Duration(h * float64(time.Hour)))
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			q.Since = t
		}
	}
	return q
}

func (a *App) alertsSources() map[string]bool {
	return map[string]bool{
		suite.AlertQuota: a.suite.ModuleMode("forecast") != suite.ModeOff, suite.AlertUsage: a.suite.ModuleMode("ranking") == suite.ModeOn,
		suite.AlertBlocks: a.suite.ModuleMode("client_guard") != suite.ModeOff, suite.AlertMonitor: a.suite.ModuleMode("grok_monitor") == suite.ModeOn,
		suite.AlertAuthErrors: a.suite.ModuleMode("auth_alerts") == suite.ModeOn}
}

func (a *App) suiteAlerts7(req ManagementRequest, suffix string) (ManagementResponse, bool) {
	now := time.Now().UTC()
	switch {
	case req.Method == http.MethodGet && suffix == routeSuiteRetention:
		st, err := a.suite.RetentionState()
		if err != nil {
			return JSONError(http.StatusInternalServerError, "retention_failed", err.Error()), true
		}
		return JSONResponse(http.StatusOK, st), true
	case req.Method == http.MethodPut && suffix == routeSuiteRetention:
		var body struct {
			Retention suite.Retention `json:"retention"`
			Reset     bool            `json:"reset"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return JSONError(http.StatusBadRequest, "invalid_json", err.Error()), true
		}
		st, err := a.suite.SaveRetention(body.Retention, body.Reset, now)
		if err != nil {
			return JSONError(http.StatusBadRequest, "invalid_retention", err.Error()), true
		}
		return JSONResponse(http.StatusOK, st), true
	}
	isAlert := suffix == routeSuiteAlertsList || suffix == routeSuiteAlertsCount || suffix == routeSuiteAlertsHistory ||
		suffix == routeSuiteAlertsExport || suffix == routeSuiteAlertsBulk || suffix == routeSuiteAlertsClearRes
	if !isAlert {
		return ManagementResponse{}, false
	}
	mode := a.suite.ModuleMode("alerts")
	if req.Method == http.MethodGet && suffix == routeSuiteAlertsCount {
		if mode != suite.ModeOn {
			return JSONResponse(http.StatusOK, map[string]any{"mode": mode, "badge": 0, "highest": ""}), true
		}
		a.alertsEvaluate(now, false)
		n, h, err := a.suite.AlertBadge(now)
		if err != nil {
			return JSONError(http.StatusInternalServerError, "count_failed", err.Error()), true
		}
		return JSONResponse(http.StatusOK, map[string]any{"mode": mode, "badge": n, "highest": h}), true
	}
	if mode != suite.ModeOn {
		if req.Method == http.MethodGet && suffix != routeSuiteAlertsExport {
			return JSONResponse(http.StatusOK, map[string]any{"mode": mode, "items": []any{}, "groups": []any{}, "total": 0}), true
		}
		return JSONError(http.StatusConflict, "alerts_off", "The alerts module is off"), true
	}
	switch {
	case req.Method == http.MethodGet && suffix == routeSuiteAlertsList:
		a.alertsEvaluate(now, req.Query.Get("refresh") == "1")
		q := alertQueryFrom(req.Query, now)
		if q.Source != "" && !suite.ValidAlertSource(q.Source) {
			return JSONError(http.StatusBadRequest, "invalid_source", "unknown source"), true
		}
		page, err := a.suite.QueryAlerts(q, now)
		if err != nil {
			return JSONError(http.StatusInternalServerError, "list_failed", err.Error()), true
		}
		out := map[string]any{"mode": mode, "items": page.Items, "groups": page.Groups, "total": page.Total, "offset": page.Offset, "limit": page.Limit,
			"retention_days": a.suite.CurrentRetention().AlertsDays, "sources": a.alertsSources()}
		if req.Query.Get("summary") != "0" {
			sum, err := a.suite.AlertSummaryAt(now)
			if err != nil {
				return JSONError(http.StatusInternalServerError, "list_failed", err.Error()), true
			}
			mutes, _ := a.suite.ActiveMutes(now)
			out["summary"], out["mutes"], out["badge"], out["highest"] = sum, mutes, sum.Badge, sum.Highest
		}
		return JSONResponse(http.StatusOK, out), true
	case req.Method == http.MethodGet && suffix == routeSuiteAlertsHistory:
		off, _ := strconv.Atoi(req.Query.Get("offset"))
		lim, _ := strconv.Atoi(req.Query.Get("limit"))
		items, total, err := a.suite.AlertHistory(req.Query.Get("kind"), req.Query.Get("source"), req.Query.Get("alert_id"), off, lim)
		if err != nil {
			return JSONError(http.StatusInternalServerError, "history_failed", err.Error()), true
		}
		return JSONResponse(http.StatusOK, map[string]any{"items": items, "total": total, "offset": max(off, 0)}), true
	case req.Method == http.MethodGet && suffix == routeSuiteAlertsExport:
		q := alertQueryFrom(req.Query, now)
		if req.Query.Get("status") == "" {
			q.Status = "all"
		}
		body, err := a.suite.AlertsCSV(q, now)
		if err != nil {
			return JSONError(http.StatusInternalServerError, "export_failed", err.Error()), true
		}
		name := "cpa-suite-alerts-" + now.Format("20060102-150405") + ".csv"
		return ManagementResponse{StatusCode: http.StatusOK, Body: body, Headers: http.Header{
			"Content-Type": {"text/csv; charset=utf-8"}, "Content-Disposition": {`attachment; filename="` + name + `"`}, "Cache-Control": {"no-store"}}}, true
	case req.Method == http.MethodPost && suffix == routeSuiteAlertsBulk:
		var body struct {
			Action  string   `json:"action"` // ack | mute | unmute
			IDs     []string `json:"ids"`
			Objects []struct {
				Source string `json:"source"`
				Object string `json:"object"`
			} `json:"objects"`
			Minutes int `json:"minutes"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return JSONError(http.StatusBadRequest, "invalid_json", err.Error()), true
		}
		if len(body.IDs) > 500 || len(body.Objects) > 200 {
			return JSONError(http.StatusBadRequest, "too_many", "at most 500 ids or 200 objects"), true
		}
		changed := 0
		switch body.Action {
		case "ack":
			n, err := a.suite.AckAlerts(body.IDs, now)
			if err != nil {
				return JSONError(http.StatusInternalServerError, "ack_failed", err.Error()), true
			}
			changed = n
		case "mute", "unmute":
			if body.Minutes < 0 || body.Minutes > 60*24*90 {
				return JSONError(http.StatusBadRequest, "invalid_mute", "minutes must be 0…129600"), true
			}
			for _, o := range body.Objects {
				if !suite.ValidAlertSource(o.Source) || strings.TrimSpace(o.Object) == "" {
					return JSONError(http.StatusBadRequest, "invalid_mute", "source and object are required"), true
				}
			}
			for _, o := range body.Objects {
				var err error
				if body.Action == "unmute" {
					err = a.suite.UnmuteAlerts(o.Source, o.Object, now)
				} else {
					_, err = a.suite.MuteAlerts(o.Source, o.Object, time.Duration(body.Minutes)*time.Minute, now)
				}
				if err != nil {
					return JSONError(http.StatusInternalServerError, "mute_failed", err.Error()), true
				}
				changed++
			}
		default:
			return JSONError(http.StatusBadRequest, "invalid_action", "action must be ack, mute or unmute"), true
		}
		return JSONResponse(http.StatusOK, map[string]any{"ok": true, "changed": changed}), true
	case req.Method == http.MethodPost && suffix == routeSuiteAlertsClearRes:
		n, err := a.suite.ClearResolved(now)
		if err != nil {
			return JSONError(http.StatusInternalServerError, "clear_failed", err.Error()), true
		}
		return JSONResponse(http.StatusOK, map[string]any{"ok": true, "removed": n}), true
	}
	return ManagementResponse{}, false
}

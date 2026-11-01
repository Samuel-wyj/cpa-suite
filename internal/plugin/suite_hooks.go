package plugin

// CPA Suite glue. Upstream files reach this file only through the hooks listed
// in docs/UPSTREAM_PATCHES.md; everything else lives in internal/suite.

import (
	"bytes"
	"cpa-key-billing/internal/billing"
	"encoding/json"
	"net/http"
	"time"

	"cpa-key-billing/internal/sqlite"
	"cpa-key-billing/internal/suite"
)

const (
	routeSuiteStatus      = "/suite/status"
	routeSuiteModules     = "/suite/modules"
	routeSuiteClients     = "/suite/clients"
	routeSuiteClientRules = "/suite/clients/rules"
	routeSuiteClientClear = "/suite/clients/clear-stats"
)

func newSuiteController() *suite.Controller {
	c := suite.NewController(sqlite.SupportedSchemaVersion)
	c.SetValidator(func(raw []byte) error { _, err := billing.DecodeConfig(raw); return err })
	return c
}

// suiteGate short-circuits billing, admission and scheduling while CPA Suite
// is in watch-only mode. request.complete still reaches upstream so any slot
// acquired before the switch is released. Management calls are unaffected.
//
// The client module (client-guard port) runs first on intercept_before: a
// request it refuses (403) never reaches billing admission, so it takes no
// concurrency slot, opens no quota cycle and is never charged.
func (a *App) suiteGate(method string, request []byte) ([]byte, bool, error) {
	if a == nil {
		return nil, false, nil
	}
	if method == MethodUsageHandle {
		if a.candyCaptureUsage(request) { // candy test: not billed, not observed
			raw, err := OKEnvelope(struct{}{})
			return raw, true, err
		}
		a.suiteObserveUsage(request) // passive monitor; never alters the response
	}
	if method == MethodRequestInterceptBefore {
		if out, terminated := a.suite.ClientIntercept(request); terminated {
			return out, true, nil
		}
	}
	if !a.suite.WatchOnly() {
		return nil, false, nil
	}
	switch method {
	case MethodRequestInterceptBefore, MethodRequestInterceptAfter:
		raw, err := OKEnvelope(RequestInterceptResponse{})
		return raw, true, err
	case MethodSchedulerPick:
		raw, err := OKEnvelope(SchedulerPickResponse{Handled: false})
		return raw, true, err
	case MethodUsageHandle:
		raw, err := OKEnvelope(struct{}{})
		return raw, true, err
	}
	return nil, false, nil
}

// suitePrepareConfig strips suite keys, performs the one-time import and
// returns the YAML for the unmodified upstream decoder.
func (a *App) suitePrepareConfig(raw []byte) ([]byte, error) {
	return a.suite.Prepare(raw)
}

func suiteConfigFields() []ConfigField {
	return []ConfigField{
		{Name: "import_from", Type: "string", Description: "首次启动时只读导入的 cpa-key-billing 数据库路径（填 none 表示不导入）"},
		{Name: "plugins_dir", Type: "string", Default: "plugins", Description: "CPA 插件目录，用于检测冲突插件（默认 plugins）"},
		{Name: "alerts", Type: "object", Default: map[string]any{}, Description: "告警阈值覆盖（0.7，可选）：如 quota_remain_weekly: 30、reopen_cooldown_hours: 6；写在这里的项优先于页面设置并在页面上锁定"},
		{Name: "client_guard", Type: "object", Description: "客户端限制模块配置（同 client-guard 0.1.2 的配置项；import_from 为其 rules.json/stats.json 目录，默认 <plugins_dir>/client-guard，none 表示不导入）"},
	}
}

func suiteManagementRoutes() []ManagementRoute {
	return append([]ManagementRoute{
		{Method: http.MethodGet, Path: managementBase + routeSuiteStatus, Description: "View CPA Suite status, conflicts and import record"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteModules, Description: "View CPA Suite module switches"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteClients, Description: "View client rules, recent clients, presets and statistics"},
		{Method: http.MethodPut, Path: managementBase + routeSuiteClientRules, Description: "Save client rules"},
		{Method: http.MethodPut, Path: managementBase + routeSuiteModules, Description: "Switch a module at runtime"},
		{Method: http.MethodPost, Path: managementBase + routeSuiteClientClear, Description: "Clear client statistics"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteForecast, Description: "USD quota forecast for upstream Codex and Grok accounts (read-only)"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteRanking, Description: "Usage ranking by key and upstream account, with anomaly flags (read-only)"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteMonitor, Description: "Passive degradation monitor for Grok and Codex (heuristic, read-only)"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteAlerts, Description: "Page-only alert center: active alerts, mutes and history"},
		{Method: http.MethodPost, Path: managementBase + routeSuiteAlertsAck, Description: "Acknowledge an alert"},
		{Method: http.MethodPost, Path: managementBase + routeSuiteAlertsMute, Description: "Mute alerts of one source and object"},
		{Method: http.MethodPost, Path: managementBase + routeSuiteAlertsUnmute, Description: "Unmute alerts of one source and object"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteAlertSettings, Description: "Alert thresholds, cooldown and minimum severity (effective, defaults, config.yaml locks)"},
		{Method: http.MethodPut, Path: managementBase + routeSuiteAlertSettings, Description: "Save alert thresholds from the page (reset=true restores the defaults)"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteCandy, Description: "Candy test accounts and recent results (module candy_test)"},
		{Method: http.MethodPost, Path: managementBase + routeSuiteCandyRun, Description: "Run one confirmed candy test on one upstream account (spends quota; rate-limited)"},
		{Method: http.MethodGet, Path: managementBase + routeSuiteXAIDiag, Description: "Diagnose the xAI billing quota response (read-only, redacted)"},
	}, suiteAlerts7Routes()...)
}

func (a *App) suiteManagement(req ManagementRequest, suffix string) (ManagementResponse, bool) {
	if resp, ok := a.suiteAlerts7(req, suffix); ok {
		return resp, true
	}
	switch {
	case req.Method == http.MethodGet && suffix == routeSuiteClients:
		state, err := a.suite.ClientState()
		if err != nil {
			return JSONResponse(http.StatusInternalServerError, map[string]any{"error": err.Error()}), true
		}
		return JSONResponse(http.StatusOK, state), true
	case req.Method == http.MethodPut && suffix == routeSuiteModules:
		var body struct {
			ID      string `json:"id"`
			Mode    string `json:"mode"`
			Confirm bool   `json:"confirm"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return JSONResponse(http.StatusBadRequest, map[string]any{"error": "请求体不是有效 JSON"}), true
		}
		if body.ID == "candy_test" && body.Mode == "on" && !body.Confirm {
			return JSONResponse(http.StatusBadRequest, map[string]any{"error": "开启糖果测试需要确认（confirm: true）"}), true
		}
		modules, err := a.suite.SetModuleMode(body.ID, body.Mode)
		if err != nil {
			return JSONResponse(http.StatusConflict, map[string]any{"error": err.Error()}), true
		}
		return JSONResponse(http.StatusOK, map[string]any{"ok": true, "modules": modules}), true
	case req.Method == http.MethodPut && suffix == routeSuiteClientRules:
		saved, err := a.suite.SaveClientRules(req.Body)
		if err != nil {
			return JSONResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), true
		}
		return JSONResponse(http.StatusOK, saved), true
	case req.Method == http.MethodPost && (suffix == routeSuiteAlertsAck || suffix == routeSuiteAlertsMute || suffix == routeSuiteAlertsUnmute):
		return a.suiteAlertAction(req, suffix), true
	case req.Method == http.MethodPut && suffix == routeSuiteAlertSettings:
		var body struct {
			Settings map[string]any `json:"settings"`
			Reset    bool           `json:"reset"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return JSONResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), true
		}
		view, err := a.suite.SaveAlertSettings(body.Settings, body.Reset)
		if err != nil {
			return JSONResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), true
		}
		a.suite.AlertsDue(time.Time{}, true) // re-evaluate on the next page load
		return JSONResponse(http.StatusOK, view), true
	case req.Method == http.MethodPost && suffix == routeSuiteCandyRun:
		return a.suiteCandyRun(req), true
	case req.Method == http.MethodPost && suffix == routeSuiteClientClear:
		if err := a.suite.ClearClientStats(); err != nil {
			return JSONResponse(http.StatusConflict, map[string]any{"error": err.Error()}), true
		}
		return JSONResponse(http.StatusOK, map[string]any{"ok": true}), true
	}
	if req.Method != http.MethodGet {
		return ManagementResponse{}, false
	}
	switch suffix {
	case routeSuiteStatus:
		return JSONResponse(http.StatusOK, a.suite.Status()), true
	case routeSuiteForecast:
		return a.suiteForecast(req), true
	case routeSuiteRanking:
		return a.suiteRanking(req), true
	case routeSuiteMonitor:
		return a.suiteMonitor(req), true
	case routeSuiteAlerts:
		return a.suiteAlerts(req), true
	case routeSuiteCandy:
		return a.suiteCandy(req), true
	case routeSuiteAlertSettings:
		return JSONResponse(http.StatusOK, a.suite.AlertSettings()), true
	case routeSuiteXAIDiag:
		return a.suiteXAIDiag(req), true
	case routeSuiteModules:
		return JSONResponse(http.StatusOK, map[string]any{"modules": a.suite.Status().Modules}), true
	}
	return ManagementResponse{}, false
}

// suiteUI injects the status banner right after <body>.
func (a *App) suiteUI(page []byte) []byte {
	// The UI reads its plugin ID from this tag to build every API URL.
	page = bytes.Replace(page, []byte("<head>"), []byte(`<head><meta name="cpa-plugin-id" content="`+PluginID+`">`), 1)
	banner := a.suite.BannerHTML()
	if banner == "" {
		return page
	}
	marker := []byte("<body>")
	index := bytes.Index(page, marker)
	if index < 0 {
		return page
	}
	index += len(marker)
	out := make([]byte, 0, len(page)+len(banner))
	out = append(out, page[:index]...)
	out = append(out, banner...)
	return append(out, page[index:]...)
}

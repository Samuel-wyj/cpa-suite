package suite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Alert thresholds (0.7). Precedence: config.yaml alerts: > values saved
// from the page > the built-in defaults below (approved plan).

// AlertSettings are the tunable alert thresholds.
type AlertSettings struct {
	// Quota risk: alert only when remaining ≤ Remain* AND the 24 h pace runs
	// out before the reset. High when remaining ≤ High* or the pace runs out
	// at least EarlyFrac of the window before the reset.
	QuotaRemain5h      float64 `json:"quota_remain_5h"`
	QuotaRemainWeekly  float64 `json:"quota_remain_weekly"`
	QuotaHigh5h        float64 `json:"quota_high_5h"`
	QuotaHighWeekly    float64 `json:"quota_high_weekly"`
	QuotaEarlyFrac     float64 `json:"quota_early_frac"`
	QuotaQuiet5hMin    float64 `json:"quota_quiet_5h_minutes"`
	QuotaQuietWeeklyH  float64 `json:"quota_quiet_weekly_hours"`
	UsageMinActiveDays float64 `json:"usage_min_active_days"`
	UsageActiveUSD     float64 `json:"usage_active_usd"`
	UsageActiveReqs    float64 `json:"usage_active_requests"`
	UsageRatio         float64 `json:"usage_ratio"`
	UsageMinDeltaUSD   float64 `json:"usage_min_delta_usd"`
	UsageMinDeltaReqs  float64 `json:"usage_min_delta_requests"`
	UsageMinBaseUSD    float64 `json:"usage_min_base_usd"`
	UsageHighRatio     float64 `json:"usage_high_ratio"`
	UsageHighDeltaUSD  float64 `json:"usage_high_delta_usd"`
	UsageNewKeyUSD     float64 `json:"usage_new_key_usd"`
	UsageSeverity      string  `json:"usage_severity"` // info = shown in the list only (default)
	OpenAfter          float64 `json:"open_after_evaluations"`
	CloseAfterMin      float64 `json:"close_after_minutes"`
	ReopenCooldownH    float64 `json:"reopen_cooldown_hours"`
	// Lowest severity kept per source (info | medium | high).
	MinSeverity map[string]string `json:"min_severity"`
}

// DefaultAlertSettings are the approved defaults.
func DefaultAlertSettings() AlertSettings {
	return AlertSettings{QuotaRemain5h: 40, QuotaRemainWeekly: 30, QuotaHigh5h: 15, QuotaHighWeekly: 10, QuotaEarlyFrac: 0.25,
		QuotaQuiet5hMin: 60, QuotaQuietWeeklyH: 12,
		UsageMinActiveDays: 3, UsageActiveUSD: 0.5, UsageActiveReqs: 20, UsageRatio: 4, UsageMinDeltaUSD: 10, UsageMinDeltaReqs: 1000,
		UsageMinBaseUSD: 1, UsageHighRatio: 8, UsageHighDeltaUSD: 30, UsageNewKeyUSD: 20, UsageSeverity: "info",
		OpenAfter: 2, CloseAfterMin: 30, ReopenCooldownH: 6,
		MinSeverity: map[string]string{AlertQuota: "info", AlertUsage: "info", AlertBlocks: "info", AlertMonitor: "info", AlertAuthErrors: "info"}}
}

const alertSettingsKey = "alert_settings"

// AlertSettingsView is what the settings page shows.
type AlertSettingsView struct {
	Settings AlertSettings `json:"settings"`
	Defaults AlertSettings `json:"defaults"`
	Locked   []string      `json:"locked"` // keys fixed by config.yaml
}

func settingsMap(s AlertSettings) map[string]any {
	raw, _ := json.Marshal(s)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// mergeSettings overlays a key → value map on s, validating each value.
func mergeSettings(s AlertSettings, over map[string]any) (AlertSettings, []string, error) {
	base := settingsMap(s)
	var keys []string
	for k, v := range over {
		def, ok := base[k]
		if !ok {
			return s, nil, fmt.Errorf("未知告警设置 %q", k)
		}
		switch d := def.(type) {
		case float64:
			f, ok := toFloat(v)
			if !ok || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
				return s, nil, fmt.Errorf("告警设置 %s 必须是非负数字", k)
			}
			base[k] = f
		case string:
			str, _ := v.(string)
			if !validSeverity(str) {
				return s, nil, fmt.Errorf("告警设置 %s 只能是 info / medium / high", k)
			}
			base[k] = str
		case map[string]any:
			mv, ok := v.(map[string]any)
			if !ok {
				return s, nil, fmt.Errorf("告警设置 %s 必须是来源到级别的映射", k)
			}
			for src, sev := range mv {
				str, _ := sev.(string)
				if !ValidAlertSource(src) || !validSeverity(str) {
					return s, nil, fmt.Errorf("告警设置 %s.%s 无效", k, src)
				}
				d[src] = str
			}
			base[k] = d
		}
		keys = append(keys, k)
	}
	raw, _ := json.Marshal(base)
	var out AlertSettings
	if err := json.Unmarshal(raw, &out); err != nil {
		return s, nil, err
	}
	if out.QuotaRemain5h > 100 || out.QuotaRemainWeekly > 100 || out.QuotaHigh5h > 100 || out.QuotaHighWeekly > 100 || out.QuotaEarlyFrac > 1 {
		return s, nil, fmt.Errorf("百分比阈值不能超过 100，quota_early_frac 不能超过 1")
	}
	sort.Strings(keys)
	return out, keys, nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func validSeverity(s string) bool { return s == "info" || s == "medium" || s == "high" }

// normalizeYAML turns yaml-decoded nested maps into map[string]any.
func normalizeYAML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = normalizeYAML(val)
		}
		return x
	case map[any]any:
		m := map[string]any{}
		for k, val := range x {
			m[fmt.Sprint(k)] = normalizeYAML(val)
		}
		return m
	}
	return v
}

// AlertSettings returns the effective settings (cheap; read once per evaluation).
func (c *Controller) AlertSettings() AlertSettingsView {
	view := AlertSettingsView{Settings: DefaultAlertSettings(), Defaults: DefaultAlertSettings(), Locked: []string{}}
	var saved map[string]any
	_ = c.withExt(func(db *sql.DB) error {
		var raw string
		if db.QueryRow(`SELECT value FROM suite_meta WHERE key = ?`, alertSettingsKey).Scan(&raw) == nil {
			_ = json.Unmarshal([]byte(raw), &saved)
		}
		return nil
	})
	if saved != nil {
		if s, _, err := mergeSettings(view.Settings, saved); err == nil {
			view.Settings = s
		}
	}
	c.mu.Lock()
	cfg := c.cfg.Alerts
	c.mu.Unlock()
	if len(cfg) > 0 {
		norm, _ := normalizeYAML(cloneAny(cfg)).(map[string]any)
		if s, keys, err := mergeSettings(view.Settings, norm); err == nil {
			view.Settings, view.Locked = s, keys
		}
	}
	return view
}

func cloneAny(m map[string]any) map[string]any {
	raw, err := json.Marshal(normalizeYAML(copyMap(m)))
	out := map[string]any{}
	if err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ValidateAlertConfig checks config.yaml alerts: at startup.
func ValidateAlertConfig(m map[string]any) error {
	if len(m) == 0 {
		return nil
	}
	_, _, err := mergeSettings(DefaultAlertSettings(), cloneAny(m))
	return err
}

// SaveAlertSettings stores page changes. reset=true restores the defaults.
// Keys locked by config.yaml are rejected.
func (c *Controller) SaveAlertSettings(changes map[string]any, reset bool) (AlertSettingsView, error) {
	view := c.AlertSettings()
	for _, k := range view.Locked {
		if _, ok := changes[k]; ok && !reset {
			return view, fmt.Errorf("告警设置 %s 由 config.yaml 锁定；要在页面上修改，请先从 config.yaml 的 alerts 中删除它并重启", k)
		}
	}
	var saved map[string]any
	if !reset {
		_ = c.withExt(func(db *sql.DB) error {
			var raw string
			if db.QueryRow(`SELECT value FROM suite_meta WHERE key = ?`, alertSettingsKey).Scan(&raw) == nil {
				_ = json.Unmarshal([]byte(raw), &saved)
			}
			return nil
		})
		if saved == nil {
			saved = map[string]any{}
		}
		for k, v := range changes {
			if strings.TrimSpace(k) == "" {
				continue
			}
			if old, ok := saved[k].(map[string]any); ok {
				if nv, ok := v.(map[string]any); ok {
					for sk, sv := range nv {
						old[sk] = sv
					}
					continue
				}
			}
			saved[k] = v
		}
		if _, _, err := mergeSettings(DefaultAlertSettings(), cloneAny(saved)); err != nil {
			return view, err
		}
	}
	err := c.withExt(func(db *sql.DB) error {
		if reset {
			_, err := db.Exec(`DELETE FROM suite_meta WHERE key = ?`, alertSettingsKey)
			return err
		}
		raw, _ := json.Marshal(saved)
		_, err := db.Exec(`INSERT INTO suite_meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, alertSettingsKey, string(raw))
		return err
	})
	if err != nil {
		return view, err
	}
	return c.AlertSettings(), nil
}

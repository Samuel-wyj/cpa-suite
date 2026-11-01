package suite

import (
	"fmt"
	"os"
	"sort"
)

// Module modes.
const (
	ModeEnforce = "enforce" // core only: normal billing, admission and routing
	ModeObserve = "observe" // core only: watch-only, never blocks/schedules/charges
	ModeOn      = "on"
	ModeOff     = "off"
	ModePassive = "passive" // forecast: never call quota endpoints, use recorded headers only
)

// Module describes one switchable feature.
type Module struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Core        bool     `json:"core"`
	Available   bool     `json:"available"`
	Modes       []string `json:"modes"`
	Default     string   `json:"default"`
	Mode        string   `json:"mode"`
	Description string   `json:"description"`
	// Source of Mode: "default", "ui" (saved from the 模块 page in suite_meta)
	// or "config" (config.yaml modules: — wins and locks the switch in the UI).
	Source string `json:"source"`
	Locked bool   `json:"locked"`
}

// Module precedence (0.6): config.yaml modules: > saved UI switch > built-in default.
const moduleOverridesKey = "module_overrides"

// applyModuleOverrides applies saved UI switches to modules not set in config.yaml.
// Invalid saved values (e.g. a module or mode that no longer exists) are ignored.
func applyModuleOverrides(modules []Module, saved map[string]string) {
	for i := range modules {
		m := &modules[i]
		if m.Locked || !m.Available {
			continue
		}
		mode, ok := saved[m.ID]
		if !ok {
			continue
		}
		for _, candidate := range m.Modes {
			if candidate == mode {
				m.Mode, m.Source = mode, "ui"
			}
		}
	}
}

// registry lists every planned module. Modules that are not implemented in
// this build are listed so the framework and UI are stable, but they cannot
// be switched on yet.
func registry() []Module {
	return []Module{
		{ID: "core", Name: "计费 / 计划 / 额度 / 路由 / 调度", Core: true, Available: true, Modes: []string{ModeEnforce, ModeObserve}, Default: ModeEnforce,
			Description: "cpa-key-billing 原有核心功能。observe = 只观察：不拦截、不调度、不扣费"},
		{ID: "client_guard", Name: "客户端限制", Available: true, Modes: []string{ModeOn, ModeObserve, ModeOff}, Default: ModeOn,
			Description: "合并自 client-guard 0.1.2。on = 按规则执行（规则本身可设 仅记录/拦截）；observe = 只记录不拦截；off = 关闭。client-guard.so 仍在加载时自动暂停"},
		{ID: "forecast", Name: "额度美元预测（Codex / Grok）", Available: true, Modes: []string{ModeOn, ModePassive, ModeOff}, Default: ModeOn,
			Description: "只读。on = 用请求响应头里记录的官方额度，必要时按限频查询官方额度接口（每账号至多 10 分钟一次，有缓存，不消耗额度）；passive = 只用已记录的响应头，从不查询上游；off = 关闭"},
		{ID: "ranking", Name: "用量排行与异常", Available: true, Modes: []string{ModeOn, ModeOff}, Default: ModeOn,
			Description: "只读。按 Key / 上游账号统计今日、7 天、30 天的费用、tokens、请求数；Key 用量远超自身基线、客户端拦截激增时在页面上标记"},
		{ID: "grok_monitor", Name: "降智被动监控（Grok / Codex）", Available: true, Modes: []string{ModeOn, ModeOff}, Default: ModeOn,
			Description: "只读、被动。只分析正常请求经 usage.handle 带回的元数据（模型、tokens、耗时、错误码），不发探测请求、不消耗额度、不保存正文；最近 24 小时对比前 7 天基线，结果是启发式提示，不能证明降智"},
		{ID: "alerts", Name: "告警中心", Available: true, Modes: []string{ModeOn, ModeOff}, Default: ModeOn,
			Description: "只在页面显示，不推送。汇总额度风险、用量异常、客户端拦截激增、降智信号与上游认证错误；支持确认、静音、历史（保留 90 天）"},
		{ID: "auth_alerts", Name: "告警：上游认证错误", Available: true, Modes: []string{ModeOn, ModeOff}, Default: ModeOn,
			Description: "告警中心的一个来源：同一上游账号 15 分钟内 401/403 ≥ 5 次时提示（只读本地请求记录）"},
		{ID: "shared_quota", Name: "同事自查：共享上游额度", Available: true, Modes: []string{ModeOn, ModeOff}, Default: ModeOn,
			Description: "只读。API Key 用户在“我的余量”和 /usage（cc-switch 用量查询）里额外看到自己按路由规则能用到的上游账号（Codex / Grok）5 小时 / 周剩余百分比和重置时间；只用已缓存的读数，不请求上游，账号名遮挡；off = 只显示自己的套餐"},
		{ID: "candy_test", Name: "糖果测试（手动、消耗额度）", Available: true, Modes: []string{ModeOn, ModeOff}, Default: ModeOff,
			Description: "默认关闭。开启后可在降智监控页对选定的单个上游账号手动发起一次糖果题测试：每次真实请求上游，Codex 约 1.4 万输入 tokens，Grok 约 300；每个账号 10 分钟最多 1 次，不会自动或定时运行；测试请求不计入计费、用量统计和被动监控"},
	}
}

// ResolveModules validates configured modes and returns the effective list.
// Unknown modules and unsupported modes are configuration errors so typos are
// caught instead of silently ignored.
func ResolveModules(configured map[string]string) ([]Module, error) {
	modules := registry()
	byID := make(map[string]int, len(modules))
	for i := range modules {
		modules[i].Mode = modules[i].Default
		modules[i].Source = "default"
		byID[modules[i].ID] = i
	}
	ids := make([]string, 0, len(configured))
	for id := range configured {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		index, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("Unknown module %q in modules", id)
		}
		mode := configured[id]
		valid := false
		for _, candidate := range modules[index].Modes {
			valid = valid || candidate == mode
		}
		if !valid {
			if !modules[index].Available {
				return nil, fmt.Errorf("Module %q is not available in this build (%s)", id, Version)
			}
			return nil, fmt.Errorf("Module %q does not support mode %q (allowed: %v)", id, mode, modules[index].Modes)
		}
		modules[index].Mode = mode
		modules[index].Source, modules[index].Locked = "config", true
	}
	return modules, nil
}

// SetModuleMode switches one module at runtime and persists it in suite_meta.
// It refuses modules locked by config.yaml.
func (c *Controller) SetModuleMode(id, mode string) ([]Module, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ext == nil {
		return nil, fmt.Errorf("扩展库未打开")
	}
	index := -1
	for i := range c.modules {
		if c.modules[i].ID == id {
			index = i
		}
	}
	if index < 0 {
		return nil, fmt.Errorf("未知模块 %q", id)
	}
	m := &c.modules[index]
	if m.Locked {
		return nil, fmt.Errorf("模块 %s 由 config.yaml 锁定为 %s；要在页面上切换，请先从 config.yaml 的 modules 中删除它并重启", id, m.Mode)
	}
	valid := false
	for _, candidate := range m.Modes {
		valid = valid || candidate == mode
	}
	if !m.Available || !valid {
		return nil, fmt.Errorf("模块 %s 不支持模式 %q（可选：%v）", id, mode, m.Modes)
	}
	saved := map[string]string{}
	if _, err := c.ext.get(moduleOverridesKey, &saved); err != nil {
		return nil, err
	}
	if saved == nil {
		saved = map[string]string{}
	}
	saved[id] = mode
	if err := c.ext.put(moduleOverridesKey, saved); err != nil {
		return nil, err
	}
	m.Mode, m.Source = mode, "ui"
	switch id {
	case "core":
		c.scanLocked()
	case "client_guard":
		c.prepareClientModuleLocked()
	}
	fmt.Fprintf(os.Stderr, "[cpa-suite] module %s set to %s from the management UI\n", id, mode)
	return append([]Module{}, c.modules...), nil
}

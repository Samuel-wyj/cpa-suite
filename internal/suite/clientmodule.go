package suite

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cpa-key-billing/internal/suite/clientguard"
)

// Ext DB keys of the client module. They live in suite_meta, so no schema change.
const (
	cgRulesKey  = "client_guard.rules"
	cgStatsKey  = "client_guard.stats"
	cgImportKey = "client_guard.import"
)

// ClientGuardImport records the one-time import of client-guard 0.1.2 files.
type ClientGuardImport struct {
	SourceDir    string    `json:"source_dir"`
	FoundRules   bool      `json:"found_rules"`
	FoundStats   bool      `json:"found_stats"`
	RulesSHA256  string    `json:"rules_sha256,omitempty"`
	StatsSHA256  string    `json:"stats_sha256,omitempty"`
	KeyRules     int       `json:"key_rules"`
	DefaultMode  string    `json:"default_mode,omitempty"`
	StatsKeys    int       `json:"stats_keys"`
	RecentCount  int       `json:"recent_count"`
	ImportedAt   time.Time `json:"imported_at"`
	SourceIntact bool      `json:"source_unchanged"`
}

// ClientModuleStatus is part of the suite status and client page.
type ClientModuleStatus struct {
	Mode   string             `json:"mode"`
	Active bool               `json:"active"`
	Paused bool               `json:"paused"`
	Reason string             `json:"reason,omitempty"`
	Error  string             `json:"error,omitempty"`
	Import *ClientGuardImport `json:"import,omitempty"`
	Source string             `json:"source"`
}

// extBackend stores client-guard rules/stats in the suite extension DB.
type extBackend struct{ c *Controller }

func (b extBackend) read(key string) ([]byte, error) {
	b.c.extMu.Lock()
	defer b.c.extMu.Unlock()
	if b.c.ext == nil {
		return nil, os.ErrNotExist
	}
	var raw json.RawMessage
	ok, err := b.c.ext.get(key, &raw)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, os.ErrNotExist
	}
	return raw, nil
}

func (b extBackend) write(key string, data []byte) error {
	b.c.extMu.Lock()
	defer b.c.extMu.Unlock()
	if b.c.ext == nil {
		return fmt.Errorf("extension database closed")
	}
	return b.c.ext.put(key, json.RawMessage(data))
}

func (b extBackend) ReadRules() ([]byte, error) { return b.read(cgRulesKey) }
func (b extBackend) WriteRules(d []byte) error  { return b.write(cgRulesKey, d) }
func (b extBackend) ReadStats() ([]byte, error) { return b.read(cgStatsKey) }
func (b extBackend) WriteStats(d []byte) error  { return b.write(cgStatsKey, d) }
func (b extBackend) Describe() string           { return "CPA Suite 扩展库 " + b.c.extPath }

func optionalSHA256(path string) string {
	sum, err := fileSHA256(path)
	if err != nil {
		return ""
	}
	return sum
}

func (c *Controller) clientModuleMode() string {
	for _, m := range c.modules {
		if m.ID == "client_guard" {
			return m.Mode
		}
	}
	return ModeOff
}

func (c *Controller) clientGuardLoadedLocked() string {
	for _, co := range c.coexist {
		if co.PluginID == clientguard.StandalonePluginID {
			return co.Path
		}
	}
	return ""
}

// prepareClientModuleLocked runs after the ext DB is open and the plugin dir
// was scanned. Import happens once, only when client-guard.so is gone, and
// reads the client-guard files without modifying them.
func (c *Controller) prepareClientModuleLocked() {
	c.cgError = ""
	if err := clientguard.ValidateConfig(c.cfg.ClientGuard); err != nil {
		c.cgError = "client_guard 配置有误：" + err.Error()
	}
	mode := c.clientModuleMode()
	if mode == ModeOff {
		c.closeClientGuardLocked()
		return
	}
	var record ClientGuardImport
	imported, err := c.ext.get(cgImportKey, &record)
	if err != nil {
		c.cgError = "读取导入记录失败：" + err.Error()
		c.closeClientGuardLocked()
		return
	}
	if imported {
		c.cgImport = &record
	}
	source := c.cfg.ClientGuardImportFrom
	if source == "" {
		source = clientguard.ConfiguredDataDir(c.cfg.ClientGuard)
	}
	if source == "" {
		source = filepath.Join(c.cfg.PluginsDir, "client-guard")
	}
	c.cgPendingImport = !imported && !strings.EqualFold(source, "none")
	if c.cgPendingImport && c.clientGuardLoadedLocked() == "" {
		result, errImport := c.importClientGuardLocked(source)
		if errImport != nil {
			// Never fall back to empty rules: keep the module paused and retry next start.
			c.cgError = "导入 client-guard 规则失败（模块暂停，下次启动重试）：" + errImport.Error()
			c.closeClientGuardLocked()
			return
		}
		c.cgImport = result
		c.cgPendingImport = false
	}
	if c.cg == nil {
		c.cg = clientguard.New(extBackend{c: c})
	}
	c.cg.Configure(c.cfg.ClientGuard, 6)
}

func (c *Controller) importClientGuardLocked(dir string) (*ClientGuardImport, error) {
	rulesPath, statsPath := filepath.Join(dir, "rules.json"), filepath.Join(dir, "stats.json")
	beforeRules, beforeStats := optionalSHA256(rulesPath), optionalSHA256(statsPath)
	rules, statsRaw, err := clientguard.LoadDir(dir)
	if err != nil {
		return nil, err
	}
	record := &ClientGuardImport{SourceDir: dir, ImportedAt: c.now().UTC(), RulesSHA256: beforeRules, StatsSHA256: beforeStats}
	if rules != nil {
		raw, _ := json.Marshal(rules)
		if err := c.ext.put(cgRulesKey, json.RawMessage(raw)); err != nil {
			return nil, err
		}
		record.FoundRules, record.KeyRules, record.DefaultMode = true, len(rules.Keys), rules.Default.Mode
	}
	if statsRaw != nil {
		if err := c.ext.put(cgStatsKey, json.RawMessage(statsRaw)); err != nil {
			return nil, err
		}
		var s struct {
			Recent []json.RawMessage          `json:"recent"`
			Seen   map[string]json.RawMessage `json:"seen"`
		}
		_ = json.Unmarshal(statsRaw, &s)
		record.FoundStats, record.StatsKeys, record.RecentCount = true, len(s.Seen), len(s.Recent)
	}
	record.SourceIntact = optionalSHA256(rulesPath) == beforeRules && optionalSHA256(statsPath) == beforeStats
	if err := c.ext.put(cgImportKey, record); err != nil {
		return nil, err
	}
	return record, nil
}

func (c *Controller) closeClientGuardLocked() {
	if c.cg != nil {
		c.cg.Close()
		c.cg = nil
	}
}

// ClientIntercept runs the client check for request.intercept_before. It must
// run before billing admission so a blocked request takes no slot and is not
// charged. terminated=false means "continue to billing".
func (c *Controller) ClientIntercept(raw []byte) (out []byte, terminated bool) {
	if c == nil {
		return nil, false
	}
	c.WatchOnly() // refresh the plugin scan (client-guard.so may have been added)
	c.mu.Lock()
	guard, mode, loaded := c.cg, c.clientModuleMode(), c.clientGuardLoadedLocked()
	pending := c.cgPendingImport
	c.mu.Unlock()
	if guard == nil || mode == ModeOff || loaded != "" || pending {
		return nil, false
	}
	return guard.InterceptBefore(raw, clientguard.Options{Observe: mode == ModeObserve})
}

// ClientStatus reports the client module state.
func (c *Controller) ClientStatus() ClientModuleStatus {
	c.WatchOnly()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clientStatusLocked()
}

func (c *Controller) clientStatusLocked() ClientModuleStatus {
	st := ClientModuleStatus{Mode: c.clientModuleMode(), Error: c.cgError, Import: c.cgImport, Source: clientguard.SourceVersion}
	loaded := c.clientGuardLoadedLocked()
	switch {
	case st.Mode == ModeOff:
		st.Reason = "模块开关 client_guard: off"
	case loaded != "":
		st.Paused = true
		st.Reason = "client-guard.so 仍在加载（" + loaded + "）：本模块暂停，不记录、不拦截，由 client-guard 继续执行；移走它并重启 CPA 后自动导入并接管"
	case c.cgPendingImport:
		st.Paused = true
		st.Reason = "client-guard.so 已不在插件目录，但规则尚未导入：请重启 CPA 完成导入后接管"
	case c.cg == nil:
		st.Paused = true
		st.Reason = "模块未启动"
	default:
		st.Active = true
	}
	return st
}

// ClientState is GET /suite/clients.
func (c *Controller) ClientState() (map[string]any, error) {
	status := c.ClientStatus()
	c.mu.Lock()
	guard := c.cg
	c.mu.Unlock()
	out := map[string]any{"module": status}
	if guard != nil {
		out["state"] = guard.Snapshot()
	}
	return out, nil
}

// SaveClientRules is PUT /suite/clients/rules.
func (c *Controller) SaveClientRules(raw []byte) (any, error) {
	c.mu.Lock()
	guard := c.cg
	c.mu.Unlock()
	if guard == nil {
		return nil, fmt.Errorf("客户端限制模块未运行：%s", c.ClientStatus().Reason)
	}
	var rs clientguard.RuleSet
	if err := json.Unmarshal(raw, &rs); err != nil {
		return nil, fmt.Errorf("请求体不是有效 JSON: %w", err)
	}
	saved, state, err := guard.SaveRules(rs)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "rules": saved, "rules_from": state.RulesFrom, "warning": state.LastError}, nil
}

// ClearClientStats is POST /suite/clients/clear-stats.
func (c *Controller) ClearClientStats() error {
	c.mu.Lock()
	guard := c.cg
	c.mu.Unlock()
	if guard == nil {
		return fmt.Errorf("客户端限制模块未运行")
	}
	guard.ClearStats()
	return nil
}

package suite

import (
	"database/sql"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cpa-key-billing/internal/suite/clientguard"
)

// rescanInterval bounds how often the hot path re-checks the plugin directory
// for a conflicting library that appeared after start-up.
const rescanInterval = 30 * time.Second

// Status is served at GET <management>/suite/status.
type Status struct {
	Version      string             `json:"version"`
	UpstreamBase string             `json:"upstream_base"`
	WatchOnly    bool               `json:"watch_only"`
	WatchReasons []string           `json:"watch_reasons"`
	Conflicts    []Conflict         `json:"conflicts"`
	Coexisting   []Coexisting       `json:"coexisting"`
	Modules      []Module           `json:"modules"`
	StateFile    string             `json:"state_file"`
	ExtFile      string             `json:"ext_file"`
	ImportFrom   string             `json:"import_from"`
	Import       *ImportResult      `json:"import,omitempty"`
	ImportNote   string             `json:"import_note,omitempty"`
	PluginsDir   string             `json:"plugins_dir"`
	LastScanAt   time.Time          `json:"last_scan_at"`
	ClientGuard  ClientModuleStatus `json:"client_guard"`
}

// Controller owns suite state. It is safe for concurrent use.
type Controller struct {
	mu        sync.Mutex
	cfg       Config
	modules   []Module
	conflicts []Conflict
	coexist   []Coexisting
	imported  *ImportResult
	note      string
	ext       *extDB
	mainRO    *sql.DB // read-only view of the upstream database (forecast, ranking)
	mainPath  string
	extPath   string
	lastScan  time.Time
	watchOnly atomic.Bool
	nextScan  atomic.Int64
	now       func() time.Time
	maxSchema int
	validate  func([]byte) error

	// Client module (client-guard port). extMu serialises ext DB access from
	// the guard's background writer with Close.
	extMu sync.Mutex

	// monitor sample queue (see monitor_store.go).
	monMu    sync.Mutex
	monBuf   []MonitorSample
	monTimer *time.Timer

	// alert evaluation throttle (see alerts.go).
	alertMu         sync.Mutex
	alertEval       time.Time
	cg              *clientguard.Guard
	cgImport        *ClientGuardImport
	cgPendingImport bool
	cgError         string

	// page-configurable retention (see retention.go).
	retention atomic.Pointer[Retention]
}

// NewController creates a controller. maxSchema is the upstream schema
// version this build supports.
func NewController(maxSchema int) *Controller {
	return &Controller{now: time.Now, maxSchema: maxSchema}
}

// SetValidator installs the upstream config decoder. Prepare runs it on the
// upstream part of the YAML before any file is imported or created, so a
// config upstream would reject never leaves files behind.
func (c *Controller) SetValidator(validate func([]byte) error) { c.validate = validate }

// Prepare runs on every plugin.register/reconfigure before the upstream
// configure step. It validates suite keys, performs the one-time import,
// scans for conflicts and returns the YAML for the upstream decoder.
//
// Import policy (never creates an empty database by accident):
//   - state_file exists: use it, never import again.
//   - state_file missing, import_from set: copy import_from read-only; any
//     failure aborts configuration so CPA logs the error and nothing is created.
//   - state_file missing, import_from unset, but the cpa-key-billing default
//     database exists: abort and ask the operator to set import_from (or
//     import_from: none) explicitly, so data is never silently left behind.
//   - otherwise (or import_from: none): upstream creates a fresh database.
func (c *Controller) Prepare(raw []byte) ([]byte, error) {
	cfg, upstreamYAML, err := SplitConfig(raw)
	if err != nil {
		return nil, err
	}
	if c.validate != nil {
		if err := c.validate(upstreamYAML); err != nil {
			return nil, err
		}
	}
	modules, err := ResolveModules(cfg.Modules)
	if err != nil {
		return nil, err
	}
	if err := ValidateAlertConfig(cfg.Alerts); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var imported *ImportResult
	note := ""
	stateExists, err := regularFileExists(cfg.StateFile)
	if err != nil {
		return nil, err
	}
	source := cfg.ImportFrom
	switch {
	case stateExists:
	case strings.EqualFold(source, "none"):
		note = "import_from: none，已创建新的空数据库"
	case source != "":
		result, errImport := ImportDatabase(source, cfg.StateFile, c.maxSchema, c.now())
		if errImport != nil {
			return nil, fmt.Errorf("CPA Suite import from %s failed, nothing was created: %w", source, errImport)
		}
		imported = &result
	default:
		legacy, errLegacy := regularFileExists(LegacyStateFile)
		if errLegacy != nil {
			return nil, errLegacy
		}
		if legacy {
			return nil, fmt.Errorf("Found cpa-key-billing database %s but import_from is not set; set import_from to the real cpa-key-billing state_file (or import_from: none to start empty)", LegacyStateFile)
		}
		note = "未配置 import_from，已创建新的空数据库"
	}

	extPath := ExtPath(cfg.StateFile)
	if c.ext == nil || c.extPath != extPath {
		ext, errExt := openExt(extPath)
		if errExt != nil {
			return nil, errExt
		}
		if c.ext != nil {
			_ = c.ext.close()
		}
		c.ext, c.extPath = ext, extPath
		c.retention.Store(nil)
	}
	if imported != nil {
		if err := c.ext.put("import", imported); err != nil {
			return nil, fmt.Errorf("Record import: %w", err)
		}
	}
	var stored ImportResult
	if ok, errGet := c.ext.get("import", &stored); errGet == nil && ok {
		imported = &stored
	}
	if note != "" {
		_ = c.ext.put("import_note", note)
	} else {
		_, _ = c.ext.get("import_note", &note)
	}
	_ = c.ext.put("version", Version)
	var savedModules map[string]string
	if ok, errGet := c.ext.get(moduleOverridesKey, &savedModules); errGet == nil && ok {
		applyModuleOverrides(modules, savedModules)
	}

	c.cfg, c.modules, c.imported, c.note = cfg, modules, imported, note
	c.scanLocked()
	c.prepareClientModuleLocked()
	return upstreamYAML, nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", path)
	}
	return true, nil
}

func (c *Controller) coreObserve() bool {
	for _, m := range c.modules {
		if m.ID == "core" {
			return m.Mode == ModeObserve
		}
	}
	return false
}

func (c *Controller) scanLocked() {
	c.conflicts, c.coexist = ScanPlugins(c.cfg.PluginsDir)
	c.lastScan = c.now()
	c.watchOnly.Store(len(c.conflicts) > 0 || c.coreObserve())
	c.nextScan.Store(c.lastScan.Add(rescanInterval).UnixNano())
}

// WatchOnly reports whether billing, admission and scheduling must be
// bypassed. It re-scans the plugin directory at most every rescanInterval.
func (c *Controller) WatchOnly() bool {
	if c == nil {
		return false
	}
	if c.now().UnixNano() >= c.nextScan.Load() {
		c.mu.Lock()
		if c.now().UnixNano() >= c.nextScan.Load() && c.cfg.PluginsDir != "" {
			c.scanLocked()
		}
		c.mu.Unlock()
	}
	return c.watchOnly.Load()
}

// Status returns a snapshot for the management API and UI.
func (c *Controller) Status() Status {
	c.WatchOnly()
	c.mu.Lock()
	defer c.mu.Unlock()
	reasons := []string{}
	for _, conflict := range c.conflicts {
		reasons = append(reasons, fmt.Sprintf("检测到冲突插件 %s（%s）：%s", conflict.PluginID, conflict.Path, conflict.Reason))
	}
	if c.coreObserve() {
		reasons = append(reasons, "模块开关 core 设置为 observe（只观察）")
	}
	abs := func(p string) string {
		if p == "" {
			return ""
		}
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	return Status{
		Version: Version, UpstreamBase: UpstreamVersion,
		WatchOnly: c.watchOnly.Load(), WatchReasons: reasons,
		Conflicts: append([]Conflict{}, c.conflicts...), Coexisting: append([]Coexisting{}, c.coexist...),
		Modules: append([]Module{}, c.modules...), StateFile: abs(c.cfg.StateFile), ExtFile: abs(c.extPath),
		ImportFrom: c.cfg.ImportFrom, Import: c.imported, ImportNote: c.note,
		PluginsDir: abs(c.cfg.PluginsDir), LastScanAt: c.lastScan,
		ClientGuard: c.clientStatusLocked(),
	}
}

// BannerHTML is injected at the top of the upstream UI page. Since 0.6 it is
// empty on a healthy install: version and import record live in 设置 → 模块 → 关于.
// Only real problems are shown (watch-only, client-guard paused / failed), each
// dismissible for the current page view.
func (c *Controller) BannerHTML() string {
	if c == nil {
		return ""
	}
	status := c.Status()
	var b strings.Builder
	if status.WatchOnly {
		b.WriteString(`<div id="cpa-suite-banner" style="font:13px/1.6 -apple-system,'PingFang SC',sans-serif;padding:10px 16px;`)
		b.WriteString(`background:#ef4444;color:#fff;">` + bannerClose + `<b>CPA Suite 只观察模式：不拦截、不调度、不扣费。</b>`)
		for _, reason := range status.WatchReasons {
			b.WriteString(`<div>` + html.EscapeString(reason) + `</div>`)
		}
		b.WriteString(`<div>处理方法：把冲突插件移出 plugins 目录（或把 modules.core 改回 enforce）后重启 CPA。</div></div>`)
		return b.String()
	}
	if cg := status.ClientGuard; cg.Paused || cg.Error != "" {
		b.WriteString(`<div id="cpa-suite-client-banner" style="font:13px/1.6 -apple-system,'PingFang SC',sans-serif;padding:8px 16px;background:#f59e0b;color:#1f2937;">` + bannerClose)
		if cg.Paused {
			b.WriteString(`<b>客户端限制：</b>` + html.EscapeString(cg.Reason))
		}
		if cg.Error != "" {
			b.WriteString(`<div><b>客户端限制错误：</b>` + html.EscapeString(cg.Error) + `</div>`)
		}
		b.WriteString(`</div>`)
	}
	return b.String()
}

const bannerClose = `<button type="button" aria-label="关闭" title="关闭（本次页面）" onclick="this.parentNode.remove()" style="float:right;background:none;border:0;color:inherit;font-size:16px;cursor:pointer">×</button>`

// Close releases the extension database.
func (c *Controller) Close() {
	if c == nil {
		return
	}
	_ = c.FlushSamples() // monitor queue → ext DB before closing it
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeClientGuardLocked() // flushes stats into the ext DB
	if c.mainRO != nil {
		_ = c.mainRO.Close()
		c.mainRO, c.mainPath = nil, ""
	}
	c.extMu.Lock()
	defer c.extMu.Unlock()
	if c.ext != nil {
		_ = c.ext.close()
		c.ext = nil
		c.extPath = ""
	}
}

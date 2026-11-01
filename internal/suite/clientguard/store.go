package clientguard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// pluginConfig 对应 config.yaml 中 plugins.configs.client-guard。
type pluginConfig struct {
	Enabled      *bool    `yaml:"enabled"`
	Priority     int      `yaml:"priority"`
	DefaultMode  string   `yaml:"default_mode"`
	DefaultAllow []string `yaml:"default_allow"`
	FailOpen     *bool    `yaml:"fail_open"`
	DataDir      string   `yaml:"data_dir"`
	RecentLimit  int      `yaml:"recent_limit"`
	BlockMessage string   `yaml:"block_message"`
	ExtraHeaders []string `yaml:"extra_headers"`
}

type runtimeConfig struct {
	DefaultMode  string   `json:"default_mode"`
	DefaultAllow []string `json:"default_allow"`
	FailOpen     bool     `json:"fail_open"`
	DataDir      string   `json:"data_dir"`
	RecentLimit  int      `json:"recent_limit"`
	BlockMessage string   `json:"block_message"`
	ExtraHeaders []string `json:"extra_headers"`
}

const (
	defaultDataDir      = "plugins/client-guard"
	defaultRecentLimit  = 300
	maxRecentLimit      = 2000
	maxScopes           = 200
	maxClientsPerScope  = 50
	defaultBlockMessage = "此 API Key 不允许使用当前客户端，请改用管理员允许的客户端（例如 Codex）。"
)

func parseConfig(raw []byte) (runtimeConfig, error) {
	var pc pluginConfig
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &pc); err != nil {
			return runtimeConfig{}, fmt.Errorf("解析 client-guard 配置失败: %w", err)
		}
	}
	rc := runtimeConfig{
		DefaultMode:  strings.ToLower(strings.TrimSpace(pc.DefaultMode)),
		DefaultAllow: pc.DefaultAllow,
		FailOpen:     true,
		DataDir:      strings.TrimSpace(pc.DataDir),
		RecentLimit:  pc.RecentLimit,
		BlockMessage: strings.TrimSpace(pc.BlockMessage),
	}
	if rc.DefaultMode == "" {
		rc.DefaultMode = ModeLog
	}
	if !validMode(rc.DefaultMode, false) {
		return rc, fmt.Errorf("default_mode 只能是 off / log-only / enforce，当前为 %q", rc.DefaultMode)
	}
	if rc.DefaultAllow == nil {
		rc.DefaultAllow = []string{CodexFamilyPattern}
	}
	if pc.FailOpen != nil {
		rc.FailOpen = *pc.FailOpen
	}
	if rc.DataDir == "" {
		rc.DataDir = defaultDataDir
	}
	if rc.RecentLimit <= 0 {
		rc.RecentLimit = defaultRecentLimit
	}
	if rc.RecentLimit > maxRecentLimit {
		rc.RecentLimit = maxRecentLimit
	}
	if pc.ExtraHeaders == nil {
		rc.ExtraHeaders = []string{"X-Client"}
	} else {
		seen := map[string]bool{}
		rc.ExtraHeaders = []string{}
		for _, n := range pc.ExtraHeaders {
			cn := canonHeader(n)
			if cn == "" || seen[cn] {
				continue
			}
			if !headerNameRe.MatchString(cn) {
				return rc, fmt.Errorf("extra_headers 里的请求头名 %q 无效", n)
			}
			if sensitiveHeaders[cn] {
				return rc, fmt.Errorf("extra_headers 不允许包含敏感请求头 %s", cn)
			}
			seen[cn] = true
			rc.ExtraHeaders = append(rc.ExtraHeaders, cn)
		}
		if len(rc.ExtraHeaders) > 10 {
			return rc, fmt.Errorf("extra_headers 最多 10 个")
		}
	}
	if rc.BlockMessage == "" {
		rc.BlockMessage = defaultBlockMessage
	}
	return rc, nil
}

// LogEntry 是一条请求记录（不含明文 key）。
type LogEntry struct {
	Time       string `json:"time"`
	Scope      string `json:"scope"`
	UA         string `json:"ua"`
	Originator string `json:"originator"`
	Client     string `json:"client"`
	// 0.1.2 新增：更多识别头、预设与建议特征（旧 stats.json 没有这些字段也能读取）
	XTitle      string            `json:"x_title,omitempty"`
	HTTPReferer string            `json:"http_referer,omitempty"`
	Referer     string            `json:"referer,omitempty"`
	XClientName string            `json:"x_client_name,omitempty"`
	Custom      map[string]string `json:"custom,omitempty"`
	Preset      string            `json:"preset,omitempty"`
	Suggest     string            `json:"suggest,omitempty"`
	Path        string            `json:"path,omitempty"`
	Model       string            `json:"model,omitempty"`
	Format      string            `json:"format,omitempty"`
	Mode        string            `json:"mode"`
	Matched     string            `json:"matched,omitempty"`
	Result      string            `json:"result"` // allowed / blocked / not-in-list(log-only) / unchecked
}

type ClientStat struct {
	Client   string `json:"client"`
	SampleUA string `json:"sample_ua"`
	// 0.1.2 新增
	Sample    *ClientHeaders `json:"sample,omitempty"`
	Preset    string         `json:"preset,omitempty"`
	Suggest   string         `json:"suggest,omitempty"`
	Count     int64          `json:"count"`
	Blocked   int64          `json:"blocked"`
	FirstSeen string         `json:"first_seen"`
	LastSeen  string         `json:"last_seen"`
}

type stats struct {
	Recent []LogEntry                        `json:"recent"`
	Seen   map[string]map[string]*ClientStat `json:"seen"`
	// Daily 按北京时间日期（2006-01-02）计数；0.1.1 新增，旧 stats.json 没有该字段也能读取。
	Daily map[string]*DayCount `json:"daily,omitempty"`
}

// DayCount 是某一天的请求计数。
type DayCount struct {
	Requests  int64 `json:"requests"`
	Blocked   int64 `json:"blocked"`
	NotInList int64 `json:"not_in_list"`
}

// Summary 是页面顶部统计卡片的数据。
type Summary struct {
	Today          string `json:"today"`
	TodayRequests  int64  `json:"today_requests"`
	TodayBlocked   int64  `json:"today_blocked"`
	TodayNotInList int64  `json:"today_not_in_list"`
	TotalRequests  int64  `json:"total_requests"`
	TotalBlocked   int64  `json:"total_blocked"`
	KeysSeen       int    `json:"keys_seen"`
	ClientsSeen    int    `json:"clients_seen"`
	KeysWithRules  int    `json:"keys_with_rules"`
	EnforcedKeys   int    `json:"enforced_keys"`
}

const maxDailyDays = 31

type guardState struct {
	migrated   int // 启动时迁移的旧预设特征数
	mu         sync.RWMutex
	cfg        runtimeConfig
	configured bool
	hostSchema uint32
	rules      RuleSet
	compiled   compiledRuleSet
	rulesFrom  string
	stats      stats
	dirty      bool
	lastError  string
	lastSaveAt string
	stopFlush  chan struct{}
	loc        *time.Location
	// CPA Suite: where rules/stats live. nil = client-guard's own files in DataDir.
	backend Backend
}

// Backend persists rules and stats. Read returns an error satisfying
// os.IsNotExist when nothing has been stored yet.
type Backend interface {
	ReadRules() ([]byte, error)
	WriteRules([]byte) error
	ReadStats() ([]byte, error)
	WriteStats([]byte) error
	Describe() string
}

// DirBackend is client-guard 0.1.2's on-disk layout (rules.json / stats.json).
type DirBackend struct{ Dir string }

func (d DirBackend) ReadRules() ([]byte, error) {
	return os.ReadFile(filepath.Join(d.Dir, "rules.json"))
}
func (d DirBackend) WriteRules(b []byte) error {
	return atomicWrite(filepath.Join(d.Dir, "rules.json"), b)
}
func (d DirBackend) ReadStats() ([]byte, error) {
	return os.ReadFile(filepath.Join(d.Dir, "stats.json"))
}
func (d DirBackend) WriteStats(b []byte) error {
	return atomicWrite(filepath.Join(d.Dir, "stats.json"), b)
}
func (d DirBackend) Describe() string { return filepath.Join(d.Dir, "rules.json") }

func (g *guardState) store() Backend {
	if g.backend != nil {
		return g.backend
	}
	return DirBackend{Dir: g.cfg.DataDir}
}

func newGuardState() *guardState {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return &guardState{loc: loc, stats: stats{Seen: map[string]map[string]*ClientStat{}}}
}

func (g *guardState) now() string { return time.Now().In(g.loc).Format("2006-01-02 15:04:05") }

func (g *guardState) rulesPath() string { return g.store().Describe() }

// configure 在 plugin.register / plugin.reconfigure 时调用。
func (g *guardState) configure(cfg runtimeConfig, hostSchema uint32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	dirChanged := !g.configured || (g.backend == nil && g.cfg.DataDir != cfg.DataDir)
	g.cfg = cfg
	g.hostSchema = hostSchema
	g.configured = true
	g.lastError = ""

	rules := RuleSet{Default: Rule{Mode: cfg.DefaultMode, Allow: cfg.DefaultAllow}, Keys: map[string]Rule{}}
	g.rulesFrom = "config.yaml 默认值"
	if raw, err := g.store().ReadRules(); err == nil {
		var loaded RuleSet
		if err := json.Unmarshal(raw, &loaded); err != nil {
			g.lastError = "rules.json 解析失败，已改用 config.yaml 默认值: " + err.Error()
		} else if norm, err := NormalizeRuleSet(loaded); err != nil {
			g.lastError = "rules.json 校验失败，已改用 config.yaml 默认值: " + err.Error()
		} else {
			rules = norm
			g.rulesFrom = g.rulesPath()
			if migrated, n := MigrateLegacyPatterns(rules); n > 0 {
				raw, _ := json.MarshalIndent(migrated, "", "  ")
				if err := g.store().WriteRules(raw); err != nil {
					warnf("旧预设特征迁移写盘失败（本次仅在内存中生效）: %v", err)
				}
				rules = migrated
				g.migrated = n
				infof("已把 %d 处旧预设特征 re:^codex[_-] 等更新为新特征（%s 等，覆盖 Codex Desktop）", n, CodexFamilyPattern)
			}
		}
	} else if !os.IsNotExist(err) {
		g.lastError = "读取 rules.json 失败: " + err.Error()
	}
	if g.lastError != "" {
		warnf("%s", g.lastError)
	}
	g.setRulesLocked(rules)

	if dirChanged {
		g.loadStatsLocked()
	}
	g.trimLocked()
	if g.stopFlush == nil {
		g.stopFlush = make(chan struct{})
		go g.flushLoop(g.stopFlush)
	}
}

func (g *guardState) setRulesLocked(rs RuleSet) {
	norm, err := NormalizeRuleSet(rs)
	if err != nil {
		norm = RuleSet{Default: Rule{Mode: ModeLog, Allow: []string{CodexFamilyPattern}}, Keys: map[string]Rule{}}
	}
	c, _ := compileRuleSet(norm)
	g.rules = norm
	g.compiled = c
}

// saveRules 由管理页面调用：校验 → 原子写盘 → 生效。
func (g *guardState) saveRules(rs RuleSet) (RuleSet, error) {
	norm, err := NormalizeRuleSet(rs)
	if err != nil {
		return rs, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	raw, _ := json.MarshalIndent(norm, "", "  ")
	if err := g.store().WriteRules(raw); err != nil {
		g.lastError = "保存 rules.json 失败（规则仅在内存中生效，重启后丢失）: " + err.Error()
		warnf("%s", g.lastError)
		g.setRulesLocked(norm)
		g.rulesFrom = "内存（写盘失败）"
		return norm, nil
	}
	g.setRulesLocked(norm)
	g.rulesFrom = g.rulesPath()
	g.lastError = ""
	return norm, nil
}

func (g *guardState) decide(scope, ua, originator string) (Decision, runtimeConfig) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.compiled.decide(scope, ua, originator), g.cfg
}

// headerNames 返回请求时需要读取的自定义头：extra_headers + 规则里 header: 引用的头。
func (g *guardState) headerNames() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append(append([]string{}, g.cfg.ExtraHeaders...), g.compiled.referencedHeaders()...)
}

func (g *guardState) decideHeaders(scope string, h ClientHeaders) (Decision, runtimeConfig) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.compiled.decideHeaders(scope, h), g.cfg
}

func (e LogEntry) headers() ClientHeaders {
	return ClientHeaders{UA: e.UA, Originator: e.Originator, XTitle: e.XTitle, HTTPReferer: e.HTTPReferer, Referer: e.Referer, XClientName: e.XClientName, Custom: e.Custom}
}

func (g *guardState) record(e LogEntry, blocked bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats.Recent = append(g.stats.Recent, e)
	m := g.stats.Seen[e.Scope]
	if m == nil {
		if len(g.stats.Seen) >= maxScopes {
			g.evictOldestScopeLocked()
		}
		m = map[string]*ClientStat{}
		g.stats.Seen[e.Scope] = m
	}
	cs := m[e.Client]
	if cs == nil {
		if len(m) >= maxClientsPerScope {
			evictOldestClient(m)
		}
		cs = &ClientStat{Client: e.Client, FirstSeen: e.Time}
		m[e.Client] = cs
	}
	cs.Count++
	if blocked {
		cs.Blocked++
	}
	cs.LastSeen = e.Time
	cs.SampleUA = e.UA
	sample := e.headers()
	cs.Sample = &sample
	cs.Preset, cs.Suggest = e.Preset, e.Suggest
	g.countDayLocked(e)
	g.trimLocked()
	g.dirty = true
}

func (g *guardState) countDayLocked(e LogEntry) {
	day := e.Time
	if len(day) >= 10 {
		day = day[:10]
	}
	if g.stats.Daily == nil {
		g.stats.Daily = map[string]*DayCount{}
	}
	d := g.stats.Daily[day]
	if d == nil {
		d = &DayCount{}
		g.stats.Daily[day] = d
		for len(g.stats.Daily) > maxDailyDays { // 只保留最近 31 天
			oldest := ""
			for k := range g.stats.Daily {
				if oldest == "" || k < oldest {
					oldest = k
				}
			}
			delete(g.stats.Daily, oldest)
		}
	}
	d.Requests++
	switch e.Result {
	case "blocked":
		d.Blocked++
	case "not-in-list":
		d.NotInList++
	}
}

// summaryLocked 汇总统计卡片数据；调用方需持有读锁。
func (g *guardState) summaryLocked(today string) Summary {
	sum := Summary{Today: today, KeysSeen: len(g.stats.Seen)}
	clients := map[string]struct{}{}
	for _, m := range g.stats.Seen {
		for name, c := range m {
			sum.TotalRequests += c.Count
			sum.TotalBlocked += c.Blocked
			clients[name] = struct{}{}
		}
	}
	sum.ClientsSeen = len(clients)
	if d := g.stats.Daily[today]; d != nil {
		sum.TodayRequests, sum.TodayBlocked, sum.TodayNotInList = d.Requests, d.Blocked, d.NotInList
	}
	for _, r := range g.rules.Keys {
		if (r.Mode != "" && r.Mode != ModeInherit) || len(r.Allow) > 0 {
			sum.KeysWithRules++
		}
		eff := r.Mode
		if eff == "" || eff == ModeInherit {
			eff = g.rules.Default.Mode
		}
		if eff == ModeEnforce {
			sum.EnforcedKeys++
		}
	}
	return sum
}

func (g *guardState) trimLocked() {
	limit := g.cfg.RecentLimit
	if limit <= 0 {
		limit = defaultRecentLimit
	}
	if n := len(g.stats.Recent); n > limit {
		g.stats.Recent = append([]LogEntry(nil), g.stats.Recent[n-limit:]...)
	}
}

func (g *guardState) evictOldestScopeLocked() {
	oldest, oldestTime := "", "9999"
	for s, m := range g.stats.Seen {
		last := ""
		for _, c := range m {
			if c.LastSeen > last {
				last = c.LastSeen
			}
		}
		if last < oldestTime {
			oldest, oldestTime = s, last
		}
	}
	delete(g.stats.Seen, oldest)
}

func evictOldestClient(m map[string]*ClientStat) {
	oldest, t := "", "9999"
	for k, c := range m {
		if c.LastSeen < t {
			oldest, t = k, c.LastSeen
		}
	}
	delete(m, oldest)
}

func (g *guardState) clearStats() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats = stats{Seen: map[string]map[string]*ClientStat{}}
	g.dirty = true
}

func (g *guardState) loadStatsLocked() {
	g.stats = stats{Seen: map[string]map[string]*ClientStat{}}
	raw, err := g.store().ReadStats()
	if err != nil {
		return
	}
	var s stats
	if err := json.Unmarshal(raw, &s); err != nil {
		warnf("stats.json 解析失败，已忽略: %v", err)
		return
	}
	if s.Seen == nil {
		s.Seen = map[string]map[string]*ClientStat{}
	}
	if s.Daily == nil {
		s.Daily = map[string]*DayCount{}
	}
	g.stats = s
}

func (g *guardState) flushLoop(stop chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			g.flush()
		}
	}
}

func (g *guardState) flush() {
	g.mu.Lock()
	if !g.dirty || !g.configured {
		g.mu.Unlock()
		return
	}
	raw, err := json.Marshal(g.stats)
	backend := g.store()
	g.dirty = false
	g.mu.Unlock()
	if err != nil {
		return
	}
	if err := backend.WriteStats(raw); err != nil {
		warnf("写入 stats.json 失败: %v", err)
		return
	}
	g.mu.Lock()
	g.lastSaveAt = g.now()
	g.mu.Unlock()
}

func (g *guardState) shutdown() {
	g.flush()
	g.mu.Lock()
	if g.stopFlush != nil {
		close(g.stopFlush)
		g.stopFlush = nil
	}
	g.mu.Unlock()
}

type snapshotView struct {
	Plugin     string                  `json:"plugin"`
	Version    string                  `json:"version"`
	HostSchema uint32                  `json:"host_schema_version"`
	Config     runtimeConfig           `json:"config"`
	Rules      RuleSet                 `json:"rules"`
	RulesFrom  string                  `json:"rules_from"`
	Seen       map[string][]ClientStat `json:"seen"`
	Recent     []LogEntry              `json:"recent"`
	Presets    []Preset                `json:"presets"`
	Outdated   []OutdatedPattern       `json:"outdated_patterns"`
	Mismatch   []FamilyMismatch        `json:"family_mismatches"`
	Migrated   int                     `json:"migrated_patterns,omitempty"`
	Summary    Summary                 `json:"summary"`
	LastError  string                  `json:"last_error,omitempty"`
	LastSaveAt string                  `json:"last_stats_save_at,omitempty"`
	Now        string                  `json:"now"`
}

func (g *guardState) snapshot() snapshotView {
	g.mu.RLock()
	defer g.mu.RUnlock()
	seen := map[string][]ClientStat{}
	for s, m := range g.stats.Seen {
		list := make([]ClientStat, 0, len(m))
		for _, c := range m {
			list = append(list, *c)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Count > list[j].Count })
		seen[s] = list
	}
	recent := make([]LogEntry, len(g.stats.Recent))
	for i := range g.stats.Recent { // 新的在前
		recent[i] = g.stats.Recent[len(g.stats.Recent)-1-i]
	}
	keys := map[string]Rule{}
	for k, v := range g.rules.Keys {
		keys[k] = v
	}
	now := g.now()
	return snapshotView{
		Summary: g.summaryLocked(now[:10]),
		Plugin:  pluginID, Version: pluginVersion, HostSchema: g.hostSchema,
		Config: g.cfg, Rules: RuleSet{Default: g.rules.Default, Keys: keys}, RulesFrom: g.rulesFrom,
		Seen: seen, Recent: recent, Presets: presets, Outdated: OutdatedPatterns(g.rules),
		Mismatch: familyMismatches(g.compiled, g.rules, g.stats.Seen), Migrated: g.migrated, LastError: g.lastError, LastSaveAt: g.lastSaveAt, Now: now,
	}
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func infof(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[cpa-suite client_guard] INFO "+format+"\n", args...)
}

func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[cpa-suite client_guard] WARN "+format+"\n", args...)
}

func isNotExist(err error) bool { return os.IsNotExist(err) }
